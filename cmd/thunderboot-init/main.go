// Command thunderboot-init is PID 1 for a thundersnap appliance VM.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const newRoot = "/newroot"

func main() {
	log.SetPrefix("thunderboot-init: ")
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if err := boot(); err != nil {
		log.Printf("boot failed: %v", err)
		// Remain alive so a boot failure and its serial log can be inspected.
		for {
			time.Sleep(time.Hour)
		}
	}
}

func boot() error {
	mounts := []struct {
		source, target, fstype, data string
		flags                        uintptr
	}{
		{"proc", "/proc", "proc", "", 0},
		{"sysfs", "/sys", "sysfs", "", 0},
		{"devtmpfs", "/dev", "devtmpfs", "mode=0755", 0},
		{"devpts", "/dev/pts", "devpts", "mode=0620", 0},
		{"tmpfs", "/run", "tmpfs", "mode=0755", 0},
		{"tmpfs", "/tmp", "tmpfs", "mode=1777", 0},
		{"cgroup2", "/sys/fs/cgroup", "cgroup2", "", 0},
	}
	for _, m := range mounts {
		if err := mount(m.source, m.target, m.fstype, m.flags, m.data); err != nil {
			return err
		}
	}
	rootDevice, err := setupStorage()
	if err != nil {
		return err
	}
	if err := mount(rootDevice, newRoot, "btrfs", 0, "compress=zstd"); err != nil {
		return err
	}

	params, err := kernelParams()
	if err != nil {
		return err
	}
	if params["thundersnap.testonly"] == "storage" {
		log.Printf("THUNDERBOOT STORAGE OK: %s", rootDevice)
		return syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
	}
	logMemory("before installing appliance")
	if err := installAppliance(); err != nil {
		return err
	}
	if err := switchRoot(); err != nil {
		return err
	}
	logMemory("after switch_root")
	for _, tool := range []string{"/bin/btrfs", "/bin/mkfs.btrfs"} {
		out, err := exec.Command(tool, "--version").CombinedOutput()
		if err != nil {
			return fmt.Errorf("verify %s: %w: %s", tool, err, out)
		}
		log.Printf("installed %s", strings.TrimSpace(string(out)))
	}

	env := append(os.Environ(), "PATH=/bin")
	if authKey := params["thunderboot.authkey"]; authKey != "" {
		// The host supplies a test/automation auth key through the VM kernel
		// command line. Do not persist it in appliance metadata or logs.
		env = append(env, "TS_AUTHKEY="+authKey)
	}
	// Stay PID 1 and supervise the daemon (fix #1: a reaping PID 1). The old
	// design Exec'd into thunderboot-logrelay, making the relay PID 1 — but it
	// never reaped, which was the root cause of the permanent-wedge bug. See
	// runSupervisor.
	return runSupervisor(env)
}

