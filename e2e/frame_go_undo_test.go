//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// TestTsFrame tests the ts frame command with various syntaxes.
func TestTsFrame(t *testing.T) {
	env := newTestEnv(t)
	d := startDaemon(t, env)

	// Create a frame to work with
	createFrameViaDaemon(t, d, "frametest")

	// Test: ts frame (no args) prints current frame UUID
	output, exitCode, err := sshExec(t, d, "root@frametest", "ts frame")
	if err != nil {
		t.Fatalf("ts frame failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts frame: expected exit 0, got %d (output: %q)", exitCode, output)
	}
	currentUUID := strings.TrimSpace(output)
	if currentUUID == "" {
		t.Fatalf("ts frame: expected UUID output, got empty")
	}
	t.Logf("ts frame -> %s", currentUUID)

	// Test: ts frame :: is a synonym for ts frame (prints same UUID)
	output, exitCode, err = sshExec(t, d, "root@frametest", "ts frame ::")
	if err != nil {
		t.Fatalf("ts frame :: failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts frame :: expected exit 0, got %d (output: %q)", exitCode, output)
	}
	if strings.TrimSpace(output) != currentUUID {
		t.Errorf("ts frame :: should return same UUID as ts frame: got %q, want %q",
			strings.TrimSpace(output), currentUUID)
	}

	// Test: ts frame <uuid> validates and prints the UUID
	output, exitCode, err = sshExec(t, d, "root@frametest", "ts frame "+currentUUID)
	if err != nil {
		t.Fatalf("ts frame <uuid> failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts frame <uuid>: expected exit 0, got %d (output: %q)", exitCode, output)
	}
	if strings.TrimSpace(output) != currentUUID {
		t.Errorf("ts frame <uuid>: got %q, want %q", strings.TrimSpace(output), currentUUID)
	}

	// Test: ts frame <ref> resolves ref to UUID
	output, exitCode, err = sshExec(t, d, "root@frametest", "ts frame frametest")
	if err != nil {
		t.Fatalf("ts frame <ref> failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts frame <ref>: expected exit 0, got %d (output: %q)", exitCode, output)
	}
	if strings.TrimSpace(output) != currentUUID {
		t.Errorf("ts frame <ref>: got %q, want %q", strings.TrimSpace(output), currentUUID)
	}

	// Test: ts frame with invalid spec (one colon) should error
	output, exitCode, err = sshExec(t, d, "root@frametest", "ts frame foo:bar")
	if err != nil {
		t.Fatalf("ts frame foo:bar failed: %v", err)
	}
	if exitCode == 0 {
		t.Errorf("ts frame foo:bar: expected non-zero exit for invalid spec")
	}

	// Test: ts frame with too many colons should error
	output, exitCode, err = sshExec(t, d, "root@frametest", "ts frame a:b:c:d")
	if err != nil {
		t.Fatalf("ts frame a:b:c:d failed: %v", err)
	}
	if exitCode == 0 {
		t.Errorf("ts frame a:b:c:d: expected non-zero exit for too many colons")
	}

	// Test: ts frame <snap>:: creates a new frame (inheriting home/work)
	// First, take a snapshot so we have a snap ID to use
	snapStdout, _, exitCode, err := sshExecSplit(t, d, "root@frametest", "ts snap")
	if err != nil {
		t.Fatalf("ts snap failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts snap: expected exit 0, got %d", exitCode)
	}
	snapTriplet := strings.TrimSpace(snapStdout)
	t.Logf("Created snap: %s", snapTriplet)

	// Extract just the root snap from the triplet (root:home:work -> root)
	// to test the <snap>:: syntax (use this root, inherit home/work)
	snapParts := strings.SplitN(snapTriplet, ":", 2)
	rootSnap := snapParts[0]

	// Now create a new frame from that snap
	output, exitCode, err = sshExec(t, d, "root@frametest", "ts frame "+rootSnap+"::")
	if err != nil {
		t.Fatalf("ts frame <snap>:: failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts frame <snap>::: expected exit 0, got %d (output: %q)", exitCode, output)
	}
	newUUID := strings.TrimSpace(output)
	if newUUID == "" || newUUID == currentUUID {
		t.Errorf("ts frame <snap>::: expected new UUID, got %q", newUUID)
	}
	t.Logf("ts frame <snap>:: -> %s (new frame)", newUUID)
}

