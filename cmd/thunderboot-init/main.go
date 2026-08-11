// Command thunderboot-init is PID 1 for a thundersnap appliance VM.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
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
	}
	for _, m := range mounts {
		if err := mount(m.source, m.target, m.fstype, m.flags, m.data); err != nil {
			return err
		}
	}
	if err := mount("bootconfig", "/bootconfig", "virtiofs", syscall.MS_RDONLY, ""); err != nil {
		return err
	}
	if err := mount("/dev/vda", newRoot, "btrfs", 0, "compress=zstd"); err != nil {
		return err
	}

	logMemory("before installing appliance")
	if err := installAppliance(); err != nil {
		return err
	}

	authKey, err := os.ReadFile("/bootconfig/authkey")
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read authkey: %w", err)
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
	if key := strings.TrimSpace(string(authKey)); key != "" {
		env = append(env, "TS_AUTHKEY="+key)
	}
	args := []string{
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
		newRoot + "/bootconfig",
		newRoot + "/dev/pts",
		newRoot + "/etc",
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
		"/bin/thundersnapd":             newRoot + "/bin/thundersnapd",
		"/bin/ts":                       newRoot + "/bin/ts",
		"/bin/vshd":                     newRoot + "/bin/vshd",
		"/bin/busybox":                  newRoot + "/bin/busybox",
		"/bin/btrfs":                    newRoot + "/bin/btrfs",
		"/bin/mkfs.btrfs":               newRoot + "/bin/mkfs.btrfs",
		"/bin/cloud-hypervisor":         newRoot + "/bin/cloud-hypervisor",
		"/bin/vmlinux":                  newRoot + "/bin/vmlinux",
		"/bin/thundersnap-policy.jsonc": newRoot + "/bin/thundersnap-policy.jsonc",
	}
	for src, dst := range files {
		if err := copyFile(src, dst); err != nil {
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
	if err := os.WriteFile(newRoot+"/etc/resolv.conf", []byte("nameserver 10.0.2.3\n"), 0644); err != nil {
		return fmt.Errorf("write resolv.conf: %w", err)
	}
	return nil
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
	for _, path := range []string{"/dev", "/proc", "/sys", "/run", "/tmp", "/bootconfig"} {
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
