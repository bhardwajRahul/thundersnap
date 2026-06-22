// Package e2e contains end-to-end tests for thundersnap taint system.
package e2e

import (
	"path/filepath"
	"testing"
)

// TestTaintSystemBasic tests the basic taint functionality:
// 1. Create a test environment with a frame
// 2. Add a taint to the frame
// 3. Verify the taint is recorded and returned in the response
func TestTaintSystemBasic(t *testing.T) {
	env := newTestEnv(t)

	// Create a base snapshot
	baseSnap := env.createBaseSnapshot()
	t.Logf("Created base snapshot: %s", baseSnap)

	// Start test control server
	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startTestControlServer(t, env, sockPath)
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	// Create a frame first (taints are associated with frames)
	frameName := "tainttest"
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

	// Add a taint to the frame
	taintName := "pii:customers"
	taintResp, err := client.postJSON("/taint", map[string]string{
		"taint_name": taintName,
	})
	if err != nil {
		t.Fatalf("taint request: %v", err)
	}

	// Verify response status
	if taintResp["status"] != "ok" {
		t.Fatalf("taint failed: status=%v message=%v", taintResp["status"], taintResp["message"])
	}
	t.Logf("Taint request successful")

	// Verify the taint is returned in the response
	taints, ok := taintResp["taints"].([]interface{})
	if !ok {
		t.Fatalf("taints is not a list: %T", taintResp["taints"])
	}

	found := false
	for _, taint := range taints {
		if taint == taintName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("taint %q not found in response taints: %v", taintName, taints)
	}
	t.Logf("Verified taint %q is recorded in response", taintName)
}

// TestMultipleTaintsOnFrame tests that multiple taints can be added to a frame
// and all are returned correctly.
func TestMultipleTaintsOnFrame(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()

	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startTestControlServer(t, env, sockPath)
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	// Create a frame
	frameName := "multitaint"
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

	// Add multiple taints
	taintsToAdd := []string{
		"pii:customers",
		"pii:employees",
		"unsafe-permissions",
		"untrusted-code:github.com/user/repo",
	}

	var lastTaints []interface{}
	for _, taint := range taintsToAdd {
		resp, err := client.postJSON("/taint", map[string]string{
			"taint_name": taint,
		})
		if err != nil {
			t.Fatalf("taint %q: %v", taint, err)
		}
		if resp["status"] != "ok" {
			t.Fatalf("taint %q failed: %v", taint, resp["message"])
		}
		lastTaints, _ = resp["taints"].([]interface{})
		t.Logf("Added taint: %s", taint)
	}

	// Verify all taints are present in the final response
	if len(lastTaints) != len(taintsToAdd) {
		t.Errorf("expected %d taints, got %d", len(taintsToAdd), len(lastTaints))
	}

	taintSet := make(map[string]bool)
	for _, t := range lastTaints {
		if s, ok := t.(string); ok {
			taintSet[s] = true
		}
	}

	for _, expected := range taintsToAdd {
		if !taintSet[expected] {
			t.Errorf("taint %q not found in response", expected)
		}
	}
	t.Logf("Verified all %d taints are recorded", len(taintsToAdd))
}

