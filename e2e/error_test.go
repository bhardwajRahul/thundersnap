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
