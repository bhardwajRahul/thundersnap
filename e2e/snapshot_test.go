// Package e2e contains end-to-end tests for thundersnap snapshot operations.
package e2e

import (
	"path/filepath"
	"testing"
)

// TestSnapshotOperationsBasic tests the basic snapshot workflow:
// 1. Create a test environment
// 2. Create a base snapshot
// 3. Start the test control server
// 4. Call /snap to create a new snapshot from a frame
// 5. Call /list-snaps to verify the snapshot appears
// 6. Verify the size is greater than 0
func TestSnapshotOperationsBasic(t *testing.T) {
	env := newTestEnv(t)

	// Create a base snapshot
	baseSnap := env.createBaseSnapshot()
	t.Logf("Created base snapshot: %s", baseSnap)

	// Start the test control server
	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startTestControlServer(t, env, sockPath)
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	// Create a frame from the base snapshot
	frameName := "snaptest"
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
	t.Logf("Created frame: %s", frameName)

	// Call /snap to create a snapshot of the frame
	snapResp, err := client.postJSON("/snap", map[string]string{
		"frame_name": frameName,
	})
	if err != nil {
		t.Fatalf("snap: %v", err)
	}
	if snapResp["status"] != "ok" {
		t.Fatalf("snap failed: %v", snapResp["message"])
	}

	snapshotID, ok := snapResp["snapshot_id"].(string)
	if !ok || snapshotID == "" {
		t.Fatalf("snap did not return snapshot_id: %v", snapResp)
	}
	t.Logf("Created snapshot: %s", snapshotID)

	// Call /list-snaps to verify the snapshot appears
	listResp, err := client.getJSON("/list-snaps")
	if err != nil {
		t.Fatalf("list-snaps: %v", err)
	}
	if listResp["status"] != "ok" {
		t.Fatalf("list-snaps failed: %v", listResp["error"])
	}

	snaps, ok := listResp["snaps"].([]interface{})
	if !ok {
		t.Fatalf("snaps is not a list: %T", listResp["snaps"])
	}

	// Find our snapshot in the list
	var foundSnap map[string]interface{}
	for _, s := range snaps {
		smap := s.(map[string]interface{})
		if smap["id"] == snapshotID {
			foundSnap = smap
			break
		}
	}

	if foundSnap == nil {
		t.Fatalf("snapshot %q not found in snaps list. Available: %v", snapshotID, snaps)
	}
	t.Logf("Found snapshot in list: %v", foundSnap)

	// Verify size is greater than 0
	size, ok := foundSnap["size"].(float64) // JSON numbers are float64
	if !ok {
		t.Fatalf("snapshot size is not a number: %T", foundSnap["size"])
	}
	if size <= 0 {
		t.Errorf("snapshot size should be > 0, got %v", size)
	}
	t.Logf("Snapshot size: %v bytes", size)
}
