// Command thunderboot-init is PID 1 for a thundersnap appliance VM.
package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

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
	if err := os.MkdirAll("/etc", 0755); err != nil {
		return err
	}
	if err := os.WriteFile("/etc/resolv.conf", []byte("nameserver 10.0.2.3\n"), 0644); err != nil {
		return fmt.Errorf("write resolv.conf: %w", err)
	}
	if err := mount("bootconfig", "/bootconfig", "virtiofs", syscall.MS_RDONLY, ""); err != nil {
		return err
	}
	if err := mount("/dev/vda", "/var/lib/thundersnap", "btrfs", 0, "compress=zstd"); err != nil {
		return err
	}

	env := append(os.Environ(), "PATH=/sbin:/bin")
	if key, err := os.ReadFile("/bootconfig/authkey"); err == nil {
		if key := strings.TrimSpace(string(key)); key != "" {
			env = append(env, "TS_AUTHKEY="+key)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read authkey: %w", err)
	}

	args := []string{
		"--policy=/etc/thundersnap-policy.jsonc",
		"--data-dir=/var/lib/thundersnap",
		"--state-dir=/var/lib/thundersnap",
		"--libexec-dir=/sbin",
		"--vm-dir=/vm",
	}
	cmd := exec.Command("/sbin/thundersnapd", args...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start thundersnapd: %w", err)
	}
	log.Printf("started thundersnapd pid %d", cmd.Process.Pid)

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case sig := <-sigc:
		_ = cmd.Process.Signal(sig)
		return fmt.Errorf("thundersnapd stopped after %s: %w", sig, <-done)
	case err := <-done:
		return fmt.Errorf("thundersnapd exited: %w", err)
	}
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