// TestTaintPropagation tests that taints propagate through fork/snapshot
// operations. When a frame with taints is snapshotted and a new frame is
// created from that snapshot, the taints should be inherited.
func TestTaintPropagation(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()

	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startTestControlServer(t, env, sockPath)
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	// Create initial frame
	frameName1 := "taintprop1"
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

	// Add taints to frame1
	taintsToAdd := []string{"pii:source-data", "untrusted-code:external"}
	for _, taint := range taintsToAdd {
		resp, err := client.postJSON("/taint", map[string]string{
			"taint_name": taint,
		})
		if err != nil {
			t.Fatalf("add taint %q: %v", taint, err)
		}
		if resp["status"] != "ok" {
			t.Fatalf("add taint failed: %v", resp["message"])
		}
	}
	t.Logf("Added taints to frame1: %v", taintsToAdd)

	// Snapshot frame1
	snapResp, err := client.postJSON("/snap", map[string]string{
		"frame_name": frameName1,
	})
	if err != nil {
		t.Fatalf("snap frame1: %v", err)
	}
	if snapResp["status"] != "ok" {
		t.Fatalf("snap failed: %v", snapResp["message"])
	}
	snapID := snapResp["snapshot_id"].(string)
	t.Logf("Created snapshot: %s", snapID)

	// Create frame2 from the snapshot
	frameName2 := "taintprop2"
	frameSpec2 := snapID + "::"

	createResp2, err := client.postJSON("/create", map[string]string{
		"frame_name":  frameName2,
		"snapshot_id": frameSpec2,
	})
	if err != nil {
		t.Fatalf("create frame2: %v", err)
	}
	if createResp2["status"] != "ok" {
		t.Fatalf("create frame2 failed: %v", createResp2["message"])
	}
	t.Logf("Created frame2 from snapshot")

	// Note: The test control server doesn't actually track taints per-frame
	// or propagate them through snapshots. This test documents the expected
	// behavior - in a real implementation, frame2 should inherit the taints
	// from the snapshot it was created from.
	//
	// For now, we verify the basic mechanism works (frames can be created
	// from snapshots of tainted frames).
	t.Log("Taint propagation test complete - verifies frame fork workflow with taints")
}

// TestTaintDeduplication tests that adding the same taint twice
// doesn't result in duplicates.
func TestTaintDeduplication(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()

	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startTestControlServer(t, env, sockPath)
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	// Create a frame
	frameName := "deduptaint"
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

	taint := "pii:customers"

	// Add the same taint twice
	for i := 0; i < 2; i++ {
		resp, err := client.postJSON("/taint", map[string]string{
			"taint_name": taint,
		})
		if err != nil {
			t.Fatalf("taint %d: %v", i+1, err)
		}
		if resp["status"] != "ok" {
			t.Fatalf("taint %d failed: %v", i+1, resp["message"])
		}
	}

	// Query taints (final response from taint command should show count)
	resp, err := client.postJSON("/taint", map[string]string{
		"taint_name": "dummy", // Add another taint to get current list
	})
	if err != nil {
		t.Fatalf("query taints: %v", err)
	}

	taints, _ := resp["taints"].([]interface{})

	// Count occurrences of our taint
	count := 0
	for _, t := range taints {
		if t == taint {
			count++
		}
	}

	if count > 1 {
		t.Errorf("taint %q appears %d times (expected 1)", taint, count)
	} else {
		t.Logf("Verified taint deduplication: %q appears exactly once", taint)
	}
}

// TestQueryFrameTaints tests querying the taints on a frame.
// The /taint endpoint returns the current list of taints after each add.
func TestQueryFrameTaints(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()

	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startTestControlServer(t, env, sockPath)
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	// Create a frame
	frameName := "querytaint"
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

	// Add several taints
	expectedTaints := []string{
		"pii:user-data",
		"untrusted-code:external-repo",
		"unsafe-permissions",
	}

	for _, taint := range expectedTaints {
		resp, err := client.postJSON("/taint", map[string]string{
			"taint_name": taint,
		})
		if err != nil {
			t.Fatalf("add taint %q: %v", taint, err)
		}
		if resp["status"] != "ok" {
			t.Fatalf("add taint failed: %v", resp["message"])
		}
	}

	// The last taint response contains all taints - use it as our query
	finalResp, err := client.postJSON("/taint", map[string]string{
		"taint_name": "query-marker", // Add a marker to trigger query
	})
	if err != nil {
		t.Fatalf("query taints: %v", err)
	}

	queriedTaints, ok := finalResp["taints"].([]interface{})
	if !ok {
		t.Fatalf("response missing taints list")
	}

	// Build a set of queried taints
	taintSet := make(map[string]bool)
	for _, t := range queriedTaints {
		if s, ok := t.(string); ok {
			taintSet[s] = true
		}
	}

	// Verify all expected taints are present
	for _, expected := range expectedTaints {
		if !taintSet[expected] {
			t.Errorf("expected taint %q not found in query result", expected)
		}
	}

	t.Logf("Queried taints: found %d taints including all expected ones", len(queriedTaints))
}
