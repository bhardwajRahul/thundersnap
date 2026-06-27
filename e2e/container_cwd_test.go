//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestContainerLoginShellWorkingDirectory verifies that when thundersnapd runs a
// non-interactive command in a container (the path used by ssh "cmd", scp, and
// rsync), it switches to the target user with a *login* shell so that the
// working directory becomes the user's home directory.
//
// This is the regression test for the bug "When I rsync into a container, it
// starts in / instead of /home". The production fix runs the command via
// `su - user -c <cmd>` (login shell) rather than `su user -c <cmd>`; the login
// form chdirs to the user's home as recorded in /etc/passwd.
//
// The test exercises the exact production command form:
//
//	ts drop-caps-and-run --chroot=<frame> -- su - user -c pwd
//
// and asserts the reported working directory is the user's home (/home/user),
// not / (which is where a non-login shell would leave it).
func TestContainerLoginShellWorkingDirectory(t *testing.T) {
	env := newTestEnv(t)

	// busybox provides a statically-linked `su` (and shell builtins like `pwd`)
	// that works inside the minimal chrooted frame without glibc.
	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Fatalf("busybox is required for this test: %v", err)
	}

	baseSnap := env.createBaseSnapshot()
	framePath := filepath.Join(env.fsDir, "testuser", "cwdlogintest")
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	snapPath := filepath.Join(env.snapshotsDir, baseSnap)
	cmd := exec.Command("btrfs", "subvolume", "snapshot", snapPath, framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	// Copy ts into the frame.
	if err := copyFile(env.tsBinary, filepath.Join(framePath, "bin/ts")); err != nil {
		t.Fatalf("copy ts to frame: %v", err)
	}

	// Install busybox as /bin/su and /bin/sh inside the frame. The fixture
	// already created /bin/sh as a hardlink to ts; replace it with busybox so
	// that `su -c pwd` runs a real shell with a working `pwd` builtin.
	suDst := filepath.Join(framePath, "bin/su")
	if err := copyFile(busybox, suDst); err != nil {
		t.Fatalf("copy busybox to frame su: %v", err)
	}
	if err := os.Chmod(suDst, 0755); err != nil {
		t.Fatalf("chmod su: %v", err)
	}
	shDst := filepath.Join(framePath, "bin/sh")
	if err := os.Remove(shDst); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove existing sh: %v", err)
	}
	if err := copyFile(busybox, shDst); err != nil {
		t.Fatalf("copy busybox to frame sh: %v", err)
	}
	if err := os.Chmod(shDst, 0755); err != nil {
		t.Fatalf("chmod sh: %v", err)
	}

	absFramePath, err := filepath.Abs(framePath)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}

	tsBinary := filepath.Join(absFramePath, "bin", "ts")

	// This mirrors the production non-PTY command branch in thundersnapd's
	// runContainerSession: `su - <user> -c <rawCmd>`. We deliberately start the
	// outer process in "/" so that a non-login shell would report "/", letting
	// the test distinguish the login-shell fix from the bug.
	cmd = exec.Command(tsBinary, "drop-caps-and-run",
		"--chroot="+absFramePath,
		"--", "su", "-", "user", "-c", "pwd")
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS,
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run su - user -c pwd: %v\noutput:\n%s", err, output)
	}

	// The output may contain login-shell noise; the pwd line is what matters.
	got := lastNonEmptyLine(string(output))
	t.Logf("pwd as user (login shell): %q (full output: %q)", got, string(output))

	const wantHome = "/home/user"
	if got != wantHome {
		t.Errorf("login-shell working directory: got %q, want %q\nfull output:\n%s",
			got, wantHome, output)
	}
}

// lastNonEmptyLine returns the last non-empty, trimmed line of s.
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			return line
		}
	}
	return ""
}
