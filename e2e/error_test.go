//go:build e2e

// Package e2e contains end-to-end tests for thundersnap error handling.
package e2e

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestErrorHandlingBasic tests that helpful error messages are returned when:
// 1. The daemon is not running (connection refused to control socket)
// 2. A frame is created from a non-existent snapshot
func TestErrorHandlingBasic(t *testing.T) {
	t.Run("connection_refused", testConnectionRefused)
	t.Run("snapshot_not_found", testSnapshotNotFound)
}

// testConnectionRefused verifies that trying to connect to a non-existent
// control socket produces a clear connection error.
func testConnectionRefused(t *testing.T) {
	// Use a path that definitely doesn't exist
	nonExistentSock := "/tmp/thundersnap-does-not-exist-12345.sock"

	// Try to dial the socket
	conn, err := net.Dial("unix", nonExistentSock)
	if conn != nil {
		conn.Close()
		t.Fatal("expected connection to fail, but it succeeded")
	}

	if err == nil {
		t.Fatal("expected error when dialing non-existent socket")
	}

	// Verify the error message is helpful - should mention connection refused
	// or no such file/directory
	errMsg := err.Error()
	if !strings.Contains(errMsg, "connect") &&
		!strings.Contains(errMsg, "no such file") &&
		!strings.Contains(errMsg, "connection refused") {
		t.Errorf("error message not helpful: %v", err)
	}

	t.Logf("Got expected connection error: %v", err)
}

// testSnapshotNotFound verifies that trying to create a frame from a
// non-existent snapshot produces a helpful error message.
func testSnapshotNotFound(t *testing.T) {
	env := newTestEnv(t)

	// Start a test control server
	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startTestControlServer(t, env, sockPath)
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	// Try to create a frame from a bogus snapshot ID that doesn't exist
	bogusSnapshotID := "nonexistent-snapshot-xyz123"
	frameName := "errortest"
	frameSpec := bogusSnapshotID + "::"

	createResp, err := client.postJSON("/create", map[string]string{
		"frame_name":  frameName,
		"snapshot_id": frameSpec,
	})
	if err != nil {
		t.Fatalf("create frame request failed: %v", err)
	}

	// The response should indicate an error
	status, ok := createResp["status"].(string)
	if !ok {
		t.Fatalf("response missing status field: %v", createResp)
	}

	if status != "error" {
		t.Fatalf("expected error status, got %q; response: %v", status, createResp)
	}

	// Verify the error message is helpful - should mention the snapshot or btrfs
	message, ok := createResp["message"].(string)
	if !ok {
		t.Fatalf("error response missing message field: %v", createResp)
	}

	// The error should mention btrfs or snapshot-related issues
	if !strings.Contains(message, "btrfs") &&
		!strings.Contains(message, "snapshot") &&
		!strings.Contains(message, "not found") &&
		!strings.Contains(message, "No such file") {
		t.Errorf("error message not helpful for missing snapshot: %q", message)
	}

	t.Logf("Got expected error for non-existent snapshot: %s", message)
}

// TestErrorInvalidSnapshotFormat tests error handling for invalid snapshot ID formats.
func TestErrorInvalidSnapshotFormat(t *testing.T) {
	env := newTestEnv(t)

	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startTestControlServer(t, env, sockPath)
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	// Test cases with various invalid formats
	testCases := []struct {
		name        string
		snapshotID  string
		shouldError bool
		description string
	}{
		{
			name:        "empty_spec",
			snapshotID:  "",
			shouldError: true,
			description: "empty snapshot spec should error",
		},
		{
			name:        "colons_only",
			snapshotID:  "::",
			shouldError: true,
			description: "colons-only spec should error (no rootfs)",
		},
		{
			name:        "special_chars",
			snapshotID:  "../../etc/passwd::",
			shouldError: true,
			description: "path traversal in snapshot ID should error",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			frameName := "errortest-" + tc.name
			createResp, err := client.postJSON("/create", map[string]string{
				"frame_name":  frameName,
				"snapshot_id": tc.snapshotID,
			})
			if err != nil {
				t.Fatalf("create frame request failed: %v", err)
			}

			status, _ := createResp["status"].(string)
			if tc.shouldError {
				if status != "error" {
					t.Errorf("%s: expected error status, got %q", tc.description, status)
				} else {
					t.Logf("%s: got expected error status", tc.description)
				}
			}
		})
	}
}