// TestTsLog tests ts log shows frame history.
func TestTsLog(t *testing.T) {
	env := newTestEnv(t)
	d := startDaemon(t, env)

	// Create a frame
	createFrameViaDaemon(t, d, "logtest")

	// Initially no history
	output, exitCode, err := sshExec(t, d, "root@logtest", "ts log")
	if err != nil {
		t.Fatalf("ts log failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts log: expected exit 0, got %d", exitCode)
	}
	t.Logf("ts log (initial): %s", output)

	// Take a snap - this should add to history
	snapStdout, _, exitCode, err := sshExecSplit(t, d, "root@logtest", "ts snap")
	if err != nil {
		t.Fatalf("ts snap failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts snap: expected exit 0, got %d", exitCode)
	}
	snapID := strings.TrimSpace(snapStdout)
	t.Logf("Created snap: %s", snapID)

	// Now log should show the snap
	output, exitCode, err = sshExec(t, d, "root@logtest", "ts log")
	if err != nil {
		t.Fatalf("ts log failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ts log: expected exit 0, got %d", exitCode)
	}
	if !strings.Contains(output, snapID) {
		t.Errorf("ts log: expected to contain snap %s, got: %q", snapID, output)
	}
	t.Logf("ts log (after snap): %s", output)
}

// TestTsFrameCreatesNewFrameWithHistory tests that creating a new frame
// via ts frame clones the parent's history when done via ts go.
// This is a simpler test that just verifies ts frame creates frames.
func TestTsFrameCreatesNewFrame(t *testing.T) {
	env := newTestEnv(t)
	d := startDaemon(t, env)

	// Create initial frame
	createFrameViaDaemon(t, d, "parent")

	// Get parent UUID
	output, exitCode, err := sshExec(t, d, "root@parent", "ts frame")
	if err != nil {
		t.Fatalf("ts frame failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts frame: expected exit 0, got %d", exitCode)
	}
	parentUUID := strings.TrimSpace(output)

	// Take a snap in the parent
	snapStdout, _, exitCode, err := sshExecSplit(t, d, "root@parent", "ts snap")
	if err != nil {
		t.Fatalf("ts snap failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts snap: expected exit 0, got %d", exitCode)
	}
	snapTriplet := strings.TrimSpace(snapStdout)
	t.Logf("Parent snap: %s", snapTriplet)

	// Extract just the root snap from the triplet to use as the root component
	snapParts := strings.SplitN(snapTriplet, ":", 2)
	rootSnap := snapParts[0]

	// Create a child frame from the snap with a ref
	// Use rootSnap:nil:nil to use this root and nil for home/work
	output, exitCode, err = sshExec(t, d, "root@parent", "ts frame --ref=child "+rootSnap+":nil:nil")
	if err != nil {
		t.Fatalf("ts frame (create child) failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts frame (create child): expected exit 0, got %d (output: %q)", exitCode, output)
	}
	childUUID := strings.TrimSpace(output)
	t.Logf("Child UUID: %s", childUUID)

	if childUUID == parentUUID {
		t.Errorf("Child UUID should be different from parent")
	}

	// SSH to the child frame and verify it works
	output, exitCode, err = sshExec(t, d, "root@child", "echo hello from child")
	if err != nil {
		t.Fatalf("sshExec to child failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("echo in child: expected exit 0, got %d", exitCode)
	}
	if !strings.Contains(output, "hello from child") {
		t.Errorf("expected 'hello from child', got: %q", output)
	}
}
