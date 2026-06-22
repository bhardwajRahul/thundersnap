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
