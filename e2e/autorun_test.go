//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
)

// TestAutorunBasic tests that the ts autorun command correctly stores the
// autorun configuration and that it's displayed in ts refs.
func TestAutorunBasic(t *testing.T) {
	env := newTestEnv(t)

	// Create a frame for the test (valid hex UUID)
	frameUUID := "00000000-0000-0000-0000-a00000000001"
	createTestFrame(t, env, frameUUID)

	// Create a ref pointing to this frame
	createTestRef(t, env, "autoref", frameUUID)

	// Start the daemon
	d := startDaemon(t, env)

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
	autorunCmd := "ts autorun --ref autoref /bin/echo hello"
	output, exitCode, err = sshExec(t, d, "root@autoref", autorunCmd)
	if err != nil {
		t.Fatalf("ts autorun failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts autorun returned exit code %d: %s", exitCode, output)
	}
	t.Logf("ts autorun output: %s", output)

	// Verify the autorun is listed via ts refs
	output, exitCode, err = sshExec(t, d, "root@autoref", "ts refs")
	if err != nil {
		t.Fatalf("ts refs failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts refs returned exit code %d: %s", exitCode, output)
	}
	// The output should show the autorun config
	if !strings.Contains(output, "autorun:") {
		t.Errorf("expected autorun to be shown in refs output, got: %s", output)
	}
	if !strings.Contains(output, "/bin/echo hello") {
		t.Errorf("expected autorun command in refs output, got: %s", output)
	}
	t.Logf("ts refs (after autorun set): %s", output)
}

// TestAutorunRefMove tests that when a ref is moved, the reflog records the move.
// NOTE: Autorun process restart on ref move is not implemented yet.
func TestAutorunRefMove(t *testing.T) {
	env := newTestEnv(t)

	// Create two frames (valid hex UUIDs)
	frameUUID1 := "00000000-0000-0000-0000-a00000000002"
	createTestFrame(t, env, frameUUID1)

	frameUUID2 := "00000000-0000-0000-0000-a00000000003"
	createTestFrame(t, env, frameUUID2)

	// Create ref pointing to first frame
	createTestRef(t, env, "moveref", frameUUID1)

	// Start the daemon
	d := startDaemon(t, env)

	// Set autorun on moveref
	autorunScript := "ts autorun --ref moveref /bin/echo running"
	output, exitCode, err := sshExec(t, d, "root@moveref", autorunScript)
	if err != nil {
		t.Fatalf("ts autorun failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts autorun returned exit code %d: %s", exitCode, output)
	}
	t.Logf("Set autorun on moveref: %s", output)

	// Verify autorun is shown
	output, exitCode, err = sshExec(t, d, "root@moveref", "ts refs")
	if err != nil || exitCode != 0 {
		t.Fatalf("ts refs failed: exit=%d err=%v output=%s", exitCode, err, output)
	}
	if !strings.Contains(output, "autorun:") {
		t.Errorf("expected autorun in refs, got: %s", output)
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
	// Autorun config should still be there
	if !strings.Contains(output, "autorun:") {
		t.Errorf("expected autorun to persist after move, got: %s", output)
	}
	t.Logf("Refs after move: %s", output)

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
func TestAutorunStop(t *testing.T) {
	env := newTestEnv(t)

	// Create a frame (valid hex UUID)
	frameUUID := "00000000-0000-0000-0000-a00000000004"
	createTestFrame(t, env, frameUUID)

	createTestRef(t, env, "stopref", frameUUID)

	d := startDaemon(t, env)

	// Set autorun
	autorunScript := "ts autorun --ref stopref /bin/echo hello"
	output, exitCode, err := sshExec(t, d, "root@stopref", autorunScript)
	if err != nil {
		t.Fatalf("ts autorun failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts autorun returned exit code %d: %s", exitCode, output)
	}

	// Verify autorun is shown in refs
	output, exitCode, err = sshExec(t, d, "root@stopref", "ts refs")
	if err != nil {
		t.Fatalf("ts refs failed: %v", err)
	}
	if !strings.Contains(output, "autorun:") {
		t.Errorf("autorun not shown in refs output: %s", output)
	}
	t.Logf("Refs before stop: %s", output)

	// Now stop the autorun
	output, exitCode, err = sshExec(t, d, "root@stopref", "ts autorun --ref stopref --stop")
	if err != nil {
		t.Fatalf("ts autorun --stop failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts autorun --stop returned exit code %d: %s", exitCode, output)
	}
	t.Logf("Stopped autorun: %s", output)

	// Verify autorun is no longer shown in refs
	output, exitCode, err = sshExec(t, d, "root@stopref", "ts refs")
	if err != nil {
		t.Fatalf("ts refs failed: %v", err)
	}
	// Check if the autorun config is actually still there for stopref specifically
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "stopref") && strings.Contains(line, "autorun:") {
			t.Errorf("autorun should be cleared for stopref, but refs still shows: %s", line)
		}
	}
	t.Logf("Refs after stop:\n%s", output)
}

