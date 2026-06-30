//go:build e2e

package e2e

import (
	"context"
	"fmt"
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

	cmd := exec.Command(env.daemonBinary,
		"--test-listen="+addr,
		"--test-user="+testUser,
		"--data-dir="+env.root, // Uses env.root; fs/ and snaps/ are created inside
		"--state-dir="+stateDir,
		"--libexec-dir="+env.libexecDir,
		"--policy="+policyPath,
	)
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

// TestSSHContainerBasic tests basic SSH connection to a container frame.
// This is the simplest possible e2e SSH test: start daemon, connect, run command.
func TestSSHContainerBasic(t *testing.T) {
	env := newTestEnv(t)

	// Create a test frame with a known UUID
	frameUUID := "00000000-0000-0000-0000-000000000001"
	createTestFrame(t, env, frameUUID)

	// Create a ref named "testframe" pointing to this UUID
	createTestRef(t, env, "testframe", frameUUID)

	// Start the daemon
	d := startDaemon(t, env)

	// SSH to the frame and run a simple command
	output, exitCode, err := sshExec(t, d, "testframe", "echo hello")
	if err != nil {
		t.Fatalf("sshExec failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("Expected exit code 0, got %d", exitCode)
	}
	if !strings.Contains(output, "hello") {
		t.Errorf("Expected output to contain 'hello', got: %q", output)
	}
	t.Logf("Output: %s", output)
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
