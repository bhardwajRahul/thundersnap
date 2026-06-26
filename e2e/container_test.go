// Package e2e contains end-to-end tests for thundersnap container isolation.
package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tailscale/thundersnap/thundersnap"
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

// TestContainerSharedPIDNamespace tests that multiple sessions to the same container
// share the same PID namespace, so processes started by one session are visible
// to another session via /proc.
//
// This test uses the same ContainerNsManager that thundersnapd uses, exercising
// the exact production code path:
// 1. ContainerNsManager spawns container-init to create and anchor the namespaces
// 2. Session 1 runs via ContainerNsManager.StartInContainerNs (writes PID file, sleeps)
// 3. Session 2 runs via ContainerNsManager.RunInContainerNs (lists /proc)
// 4. Session 2 should see session 1's process in /proc
//
// This is the expected behavior: when you SSH into a container twice, both sessions
// should see each other's processes. This is how Docker exec works - you're joining
// an existing container's namespace, not creating a new one each time.
func TestContainerSharedPIDNamespace(t *testing.T) {
	env := newTestEnv(t)

	// Create a frame for testing
	baseSnap := env.createBaseSnapshot()
	framePath := filepath.Join(env.fsDir, "testuser", "sharedpidtest")
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

	// Create a ContainerNsManager - this is the same code thundersnapd uses
	// to manage shared namespaces across SSH sessions
	nsManager := thundersnap.NewContainerNsManager()

	// Session 1: Start a long-running process that writes its PID
	// This uses the production code path: container-init + nsenter + drop-caps-and-run
	session1Cmd, _, err := nsManager.StartInContainerNs(
		absFramePath, "", "", "",
		"/bin/sh", "-c", "echo $$ > /tmp/session1.pid && sleep 30")
	if err != nil {
		t.Fatalf("start session 1: %v", err)
	}
	defer func() {
		session1Cmd.Process.Kill()
		session1Cmd.Wait()
		nsManager.ReleaseContainerNs(absFramePath)
	}()
	t.Logf("Session 1 started with host PID %d", session1Cmd.Process.Pid)

	// Wait for session 1 to write its PID file
	pidFile := filepath.Join(framePath, "tmp", "session1.pid")
	var session1PID string
	for i := 0; i < 50; i++ { // wait up to 5 seconds
		if data, err := os.ReadFile(pidFile); err == nil {
			session1PID = strings.TrimSpace(string(data))
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if session1PID == "" {
		t.Fatal("session 1 did not write PID file")
	}
	t.Logf("Session 1 reports PID %s in the container namespace", session1PID)

	// Session 2: List /proc to see what PIDs are visible
	// This uses the same ContainerNsManager, which will reuse the same namespace
	output, err := nsManager.RunInContainerNs(
		absFramePath, "", "", "",
		"/bin/sh", "-c", "echo /proc/[0-9]*")
	if err != nil {
		t.Logf("proc scan output: %s", output)
		t.Fatalf("session 2 proc scan failed: %v", err)
	}

	// Count the PIDs visible (glob expansion gives space-separated paths)
	outputStr := strings.TrimSpace(string(output))
	pids := strings.Fields(outputStr)
	t.Logf("Session 2 sees %d PIDs: %v", len(pids), pids)

	// In a shared namespace, session 2 should see:
	// - PID 1 (container-init)
	// - Session 1's shell + sleep processes
	// - Session 2's own shell process
	// So at least 3+ PIDs. In separate namespaces, just 1 (itself as PID 1).
	if len(pids) <= 1 {
		t.Errorf("Session 2 only sees %d PID(s) - PID namespaces are NOT shared", len(pids))
		t.Log("This test verifies that multiple sessions share the same PID namespace.")
	} else {
		t.Logf("Verified: Session 2 sees %d PIDs - PID namespaces are shared correctly", len(pids))
	}
}
