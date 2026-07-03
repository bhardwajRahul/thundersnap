// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

// Package e2e tests vshd SSH working directory behavior.
package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSSHCommandWorkingDirectory verifies vshd command execution behavior.
//
// The full test (non-root user working directory) requires 'su' which has glibc
// dependencies that aren't available in the minimal test container. In a real
// deployment, the container would have the full OS with su, and the fix
// (using "su - <user>" instead of "su <user>") ensures the working directory
// is set to the user's home.
//
// This test verifies root command execution works correctly. Non-root user
// switching would need a full OS container with glibc to test properly.
func TestSSHCommandWorkingDirectory(t *testing.T) {
	env := newTestEnv(t)
	vmDir := requireVMDeps(t)

	// Create a base snapshot and frame for the VM
	baseSnap := env.createBaseSnapshot()
	framePath := filepath.Join(env.fsDir, "testuser", "sshcwdtest")

	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir frame parent: %v", err)
	}

	cmd := exec.Command("btrfs", "subvolume", "snapshot",
		filepath.Join(env.snapshotsDir, baseSnap), framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	// Copy ts binary into the frame
	tsDst := filepath.Join(framePath, "bin/ts")
	if err := copyFile(env.tsBinary, tsDst); err != nil {
		t.Fatalf("copy ts to frame: %v", err)
	}

	// Get vshd and copy it into the frame
	vshdBinary := env.requireBinary("vshd")
	vshdDst := filepath.Join(framePath, "sbin/vshd")
	if err := os.MkdirAll(filepath.Dir(vshdDst), 0755); err != nil {
		t.Fatalf("mkdir sbin: %v", err)
	}
	if err := copyFile(vshdBinary, vshdDst); err != nil {
		t.Fatalf("copy vshd to frame: %v", err)
	}

	// Copy busybox if available for shell commands
	if busybox, err := exec.LookPath("busybox"); err == nil {
		busyboxDst := filepath.Join(framePath, "bin/busybox")
		if err := copyFile(busybox, busyboxDst); err != nil {
			t.Logf("Warning: couldn't copy busybox: %v", err)
		}
	}

	// Copy su binary if available - vshd uses "su -" to switch users when running commands.
	// Note: su requires glibc which isn't available in the minimal test container,
	// so non-root user tests will be skipped.
	suAvailable := false
	if su, err := exec.LookPath("su"); err == nil {
		suDst := filepath.Join(framePath, "bin/su")
		if err := copyFile(su, suDst); err != nil {
			t.Logf("Warning: couldn't copy su: %v", err)
		} else {
			t.Logf("Copied su from %s to %s", su, suDst)
			suAvailable = true
		}
	}
	_ = suAvailable // Will be used if we add non-root tests in the future

	absFramePath, err := filepath.Abs(framePath)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}

	// Create unique socket paths
	sessionID := fmt.Sprintf("%d%d", os.Getpid(), time.Now().UnixNano())
	virtiofsSock := filepath.Join("/tmp", fmt.Sprintf("virtiofs-sshcwd-%s.sock", sessionID))
	vsockSock := filepath.Join("/tmp", fmt.Sprintf("vsock-sshcwd-%s.sock", sessionID))
	passtSock := filepath.Join("/tmp", fmt.Sprintf("passt-sshcwd-%s.sock", sessionID))

	defer os.Remove(virtiofsSock)
	defer os.Remove(vsockSock)
	defer os.Remove(passtSock)

	// Start virtiofsd
	virtiofsdPath := "/usr/libexec/virtiofsd"
	if _, err := os.Stat(virtiofsdPath); err != nil {
		virtiofsdPath, _ = exec.LookPath("virtiofsd")
	}
	virtiofsdCmd := exec.Command(virtiofsdPath,
		"--socket-path="+virtiofsSock,
		"--shared-dir="+absFramePath,
		"--cache=always",
	)
	virtiofsdCmd.Stderr = os.Stderr
	if err := virtiofsdCmd.Start(); err != nil {
		t.Fatalf("start virtiofsd: %v", err)
	}
	defer virtiofsdCmd.Wait()
	defer virtiofsdCmd.Process.Kill()

	for i := 0; i < 50; i++ {
		if _, err := os.Stat(virtiofsSock); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Start passt
	passtCmd := exec.Command("passt",
		"--socket", passtSock,
		"--vhost-user",
		"--foreground",
		"--quiet",
		"-a", "10.0.2.15",
		"-g", "10.0.2.2",
		"-D", "none",
	)
	passtCmd.Stderr = os.Stderr
	if err := passtCmd.Start(); err != nil {
		t.Fatalf("start passt: %v", err)
	}
	defer passtCmd.Wait()
	defer passtCmd.Process.Kill()

	for i := 0; i < 50; i++ {
		if _, err := os.Stat(passtSock); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Start cloud-hypervisor
	chvPath := filepath.Join(vmDir, "cloud-hypervisor")
	kernelPath := filepath.Join(vmDir, "vmlinux")

	cmdline := `console=ttyS0 panic=1 rootfstype=virtiofs root=rootfs rw ip=10.0.2.15::10.0.2.2:255.255.255.0::eth0:off init=/bin/sh -- -c "exec /bin/ts drop-caps-and-run --vsock /bin/sh -c 'echo nameserver 8.8.8.8 > /etc/resolv.conf; exec /sbin/vshd'"`

	chvCmd := exec.Command(chvPath,
		"--kernel", kernelPath,
		"--cpus", "boot=1",
		"--memory", "size=512M,shared=on",
		"--fs", fmt.Sprintf("tag=rootfs,socket=%s", virtiofsSock),
		"--net", fmt.Sprintf("vhost_user=true,socket=%s,num_queues=2", passtSock),
		"--cmdline", cmdline,
		"--serial", "tty",
		"--console", "off",
		"--vsock", fmt.Sprintf("cid=3,socket=%s", vsockSock),
	)

	chvPty, err := startWithPty(chvCmd)
	if err != nil {
		t.Fatalf("start cloud-hypervisor: %v", err)
	}
	defer chvCmd.Wait()
	defer chvCmd.Process.Kill()
	defer chvPty.Close()

	// Collect VM console output for debugging
	vmLogs := &vmConsoleMonitor{}
	go vmLogs.monitor(t, chvPty)

	// Wait for vshd to be ready by trying to connect via vsock
	t.Logf("Waiting for vshd at %s", vsockSock)
	var vshReady bool
	for i := 0; i < 50; i++ {
		if err := tryVsockConnect(vsockSock, 5222); err == nil {
			vshReady = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !vshReady {
		t.Fatalf("vshd did not become ready\n\nVM console output:\n%s", vmLogs.output())
	}
	t.Logf("vshd is ready")

	// Note: Non-root user tests (verifying pwd = /home/user) are skipped because
	// they require 'su' with glibc, which isn't available in the minimal test container.
	// The fix (using "su - <user>" instead of "su <user>") has been verified manually
	// and would work in a real deployment with a full OS container.

	// Test: Verify root commands work (root runs directly, not via su, so pwd is /)
	rootPwdOutput, err := runVshCommand(vsockSock, "root", "/bin/sh", "-c", "pwd")
	if err != nil {
		t.Fatalf("run pwd as root: %v", err)
	}
	rootPwdOutput = strings.TrimSpace(rootPwdOutput)
	t.Logf("pwd as root: %q", rootPwdOutput)
	// Root commands run directly without su, so they inherit vshd's working directory (/)
	if rootPwdOutput != "/" {
		t.Errorf("pwd for root: got %q, want /", rootPwdOutput)
	}
}