// runSupervisor makes PID 1 (this process) the appliance's permanent reaper
// and lifecycle supervisor, fixing the root cause of the thunderboot
// permanent-wedge bug (see container-init-wedged-plan2.md, fix #1).
//
// Background: a container session's host-side chain is vshd -> `ts nsenter`
// (stage1, host PID ns) -> fork stage2 (`ts session-serve`, container PID ns).
// When the session is torn down via cgroup.kill while stage2 is alive, stage1
// and stage2 are SIGKILLed together; if stage1 loses the reap race, stage2's
// zombie is orphaned and reparents to the VM's PID 1. stage2 is a member of its
// container PID namespace, so when that namespace's container-init later exits,
// the kernel's zap_pid_ns_processes() blocks waiting for the zombie to be
// reaped — and the VM's PID 1 (formerly logrelay, which never reaped) never
// reaps it. containerns.stopEntry's cmd.Wait on container-init then hangs
// unboundedly, e.stopped is never closed, and every future session to that
// frame blocks in GetOrCreate on <-stopped. Permanent wedge.
//
// The fix: PID 1 itself reaps. This function spawns thundersnapd as a tracked
// child and runs a global wait4(-1) loop, so any orphaned descendant that
// reparents to PID 1 is reaped and zap_pid_ns_processes can complete. When
// thundersnapd dies, PID 1 exits with the daemon's status — the one legitimate
// PID-1 exit, which tears down the VM (PID-1 exit reaps the whole namespace).
//
// The relay (thunderboot-logrelay) is still used, but now as a fire-and-forget
// transport child: thundersnapd's stdout/stderr are piped through it to the
// host's virtio-vsock listener (and to the serial console). PID 1 reaps the
// relay when it dies; if the relay dies first, the daemon's log writes get
// EPIPE (Go ignores SIGPIPE on fd 1/2) and the daemon keeps running with silent
// logging. PID 1's own logs go to the inherited serial console directly (NOT
// through the relay), so boot/abort diagnostics survive a relay failure.
func runSupervisor(env []string) error {
	// Pipe: thundersnapd stdout/stderr -> relay -> (vsock + serial).
	r, w, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create daemon log pipe: %w", err)
	}

	// Spawn the relay (fire-and-forget). It inherits the serial console as its
	// own stdout/stderr and mirrors the daemon's piped output to the host.
	relay := exec.Command("/bin/thunderboot-logrelay")
	relay.Stdin = r
	relay.Stdout = os.Stdout
	relay.Stderr = os.Stderr
	relay.Env = env
	if err := relay.Start(); err != nil {
		r.Close()
		w.Close()
		return fmt.Errorf("start thunderboot-logrelay: %w", err)
	}

	// Spawn the daemon. Its stdout+stderr feed the relay's pipe.
	daemonArgs := []string{
		"/bin/thundersnapd",
		"--policy=/bin/thundersnap-policy.jsonc",
		"--data-dir=/var/lib/thundersnap",
		"--state-dir=/var/lib/thundersnap",
		"--libexec-dir=/bin",
		"--vm-dir=/bin",
	}
	daemon := exec.Command(daemonArgs[0], daemonArgs[1:]...)
	daemon.Stdin = os.Stdin
	daemon.Stdout = w
	daemon.Stderr = w
	daemon.Env = env
	if err := daemon.Start(); err != nil {
		r.Close()
		w.Close()
		_ = relay.Process.Signal(syscall.SIGKILL)
		_, _ = relay.Process.Wait()
		return fmt.Errorf("start thundersnapd: %w", err)
	}
	// Close our copies of the pipe so the write end reaches EOF only when the
	// daemon exits (otherwise the relay never sees EOF).
	r.Close()
	w.Close()

	daemonPid := daemon.Process.Pid
	log.Printf("executing thundersnapd (pid %d) under PID-1 supervision; global reaper active", daemonPid)

	// Global reaper (tini/dumb-init pattern): PID 1 has nothing else to do, so a
	// single blocking wait4(-1) suffices — no signal channel, no WNOHANG ticker.
	// This reaps every orphaned descendant that reparents to PID 1 (the fix),
	// plus the relay when it dies. We intentionally do NOT daemon.Wait(): that
	// would race this global loop. When the daemon dies, exit with its status to
	// tear down the VM (the one legitimate PID-1 exit).
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, 0, nil)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			// ECHILD (no children left): the daemon is gone; fall through to exit.
			log.Printf("wait4 returned %v; exiting", err)
			break
		}
		if pid == daemonPid {
			if ws.Signaled() {
				log.Printf("thundersnapd killed by signal %d; exiting", ws.Signal())
				os.Exit(128 + int(ws.Signal()))
			}
			log.Printf("thundersnapd exited status %d; exiting", ws.ExitStatus())
			os.Exit(ws.ExitStatus())
		}
		// Another child (e.g. the relay) died. Reap and keep supervising the
		// daemon (fire-and-forget: a dead relay must not tear down the VM).
		log.Printf("reaped child pid %d status %v", pid, ws)
	}
	return nil // unreachable in normal operation
}