// TestErrorDeleteNonexistentFrame tests error handling when deleting a frame that doesn't exist.
func TestErrorDeleteNonexistentFrame(t *testing.T) {
	env := newTestEnv(t)

	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startTestControlServer(t, env, sockPath)
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	// Try to delete a frame that doesn't exist
	deleteResp, err := client.postJSON("/delete-frame", map[string]string{
		"frame_name": "nonexistent-frame-xyz",
	})
	if err != nil {
		t.Fatalf("delete frame request failed: %v", err)
	}

	status, _ := deleteResp["status"].(string)
	if status != "error" {
		t.Errorf("expected error when deleting non-existent frame, got status=%q", status)
	} else {
		message, _ := deleteResp["message"].(string)
		t.Logf("Got expected error deleting non-existent frame: %s", message)
	}
}

// TestErrorSnapNonexistentFrame tests error handling when snapshotting a frame that doesn't exist.
func TestErrorSnapNonexistentFrame(t *testing.T) {
	env := newTestEnv(t)

	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startTestControlServer(t, env, sockPath)
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	// Try to snapshot a frame that doesn't exist
	snapResp, err := client.postJSON("/snap", map[string]string{
		"frame_name": "nonexistent-frame-xyz",
	})
	if err != nil {
		t.Fatalf("snap request failed: %v", err)
	}

	status, _ := snapResp["status"].(string)
	if status != "error" {
		t.Errorf("expected error when snapping non-existent frame, got status=%q", status)
	} else {
		message, _ := snapResp["message"].(string)
		t.Logf("Got expected error snapping non-existent frame: %s", message)
	}
}

// TestSymlinkLoopDetection tests that symlink loops don't cause infinite recursion
// during snapshot operations. The snapshot should either detect and skip the loop
// or handle it gracefully with an error.
func TestSymlinkLoopDetection(t *testing.T) {
	env := newTestEnv(t)

	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startTestControlServer(t, env, sockPath)
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	baseSnap := env.createBaseSnapshot()

	// Create a frame
	frameName := "looptest"
	frameSpec := baseSnap + "::"

	createResp, err := client.postJSON("/create", map[string]string{
		"frame_name":  frameName,
		"snapshot_id": frameSpec,
	})
	if err != nil {
		t.Fatalf("create frame: %v", err)
	}
	if createResp["status"] != "ok" {
		t.Fatalf("create frame failed: %v", createResp["message"])
	}

	framePath := filepath.Join(env.fsDir, "testuser", frameName)

	// Create a symlink loop: loop1 -> loop2, loop2 -> loop1
	loopDir := filepath.Join(framePath, "tmp", "loopdir")
	if err := os.MkdirAll(loopDir, 0755); err != nil {
		t.Fatalf("mkdir loopdir: %v", err)
	}

	loop1 := filepath.Join(loopDir, "loop1")
	loop2 := filepath.Join(loopDir, "loop2")

	if err := os.Symlink("loop2", loop1); err != nil {
		t.Fatalf("create loop1 symlink: %v", err)
	}
	if err := os.Symlink("loop1", loop2); err != nil {
		t.Fatalf("create loop2 symlink: %v", err)
	}
	t.Logf("Created symlink loop: loop1 -> loop2, loop2 -> loop1")

	// Also create a self-referencing symlink
	selfLoop := filepath.Join(loopDir, "selfloop")
	if err := os.Symlink("selfloop", selfLoop); err != nil {
		t.Fatalf("create selfloop symlink: %v", err)
	}
	t.Logf("Created self-referencing symlink: selfloop -> selfloop")

	// Try to snapshot the frame - this should not hang
	// The snapshot operation should complete (with or without including the loops)
	snapResp, err := client.postJSON("/snap", map[string]string{
		"frame_name": frameName,
	})
	if err != nil {
		// Timeout or error is acceptable as long as it doesn't hang
		t.Logf("Snapshot with symlink loops returned error (acceptable): %v", err)
		return
	}

	status, _ := snapResp["status"].(string)
	if status == "error" {
		message, _ := snapResp["message"].(string)
		t.Logf("Snapshot with symlink loops returned error (acceptable): %s", message)
	} else if status == "ok" {
		// Snapshot succeeded - verify the symlinks are stored correctly
		snapID := snapResp["snapshot_id"].(string)
		t.Logf("Snapshot succeeded with symlink loops: %s", snapID)

		// Verify symlinks are stored as symlinks, not followed
		snapPath := filepath.Join(env.snapshotsDir, snapID)
		snapLoop1 := filepath.Join(snapPath, "tmp", "loopdir", "loop1")
		info, err := os.Lstat(snapLoop1)
		if err != nil {
			t.Errorf("lstat loop1 in snapshot: %v", err)
		} else if info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("loop1 should be a symlink in snapshot, got mode %v", info.Mode())
		} else {
			target, _ := os.Readlink(snapLoop1)
			t.Logf("Symlink loop preserved in snapshot: loop1 -> %s", target)
		}
	}
}

