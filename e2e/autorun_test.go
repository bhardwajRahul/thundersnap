// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestAutorunBasic tests that the ts autorun command correctly stores the
// autorun configuration and that it's displayed by ts autoruns.
func testAutorunBasic(t *testing.T, d *daemonInstance) {

	// Create frame via daemon (true e2e - no manual data structure manipulation)
	createFrameViaDaemon(t, d, "autoref")

	// First, verify the ref exists via ts refs
	output, exitCode, err := sshExec(t, d, "root@autoref", "ts refs")
	if err != nil {
		t.Fatalf("ts refs failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts refs returned exit code %d: %s", exitCode, output)
	}
	if !strings.Contains(output, "autoref") {
		t.Fatalf("expected 'autoref' in refs output, got: %s", output)
	}
	t.Logf("ts refs output: %s", output)

	// Set autorun via ts autorun command
	autorunCmd := "ts autorun --ref autoref /bin/sh -c 'echo hello'"
	output, exitCode, err = sshExec(t, d, "root@autoref", autorunCmd)
	if err != nil {
		t.Fatalf("ts autorun failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts autorun returned exit code %d: %s", exitCode, output)
	}
	if output != "" {
		t.Errorf("ts autorun printed output on success: %q", output)
	}

	// Verify the autorun is listed via ts autoruns. `ts refs` is deliberately
	// limited to frame status, UUID, and ref names.
	output, exitCode, err = sshExec(t, d, "root@autoref", "ts autoruns")
	if err != nil || exitCode != 0 {
		t.Fatalf("ts autoruns failed: exit=%d err=%v output=%s", exitCode, err, output)
	}
	if !strings.Contains(output, "/bin/sh") || !strings.Contains(output, "echo hello") {
		t.Errorf("expected autorun command in autoruns output, got: %s", output)
	}
	t.Logf("ts autoruns (after autorun set): %s", output)
}

// TestAutorunRefMove tests that when a ref is moved, the reflog records the move.
// NOTE: Autorun process restart on ref move is not implemented yet.
func testAutorunRefMove(t *testing.T, d *daemonInstance) {

	// Create two frames via daemon. First one gets the moveref, second is target.
	createFrameViaDaemon(t, d, "moveref")
	frameUUID2 := createFrameViaDaemon(t, d, "targetframe")

	// Set autorun on moveref
	autorunScript := "ts autorun --ref moveref /bin/sh -c 'echo running'"
	output, exitCode, err := sshExec(t, d, "root@moveref", autorunScript)
	if err != nil {
		t.Fatalf("ts autorun failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts autorun returned exit code %d: %s", exitCode, output)
	}
	t.Logf("Set autorun on moveref: %s", output)

	// Verify autorun is shown.
	output, exitCode, err = sshExec(t, d, "root@moveref", "ts autoruns")
	if err != nil || exitCode != 0 || !strings.Contains(output, "echo running") {
		t.Fatalf("ts autoruns failed: exit=%d err=%v output=%s", exitCode, err, output)
	}

	// Now move the ref to frame2 using ts ref move
	moveCmd := fmt.Sprintf("ts ref move moveref %s", frameUUID2)
	output, exitCode, err = sshExec(t, d, "root@moveref", moveCmd)
	if err != nil {
		t.Fatalf("ts ref move failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts ref move returned exit code %d: %s", exitCode, output)
	}
	t.Logf("Moved ref: %s", output)

	// Verify ref now points to frame2
	output, exitCode, err = sshExec(t, d, "root@moveref", "ts refs")
	if err != nil || exitCode != 0 {
		t.Fatalf("ts refs failed: exit=%d err=%v output=%s", exitCode, err, output)
	}
	if !strings.Contains(output, frameUUID2) {
		t.Errorf("expected ref to point to %s, got: %s", frameUUID2, output)
	}
	t.Logf("Refs after move: %s", output)
	output, exitCode, err = sshExec(t, d, "root@moveref", "ts autoruns")
	if err != nil || exitCode != 0 || !strings.Contains(output, "echo running") {
		t.Errorf("expected autorun to persist after move: exit=%d err=%v output=%s", exitCode, err, output)
	}

	// Verify reflog shows the move
	output, exitCode, err = sshExec(t, d, "root@moveref", "ts reflog moveref")
	if err != nil {
		t.Fatalf("ts reflog failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts reflog returned exit code %d: %s", exitCode, output)
	}
	// Reflog should have two entries (create + move)
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 2 {
		t.Errorf("expected at least 2 reflog entries, got %d: %s", len(lines), output)
	}
	t.Logf("Reflog after move:\n%s", output)
}

