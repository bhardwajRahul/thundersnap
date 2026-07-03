// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

// Package e2e contains end-to-end tests for thundersnap container isolation.
package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

// TestContainerSharedPIDNamespace tests that multiple sessions to the same
// container share the same PID namespace, so processes started by one session
// are visible to another session via /proc.
//
// It drives the REAL production per-session form the daemon uses
// (cmd/thundersnapd buildSessionCommand): join the shared namespace anchored by
// container-init with nsenter, then run ts drop-caps-and-run --chroot
// --skip-mount-setup. The earlier version of this test instead called the
// thundersnap.{Start,Run}InContainerNs spawn fork, which built the command
// WITHOUT --skip-mount-setup and was never used by the daemon, so a green
// result proved nothing about the live path.
//
// This is the expected behavior: when you SSH into a container twice, both
// sessions should see each other's processes (like docker exec joining an
// existing container's namespace rather than creating a new one each time).
func TestContainerSharedPIDNamespace(t *testing.T) {
	env := newTestEnv(t)
	absFramePath, ns, initPid := setupSharedNsFrame(t, env, "sharedpidtest")
	defer ns.Release(absFramePath)

	// The base snapshot's /bin/sh is the ts binary, not a real shell; install a
	// static busybox so the sessions can run echo/sleep/read and a /proc glob.
	installBusyboxShell(t, absFramePath)

	tsBinary := filepath.Join(absFramePath, "bin", "ts")

	// runSession runs the exact production per-session command form against the
	// shared namespace: nsenter joins initPid's PID/mount/UTS namespaces, then
	// ts drop-caps-and-run --chroot --skip-mount-setup execs the script.
	runSession := func(script string) *exec.Cmd {
		args := []string{
			"-t", fmt.Sprintf("%d", initPid), "-p", "-m", "-u", "--",
			tsBinary, "drop-caps-and-run",
			"--chroot=" + absFramePath,
			"--skip-mount-setup",
			"--", "/bin/sh", "-c", script,
		}
		cmd := exec.Command("nsenter", args...)
		cmd.Dir = "/"
		return cmd
	}

	// Session 1: a long-running process that records its in-namespace PID, then
	// blocks on a fifo so it stays alive while session 2 inspects /proc.
	fifo := filepath.Join(absFramePath, "tmp", "go1")
	os.Remove(fifo)
	if err := syscall.Mkfifo(fifo, 0666); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	session1 := runSession("echo $$ > /tmp/session1.pid && read x < /tmp/go1")
	if err := session1.Start(); err != nil {
		t.Fatalf("start session 1: %v", err)
	}
	defer func() {
		session1.Process.Kill()
		session1.Wait()
	}()

	// Wait for session 1 to write its PID file.
	pidFile := filepath.Join(absFramePath, "tmp", "session1.pid")
	var session1PID string
	for i := 0; i < 50; i++ { // up to 5 seconds
		if data, err := os.ReadFile(pidFile); err == nil && len(strings.TrimSpace(string(data))) > 0 {
			session1PID = strings.TrimSpace(string(data))
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if session1PID == "" {
		t.Fatal("session 1 did not write PID file")
	}
	t.Logf("Session 1 reports PID %s in the container namespace", session1PID)

	// Session 2: list /proc to see which PIDs are visible in the shared namespace.
	output, err := runSession("echo /proc/[0-9]*").Output()
	if err != nil {
		t.Logf("proc scan output: %s", output)
		t.Fatalf("session 2 proc scan failed: %v", err)
	}

	// Release session 1.
	if f, err := os.OpenFile(fifo, os.O_WRONLY, 0); err == nil {
		fmt.Fprintln(f, "go")
		f.Close()
	}

	pids := strings.Fields(strings.TrimSpace(string(output)))
	t.Logf("Session 2 sees %d PIDs: %v", len(pids), pids)

	// In a shared namespace session 2 should see PID 1 (container-init),
	// session 1's shell, and its own shell: 3+ PIDs. In separate namespaces it
	// would see only itself as PID 1.
	if len(pids) <= 1 {
		t.Errorf("Session 2 only sees %d PID(s) - PID namespaces are NOT shared", len(pids))
	} else {
		t.Logf("Verified: Session 2 sees %d PIDs - PID namespaces are shared correctly", len(pids))
	}

	// Session 1's reported PID must be visible to session 2.
	found := false
	for _, p := range pids {
		if p == "/proc/"+session1PID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("session 1 PID %s not visible to session 2 (saw %v)", session1PID, pids)
	}
}

// TestTsNsenterJoinsSharedNamespace verifies the CGO-free in-binary `ts nsenter`
// enters a shared container namespace identically to the external nsenter(1):
// it joins the PID, mount, and UTS namespaces of the container-init process.
//
// This is the oracle for the `ts nsenter` reexec subcommand that vshd uses on
// both the host and inside a VM (where util-linux nsenter is absent). It runs
// the production per-session command form but with `ts nsenter` instead of the
// external nsenter, and asserts:
//   - mount ns joined: the session sees the container's chroot + shared devpts
//     (its tty is under /dev/pts), and
//   - PID ns joined: a second session sees the first session's in-namespace PID
//     via /proc (shared PID namespace), and
//   - the command's real exit status is propagated.
func TestTsNsenterJoinsSharedNamespace(t *testing.T) {
	env := newTestEnv(t)
	absFramePath, ns, initPid := setupSharedNsFrame(t, env, "tsnsentertest")
	defer ns.Release(absFramePath)
	installBusyboxShell(t, absFramePath)

	tsBinary := filepath.Join(absFramePath, "bin", "ts")

	// runSession mirrors the production per-session form but uses `ts nsenter`
	// (the in-binary replacement) rather than external nsenter.
	runSession := func(script string) *exec.Cmd {
		args := []string{
			"nsenter",
			"-t", fmt.Sprintf("%d", initPid), "-p", "-m", "-u", "--",
			tsBinary, "drop-caps-and-run",
			"--chroot=" + absFramePath,
			"--skip-mount-setup",
			"--", "/bin/sh", "-c", script,
		}
		cmd := exec.Command(tsBinary, args...)
		cmd.Dir = "/"
		return cmd
	}

	// Exit-status propagation: a non-zero exit must surface through the two-stage
	// reexec.
	if err := runSession("exit 7").Run(); err == nil {
		t.Errorf("ts nsenter did not propagate non-zero exit status")
	} else if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 7 {
		t.Errorf("expected exit code 7, got %v", err)
	}

	// Session 1: record its in-namespace PID, then block on a fifo.
	fifo := filepath.Join(absFramePath, "tmp", "nsgo1")
	os.Remove(fifo)
	if err := syscall.Mkfifo(fifo, 0666); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	session1 := runSession("echo $$ > /tmp/ns_session1.pid && read x < /tmp/nsgo1")
	if err := session1.Start(); err != nil {
		t.Fatalf("start session 1: %v", err)
	}
	defer func() {
		session1.Process.Kill()
		session1.Wait()
	}()

	pidFile := filepath.Join(absFramePath, "tmp", "ns_session1.pid")
	var session1PID string
	for i := 0; i < 50; i++ {
		if data, err := os.ReadFile(pidFile); err == nil && len(strings.TrimSpace(string(data))) > 0 {
			session1PID = strings.TrimSpace(string(data))
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if session1PID == "" {
		t.Fatal("session 1 did not write PID file (ts nsenter mount join likely failed)")
	}
	t.Logf("ts nsenter session 1 PID in container ns: %s", session1PID)

	// Session 2 scans /proc; in a shared PID ns it must see session 1's PID.
	output, err := runSession("echo /proc/[0-9]*").Output()
	if err != nil {
		t.Fatalf("session 2 proc scan failed: %v", err)
	}
	if f, err := os.OpenFile(fifo, os.O_WRONLY, 0); err == nil {
		fmt.Fprintln(f, "go")
		f.Close()
	}

	pids := strings.Fields(strings.TrimSpace(string(output)))
	found := false
	for _, p := range pids {
		if p == "/proc/"+session1PID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ts nsenter did not share PID namespace: session1 PID %s not visible to session2 (saw %v)", session1PID, pids)
	} else {
		t.Logf("Verified: ts nsenter shares PID namespace (session2 sees %d PIDs)", len(pids))
	}
}
