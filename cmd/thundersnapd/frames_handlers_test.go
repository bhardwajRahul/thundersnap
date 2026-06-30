package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/tailscale/thundersnap/frameid"
	"github.com/tailscale/thundersnap/frames"
)

// TestHandleLogDefaultsToCurrentFrame verifies that /log without a uuid
// parameter returns the current frame's log (derived from the control server's
// rootFS path).
func TestHandleLogDefaultsToCurrentFrame(t *testing.T) {
	const user = "testuser"
	dataDir := t.TempDir()
	fsDir := filepath.Join(dataDir, "fs")
	initRefStore(dataDir)
	initFrameStore(dataDir)
	old := flagFsDir
	flagFsDir = &fsDir
	defer func() { flagFsDir = old }()

	// Create a frame with some history
	uuid := frameid.MustNew()
	frameStore := userFrameStore(user)
	frame := &frames.Frame{}
	if err := frameStore.Create(uuid, frame); err != nil {
		t.Fatalf("create frame: %v", err)
	}
	if err := frameStore.AddHistoryEntry(uuid, "snap123", "test message"); err != nil {
		t.Fatalf("add history: %v", err)
	}

	// Create a control server whose rootFS encodes this frame
	cs := &controlServer{rootFS: filepath.Join(fsDir, user, uuid.String())}

	// Request /log without uuid parameter - should use current frame
	r := httptest.NewRequest(http.MethodGet, "/log", nil)
	w := httptest.NewRecorder()
	cs.handleLog(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var result LogResponse
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if result.Status != "ok" {
		t.Errorf("status = %q, want ok", result.Status)
	}
	if result.UUID != uuid.String() {
		t.Errorf("uuid = %q, want %q", result.UUID, uuid.String())
	}
	if len(result.History) != 1 {
		t.Fatalf("history len = %d, want 1", len(result.History))
	}
	if result.History[0].Snap != "snap123" {
		t.Errorf("history[0].snap = %q, want snap123", result.History[0].Snap)
	}
}

// TestHandleLogExplicitUUID verifies that /log?uuid=<uuid> returns the
// specified frame's log.
func TestHandleLogExplicitUUID(t *testing.T) {
	const user = "testuser"
	dataDir := t.TempDir()
	fsDir := filepath.Join(dataDir, "fs")
	initRefStore(dataDir)
	initFrameStore(dataDir)
	old := flagFsDir
	flagFsDir = &fsDir
	defer func() { flagFsDir = old }()

	// Create two frames
	currentUUID := frameid.MustNew()
	otherUUID := frameid.MustNew()
	frameStore := userFrameStore(user)

	if err := frameStore.Create(currentUUID, &frames.Frame{}); err != nil {
		t.Fatalf("create current frame: %v", err)
	}
	if err := frameStore.Create(otherUUID, &frames.Frame{}); err != nil {
		t.Fatalf("create other frame: %v", err)
	}
	if err := frameStore.AddHistoryEntry(otherUUID, "other-snap", "other frame"); err != nil {
		t.Fatalf("add history to other: %v", err)
	}

	// Control server is for currentUUID, but we query otherUUID
	cs := &controlServer{rootFS: filepath.Join(fsDir, user, currentUUID.String())}

	r := httptest.NewRequest(http.MethodGet, "/log?uuid="+otherUUID.String(), nil)
	w := httptest.NewRecorder()
	cs.handleLog(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var result LogResponse
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if result.UUID != otherUUID.String() {
		t.Errorf("uuid = %q, want %q", result.UUID, otherUUID.String())
	}
	if len(result.History) != 1 {
		t.Fatalf("history len = %d, want 1", len(result.History))
	}
	if result.History[0].Snap != "other-snap" {
		t.Errorf("history[0].snap = %q, want other-snap", result.History[0].Snap)
	}
}
