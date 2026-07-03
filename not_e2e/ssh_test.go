// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// daemonInstance represents a running thundersnapd in test mode.
type daemonInstance struct {
	t       *testing.T
	cmd     *exec.Cmd
	addr    string // listen address (e.g., "127.0.0.1:22222")
	dataDir string
	mu      sync.Mutex
	stopped bool
}

// testUser is the identity used for all test SSH connections.
// Keep this short to avoid exceeding Unix socket path length limits
// (the control socket lives inside the frame's rootFS).
const testUser = "e2e"

// startDaemon starts thundersnapd in test mode with --test-listen and --test-user.
// It returns a daemonInstance that can be used to connect via SSH.
func startDaemon(t *testing.T, env *testEnv) *daemonInstance {
	t.Helper()

	// Find a free port by listening on :0 and closing immediately
	port, err := getFreePort()
	if err != nil {
		t.Fatalf("Failed to find free port: %v", err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	// State directory for SSH keys etc
	stateDir := filepath.Join(env.root, "state")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}

	// Copy vshd binary to libexec (required by daemon)
	vshdBinary := env.requireBinary("vshd")
	if err := copyFile(vshdBinary, filepath.Join(env.libexecDir, "vshd")); err != nil {
		t.Fatalf("copy vshd to libexec: %v", err)
	}

	// Create a minimal policy file that allows container access
	policyPath := filepath.Join(env.root, "policy.json")
	policyContent := `{
		"grants": [
			{
				"principals": ["*"],
				"cap": {
					"role": "developer",
					"isolation": "container",
					"maxFrames": 10
				}
			}
		]
	}`
	if err := os.WriteFile(policyPath, []byte(policyContent), 0644); err != nil {
		t.Fatalf("write policy file: %v", err)
	}

	daemonArgs := []string{
		"--test-listen=" + addr,
		"--test-user=" + testUser,
		"--data-dir=" + env.root, // Uses env.root; fs/ and snaps/ are created inside
		"--state-dir=" + stateDir,
		"--libexec-dir=" + env.libexecDir,
		"--policy=" + policyPath,
	}

	// Point the daemon at the cloud-hypervisor + vmlinux directory so VM (vmx)
	// sessions can boot. The daemon's CWD is env.root, so an absolute path is
	// required. Resolve via the same discovery the low-level VM tests use; if
	// the VM deps are absent the daemon simply never uses this and only the
	// container tests work (the VM tests fail their own requireVMDeps).
	if dir := vmDir(); dir != "" {
		if abs, err := filepath.Abs(dir); err == nil {
			daemonArgs = append(daemonArgs, "--vm-dir="+abs)
		}
	}

	cmd := exec.Command(env.daemonBinary, daemonArgs...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Dir = env.root

	t.Logf("Starting daemon: %s %v", cmd.Path, cmd.Args[1:])

	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start daemon: %v", err)
	}

	d := &daemonInstance{
		t:       t,
		cmd:     cmd,
		addr:    addr,
		dataDir: env.root,
	}

	// Wait for daemon to be ready (SSH server accepting connections)
	if err := d.waitReady(10 * time.Second); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("Daemon failed to become ready: %v", err)
	}

	t.Logf("Daemon ready on %s", addr)

	t.Cleanup(func() {
		d.Stop()
	})

	return d
}

// waitReady waits for the daemon's SSH server to accept connections.
func (d *daemonInstance) waitReady(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for daemon to be ready")
		default:
		}

		// Try to establish an SSH connection
		config := &ssh.ClientConfig{
			User:            "probe",
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			Timeout:         500 * time.Millisecond,
		}
		client, err := ssh.Dial("tcp", d.addr, config)
		if err == nil {
			client.Close()
			return nil
		}

		// Wait a bit before retrying
		time.Sleep(100 * time.Millisecond)
	}
}

// Stop stops the daemon.
func (d *daemonInstance) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.stopped {
		return
	}
	d.stopped = true

	if d.cmd != nil && d.cmd.Process != nil {
		d.cmd.Process.Kill()
		d.cmd.Wait()
	}
}

// getFreePort finds a free TCP port by briefly binding to :0.
func getFreePort() (int, error) {
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// sshConfig returns an SSH client config for connecting to the test daemon.
// The user parameter is the SSH username (e.g., "user@frame" or just "frame").
func sshConfig(user string) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User:            user,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}
}

