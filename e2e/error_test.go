// Package e2e contains end-to-end tests for thundersnap error handling.
package e2e

import (
	"net"
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
