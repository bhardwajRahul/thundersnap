// Package e2e contains end-to-end tests for thundersnap container isolation.
package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestContainerIsolationBasic tests the basic container isolation via drop-caps-and-run.
// This test verifies:
// 1. PID namespace isolation (process is PID 1)
// 2. /proc is mounted
// 3. Dangerous capabilities are dropped (NET_ADMIN, SYS_MODULE, etc.)
func TestContainerIsolationBasic(t *testing.T) {
	env := newTestEnv(t)

	// Create a frame for testing
	baseSnap := env.createBaseSnapshot()
	framePath := filepath.Join(env.fsDir, "testuser", "isolationtest")
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	snapPath := filepath.Join(env.snapshotsDir, baseSnap)
	cmd := exec.Command("btrfs", "subvolume", "snapshot", snapPath, framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	// Copy ts binary into the frame
	tsDst := filepath.Join(framePath, "bin/ts")
	if err := copyFile(env.tsBinary, tsDst); err != nil {
		t.Fatalf("copy ts to frame: %v", err)
	}

	absFramePath, err := filepath.Abs(framePath)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}

	// Run drop-caps-and-run with check-isolation
	tsBinary := filepath.Join(absFramePath, "bin", "ts")
	cmd = exec.Command(tsBinary, "drop-caps-and-run",
		"--chroot="+absFramePath,
		"--", "/bin/ts", "check-isolation")
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS,
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("check-isolation output: %s", output)
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
		t.Error("/proc should be mounted")
	} else {
		t.Log("Verified /proc is mounted")
	}

	// Verify /sys is mounted
	if !result.sysMounted {
		t.Error("/sys should be mounted")
	} else {
		t.Log("Verified /sys is mounted")
	}

	// Verify dangerous capabilities are dropped
	droppedCaps := []string{
		"NET_ADMIN",
		"SYS_MODULE",
		"SYS_BOOT",
		"SYS_TIME",
		"MKNOD",
		"AUDIT_WRITE",
		"SETFCAP",
	}

	for _, cap := range droppedCaps {
		if result.capabilities[cap] != "dropped" {
			t.Errorf("CAP_%s should be dropped, got %s", cap, result.capabilities[cap])
		} else {
			t.Logf("Verified CAP_%s is dropped", cap)
		}
	}

	// Verify namespace inodes are reported (any non-zero value means new namespace)
	for _, ns := range []string{"pid", "mnt", "uts"} {
		if result.namespaces[ns] == "" || result.namespaces[ns] == "error" {
			t.Errorf("expected valid %s namespace inode", ns)
		} else {
			t.Logf("Verified %s namespace (inode=%s)", ns, result.namespaces[ns])
		}
	}
}

// TestContainerHostname tests that --hostname sets the container hostname.
func TestContainerHostname(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()
	framePath := filepath.Join(env.fsDir, "testuser", "hostnametest")
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	snapPath := filepath.Join(env.snapshotsDir, baseSnap)
	cmd := exec.Command("btrfs", "subvolume", "snapshot", snapPath, framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	tsDst := filepath.Join(framePath, "bin/ts")
	if err := copyFile(env.tsBinary, tsDst); err != nil {
		t.Fatalf("copy ts to frame: %v", err)
	}

	absFramePath, err := filepath.Abs(framePath)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}

	testHostname := "testcontainer"
	tsBinary := filepath.Join(absFramePath, "bin", "ts")
	cmd = exec.Command(tsBinary, "drop-caps-and-run",
		"--chroot="+absFramePath,
		"--hostname="+testHostname,
		"--", "/bin/ts", "check-isolation")
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS,
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("check-isolation output: %s", output)
		t.Fatalf("check-isolation error: %v", err)
	}

	result := parseIsolationOutput(string(output))

	if result.hostname != testHostname {
		t.Errorf("hostname: got %q, want %q", result.hostname, testHostname)
	} else {
		t.Logf("Verified hostname set to %q", testHostname)
	}
}