// sshExec connects to the daemon via SSH and executes a command, returning
// the combined stdout/stderr output and exit status.
func sshExec(t *testing.T, d *daemonInstance, user string, cmd string) (string, int, error) {
	t.Helper()

	config := sshConfig(user)
	client, err := ssh.Dial("tcp", d.addr, config)
	if err != nil {
		return "", -1, fmt.Errorf("dial: %w", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return "", -1, fmt.Errorf("new session: %w", err)
	}
	defer session.Close()

	output, err := session.CombinedOutput(cmd)
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*ssh.ExitError); ok {
			exitCode = exitErr.ExitStatus()
		} else {
			return string(output), -1, fmt.Errorf("run command: %w", err)
		}
	}

	return string(output), exitCode, nil
}

// sshExecSplit is like sshExec but captures the session's stdout and stderr
// into separate buffers instead of combining them. This is what lets a test
// observe whether the daemon keeps the two streams distinct end to end.
func sshExecSplit(t *testing.T, d *daemonInstance, user string, cmd string) (stdout, stderr string, exitCode int, err error) {
	t.Helper()

	config := sshConfig(user)
	client, err := ssh.Dial("tcp", d.addr, config)
	if err != nil {
		return "", "", -1, fmt.Errorf("dial: %w", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return "", "", -1, fmt.Errorf("new session: %w", err)
	}
	defer session.Close()

	var outBuf, errBuf strings.Builder
	session.Stdout = &outBuf
	session.Stderr = &errBuf

	exitCode = 0
	if err := session.Run(cmd); err != nil {
		if exitErr, ok := err.(*ssh.ExitError); ok {
			exitCode = exitErr.ExitStatus()
		} else {
			return outBuf.String(), errBuf.String(), -1, fmt.Errorf("run command: %w", err)
		}
	}

	return outBuf.String(), errBuf.String(), exitCode, nil
}

// sshInteractive connects to the daemon via SSH with a PTY for interactive use.
// It returns the SSH session which the caller must close.
func sshInteractive(t *testing.T, d *daemonInstance, user string) (*ssh.Client, *ssh.Session, error) {
	t.Helper()

	config := sshConfig(user)
	client, err := ssh.Dial("tcp", d.addr, config)
	if err != nil {
		return nil, nil, fmt.Errorf("dial: %w", err)
	}

	session, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, nil, fmt.Errorf("new session: %w", err)
	}

	// Request PTY
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm", 40, 80, modes); err != nil {
		session.Close()
		client.Close()
		return nil, nil, fmt.Errorf("request pty: %w", err)
	}

	return client, session, nil
}

// createTestFrame creates a test frame (btrfs subvolume) with the given name
// in the daemon's fs directory. Returns the path to the frame.
func createTestFrame(t *testing.T, env *testEnv, frameUUID string) string {
	t.Helper()

	// Frame layout is fs/<user>/<uuid>
	userDir := filepath.Join(env.fsDir, sanitizeUser(testUser))
	if err := os.MkdirAll(userDir, 0755); err != nil {
		t.Fatalf("mkdir user dir: %v", err)
	}

	framePath := filepath.Join(userDir, frameUUID)

	// Create as btrfs subvolume
	cmd := exec.Command("btrfs", "subvolume", "create", framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs subvolume create %s: %v\n%s", framePath, err, out)
	}

	// Create minimal container structure
	CreateTestContainer(t, framePath, env.tsBinary)

	// Install busybox as /bin/su (required for vshd to switch users). The shell
	// itself is ts (its built-in POSIX shell handles echo/pwd/touch-less tests),
	// so we deliberately do NOT install coreutils: the tests probe identity by
	// side effect (who can write to /) rather than by running `id`/`whoami`.
	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Fatalf("busybox required for SSH tests: %v", err)
	}
	suDst := filepath.Join(framePath, "bin/su")
	if err := copyFile(busybox, suDst); err != nil {
		t.Fatalf("copy busybox to /bin/su: %v", err)
	}

	// Create the .jsonc metadata sidecar that frameStore.List() looks for.
	// This is required for `ts frames` to see this frame.
	metaPath := framePath + ".jsonc"
	metaContent := `{"isolation": "container"}`
	if err := os.WriteFile(metaPath, []byte(metaContent), 0644); err != nil {
		t.Fatalf("write frame metadata: %v", err)
	}

	return framePath
}