// TestAutorunStop tests that ts autorun --stop correctly clears the autorun config.
func testAutorunStop(t *testing.T, d *daemonInstance) {

	// Create frame via daemon (true e2e - no manual data structure manipulation)
	createFrameViaDaemon(t, d, "stopref")

	// Set autorun
	autorunScript := "ts autorun --ref stopref /bin/sh -c 'echo stop-marker'"
	output, exitCode, err := sshExec(t, d, "root@stopref", autorunScript)
	if err != nil {
		t.Fatalf("ts autorun failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts autorun returned exit code %d: %s", exitCode, output)
	}

	// Verify autorun is shown.
	output, exitCode, err = sshExec(t, d, "root@stopref", "ts autoruns")
	if err != nil || exitCode != 0 || !strings.Contains(output, "echo stop-marker") {
		t.Fatalf("autorun not shown: exit=%d err=%v output=%s", exitCode, err, output)
	}

	// Now stop the autorun
	output, exitCode, err = sshExec(t, d, "root@stopref", "ts autorun --ref stopref --stop")
	if err != nil {
		t.Fatalf("ts autorun --stop failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts autorun --stop returned exit code %d: %s", exitCode, output)
	}
	t.Logf("Stopped autorun: %s", output)

	// Verify autorun is no longer shown.
	output, exitCode, err = sshExec(t, d, "root@stopref", "ts autoruns")
	if err != nil || exitCode != 0 {
		t.Fatalf("ts autoruns failed: exit=%d err=%v output=%s", exitCode, err, output)
	}
	if strings.Contains(output, "echo stop-marker") {
		t.Errorf("autorun should be cleared, got: %s", output)
	}
}

// TestAutorunWithNonExistentRef tests that setting autorun on a non-existent ref fails.
func testAutorunWithNonExistentRef(t *testing.T, d *daemonInstance) {

	// Create frame via daemon (true e2e - no manual data structure manipulation)
	createFrameViaDaemon(t, d, "realref")

	// Try to set autorun on a non-existent ref
	output, exitCode, err := sshExec(t, d, "root@realref", "ts autorun --ref nonexistent /bin/sh -c 'echo hello'")
	if err != nil {
		t.Fatalf("sshExec failed: %v", err)
	}
	// Should fail with non-zero exit code
	if exitCode == 0 {
		t.Errorf("expected non-zero exit code for non-existent ref, got 0: %s", output)
	}
	if !strings.Contains(strings.ToLower(output), "not found") && !strings.Contains(strings.ToLower(output), "error") {
		t.Logf("Note: error message for non-existent ref: %s", output)
	}
	t.Logf("Autorun on non-existent ref (exit %d): %s", exitCode, output)
}

// TestAutorunShowsInRefs tests that refs remain listed independently of autorun
// configuration and that the configured command appears in ts autoruns.
func testAutorunShowsInRefs(t *testing.T, d *daemonInstance) {

	// Create two frames via daemon (true e2e - no manual data structure manipulation)
	createFrameViaDaemon(t, d, "ref-with-autorun")
	createFrameViaDaemon(t, d, "ref-without-autorun")

	// Set autorun only on the first ref
	output, exitCode, err := sshExec(t, d, "root@ref-with-autorun", "ts autorun --ref ref-with-autorun /bin/sh -c 'echo test'")
	if err != nil || exitCode != 0 {
		t.Fatalf("ts autorun failed: exit=%d err=%v output=%s", exitCode, err, output)
	}

	// List refs and verify display
	output, exitCode, err = sshExec(t, d, "root@ref-with-autorun", "ts refs")
	if err != nil || exitCode != 0 {
		t.Fatalf("ts refs failed: exit=%d err=%v output=%s", exitCode, err, output)
	}

	if !strings.Contains(output, "ref-with-autorun") || !strings.Contains(output, "ref-without-autorun") {
		t.Errorf("both refs should be listed: %s", output)
	}
	if strings.Contains(output, "autorun:") {
		t.Errorf("refs output should contain only frame fields: %s", output)
	}
	output, exitCode, err = sshExec(t, d, "root@ref-with-autorun", "ts autoruns")
	if err != nil || exitCode != 0 || !strings.Contains(output, "echo test") {
		t.Errorf("configured command missing from autoruns: exit=%d err=%v output=%s", exitCode, err, output)
	}
}

