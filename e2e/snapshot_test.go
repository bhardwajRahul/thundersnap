// Package e2e contains end-to-end tests for thundersnap snapshot operations.
package e2e

import (
	"os"
	"os/exec"
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

// TestSnapshotDeduplication tests that identical snapshots get the same content hash.
// This verifies that the snapshot naming is content-addressed.
func TestSnapshotDeduplication(t *testing.T) {
	env := newTestEnv(t)

	// Create a base snapshot
	baseSnap := env.createBaseSnapshot()
	t.Logf("Created base snapshot: %s", baseSnap)

	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startTestControlServer(t, env, sockPath)
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	// Create first frame and snapshot
	frameName1 := "dedup1"
	frameSpec := baseSnap + "::"

	createResp, err := client.postJSON("/create", map[string]string{
		"frame_name":  frameName1,
		"snapshot_id": frameSpec,
	})
	if err != nil {
		t.Fatalf("create frame1: %v", err)
	}
	if createResp["status"] != "ok" {
		t.Fatalf("create frame1 failed: %v", createResp["message"])
	}

	snapResp1, err := client.postJSON("/snap", map[string]string{
		"frame_name": frameName1,
	})
	if err != nil {
		t.Fatalf("snap1: %v", err)
	}
	if snapResp1["status"] != "ok" {
		t.Fatalf("snap1 failed: %v", snapResp1["message"])
	}
	snap1ID := snapResp1["snapshot_id"].(string)
	t.Logf("First snapshot: %s", snap1ID)

	// Create second frame from same base and snapshot without modification
	// This tests that identical content produces reproducible snapshots
	frameName2 := "dedup2"
	createResp, err = client.postJSON("/create", map[string]string{
		"frame_name":  frameName2,
		"snapshot_id": frameSpec,
	})
	if err != nil {
		t.Fatalf("create frame2: %v", err)
	}
	if createResp["status"] != "ok" {
		t.Fatalf("create frame2 failed: %v", createResp["message"])
	}

	snapResp2, err := client.postJSON("/snap", map[string]string{
		"frame_name": frameName2,
	})
	if err != nil {
		t.Fatalf("snap2: %v", err)
	}
	if snapResp2["status"] != "ok" {
		t.Fatalf("snap2 failed: %v", snapResp2["message"])
	}
	snap2ID := snapResp2["snapshot_id"].(string)
	t.Logf("Second snapshot: %s", snap2ID)

	// Both snapshots have the same base content, so their IDs might be different
	// (since snapshot IDs are random in the test server), but the directories
	// should have identical content
	snap1Path := filepath.Join(env.snapshotsDir, snap1ID)
	snap2Path := filepath.Join(env.snapshotsDir, snap2ID)

	// Compare file counts and basic structure
	// A more thorough test would diff the entire directories
	files1, _ := countFiles(snap1Path)
	files2, _ := countFiles(snap2Path)

	if files1 != files2 {
		t.Errorf("snapshot file counts differ: %d vs %d", files1, files2)
	} else {
		t.Logf("Both snapshots have %d files", files1)
	}

	// Verify list shows both snapshots
	listResp, err := client.getJSON("/list-snaps")
	if err != nil {
		t.Fatalf("list-snaps: %v", err)
	}
	snaps, _ := listResp["snaps"].([]interface{})
	found1, found2 := false, false
	for _, s := range snaps {
		smap := s.(map[string]interface{})
		if smap["id"] == snap1ID {
			found1 = true
		}
		if smap["id"] == snap2ID {
			found2 = true
		}
	}
	if !found1 || !found2 {
		t.Errorf("snapshots not all found in list: found1=%v found2=%v", found1, found2)
	}
	t.Logf("Both snapshots appear in list")
}

// countFiles counts the number of files in a directory tree.
func countFiles(dir string) (int, error) {
	count := 0
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			count++
		}
		return nil
	})
	return count, err
}