// sanitizeUser mimics cmd/thundersnapd/main.go:sanitizeForPath.
func sanitizeUser(user string) string {
	// Replace / and null bytes, collapse ..
	replacer := strings.NewReplacer(
		"/", "_",
		"\x00", "_",
		"..", "_",
	)
	result := replacer.Replace(user)
	// Handle leading dots
	result = strings.TrimLeft(result, ".")
	if result == "" {
		result = "_"
	}
	return result
}

// createTestRef creates a ref pointing to a frame UUID.
func createTestRef(t *testing.T, env *testEnv, refName, frameUUID string) {
	t.Helper()

	// Refs are stored in <data-dir>/refs/<user>/<refname>.jsonc
	userRefsDir := filepath.Join(env.root, "refs", sanitizeUser(testUser))
	if err := os.MkdirAll(userRefsDir, 0755); err != nil {
		t.Fatalf("mkdir refs dir: %v", err)
	}

	refPath := filepath.Join(userRefsDir, refName+".jsonc")
	// The ref file is JSON with uuid and reflog
	now := time.Now().UTC().Format(time.RFC3339)
	refContent := fmt.Sprintf(`{"uuid":%q,"reflog":[{"uuid":%q,"time":%q}]}`, frameUUID, frameUUID, now)
	if err := os.WriteFile(refPath, []byte(refContent), 0644); err != nil {
		t.Fatalf("write ref file: %v", err)
	}
}