// TestAutorunMultiWordCommand tests that autorun commands with multiple arguments
// are stored and displayed correctly.
func testAutorunMultiWordCommand(t *testing.T, d *daemonInstance) {

	// Create frame via daemon (true e2e - no manual data structure manipulation)
	createFrameViaDaemon(t, d, "multiref")

	// Set autorun with a multi-argument command
	autorunCmd := "ts autorun --ref multiref /bin/sh -c 'echo hello world'"
	output, exitCode, err := sshExec(t, d, "root@multiref", autorunCmd)
	if err != nil {
		t.Fatalf("ts autorun failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts autorun returned exit code %d: %s", exitCode, output)
	}
	t.Logf("ts autorun output: %s", output)

	// `ts autoruns` emits one unambiguous JSON argv array per line.
	output, exitCode, err = sshExec(t, d, "root@multiref", "ts autoruns")
	if err != nil || exitCode != 0 {
		t.Fatalf("ts autoruns failed: exit=%d err=%v output=%s", exitCode, err, output)
	}
	want := []string{"/bin/sh", "-c", "echo hello world"}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		var got []string
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("ts autoruns output is not JSON argv arrays: %q: %v", output, err)
		}
		if reflect.DeepEqual(got, want) {
			found = true
		}
	}
	if !found {
		t.Errorf("ts autoruns did not contain %#v; output: %q", want, output)
	}
}