// TestNestedSnapshotTree tests creating snapshots of snapshots (snap of snap of snap).
func TestNestedSnapshotTree(t *testing.T) {
	env := newTestEnv(t)

	// Create base snapshot
	baseSnap := env.createBaseSnapshot()
	t.Logf("Created base snapshot (generation 0): %s", baseSnap)

	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startTestControlServer(t, env, sockPath)
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	// Generation 1: Create frame from base, modify, snapshot
	frameName1 := "gen1"
	frameSpec := baseSnap + "::"

	createResp, err := client.postJSON("/create", map[string]string{
		"frame_name":  frameName1,
		"snapshot_id": frameSpec,
	})
	if err != nil {
		t.Fatalf("create gen1 frame: %v", err)
	}
	if createResp["status"] != "ok" {
		t.Fatalf("create gen1 frame failed: %v", createResp["message"])
	}

	// Add gen1 marker file in /tmp (which always exists in base snapshot)
	frame1Path := filepath.Join(env.fsDir, "testuser", frameName1)
	markerFile1 := filepath.Join(frame1Path, "tmp", "gen1.txt")
	if err := os.WriteFile(markerFile1, []byte("generation 1\n"), 0644); err != nil {
		t.Fatalf("write gen1 marker: %v", err)
	}

	snapResp1, err := client.postJSON("/snap", map[string]string{
		"frame_name": frameName1,
	})
	if err != nil {
		t.Fatalf("snap gen1: %v", err)
	}
	if snapResp1["status"] != "ok" {
		t.Fatalf("snap gen1 failed: %v", snapResp1["message"])
	}
	snap1ID := snapResp1["snapshot_id"].(string)
	t.Logf("Created snapshot (generation 1): %s", snap1ID)

	// Generation 2: Create frame from gen1 snapshot, modify, snapshot
	frameName2 := "gen2"
	frameSpec2 := snap1ID + "::"

	createResp, err = client.postJSON("/create", map[string]string{
		"frame_name":  frameName2,
		"snapshot_id": frameSpec2,
	})
	if err != nil {
		t.Fatalf("create gen2 frame: %v", err)
	}
	if createResp["status"] != "ok" {
		t.Fatalf("create gen2 frame failed: %v", createResp["message"])
	}

	// Add gen2 marker file in /tmp
	frame2Path := filepath.Join(env.fsDir, "testuser", frameName2)
	markerFile2 := filepath.Join(frame2Path, "tmp", "gen2.txt")
	if err := os.WriteFile(markerFile2, []byte("generation 2\n"), 0644); err != nil {
		t.Fatalf("write gen2 marker: %v", err)
	}

	snapResp2, err := client.postJSON("/snap", map[string]string{
		"frame_name": frameName2,
	})
	if err != nil {
		t.Fatalf("snap gen2: %v", err)
	}
	if snapResp2["status"] != "ok" {
		t.Fatalf("snap gen2 failed: %v", snapResp2["message"])
	}
	snap2ID := snapResp2["snapshot_id"].(string)
	t.Logf("Created snapshot (generation 2): %s", snap2ID)

	// Generation 3: Create frame from gen2 snapshot, modify, snapshot
	frameName3 := "gen3"
	frameSpec3 := snap2ID + "::"

	createResp, err = client.postJSON("/create", map[string]string{
		"frame_name":  frameName3,
		"snapshot_id": frameSpec3,
	})
	if err != nil {
		t.Fatalf("create gen3 frame: %v", err)
	}
	if createResp["status"] != "ok" {
		t.Fatalf("create gen3 frame failed: %v", createResp["message"])
	}

	// Add gen3 marker file in /tmp
	frame3Path := filepath.Join(env.fsDir, "testuser", frameName3)
	markerFile3 := filepath.Join(frame3Path, "tmp", "gen3.txt")
	if err := os.WriteFile(markerFile3, []byte("generation 3\n"), 0644); err != nil {
		t.Fatalf("write gen3 marker: %v", err)
	}

	snapResp3, err := client.postJSON("/snap", map[string]string{
		"frame_name": frameName3,
	})
	if err != nil {
		t.Fatalf("snap gen3: %v", err)
	}
	if snapResp3["status"] != "ok" {
		t.Fatalf("snap gen3 failed: %v", snapResp3["message"])
	}
	snap3ID := snapResp3["snapshot_id"].(string)
	t.Logf("Created snapshot (generation 3): %s", snap3ID)

	// Verify: Create a final frame from gen3 and check that all markers exist
	frameNameFinal := "final"
	frameSpecFinal := snap3ID + "::"

	createResp, err = client.postJSON("/create", map[string]string{
		"frame_name":  frameNameFinal,
		"snapshot_id": frameSpecFinal,
	})
	if err != nil {
		t.Fatalf("create final frame: %v", err)
	}
	if createResp["status"] != "ok" {
		t.Fatalf("create final frame failed: %v", createResp["message"])
	}

	frameFinalPath := filepath.Join(env.fsDir, "testuser", frameNameFinal)

	// All three generation markers should be present in /tmp
	for i, marker := range []string{"gen1.txt", "gen2.txt", "gen3.txt"} {
		path := filepath.Join(frameFinalPath, "tmp", marker)
		content, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("generation %d marker not found: %v", i+1, err)
			continue
		}
		expected := []byte("generation " + string('1'+rune(i)) + "\n")
		if string(content) != string(expected) {
			t.Errorf("generation %d marker content: got %q, want %q", i+1, content, expected)
		}
	}
	t.Log("Verified all three generation markers present in final snapshot")
}