func installAppliance() error {
	for _, dir := range []string{
		newRoot + "/bin",
		newRoot + "/lib",
		newRoot + "/lib64",
		newRoot + "/usr/lib",
		newRoot + "/dev/pts",
		newRoot + "/etc/ssl/certs",
		newRoot + "/proc",
		newRoot + "/run",
		newRoot + "/sys",
		newRoot + "/tmp",
		newRoot + "/var/lib/thundersnap",
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	files := map[string]string{
		"/bin/thundersnapd":                  newRoot + "/bin/thundersnapd",
		"/bin/ts":                            newRoot + "/bin/ts",
		"/bin/vshd":                          newRoot + "/bin/vshd",
		"/bin/thunderboot-logrelay":          newRoot + "/bin/thunderboot-logrelay",
		"/bin/busybox":                       newRoot + "/bin/busybox",
		"/bin/btrfs":                         newRoot + "/bin/btrfs",
		"/bin/mkfs.btrfs":                    newRoot + "/bin/mkfs.btrfs",
		"/bin/blkid":                         newRoot + "/bin/blkid",
		"/bin/mdadm":                         newRoot + "/bin/mdadm",
		"/bin/make-bcache":                   newRoot + "/bin/make-bcache",
		"/bin/nbd-client":                    newRoot + "/bin/nbd-client",
		"/bin/thundersnap-policy.jsonc":      newRoot + "/bin/thundersnap-policy.jsonc",
		"/etc/ssl/certs/ca-certificates.crt": newRoot + "/etc/ssl/certs/ca-certificates.crt",
	}
	for src, dst := range files {
		if err := copyFile(src, dst); err != nil {
			return err
		}
	}
	// Nested Cloud Hypervisor support is optional in the appliance. Aperture's
	// first ARM64 appliance intentionally omits these large payloads, while the
	// Linux/KVM appliance keeps installing them when its builder includes them.
	for _, path := range []string{"/bin/cloud-hypervisor", "/bin/vmlinux"} {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := copyFile(path, newRoot+path); err != nil {
			return err
		}
	}
	for _, dir := range []string{"/lib", "/lib64", "/usr/lib"} {
		if err := copyTree(dir, newRoot+dir); err != nil {
			return err
		}
	}
	if err := os.Remove(newRoot + "/bin/cp"); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Symlink("busybox", newRoot+"/bin/cp"); err != nil {
		return fmt.Errorf("create cp symlink: %w", err)
	}
	resolvConf, err := resolverConfigFromDHCP("/proc/net/pnp")
	if err != nil {
		return err
	}
	if err := os.WriteFile(newRoot+"/etc/resolv.conf", resolvConf, 0644); err != nil {
		return fmt.Errorf("write resolv.conf: %w", err)
	}
	return nil
}

// resolverConfigFromDHCP uses the DNS servers recorded by Linux's kernel DHCP
// client. This works with both Virtualization.framework NAT (currently
// 192.168.64.1) and any future DHCP network without baking its subnet into the
// appliance. The fallback preserves the static passt configuration used by the
// Linux/KVM thunderboot path, whose kernel command line does not carry DNS.
func resolverConfigFromDHCP(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []byte("nameserver 10.0.2.3\n"), nil
		}
		return nil, fmt.Errorf("open DHCP resolver data: %w", err)
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[0] == "nameserver" && net.ParseIP(fields[1]) != nil {
			lines = append(lines, "nameserver "+fields[1])
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read DHCP resolver data: %w", err)
	}
	if len(lines) == 0 {
		lines = append(lines, "nameserver 10.0.2.3")
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return fmt.Errorf("copy %s: %w", src, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s: %w", dst, closeErr)
	}
	return nil
}

func copyTree(src, dst string) error {
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return copyFile(path, target)
	})
}

// switchRoot implements the initramfs switch_root operation. The kernel's
// special rootfs cannot be pivot_root(2)'d, so move the live mounts into the
// disk root, unlink the unpacked initramfs, move the disk mount over /, and
// chroot into it. The following Exec releases the old /init mappings too.
func switchRoot() error {
	for _, path := range []string{"/dev", "/proc", "/sys", "/run", "/tmp"} {
		target := newRoot + path
		if err := syscall.Mount(path, target, "", syscall.MS_MOVE, ""); err != nil {
			return fmt.Errorf("move mount %s to %s: %w", path, target, err)
		}
	}
	entries, err := os.ReadDir("/")
	if err != nil {
		return fmt.Errorf("read old root: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() == strings.TrimPrefix(newRoot, "/") {
			continue
		}
		if err := os.RemoveAll(filepath.Join("/", entry.Name())); err != nil {
			return fmt.Errorf("remove old root entry %s: %w", entry.Name(), err)
		}
	}
	if err := os.Chdir(newRoot); err != nil {
		return fmt.Errorf("chdir new root: %w", err)
	}
	if err := syscall.Mount(".", "/", "", syscall.MS_MOVE, ""); err != nil {
		return fmt.Errorf("move new root onto /: %w", err)
	}
	if err := syscall.Chroot("."); err != nil {
		return fmt.Errorf("chroot: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return fmt.Errorf("chdir /: %w", err)
	}
	return nil
}

func mount(source, target, fstype string, flags uintptr, data string) error {
	if err := os.MkdirAll(target, 0755); err != nil {
		return fmt.Errorf("create mountpoint %s: %w", target, err)
	}
	if err := syscall.Mount(source, target, fstype, flags, data); err != nil && !errors.Is(err, syscall.EBUSY) {
		return fmt.Errorf("mount %s on %s: %w", source, target, err)
	}
	return nil
}

func logMemory(stage string) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "MemAvailable:") {
			log.Printf("%s: %s", stage, scanner.Text())
			return
		}
	}
}
