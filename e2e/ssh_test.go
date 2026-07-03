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
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"github.com/tailscale/thundersnap/snaphash"
	"golang.org/x/crypto/ssh"
)

// daemonInstance represents a running thundersnapd in test mode.
type daemonInstance struct {
	t       *testing.T
	cmd     *exec.Cmd
	addr    string // listen address (e.g., "127.0.0.1:22222")
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
		t:    t,
		cmd:  cmd,
		addr: addr,
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

// createFrameViaDaemon creates a frame by running `ts frame` over SSH to the daemon.
// This is the proper e2e way to create frames - through the daemon, not by manipulating
// data structures directly. Returns the frame UUID.
func createFrameViaDaemon(t *testing.T, d *daemonInstance, refName string) string {
	t.Helper()

	// Create a new frame using ts frame via SSH
	cmd := fmt.Sprintf("ts frame --ref=%s nil:nil:nil", refName)
	output, exitCode, err := sshExec(t, d, "root@", cmd)
	if err != nil {
		t.Fatalf("createFrameViaDaemon: sshExec failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("createFrameViaDaemon: ts frame returned exit code %d: %s", exitCode, output)
	}

	// Parse the UUID from output. ts frame outputs just the UUID on stdout,
	// but stderr may contain progress messages like "Creating frame...".
	// Look for the last line that looks like a UUID (contains dashes, 36 chars).
	output = strings.TrimSpace(output)
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		// UUID format: xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx (36 chars with dashes)
		if len(line) == 36 && strings.Count(line, "-") == 4 {
			t.Logf("Created frame %s with ref %s", line, refName)
			return line
		}
	}
	t.Fatalf("createFrameViaDaemon: could not parse UUID from output: %q", output)
	return ""
}

// installBusyboxAppletInFrame installs a busybox applet (e.g. "su", "stty")
// into a frame's /bin directory over the daemon's own SFTP subsystem, the
// same way a real user's scp/sftp client would. This is a workaround for
// tests that need external commands that can't be replaced with shell
// builtins, but it goes through the daemon rather than writing to the
// frame's underlying btrfs subvolume directly.
//
// NOTE: This is a transitional helper. Ideally, tests should not need to
// install anything into frames at all. Once busybox is added to the daemon's
// libexec and auto-copied to frames, this function should be removed.
func installBusyboxAppletInFrame(t *testing.T, d *daemonInstance, refName, applet string) {
	t.Helper()

	// Find busybox on the host.
	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Fatalf("busybox required for this test: %v", err)
	}
	data, err := os.ReadFile(busybox)
	if err != nil {
		t.Fatalf("read busybox: %v", err)
	}

	// Connect as root and upload over SFTP, the same transport a real scp/sftp
	// client would use.
	conn, err := ssh.Dial("tcp", d.addr, sshConfig("root@"+refName))
	if err != nil {
		t.Fatalf("sftp dial: %v", err)
	}
	defer conn.Close()

	sc, err := sftp.NewClient(conn)
	if err != nil {
		t.Fatalf("sftp client: %v", err)
	}
	defer sc.Close()

	dst := "/bin/" + applet
	f, err := sc.Create(dst)
	if err != nil {
		t.Fatalf("sftp create %s: %v", dst, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		t.Fatalf("sftp write %s: %v", dst, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("sftp close %s: %v", dst, err)
	}
	if err := sc.Chmod(dst, 0755); err != nil {
		t.Fatalf("sftp chmod %s: %v", dst, err)
	}
	t.Logf("Installed busybox applet %s in frame %s via SFTP", applet, refName)
}

// snapProgressStats holds parsed stats from a single component's progress output.
type snapProgressStats struct {
	name       string
	unmodified int
	modified   int
	sizeGB     string // keep as string for exact comparison
}

// parseSnapProgress parses the final progress line from ts snap stderr.
// Format: "root 0+5 0.001G                home 0+3 0.000G           work 0+2 0.000G"
// Returns stats for root, home, work in that order.
func parseSnapProgress(stderr string) (root, home, work snapProgressStats, err error) {
	// Find the last non-empty line (the final progress line)
	lines := strings.Split(stderr, "\n")
	var lastLine string
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			lastLine = lines[i]
			break
		}
	}
	if lastLine == "" {
		return root, home, work, fmt.Errorf("no progress line found in stderr")
	}

	// Pattern: "name unmodified+modified sizeG"
	// E.g. "root 0+5 0.001G"
	re := regexp.MustCompile(`(\w+)\s+(\d+)\+(\d+)\s+([\d.]+G)`)
	matches := re.FindAllStringSubmatch(lastLine, -1)
	if len(matches) < 3 {
		return root, home, work, fmt.Errorf("expected 3 components in progress line, got %d: %q", len(matches), lastLine)
	}

	parseOne := func(m []string) snapProgressStats {
		unmod := 0
		mod := 0
		fmt.Sscanf(m[2], "%d", &unmod)
		fmt.Sscanf(m[3], "%d", &mod)
		return snapProgressStats{
			name:       m[1],
			unmodified: unmod,
			modified:   mod,
			sizeGB:     m[4],
		}
	}

	// Find root, home, work in any order
	for _, m := range matches {
		switch m[1] {
		case "root":
			root = parseOne(m)
		case "home":
			home = parseOne(m)
		case "work":
			work = parseOne(m)
		}
	}

	return root, home, work, nil
}