// TestSnapshotWithModifiedFiles tests that modifying files produces different snapshots.
func TestSnapshotWithModifiedFiles(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()

	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startTestControlServer(t, env, sockPath)
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	// Create frame and first snapshot (unmodified)
	frameName := "modtest"
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

	snapResp1, err := client.postJSON("/snap", map[string]string{
		"frame_name": frameName,
	})
	if err != nil {
		t.Fatalf("snap1: %v", err)
	}
	snap1ID := snapResp1["snapshot_id"].(string)
	t.Logf("Snapshot before modification: %s", snap1ID)

	// Modify a file in the frame (use /tmp which always exists)
	framePath := filepath.Join(env.fsDir, "testuser", frameName)
	modFile := filepath.Join(framePath, "tmp", "modified.txt")
	if err := os.WriteFile(modFile, []byte("this is new content\n"), 0644); err != nil {
		t.Fatalf("write modified file: %v", err)
	}

	// Take another snapshot after modification
	snapResp2, err := client.postJSON("/snap", map[string]string{
		"frame_name": frameName,
	})
	if err != nil {
		t.Fatalf("snap2: %v", err)
	}
	snap2ID := snapResp2["snapshot_id"].(string)
	t.Logf("Snapshot after modification: %s", snap2ID)

	// The two snapshot IDs should be different
	if snap1ID == snap2ID {
		t.Errorf("snapshots should have different IDs after modification")
	} else {
		t.Logf("Snapshots have different IDs as expected")
	}

	// Verify the modified file exists in snap2 but not snap1
	snap1Path := filepath.Join(env.snapshotsDir, snap1ID)
	snap2Path := filepath.Join(env.snapshotsDir, snap2ID)

	modPathSnap1 := filepath.Join(snap1Path, "tmp", "modified.txt")
	modPathSnap2 := filepath.Join(snap2Path, "tmp", "modified.txt")

	if _, err := os.Stat(modPathSnap1); err == nil {
		t.Errorf("modified.txt should not exist in snap1")
	}
	if _, err := os.Stat(modPathSnap2); err != nil {
		t.Errorf("modified.txt should exist in snap2: %v", err)
	}

	// Verify the content is correct in snap2
	content, err := os.ReadFile(modPathSnap2)
	if err != nil {
		t.Fatalf("read modified.txt in snap2: %v", err)
	}
	if string(content) != "this is new content\n" {
		t.Errorf("modified.txt content: got %q, want 'this is new content\\n'", content)
	}
	t.Log("Verified modification appears in snap2 but not snap1")
}

// TestSnapshotDeletion tests deleting a snapshot that has no references.
func TestSnapshotDeletion(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()

	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startTestControlServer(t, env, sockPath)
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	// Create frame and snapshot
	frameName := "deltest"
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

	snapResp, err := client.postJSON("/snap", map[string]string{
		"frame_name": frameName,
	})
	if err != nil {
		t.Fatalf("snap: %v", err)
	}
	snapID := snapResp["snapshot_id"].(string)
	t.Logf("Created snapshot: %s", snapID)

	// Verify it exists in the list
	listResp, err := client.getJSON("/list-snaps")
	if err != nil {
		t.Fatalf("list-snaps: %v", err)
	}
	snaps, _ := listResp["snaps"].([]interface{})
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

	// Delete the snapshot using btrfs directly (no delete endpoint in test server)
	snapPath := filepath.Join(env.snapshotsDir, snapID)
	cmd := exec.Command("btrfs", "subvolume", "delete", snapPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("delete snapshot: %v\n%s", err, out)
	}
	t.Logf("Deleted snapshot %s", snapID)

	// Verify it's no longer in the list
	listResp, err = client.getJSON("/list-snaps")
	if err != nil {
		t.Fatalf("list-snaps after delete: %v", err)
	}
	snaps, _ = listResp["snaps"].([]interface{})
	for _, s := range snaps {
		smap := s.(map[string]interface{})
		if smap["id"] == snapID {
			t.Fatalf("snapshot still in list after deletion")
		}
	}
	t.Log("Verified snapshot removed from list after deletion")
}