// TestContainerDomainname tests that --domainname sets the container domainname.
func TestContainerDomainname(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()
	framePath := filepath.Join(env.fsDir, "testuser", "domainnametest")
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	snapPath := filepath.Join(env.snapshotsDir, baseSnap)
	cmd := exec.Command("btrfs", "subvolume", "snapshot", snapPath, framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	tsDst := filepath.Join(framePath, "bin/ts")
	if err := copyFile(env.tsBinary, tsDst); err != nil {
		t.Fatalf("copy ts to frame: %v", err)
	}

	absFramePath, err := filepath.Abs(framePath)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}

	testDomainname := "test.domain"
	tsBinary := filepath.Join(absFramePath, "bin", "ts")
	cmd = exec.Command(tsBinary, "drop-caps-and-run",
		"--chroot="+absFramePath,
		"--domainname="+testDomainname,
		"--", "/bin/ts", "check-isolation")
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS,
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("check-isolation output: %s", output)
		t.Fatalf("check-isolation error: %v", err)
	}

	result := parseIsolationOutput(string(output))

	if result.domainname != testDomainname {
		t.Errorf("domainname: got %q, want %q", result.domainname, testDomainname)
	} else {
		t.Logf("Verified domainname set to %q", testDomainname)
	}
}

// TestContainerMountPropagation tests that mount propagation is private
// to ensure mounts in the container don't leak to the host.
func TestContainerMountPropagation(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()
	framePath := filepath.Join(env.fsDir, "testuser", "mountproptest")
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	snapPath := filepath.Join(env.snapshotsDir, baseSnap)
	cmd := exec.Command("btrfs", "subvolume", "snapshot", snapPath, framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	tsDst := filepath.Join(framePath, "bin/ts")
	if err := copyFile(env.tsBinary, tsDst); err != nil {
		t.Fatalf("copy ts to frame: %v", err)
	}

	absFramePath, err := filepath.Abs(framePath)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}

	tsBinary := filepath.Join(absFramePath, "bin", "ts")
	cmd = exec.Command(tsBinary, "drop-caps-and-run",
		"--chroot="+absFramePath,
		"--", "/bin/ts", "check-isolation")
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS,
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("check-isolation output: %s", output)
		t.Fatalf("check-isolation error: %v", err)
	}

	result := parseIsolationOutput(string(output))

	// Verify mount propagation is private (not shared)
	if result.mountPropagation == "" || result.mountPropagation == "error" {
		t.Errorf("could not determine mount propagation, got %q", result.mountPropagation)
	} else if result.mountPropagation != "private" {
		t.Errorf("mount propagation: got %q, want %q", result.mountPropagation, "private")
	} else {
		t.Logf("Verified mount propagation is private")
	}
}

// isolationCheckResult holds parsed output from "ts check-isolation".
type isolationCheckResult struct {
	hostname         string
	domainname       string
	isPID1           bool
	pid              string
	procMounted      bool
	sysMounted       bool
	capabilities     map[string]string // cap name -> "has" or "dropped"
	namespaces       map[string]string // ns name -> inode
	mountPropagation string            // "private", "shared", "slave", "unbindable"
}

// parseIsolationOutput parses the output of "ts check-isolation".
func parseIsolationOutput(output string) isolationCheckResult {
	result := isolationCheckResult{
		capabilities: make(map[string]string),
		namespaces:   make(map[string]string),
	}

	for _, line := range strings.Split(output, "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 2 {
			continue
		}
		switch parts[0] {
		case "HOSTNAME":
			result.hostname = parts[1]
		case "DOMAINNAME":
			result.domainname = parts[1]
		case "PID1":
			if parts[1] == "yes" {
				result.isPID1 = true
				result.pid = "1"
			} else if len(parts) >= 3 {
				result.pid = parts[2]
			}
		case "PROC":
			result.procMounted = parts[1] == "mounted"
		case "SYS":
			result.sysMounted = parts[1] == "mounted"
		case "CAP":
			if len(parts) >= 3 {
				result.capabilities[parts[1]] = parts[2]
			}
		case "NS":
			if len(parts) >= 3 {
				result.namespaces[parts[1]] = parts[2]
			}
		case "MOUNT_PROPAGATION":
			result.mountPropagation = parts[1]
		}
	}

	return result
}
