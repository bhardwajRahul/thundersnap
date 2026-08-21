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
	args := []string{
		// Keep thundersnapd's ordinary log output unchanged. The tiny relay
		// mirrors it to the host's virtio-vsock listener while preserving the
		// serial console as the primary diagnostic path.
		"/bin/thunderboot-logrelay",
		"/bin/thundersnapd",
		"--policy=/bin/thundersnap-policy.jsonc",
		"--data-dir=/var/lib/thundersnap",
		"--state-dir=/var/lib/thundersnap",
		"--libexec-dir=/bin",
		"--vm-dir=/bin",
	}
	log.Printf("executing thundersnapd as PID 1")
	return syscall.Exec(args[0], args, env)
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