// TestAutorunProcessStarts tests that setting autorun actually starts the process.
// The process creates a marker file; we verify the file appears via SSH.
func testAutorunProcessStarts(t *testing.T, d *daemonInstance) {

	// Create frame via daemon (true e2e - no manual data structure manipulation)
	createFrameViaDaemon(t, d, "procstart")

	// Use a command that creates a marker file and keeps running.
	// We use "while true; do :; done" instead of sleep - it's pure shell, no external commands.
	autorunCmd := "ts autorun --ref procstart /bin/sh -c 'echo $HOME > /tmp/autorun-home; echo started > /tmp/autorun-marker; while true; do :; done'"
	output, exitCode, err := sshExec(t, d, "root@procstart", autorunCmd)
	if err != nil {
		t.Fatalf("ts autorun failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts autorun returned exit code %d: %s", exitCode, output)
	}
	t.Logf("Set autorun: %s", output)

	// Wait for the marker file to appear (check via SSH)
	if err := waitForFileViaSSH(t, d, "procstart", "/tmp/autorun-marker", 5*time.Second); err != nil {
		t.Fatalf("autorun process did not start: marker file not created: %v", err)
	}
	t.Logf("Autorun process started (marker file exists)")

	// Verify the marker content via SSH
	output, exitCode, err = sshExec(t, d, "root@procstart", "read -r content < /tmp/autorun-marker; echo $content")
	if err != nil {
		t.Fatalf("read marker file: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("read marker file failed: exit %d, output: %s", exitCode, output)
	}
	if !strings.Contains(output, "started") {
		t.Errorf("marker file content = %q, want 'started'", output)
	}

	output, exitCode, err = sshExec(t, d, "root@procstart", "read -r home < /tmp/autorun-home; echo $home")
	if err != nil || exitCode != 0 {
		t.Fatalf("read autorun HOME: err=%v exit=%d output=%q", err, exitCode, output)
	}
	if strings.TrimSpace(output) != "/home" {
		t.Errorf("autorun HOME = %q, want /home (same as user@ session)", strings.TrimSpace(output))
	}
}

// TestAutorunProcessStops tests that clearing autorun stops the running process.
func testAutorunProcessStops(t *testing.T, d *daemonInstance) {

	// Create frame via daemon (true e2e - no manual data structure manipulation)
	createFrameViaDaemon(t, d, "procstop")

	// Keep the autorun wrapper's stdout open in a background child. The shell can
	// exit on SIGHUP while that child remains, which exercises complete session
	// teardown rather than only reaping the session leader.
	autorunCmd := "ts autorun --ref procstop /bin/sh -c 'echo $$ > /tmp/autorun.pid; (while true; do echo alive > /tmp/autorun-alive; done) & wait'"
	output, exitCode, err := sshExec(t, d, "root@procstop", autorunCmd)
	if err != nil {
		t.Fatalf("ts autorun failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts autorun returned exit code %d: %s", exitCode, output)
	}

	// Wait for the PID file to appear (check via SSH)
	if err := waitForFileViaSSH(t, d, "procstop", "/tmp/autorun.pid", 5*time.Second); err != nil {
		t.Fatalf("autorun process did not start: PID file not created: %v", err)
	}
	t.Logf("Autorun process started (PID file exists)")

	// Now stop the autorun
	output, exitCode, err = sshExec(t, d, "root@procstop", "ts autorun --ref procstop --stop")
	if err != nil {
		t.Fatalf("ts autorun --stop failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts autorun --stop returned exit code %d: %s", exitCode, output)
	}
	t.Logf("Stopped autorun: %s", output)

	// The stop request waits for teardown. Replacing the liveness value and
	// observing that it remains replaced verifies the command itself is gone,
	// not merely its config. Retry because a write already in progress when the
	// process is killed can leave the file truncated once.
	deadline := time.Now().Add(time.Second)
	for {
		output, exitCode, err = sshExec(t, d, "root@procstop", "echo stopped > /tmp/autorun-alive")
		if err != nil || exitCode != 0 {
			t.Fatalf("replace autorun liveness value: err=%v exit=%d output=%q", err, exitCode, output)
		}
		time.Sleep(20 * time.Millisecond)
		output, exitCode, err = sshExec(t, d, "root@procstop", "read -r value < /tmp/autorun-alive; echo $value")
		if err != nil || exitCode != 0 {
			t.Fatalf("read autorun liveness value: err=%v exit=%d output=%q", err, exitCode, output)
		}
		if strings.TrimSpace(output) == "stopped" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("autorun command remained alive after --stop: liveness value %q", output)
		}
	}
}

// TestAutorunProcessRestartsOnRefMove tests that moving a ref stops the process
// in the old frame and starts it in the new frame.
func testAutorunProcessRestartsOnRefMove(t *testing.T, d *daemonInstance) {

	// Create two frames via daemon. First gets the procmove ref, second is target.
	createFrameViaDaemon(t, d, "procmove")
	frameUUID2 := createFrameViaDaemon(t, d, "targetmove")

	// Start autorun - it creates a marker file in the frame.
	// We use "while true; do :; done" instead of sleep - pure shell.
	autorunCmd := "ts autorun --ref procmove /bin/sh -c 'echo running > /tmp/autorun-marker; while true; do :; done'"
	output, exitCode, err := sshExec(t, d, "root@procmove", autorunCmd)
	if err != nil {
		t.Fatalf("ts autorun failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts autorun returned exit code %d: %s", exitCode, output)
	}

	// Wait for marker in frame1 (via SSH to procmove ref)
	if err := waitForFileViaSSH(t, d, "procmove", "/tmp/autorun-marker", 5*time.Second); err != nil {
		t.Fatalf("autorun process did not start in frame1: %v", err)
	}
	t.Logf("Autorun process started in frame1")

	// Verify marker doesn't exist in frame2 yet (via SSH to targetmove ref)
	_, exitCode, _ = sshExec(t, d, "root@targetmove", "test -f /tmp/autorun-marker")
	if exitCode == 0 {
		t.Fatalf("marker in frame2 should not exist yet")
	}

	// Now move the ref to frame2
	moveCmd := fmt.Sprintf("ts ref move procmove %s", frameUUID2)
	output, exitCode, err = sshExec(t, d, "root@procmove", moveCmd)
	if err != nil {
		t.Fatalf("ts ref move failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts ref move returned exit code %d: %s", exitCode, output)
	}
	t.Logf("Moved ref: %s", output)

	// Wait for marker in frame2 - the autorun should restart there
	// After the move, procmove ref now points to frame2
	if err := waitForFileViaSSH(t, d, "procmove", "/tmp/autorun-marker", 5*time.Second); err != nil {
		t.Fatalf("autorun process did not start in frame2 after ref move: %v", err)
	}
	t.Logf("Autorun process started in frame2 after ref move")

	// Verify marker content via SSH
	output, exitCode, err = sshExec(t, d, "root@procmove", "read -r content < /tmp/autorun-marker; echo $content")
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("read marker failed: exit %d", exitCode)
	}
	if !strings.Contains(output, "running") {
		t.Errorf("marker content = %q, want 'running'", output)
	}
}

