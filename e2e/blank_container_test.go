// Package e2e contains end-to-end tests for thundersnap blank container functionality.
package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestBlankContainerDevSetup tests that a completely blank container (simulating nil:nil:nil)
// can boot and set up /dev, /proc, /sys correctly.
//
// This is a critical test because blank containers have no pre-existing directory structure.
// The drop-caps-and-run code must create /dev, /proc, /sys before mounting on them.
func TestBlankContainerDevSetup(t *testing.T) {
	env := newTestEnv(t)

	// Create a completely empty btrfs subvolume (simulating nil:nil:nil frame)
	framePath := filepath.Join(env.fsDir, "testuser", "blanktest")
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cmd := exec.Command("btrfs", "subvolume", "create", framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs subvolume create: %v\n%s", err, out)
	}

	// Create only /bin and copy ts binary - nothing else
	binDir := filepath.Join(framePath, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	tsDst := filepath.Join(binDir, "ts")
	if err := copyFile(env.tsBinary, tsDst); err != nil {
		t.Fatalf("copy ts to frame: %v", err)
	}

	absFramePath, err := filepath.Abs(framePath)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}

	// Run drop-caps-and-run with ts check-dev to verify /dev setup
	tsBinary := filepath.Join(absFramePath, "bin", "ts")
	cmd = exec.Command(tsBinary, "drop-caps-and-run",
		"--chroot="+absFramePath,
		"--", "/bin/ts", "check-dev")
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS,
	}

	output, err := cmd.CombinedOutput()
	t.Logf("check-dev output:\n%s", output)
	if err != nil {
		t.Fatalf("check-dev error: %v", err)
	}

	// Verify /dev/pts directory exists (required for PTY)
	if !strings.Contains(string(output), "DIR:pts:exists") {
		t.Error("/dev/pts should exist")
	} else {
		t.Log("Verified /dev/pts exists")
	}

	// Verify /dev/null exists (basic device node)
	if !strings.Contains(string(output), "DEV:null:exists") {
		t.Error("/dev/null should exist")
	} else {
		t.Log("Verified /dev/null exists")
	}
}

// TestBlankContainerIsolation tests full isolation in a blank container.
func TestBlankContainerIsolation(t *testing.T) {
	env := newTestEnv(t)

	// Create a completely empty btrfs subvolume
	framePath := filepath.Join(env.fsDir, "testuser", "blankisolation")
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cmd := exec.Command("btrfs", "subvolume", "create", framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs subvolume create: %v\n%s", err, out)
	}

	// Create only /bin and copy ts binary
	binDir := filepath.Join(framePath, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	tsDst := filepath.Join(binDir, "ts")
	if err := copyFile(env.tsBinary, tsDst); err != nil {
		t.Fatalf("copy ts to frame: %v", err)
	}

	absFramePath, err := filepath.Abs(framePath)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}

	// Run drop-caps-and-run with ts check-isolation
	tsBinary := filepath.Join(absFramePath, "bin", "ts")
	cmd = exec.Command(tsBinary, "drop-caps-and-run",
		"--chroot="+absFramePath,
		"--", "/bin/ts", "check-isolation")
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS,
	}

	output, err := cmd.CombinedOutput()
	t.Logf("check-isolation output:\n%s", output)
	if err != nil {
		t.Fatalf("check-isolation error: %v", err)
	}

	result := parseIsolationOutput(string(output))

	// Verify PID namespace isolation (should be PID 1)
	if !result.isPID1 {
		t.Errorf("expected PID 1 in new PID namespace, got pid=%s", result.pid)
	} else {
		t.Log("Verified PID namespace isolation (PID 1)")
	}

	// Verify /proc is mounted
	if !result.procMounted {
		t.Error("/proc should be mounted in blank container")
	} else {
		t.Log("Verified /proc is mounted")
	}

	// Verify /sys is mounted
	if !result.sysMounted {
		t.Error("/sys should be mounted in blank container")
	} else {
		t.Log("Verified /sys is mounted")
	}
}

// TestBlankContainerShell tests that ts as shell works in a blank container.
// This tests the builtin shell functionality that's critical for blank containers.
func TestBlankContainerShell(t *testing.T) {
	env := newTestEnv(t)

	// Create a completely empty btrfs subvolume
	framePath := filepath.Join(env.fsDir, "testuser", "blankshell")
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cmd := exec.Command("btrfs", "subvolume", "create", framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs subvolume create: %v\n%s", err, out)
	}

	// Create /bin with ts and sh symlink
	binDir := filepath.Join(framePath, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	tsDst := filepath.Join(binDir, "ts")
	if err := copyFile(env.tsBinary, tsDst); err != nil {
		t.Fatalf("copy ts to frame: %v", err)
	}

	// Create /bin/sh symlink to ts (ts acts as shell when invoked as sh)
	shDst := filepath.Join(binDir, "sh")
	if err := os.Symlink("ts", shDst); err != nil {
		t.Fatalf("symlink sh -> ts: %v", err)
	}

	absFramePath, err := filepath.Abs(framePath)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}

	// Run sh -c 'echo hello' using the ts-as-shell
	tsBinary := filepath.Join(absFramePath, "bin", "ts")
	cmd = exec.Command(tsBinary, "drop-caps-and-run",
		"--chroot="+absFramePath,
		"--", "/bin/sh", "-c", "echo BLANK_SHELL_OK")
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS,
	}

	output, err := cmd.CombinedOutput()
	t.Logf("shell output: %s", output)
	if err != nil {
		t.Fatalf("shell error: %v", err)
	}

	if !strings.Contains(string(output), "BLANK_SHELL_OK") {
		t.Errorf("expected 'BLANK_SHELL_OK' in output, got: %s", output)
	} else {
		t.Log("Verified ts-as-shell works in blank container")
	}
}