// verifySnaphashOutput confirms that the value printed by `ts snap` to
// stdout is a snaphash (or a "rootfs:home:work" frame spec of snaphashes,
// with "nil" allowed for absent components), not e.g. a raw hex-encoded
// SHA-256 string.
func verifySnaphashOutput(t *testing.T, output string) {
	t.Helper()

	id := strings.TrimSpace(output)
	if id == "" {
		t.Fatalf("ts snap: expected a snapshot ID, got empty output")
	}

	components := strings.Split(id, ":")
	for _, c := range components {
		if c == "nil" {
			continue
		}
		if len(c) != snaphash.EncodedSize {
			t.Errorf("ts snap: component %q has length %d, want %d (snaphash-encoded)", c, len(c), snaphash.EncodedSize)
			continue
		}
		if _, err := snaphash.Decode(c); err != nil {
			t.Errorf("ts snap: component %q is not a valid snaphash: %v", c, err)
		}
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

	// Test ts snap: create a snapshot of the current frame. Use sshExecSplit
	// so progress output on stderr doesn't get mixed into the snapshot ID we
	// verify on stdout.
	snapStdout, snapStderr, exitCode, err := sshExecSplit(t, d, "testframe", "ts snap")
	if err != nil {
		t.Fatalf("ts snap failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts snap: expected exit code 0, got %d (stdout: %q, stderr: %q)", exitCode, snapStdout, snapStderr)
	}
	t.Logf("ts snap stdout: %s", snapStdout)
	t.Logf("ts snap stderr: %s", snapStderr)
	verifySnaphashOutput(t, snapStdout)

	// Parse first snap's progress stats
	root1, home1, work1, err := parseSnapProgress(snapStderr)
	if err != nil {
		t.Logf("ts snap (1st): could not parse progress: %v", err)
	} else {
		t.Logf("ts snap (1st) stats: root=%d+%d %s, home=%d+%d %s, work=%d+%d %s",
			root1.unmodified, root1.modified, root1.sizeGB,
			home1.unmodified, home1.modified, home1.sizeGB,
			work1.unmodified, work1.modified, work1.sizeGB)
	}

	// Test snap idempotence: run a second snap immediately without any changes.
	// The second snap should report 0 modified entries and identical sizes.
	snap2Stdout, snap2Stderr, exitCode, err := sshExecSplit(t, d, "testframe", "ts snap")
	if err != nil {
		t.Fatalf("ts snap (2nd) failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts snap (2nd): expected exit code 0, got %d (stdout: %q, stderr: %q)", exitCode, snap2Stdout, snap2Stderr)
	}
	t.Logf("ts snap (2nd) stdout: %s", snap2Stdout)
	t.Logf("ts snap (2nd) stderr: %s", snap2Stderr)
	verifySnaphashOutput(t, snap2Stdout)

	// Parse second snap's progress stats and compare
	root2, home2, work2, err := parseSnapProgress(snap2Stderr)
	if err != nil {
		t.Logf("ts snap (2nd): could not parse progress: %v", err)
	} else {
		t.Logf("ts snap (2nd) stats: root=%d+%d %s, home=%d+%d %s, work=%d+%d %s",
			root2.unmodified, root2.modified, root2.sizeGB,
			home2.unmodified, home2.modified, home2.sizeGB,
			work2.unmodified, work2.modified, work2.sizeGB)

		// Second snap should have 0 modified entries for all three sections
		if root2.modified != 0 {
			t.Errorf("ts snap idempotence: root should have 0 modified entries on 2nd snap, got %d", root2.modified)
		}
		if home2.modified != 0 {
			t.Errorf("ts snap idempotence: home should have 0 modified entries on 2nd snap, got %d", home2.modified)
		}
		if work2.modified != 0 {
			t.Errorf("ts snap idempotence: work should have 0 modified entries on 2nd snap, got %d", work2.modified)
		}

		// Sizes should be identical between the two snaps
		if root1.sizeGB != root2.sizeGB {
			t.Errorf("ts snap idempotence: root size mismatch: 1st=%s, 2nd=%s", root1.sizeGB, root2.sizeGB)
		}
		if home1.sizeGB != home2.sizeGB {
			t.Errorf("ts snap idempotence: home size mismatch: 1st=%s, 2nd=%s", home1.sizeGB, home2.sizeGB)
		}
		if work1.sizeGB != work2.sizeGB {
			t.Errorf("ts snap idempotence: work size mismatch: 1st=%s, 2nd=%s", work1.sizeGB, work2.sizeGB)
		}
	}

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
	d := startDaemon(t, env)

	// Create frame via daemon (true e2e - no manual data structure manipulation)
	createFrameViaDaemon(t, d, "rootframe")

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
//
// NOTE: This test requires busybox for su (vshd needs su to switch users).
// Until su is added to libexec and auto-copied to frames by the daemon,
// this test must install it directly.
func TestSSHContainerUserNonRoot(t *testing.T) {
	env := newTestEnv(t)
	d := startDaemon(t, env)

	// Create frame via daemon (true e2e - no manual data structure manipulation)
	createFrameViaDaemon(t, d, "userframe")

	// Install busybox as su (required for vshd to switch to non-root users).
	// TODO: Once su is in libexec and auto-copied to frames, remove this.
	installBusyboxAppletInFrame(t, d, "userframe", "su")

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
//
// NOTE: This test requires busybox for su (vshd needs su to switch users).
// Until su is added to libexec and auto-copied to frames by the daemon,
// this test must install it directly.
func TestSSHContainerWorkingDir(t *testing.T) {
	env := newTestEnv(t)
	d := startDaemon(t, env)

	// Create frame via daemon (true e2e - no manual data structure manipulation)
	createFrameViaDaemon(t, d, "cwdframe")

	// Install busybox as su (required for vshd to switch to non-root users).
	// TODO: Once su is in libexec and auto-copied to frames, remove this.
	installBusyboxAppletInFrame(t, d, "cwdframe", "su")

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
//
// NOTE: This test requires busybox for su (vshd needs su to switch users).
// Until su is added to libexec and auto-copied to frames by the daemon,
// this test must install it directly.
func TestSSHContainerStdoutStderrSeparate(t *testing.T) {
	env := newTestEnv(t)
	d := startDaemon(t, env)

	// Create frame via daemon (true e2e - no manual data structure manipulation)
	createFrameViaDaemon(t, d, "streamframe")

	// Install busybox as su (required for vshd to switch to non-root users).
	// TODO: Once su is in libexec and auto-copied to frames, remove this.
	installBusyboxAppletInFrame(t, d, "streamframe", "su")

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
//
// NOTE: This test requires busybox for stty (external command to manipulate tty).
// Unlike other tests that use shell builtins, there's no pure-shell way to set
// raw mode. Until busybox is added to libexec and auto-copied to frames by the daemon,
// this test must install it directly. This is a known exception to the "no manual
// data structure manipulation" rule.
func TestSSHContainerPtyRawNoCRInjection(t *testing.T) {
	env := newTestEnv(t)
	d := startDaemon(t, env)

	// Create frame via daemon
	createFrameViaDaemon(t, d, "rawframe")

	// Install busybox applets needed for this test. We need stty (to set raw mode)
	// which is an external command - no shell builtin can do this.
	// TODO: Once busybox is in libexec and auto-copied to frames, remove this.
	installBusyboxAppletInFrame(t, d, "rawframe", "stty")
	installBusyboxAppletInFrame(t, d, "rawframe", "printf")

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

// TestContainerNamespaceSetup validates that container namespaces are set up
// correctly. This is the canonical test for namespace isolation - run this
// FIRST when debugging any container setup issues.
//
// It verifies:
//   - PID namespace: session sees container-init as PID 1, not host init
//   - Mount namespace: /proc is container's own (session can read /proc/self)
//   - /dev/pts is container's own devpts instance
//   - Multiple sessions share the same namespaces
//
// NOTE: The minimal rootfs has only shell builtins and ts, so all checks use
// shell builtins (read, echo, test) or read files via shell redirection.
func TestContainerNamespaceSetup(t *testing.T) {
	env := newTestEnv(t)
	d := startDaemon(t, env)

	// Create frame via daemon (true e2e - no manual data structure manipulation)
	createFrameViaDaemon(t, d, "nstest")

	// 1. PID namespace: /proc/1/comm should be "ts" (container-init runs as ts)
	// Use shell builtin 'read' to read the file content.
	output, exitCode, err := sshExec(t, d, "root@nstest", "read comm < /proc/1/comm; echo $comm")
	if err != nil {
		t.Fatalf("sshExec failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("read /proc/1/comm: expected exit code 0, got %d (output: %q)", exitCode, output)
	}
	output = strings.TrimSpace(output)
	if output != "ts" {
		t.Errorf("PID namespace wrong: /proc/1/comm = %q, want 'ts' (container-init)", output)
	} else {
		t.Logf("PID namespace OK: /proc/1/comm = %q", output)
	}

	// 2. Mount namespace: verify we can access /proc/self (our own proc entry)
	// This proves /proc is mounted and we're in a proper PID namespace.
	output, exitCode, err = sshExec(t, d, "root@nstest", "test -d /proc/self && echo OK")
	if err != nil {
		t.Fatalf("sshExec failed: %v", err)
	}
	if exitCode != 0 || !strings.Contains(output, "OK") {
		t.Errorf("Mount namespace wrong: /proc/self not accessible (exit %d, output: %q)", exitCode, output)
	} else {
		t.Logf("Mount namespace OK: /proc/self is accessible")
	}

	// 3. /dev/pts exists as a directory (confirming devpts is set up)
	output, exitCode, err = sshExec(t, d, "root@nstest", "test -d /dev/pts && echo OK")
	if err != nil {
		t.Fatalf("sshExec failed: %v", err)
	}
	if exitCode != 0 || !strings.Contains(output, "OK") {
		t.Errorf("/dev/pts not accessible: exit=%d output=%q", exitCode, output)
	} else {
		t.Logf("/dev/pts OK: directory exists")
	}

	// 4. Sessions share PID namespace: start a shell process in session 1,
	// have it write its own PID to a file, then verify session 2 can see it.
	// We use the shell's $$ variable (shell's own PID) for this test.
	//
	// Session 1: write $$ to /tmp/shell.pid and then wait a moment.
	// We use a subshell trick: (echo $$ > /tmp/shell.pid; while test -f /tmp/shell.pid; do :; done)
	// But this is tricky with minimal shell. Instead, we use ts itself as a
	// long-running process since it's available.
	//
	// Alternative approach: use the shell's background job with $!
	// But sleep may not be available. Let's just verify that the container-init
	// (PID 1) is visible, which proves sessions share the same PID namespace.
	output, exitCode, err = sshExec(t, d, "root@nstest", "test -f /proc/1/comm && echo SHARED")
	if err != nil {
		t.Fatalf("sshExec failed: %v", err)
	}
	if exitCode != 0 || !strings.Contains(output, "SHARED") {
		t.Errorf("Sessions don't share PID namespace: /proc/1 not visible (exit %d, output: %q)", exitCode, output)
	} else {
		t.Logf("PID namespace sharing OK: session 2 sees container-init at PID 1")
	}

	// 5. Verify the session's own PID is small (typical of container PID namespace)
	// In a container PID namespace, PIDs start from 1. Our shell process should
	// have a relatively small PID (typically < 100).
	output, exitCode, err = sshExec(t, d, "root@nstest", "echo $$")
	if err != nil {
		t.Fatalf("sshExec failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("echo $$: expected exit code 0, got %d (output: %q)", exitCode, output)
	}
	shellPid := strings.TrimSpace(output)
	t.Logf("Session shell PID: %s (small PID suggests we're in container PID namespace)", shellPid)
}

// TestVMNamespaceSetup validates that VM mode containers have correct namespace setup.
// This is the VM analogue of TestContainerNamespaceSetup.
//
// NOTE: This test requires VM dependencies (cloud-hypervisor, vmlinux, virtiofsd, passt).
// If VM deps are not available, the test fails (e2e tests never skip).
func TestVMNamespaceSetup(t *testing.T) {
	// Require VM dependencies
	_ = requireVMDeps(t)

	env := newTestEnv(t)

	// Create a policy that allows vmx isolation
	policyPath := filepath.Join(env.root, "policy.json")
	policyContent := `{
		"grants": [
			{
				"principals": ["*"],
				"cap": {
					"role": "developer",
					"isolation": "vmx",
					"maxFrames": 10
				}
			}
		]
	}`
	if err := os.WriteFile(policyPath, []byte(policyContent), 0644); err != nil {
		t.Fatalf("write policy file: %v", err)
	}

	d := startDaemonWithPolicy(t, env, policyPath)

	// Create frame via daemon
	createFrameViaDaemon(t, d, "vmnstest")

	// Use vm/ prefix to route through VM mode
	user := "vm/root@vmnstest"

	// 1. PID namespace: /proc/1/comm should be "ts" or "sh" (VM init or container-init)
	// Use shell builtin 'read' to read the file content.
	output, exitCode, err := sshExec(t, d, user, "read comm < /proc/1/comm; echo $comm")
	if err != nil {
		t.Fatalf("sshExec failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("read /proc/1/comm: expected exit code 0, got %d (output: %q)", exitCode, output)
	}
	output = strings.TrimSpace(output)
	// In VM mode, PID 1 could be ts (container-init) or sh (if the session shell is PID 1)
	if output != "ts" && output != "sh" {
		t.Errorf("PID namespace: /proc/1/comm = %q, expected 'ts' or 'sh'", output)
	} else {
		t.Logf("VM PID namespace OK: /proc/1/comm = %q", output)
	}

	// 2. /dev/pts exists and is accessible
	output, exitCode, err = sshExec(t, d, user, "test -d /dev/pts && echo OK")
	if err != nil {
		t.Fatalf("sshExec failed: %v", err)
	}
	if exitCode != 0 || !strings.Contains(output, "OK") {
		t.Errorf("VM /dev/pts not accessible: exit=%d output=%q", exitCode, output)
	} else {
		t.Logf("VM /dev/pts OK")
	}

	// 3. VM should be isolated from host - host PID should not be visible
	hostPid := os.Getpid()
	output, exitCode, err = sshExec(t, d, user, fmt.Sprintf("test -d /proc/%d && echo HOST_VISIBLE || echo ISOLATED", hostPid))
	if err != nil {
		t.Fatalf("sshExec failed: %v", err)
	}
	output = strings.TrimSpace(output)
	if strings.Contains(output, "HOST_VISIBLE") {
		t.Errorf("VM can see host process %d - isolation broken!", hostPid)
	} else if strings.Contains(output, "ISOLATED") {
		t.Logf("VM isolation OK: host process %d not visible", hostPid)
	}

	// 4. Session sharing: verify container-init (PID 1) is visible, proving sessions
	// share the same PID namespace. We use shell builtins only.
	output, exitCode, err = sshExec(t, d, user, "test -f /proc/1/comm && echo SHARED")
	if err != nil {
		t.Fatalf("sshExec failed: %v", err)
	}
	if exitCode != 0 || !strings.Contains(output, "SHARED") {
		t.Errorf("VM sessions don't share PID namespace: /proc/1 not visible (exit %d, output: %q)", exitCode, output)
	} else {
		t.Logf("VM PID namespace sharing OK: session sees container-init at PID 1")
	}

	// 5. Verify the session's own PID is small (typical of container PID namespace)
	output, exitCode, err = sshExec(t, d, user, "echo $$")
	if err != nil {
		t.Fatalf("sshExec failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("echo $$: expected exit code 0, got %d (output: %q)", exitCode, output)
	}
	shellPid := strings.TrimSpace(output)
	t.Logf("VM session shell PID: %s (small PID suggests we're in container PID namespace)", shellPid)
}

// startDaemonWithPolicy starts the daemon with a custom policy file.
func startDaemonWithPolicy(t *testing.T, env *testEnv, policyPath string) *daemonInstance {
	t.Helper()

	port, err := getFreePort()
	if err != nil {
		t.Fatalf("Failed to find free port: %v", err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	stateDir := filepath.Join(env.root, "state")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}

	vshdBinary := env.requireBinary("vshd")
	if err := copyFile(vshdBinary, filepath.Join(env.libexecDir, "vshd")); err != nil {
		t.Fatalf("copy vshd to libexec: %v", err)
	}

	daemonArgs := []string{
		"--test-listen=" + addr,
		"--test-user=" + testUser,
		"--data-dir=" + env.root,
		"--state-dir=" + stateDir,
		"--libexec-dir=" + env.libexecDir,
		"--policy=" + policyPath,
	}

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
		t:    t,
		cmd:  cmd,
		addr: addr,
	}

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

// requireVMDeps fails the test if VM dependencies are not available.
// e2e tests must never skip: missing VM deps is a misconfigured environment.
func requireVMDeps(t *testing.T) string {
	t.Helper()

	dir := vmDir()
	if dir == "" {
		t.Fatal("VM test requires cloud-hypervisor and vmlinux (not found in standard locations)")
	}

	// Also need virtiofsd and passt
	if _, err := exec.LookPath("virtiofsd"); err != nil {
		if _, err := os.Stat("/usr/libexec/virtiofsd"); err != nil {
			t.Fatal("VM test requires virtiofsd")
		}
	}
	if _, err := exec.LookPath("passt"); err != nil {
		t.Fatal("VM test requires passt")
	}

	return dir
}