// TestAutorunProcessAutoRestart tests that if an autorun process dies, it gets restarted.
func testAutorunProcessAutoRestart(t *testing.T, d *daemonInstance) {

	// Create frame via daemon (true e2e - no manual data structure manipulation)
	createFrameViaDaemon(t, d, "autorestart")

	// Start autorun with a command that writes a sequence number each time it starts,
	// then exits immediately. The autorun manager should restart it.
	// We use shell builtins only - read instead of cat, and exit immediately (no sleep needed).
	// The counter is read via: read n < file; if file doesn't exist, n is empty so we default to 0.
	autorunCmd := `ts autorun --ref autorestart /bin/sh -c 'read n < /tmp/start-count 2>/dev/null || n=0; n=$((n+1)); echo $n > /tmp/start-count'`
	output, exitCode, err := sshExec(t, d, "root@autorestart", autorunCmd)
	if err != nil {
		t.Fatalf("ts autorun failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts autorun returned exit code %d: %s", exitCode, output)
	}

	// Wait for the counter to reach at least 2 (meaning restart happened)
	// Check via SSH
	deadline := time.Now().Add(10 * time.Second)
	var lastCount int
	for time.Now().Before(deadline) {
		output, exitCode, _ := sshExec(t, d, "root@autorestart", "read n < /tmp/start-count 2>/dev/null && echo $n")
		if exitCode == 0 {
			fmt.Sscanf(strings.TrimSpace(output), "%d", &lastCount)
			if lastCount >= 2 {
				t.Logf("Autorun process restarted (count=%d)", lastCount)
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("autorun process did not auto-restart: count=%d after timeout", lastCount)
}

// waitForFileViaSSH waits for a file to exist inside a frame, checking via SSH.
// This is the true e2e way to check for files - through SSH, not direct filesystem access.
func waitForFileViaSSH(t *testing.T, d *daemonInstance, refName, filePath string, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	user := "root@" + refName
	for time.Now().Before(deadline) {
		// Use test -f to check if file exists
		_, exitCode, err := sshExec(t, d, user, fmt.Sprintf("test -f %s", filePath))
		if err == nil && exitCode == 0 {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("file %s did not appear within %v", filePath, timeout)
}

// readFileViaSSH reads a file's content via SSH.
// Uses shell builtins (read) instead of cat which may not be available.
func readFileViaSSH(t *testing.T, d *daemonInstance, refName, filePath string) (string, error) {
	t.Helper()
	user := "root@" + refName
	// Read file line by line and echo it
	output, exitCode, err := sshExec(t, d, user, fmt.Sprintf("while read -r line; do echo \"$line\"; done < %s", filePath))
	if err != nil {
		return "", fmt.Errorf("sshExec failed: %w", err)
	}
	if exitCode != 0 {
		return "", fmt.Errorf("read failed with exit code %d", exitCode)
	}
	return output, nil
}