// TestSSHContainerBasic is a true end-to-end test: start daemon, SSH in,
// create a frame via `ts frame`, then exercise ts snap/snaps/log/frames.
// No manual frame/ref creation - everything goes through the daemon.
func TestSSHContainerBasic(t *testing.T) {
	env := newTestEnv(t)

	// Start the daemon with no pre-created frames
	d := startDaemon(t, env)

	// SSH to the default (empty) frame as root to run ts commands.
	// The empty frame name (root@@host) should give us a minimal shell.
	output, exitCode, err := sshExec(t, d, "root@", "echo hello")
	if err != nil {
		t.Fatalf("sshExec to default frame failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("echo in default frame: expected exit code 0, got %d (output: %q)", exitCode, output)
	}
	t.Logf("echo output (default frame): %s", output)

	// Create a new frame using ts frame, with a ref name
	// ts frame --ref=testframe <snapshot-spec>
	output, exitCode, err = sshExec(t, d, "root@", "ts frame --ref=testframe nil:nil:nil")
	if err != nil {
		t.Fatalf("ts frame failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts frame: expected exit code 0, got %d (output: %q)", exitCode, output)
	}
	t.Logf("ts frame output: %s", output)

	// Now SSH to the newly created frame
	output, exitCode, err = sshExec(t, d, "testframe", "echo hello from testframe")
	if err != nil {
		t.Fatalf("sshExec to testframe failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("echo in testframe: expected exit code 0, got %d (output: %q)", exitCode, output)
	}
	t.Logf("echo output (testframe): %s", output)

	// Test ts snap: create a snapshot of the current frame
	output, exitCode, err = sshExec(t, d, "testframe", "ts snap")
	if err != nil {
		t.Fatalf("ts snap failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts snap: expected exit code 0, got %d (output: %q)", exitCode, output)
	}
	t.Logf("ts snap output: %s", output)

	// Test ts snaps: list snapshots, should include the one we just created
	output, exitCode, err = sshExec(t, d, "testframe", "ts snaps")
	if err != nil {
		t.Fatalf("ts snaps failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts snaps: expected exit code 0, got %d (output: %q)", exitCode, output)
	}
	// Should list at least one snapshot
	if output == "" || strings.TrimSpace(output) == "" {
		t.Errorf("ts snaps: expected at least one snapshot listed, got empty output")
	}
	t.Logf("ts snaps output: %s", output)

	// Test ts log: show history for this frame, should include our snap
	output, exitCode, err = sshExec(t, d, "testframe", "ts log")
	if err != nil {
		t.Fatalf("ts log failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts log: expected exit code 0, got %d (output: %q)", exitCode, output)
	}
	// Should show at least one entry
	if output == "" || strings.TrimSpace(output) == "" {
		t.Errorf("ts log: expected at least one log entry, got empty output")
	}
	t.Logf("ts log output: %s", output)

	// Test ts frames: list frames and verify session count
	// First, check frames with no long-running sessions
	output, exitCode, err = sshExec(t, d, "testframe", "ts frames")
	if err != nil {
		t.Fatalf("ts frames failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts frames: expected exit code 0, got %d (output: %q)", exitCode, output)
	}
	// Should list our frame
	if !strings.Contains(output, "testframe") {
		t.Errorf("ts frames: expected output to contain 'testframe', got: %q", output)
	}
	t.Logf("ts frames output: %s", output)

	// Test session count: open a long-running session and verify ts frames
	// reports the active session count correctly.
	config := sshConfig("testframe")
	client1, err := ssh.Dial("tcp", d.addr, config)
	if err != nil {
		t.Fatalf("SSH dial for session count test: %v", err)
	}
	defer client1.Close()

	sess1, err := client1.NewSession()
	if err != nil {
		t.Fatalf("new session 1: %v", err)
	}
	defer sess1.Close()

	// Start a long-running command (cat waits for input)
	stdin1, err := sess1.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe 1: %v", err)
	}
	if err := sess1.Start("cat"); err != nil {
		t.Fatalf("start cat: %v", err)
	}

	// Give the session time to register
	time.Sleep(500 * time.Millisecond)

	// Check ts frames shows at least 1 active session
	output, exitCode, err = sshExec(t, d, "testframe", "ts frames")
	if err != nil {
		t.Fatalf("ts frames (with active session) failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts frames: expected exit code 0, got %d", exitCode)
	}
	t.Logf("ts frames (with 1 cat session): %s", output)
	// The output should show a non-zero session count for testframe.
	// Format is typically: "testframe  N" where N is the session count.
	// We check that the line with testframe doesn't say "0" or "stopped".
	if strings.Contains(output, "testframe") {
		lines := strings.Split(output, "\n")
		for _, line := range lines {
			if strings.Contains(line, "testframe") {
				// Should not be stopped or 0 sessions
				if strings.Contains(line, "stopped") || strings.HasSuffix(strings.TrimSpace(line), " 0") {
					t.Errorf("ts frames: expected active sessions, but got: %q", line)
				}
			}
		}
	}

	// Open a second long-running session
	sess2, err := client1.NewSession()
	if err != nil {
		t.Fatalf("new session 2: %v", err)
	}
	defer sess2.Close()

	stdin2, err := sess2.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe 2: %v", err)
	}
	if err := sess2.Start("cat"); err != nil {
		t.Fatalf("start cat 2: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	// Check ts frames shows at least 2 active sessions
	output, exitCode, err = sshExec(t, d, "testframe", "ts frames")
	if err != nil {
		t.Fatalf("ts frames (with 2 active sessions) failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts frames: expected exit code 0, got %d", exitCode)
	}
	t.Logf("ts frames (with 2 cat sessions): %s", output)

	// Close the long-running sessions
	stdin1.Close()
	sess1.Wait()
	stdin2.Close()
	sess2.Wait()

	time.Sleep(500 * time.Millisecond)

	// Verify session count decreased
	output, exitCode, err = sshExec(t, d, "testframe", "ts frames")
	if err != nil {
		t.Fatalf("ts frames (after closing sessions) failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts frames: expected exit code 0, got %d", exitCode)
	}
	t.Logf("ts frames (after closing cat sessions): %s", output)
}

// TestSSHContainerUserRoot tests SSH as root user to a container frame.
// Identity is probed by side effect: only uid 0 can create a file directly
// under / (mode 0755, owned by root), so a successful write proves we are root.
func TestSSHContainerUserRoot(t *testing.T) {
	env := newTestEnv(t)

	frameUUID := "00000000-0000-0000-0000-000000000002"
	createTestFrame(t, env, frameUUID)
	createTestRef(t, env, "rootframe", frameUUID)

	d := startDaemon(t, env)

	// SSH as root@frame and try to create a file in /. Root may write to /,
	// so this should succeed and echo OK.
	output, exitCode, err := sshExec(t, d, "root@rootframe", "echo hi > /rootprobe && echo OK")
	if err != nil {
		t.Fatalf("sshExec failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("Expected exit code 0 (root can write to /), got %d (output: %q)", exitCode, output)
	}
	if !strings.Contains(output, "OK") {
		t.Errorf("Expected output to contain OK, got: %q", output)
	}
	t.Logf("Output: %s", output)
}

// TestSSHContainerUserNonRoot tests SSH as a non-root user to a container frame.
// Identity is probed by side effect: a non-root user cannot create a file
// directly under / (which is owned by root, mode 0755), so the write must fail.
func TestSSHContainerUserNonRoot(t *testing.T) {
	env := newTestEnv(t)

	frameUUID := "00000000-0000-0000-0000-000000000003"
	createTestFrame(t, env, frameUUID)
	createTestRef(t, env, "userframe", frameUUID)

	d := startDaemon(t, env)

	// SSH as user@frame (runs as the unprivileged "user" account) and try to
	// create a file in /. This must FAIL: a non-root user cannot write to /.
	output, exitCode, err := sshExec(t, d, "user@userframe", "echo hi > /userprobe && echo OK")
	if err != nil {
		t.Fatalf("sshExec failed: %v", err)
	}
	if exitCode == 0 {
		t.Errorf("Expected non-zero exit (non-root cannot write to /), got 0 (output: %q)", output)
	}
	if strings.Contains(output, "OK") {
		t.Errorf("Did not expect OK (write to / should have failed), got: %q", output)
	}
	t.Logf("Output: %s (exit %d)", output, exitCode)
}

// TestSSHContainerWorkingDir tests that SSH sessions start in the correct working directory.
func TestSSHContainerWorkingDir(t *testing.T) {
	env := newTestEnv(t)

	frameUUID := "00000000-0000-0000-0000-000000000004"
	createTestFrame(t, env, frameUUID)
	createTestRef(t, env, "cwdframe", frameUUID)

	d := startDaemon(t, env)

	// SSH as user@frame and check pwd
	output, exitCode, err := sshExec(t, d, "user@cwdframe", "pwd")
	if err != nil {
		t.Fatalf("sshExec failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("Expected exit code 0, got %d", exitCode)
	}
	// Should start in user's home directory
	if !strings.Contains(output, "/home/user") {
		t.Errorf("Expected pwd to be /home/user, got: %q", output)
	}
	t.Logf("Output: %s", output)
}

// TestSSHContainerStdoutStderrSeparate verifies that the daemon keeps a
// session's stdout and stderr on distinct channels rather than merging them
// onto stdout. The command writes a known marker to each stream; with the
// streams properly separated, the stdout marker lands only in the SSH
// session's stdout and the stderr marker only in its stderr. If the daemon
// (incorrectly) folds stderr into stdout, the stderr marker leaks into the
// stdout buffer and this test fails.
func TestSSHContainerStdoutStderrSeparate(t *testing.T) {
	env := newTestEnv(t)

	frameUUID := "00000000-0000-0000-0000-000000000005"
	createTestFrame(t, env, frameUUID)
	createTestRef(t, env, "streamframe", frameUUID)

	d := startDaemon(t, env)

	// Emit OUTMARK on stdout and ERRMARK on stderr (fd 2).
	stdout, stderr, exitCode, err := sshExecSplit(t, d, "user@streamframe",
		"echo OUTMARK; echo ERRMARK 1>&2")
	if err != nil {
		t.Fatalf("sshExecSplit failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("Expected exit code 0, got %d (stdout: %q, stderr: %q)", exitCode, stdout, stderr)
	}

	if !strings.Contains(stdout, "OUTMARK") {
		t.Errorf("Expected stdout to contain OUTMARK, got: %q", stdout)
	}
	if !strings.Contains(stderr, "ERRMARK") {
		t.Errorf("Expected stderr to contain ERRMARK, got: %q", stderr)
	}
	// The bug: stderr gets merged into stdout. If so, ERRMARK shows up on
	// the SSH client's stdout where it never should.
	if strings.Contains(stdout, "ERRMARK") {
		t.Errorf("stderr leaked into stdout: stderr should go to its own channel, but stdout contained ERRMARK (stdout: %q, stderr: %q)", stdout, stderr)
	}

	t.Logf("stdout: %q stderr: %q", stdout, stderr)
}

// safeBuffer is a goroutine-safe bytes accumulator: the SSH session's I/O
// goroutine writes into it while the test reads it.
type safeBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// installBusyboxFor installs a real busybox /bin/sh plus an applet symlink for
// `name` (e.g. "stty", "printf") into the frame so PTY tests can drive a real
// shell and run real coreutils-style commands. The fixture /bin/sh is just the
// ts binary and cannot honour `stty` or termios changes.
func installBusyboxApplet(t *testing.T, framePath, name string) {
	t.Helper()
	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Fatalf("busybox required: %v", err)
	}
	dst := filepath.Join(framePath, "bin", name)
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove %s: %v", name, err)
	}
	if err := copyFile(busybox, dst); err != nil {
		t.Fatalf("copy busybox %s: %v", name, err)
	}
	if err := os.Chmod(dst, 0755); err != nil {
		t.Fatalf("chmod %s: %v", name, err)
	}
}

// TestSSHContainerPtyRawNoCRInjection verifies that a PTY session does NOT
// inject a carriage return into the byte stream when the application has put
// the terminal into raw mode. This reproduces the "joe redraws the current
// line" bug: an interactive editor sets its tty to raw (-opost -onlcr), so a
// bare "\n" it writes must reach the SSH client as a bare "\n". If the relay
// path adds a "\r" (turning "\n" into "\r\n") the editor's cursor model and the
// actual terminal disagree and the current line corrupts.
//
// The minimal oracle is: over a real PTY SSH session run `stty raw` and then
// emit a known marker followed by a single "\n". Because raw mode disables the
// inner pty's OPOST/ONLCR, the bytes after the marker must be exactly "\n" with
// no preceding "\r". If a "\r" appears, the relay injected it.
//
// Equivalent to the user-observed repro:
//
//	ssh -t deb@host 'stty raw; echo foo' | hd   # should be foo\n, not foo\r\n
func TestSSHContainerPtyRawNoCRInjection(t *testing.T) {
	env := newTestEnv(t)

	frameUUID := "00000000-0000-0000-0000-000000000006"
	framePath := createTestFrame(t, env, frameUUID)
	createTestRef(t, env, "rawframe", frameUUID)

	// A real /bin/sh, plus stty and printf applets, so the session can put the
	// pty into raw mode and emit a bare newline deterministically.
	installBusyboxShell(t, framePath)
	installBusyboxApplet(t, framePath, "stty")
	installBusyboxApplet(t, framePath, "printf")

	d := startDaemon(t, env)

	// Open an interactive PTY session as root (root runs /bin/sh -l directly).
	client, session, err := sshInteractive(t, d, "root@rawframe")
	if err != nil {
		t.Fatalf("sshInteractive: %v", err)
	}
	defer client.Close()
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	var outBuf safeBuffer
	session.Stdout = &outBuf
	session.Stderr = &outBuf

	if err := session.Shell(); err != nil {
		t.Fatalf("start shell: %v", err)
	}

	// MARKER is split across two literals so the shell's echo of the typed
	// command line (before `stty raw` silences echo) never itself contains the
	// assembled marker string — only the real printf output does.
	const marker = "RAWMARK"
	// `stty raw` disables OPOST/ONLCR + echo on the inner pty. After it, printf
	// writes the marker and a single bare \n. We then exit to end the session.
	script := `stty raw; printf '` + "RAW''MARK" + `\n'; exit` + "\n"
	if _, err := io.WriteString(stdin, script); err != nil {
		t.Fatalf("write script: %v", err)
	}

	// Wait for the session to finish (exit was sent) or time out.
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("timeout waiting for session; output so far: %q", outBuf.String())
	}

	out := outBuf.String()
	t.Logf("raw pty output: %q", out)

	idx := strings.Index(out, marker)
	if idx < 0 {
		t.Fatalf("marker %q not found in output: %q", marker, out)
	}
	rest := out[idx+len(marker):]
	if len(rest) == 0 {
		t.Fatalf("no bytes after marker; output: %q", out)
	}
	// In raw mode the byte immediately following the marker must be the bare
	// "\n" emitted by printf. A leading "\r" means the relay injected a CR.
	if rest[0] == '\r' {
		t.Errorf("relay injected a carriage return after a raw-mode newline: bytes after marker = %q (full output %q)", rest, out)
	}
	if rest[0] != '\n' {
		t.Errorf("expected bare newline after marker, got %q (full output %q)", rest, out)
	}
}