// TestAutorunWithNonExistentRef tests that setting autorun on a non-existent ref fails.
func TestAutorunWithNonExistentRef(t *testing.T) {
	env := newTestEnv(t)

	// Create a frame (but no ref named "nonexistent") - valid hex UUID
	frameUUID := "00000000-0000-0000-0000-a00000000005"
	createTestFrame(t, env, frameUUID)
	createTestRef(t, env, "realref", frameUUID)

	d := startDaemon(t, env)

	// Try to set autorun on a non-existent ref
	output, exitCode, err := sshExec(t, d, "root@realref", "ts autorun --ref nonexistent /bin/echo hello")
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

// TestAutorunShowsInRefs tests that refs with autorun configs are displayed correctly.
func TestAutorunShowsInRefs(t *testing.T) {
	env := newTestEnv(t)

	// Valid hex UUIDs
	frameUUID1 := "00000000-0000-0000-0000-a00000000006"
	createTestFrame(t, env, frameUUID1)
	createTestRef(t, env, "ref-with-autorun", frameUUID1)

	frameUUID2 := "00000000-0000-0000-0000-a00000000007"
	createTestFrame(t, env, frameUUID2)
	createTestRef(t, env, "ref-without-autorun", frameUUID2)

	d := startDaemon(t, env)

	// Set autorun only on the first ref
	output, exitCode, err := sshExec(t, d, "root@ref-with-autorun", "ts autorun --ref ref-with-autorun /bin/echo test")
	if err != nil || exitCode != 0 {
		t.Fatalf("ts autorun failed: exit=%d err=%v output=%s", exitCode, err, output)
	}

	// List refs and verify display
	output, exitCode, err = sshExec(t, d, "root@ref-with-autorun", "ts refs")
	if err != nil || exitCode != 0 {
		t.Fatalf("ts refs failed: exit=%d err=%v output=%s", exitCode, err, output)
	}

	lines := strings.Split(strings.TrimSpace(output), "\n")
	var foundWithAutorun, foundWithoutAutorun bool
	for _, line := range lines {
		if strings.Contains(line, "ref-with-autorun") {
			foundWithAutorun = true
			if !strings.Contains(line, "autorun:") {
				t.Errorf("ref-with-autorun should show autorun config, got: %s", line)
			}
		}
		if strings.Contains(line, "ref-without-autorun") {
			foundWithoutAutorun = true
			if strings.Contains(line, "autorun:") {
				t.Errorf("ref-without-autorun should NOT show autorun config, got: %s", line)
			}
		}
	}

	if !foundWithAutorun {
		t.Errorf("ref-with-autorun not found in output: %s", output)
	}
	if !foundWithoutAutorun {
		t.Errorf("ref-without-autorun not found in output: %s", output)
	}
	t.Logf("Refs output:\n%s", output)
}

// TestAutorunMultiWordCommand tests that autorun commands with multiple arguments
// are stored and displayed correctly.
func TestAutorunMultiWordCommand(t *testing.T) {
	env := newTestEnv(t)

	frameUUID := "00000000-0000-0000-0000-a00000000008"
	createTestFrame(t, env, frameUUID)
	createTestRef(t, env, "multiref", frameUUID)

	d := startDaemon(t, env)

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

	// Verify the full command is displayed
	output, exitCode, err = sshExec(t, d, "root@multiref", "ts refs")
	if err != nil || exitCode != 0 {
		t.Fatalf("ts refs failed: exit=%d err=%v output=%s", exitCode, err, output)
	}
	// Should contain the shell and its args
	if !strings.Contains(output, "/bin/sh") {
		t.Errorf("expected /bin/sh in autorun output, got: %s", output)
	}
	t.Logf("Refs output: %s", output)
}