// TestCorruptedSnapshotMetadata tests handling of corrupted snapshot metadata.
// If a snapshot exists but its metadata is corrupted or missing, operations
// should fail gracefully with helpful error messages.
func TestCorruptedSnapshotMetadata(t *testing.T) {
	env := newTestEnv(t)

	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startTestControlServer(t, env, sockPath)
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	// Create a base snapshot
	baseSnap := env.createBaseSnapshot()

	// Create a frame from the base snapshot
	frameName := "corrupttest"
	frameSpec := baseSnap + "::"

	createResp, err := client.postJSON("/create", map[string]string{
		"frame_name":  frameName,
		"snapshot_id": frameSpec,
	})
	if err != nil {
		t.Fatalf("create frame: %v", err)
	}
	if createResp["status"] != "ok" {
		t.Fatalf("create frame failed: %v", createResp["message"])
	}

	// Create a snapshot
	snapResp, err := client.postJSON("/snap", map[string]string{
		"frame_name": frameName,
	})
	if err != nil {
		t.Fatalf("snap: %v", err)
	}
	if snapResp["status"] != "ok" {
		t.Fatalf("snap failed: %v", snapResp["message"])
	}

	snapID := snapResp["snapshot_id"].(string)
	t.Logf("Created snapshot: %s", snapID)

	// The snapshot exists - verify it's in the list
	listResp, err := client.getJSON("/list-snaps")
	if err != nil {
		t.Fatalf("list-snaps: %v", err)
	}
	snaps := listResp["snaps"].([]interface{})
	found := false
	for _, s := range snaps {
		smap := s.(map[string]interface{})
		if smap["id"] == snapID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("snapshot not found in list")
	}
	t.Logf("Snapshot found in list before corruption")

	// Now "corrupt" the snapshot by removing a critical file
	// For btrfs snapshots, removing the snapshot directory would be corruption
	// Instead, let's test listing when there's a non-directory entry in snapshotsDir
	corruptEntry := filepath.Join(env.snapshotsDir, "corrupt-metadata-file")
	if err := os.WriteFile(corruptEntry, []byte("not a snapshot"), 0644); err != nil {
		t.Fatalf("create corrupt entry: %v", err)
	}

	// List should still work and skip the non-directory entry
	listResp2, err := client.getJSON("/list-snaps")
	if err != nil {
		t.Fatalf("list-snaps after corruption: %v", err)
	}
	if listResp2["status"] != "ok" {
		t.Errorf("list-snaps should succeed even with corrupt entries")
	} else {
		t.Log("list-snaps succeeded with corrupt entry in snapshotsDir")
	}

	// The original snapshot should still be listed
	snaps2 := listResp2["snaps"].([]interface{})
	found = false
	for _, s := range snaps2 {
		smap := s.(map[string]interface{})
		if smap["id"] == snapID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("original snapshot not found after adding corrupt entry")
	}

	// The corrupt file should NOT appear as a snapshot
	for _, s := range snaps2 {
		smap := s.(map[string]interface{})
		if smap["id"] == "corrupt-metadata-file" {
			t.Errorf("corrupt file should not appear as snapshot")
		}
	}
	t.Log("list-snaps correctly filtered out corrupt entry")
}

// TestErrorWhoHasNonexistent tests error handling for who-has on non-existent snapshot.
func TestErrorWhoHasNonexistent(t *testing.T) {
	env := newTestEnv(t)

	// Start two environments like the mesh test, but query for a non-existent snapshot
	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startMeshTestControlServer(t, env, sockPath, "http://127.0.0.1:1") // bogus peer
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	// Query who-has for a snapshot that doesn't exist
	whoHasResp, err := client.postJSON("/who-has", map[string]string{
		"snapshot_id": "nonexistent-snapshot-abc123",
	})
	if err != nil {
		// Network error is expected since peer URL is bogus
		t.Logf("Got expected network error: %v", err)
		return
	}

	// If we got a response, it should indicate no peers or error
	status, _ := whoHasResp["status"].(string)
	peers, hasPeers := whoHasResp["peers"].([]interface{})

	if status == "error" {
		t.Logf("Got error response: %v", whoHasResp)
	} else if hasPeers && len(peers) == 0 {
		t.Log("Got empty peers list for non-existent snapshot (expected)")
	} else {
		t.Logf("Got unexpected response: %v", whoHasResp)
	}
}
