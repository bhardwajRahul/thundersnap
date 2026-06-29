package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/tailscale/thundersnap/frameid"
	"github.com/tailscale/thundersnap/refid"
	"github.com/tailscale/thundersnap/refs"
)

func TestRefHandlers(t *testing.T) {
	// Create temp directory for ref store
	dir := t.TempDir()
	initRefStore(dir)

	// Create a UUID for testing
	uuid := frameid.MustNew()

	// Test ref create
	t.Run("create", func(t *testing.T) {
		req := RefRequest{Name: "test-ref", UUID: uuid.String()}
		body, _ := json.Marshal(req)

		r := httptest.NewRequest(http.MethodPost, "/ref/create", bytes.NewReader(body))
		w := httptest.NewRecorder()
		handleRefCreate(w, r)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp RefResponse
		json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.Status != "ok" {
			t.Errorf("status = %q, want %q", resp.Status, "ok")
		}
	})

	// Test ref create duplicate
	t.Run("create_duplicate", func(t *testing.T) {
		req := RefRequest{Name: "test-ref", UUID: uuid.String()}
		body, _ := json.Marshal(req)

		r := httptest.NewRequest(http.MethodPost, "/ref/create", bytes.NewReader(body))
		w := httptest.NewRecorder()
		handleRefCreate(w, r)

		if w.Code != http.StatusConflict {
			t.Errorf("status = %d, want %d", w.Code, http.StatusConflict)
		}
	})

	// Test list refs
	t.Run("list", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/refs", nil)
		w := httptest.NewRecorder()
		handleListRefs(w, r)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
		}

		var resp RefListResponse
		json.Unmarshal(w.Body.Bytes(), &resp)
		if len(resp.Refs) != 1 {
			t.Errorf("refs count = %d, want 1", len(resp.Refs))
		}
		if resp.Refs[0].Name != "test-ref" {
			t.Errorf("ref name = %q, want %q", resp.Refs[0].Name, "test-ref")
		}
	})

	// Test reflog
	t.Run("reflog", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/reflog?name=test-ref", nil)
		w := httptest.NewRecorder()
		handleReflog(w, r)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
		}

		var resp ReflogResponse
		json.Unmarshal(w.Body.Bytes(), &resp)
		if len(resp.Reflog) != 1 {
			t.Errorf("reflog count = %d, want 1", len(resp.Reflog))
		}
	})

	// Test ref move
	t.Run("move", func(t *testing.T) {
		newUUID := frameid.MustNew()
		req := RefRequest{Name: "test-ref", UUID: newUUID.String()}
		body, _ := json.Marshal(req)

		r := httptest.NewRequest(http.MethodPost, "/ref/move", bytes.NewReader(body))
		w := httptest.NewRecorder()
		handleRefMove(w, r)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
		}

		// Verify reflog has 2 entries now
		r = httptest.NewRequest(http.MethodGet, "/reflog?name=test-ref", nil)
		w = httptest.NewRecorder()
		handleReflog(w, r)

		var resp ReflogResponse
		json.Unmarshal(w.Body.Bytes(), &resp)
		if len(resp.Reflog) != 2 {
			t.Errorf("reflog count after move = %d, want 2", len(resp.Reflog))
		}
	})

	// Test autorun set
	t.Run("autorun_set", func(t *testing.T) {
		req := AutorunRequest{RefName: "test-ref", Argv: []string{"/bin/sh", "-c", "echo hello"}}
		body, _ := json.Marshal(req)

		r := httptest.NewRequest(http.MethodPost, "/autorun", bytes.NewReader(body))
		w := httptest.NewRecorder()
		handleAutorun(w, r)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
		}

		// Verify autorun is set
		ref, _ := refStore.Get("test-ref")
		if len(ref.Autorun) != 3 {
			t.Errorf("autorun len = %d, want 3", len(ref.Autorun))
		}
	})

	// Test autorun clear
	t.Run("autorun_clear", func(t *testing.T) {
		req := AutorunRequest{RefName: "test-ref", Argv: nil}
		body, _ := json.Marshal(req)

		r := httptest.NewRequest(http.MethodPost, "/autorun", bytes.NewReader(body))
		w := httptest.NewRecorder()
		handleAutorun(w, r)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
		}

		// Verify autorun is cleared
		ref, _ := refStore.Get("test-ref")
		if len(ref.Autorun) != 0 {
			t.Errorf("autorun len = %d, want 0", len(ref.Autorun))
		}
	})

	// Test ref delete
	t.Run("delete", func(t *testing.T) {
		req := RefRequest{Name: "test-ref"}
		body, _ := json.Marshal(req)

		r := httptest.NewRequest(http.MethodPost, "/ref/delete", bytes.NewReader(body))
		w := httptest.NewRecorder()
		handleRefDelete(w, r)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
		}

		// Verify ref is gone
		if refStore.Exists("test-ref") {
			t.Error("ref still exists after delete")
		}
	})

	// Test ref not found
	t.Run("move_not_found", func(t *testing.T) {
		req := RefRequest{Name: "nonexistent", UUID: uuid.String()}
		body, _ := json.Marshal(req)

		r := httptest.NewRequest(http.MethodPost, "/ref/move", bytes.NewReader(body))
		w := httptest.NewRecorder()
		handleRefMove(w, r)

		if w.Code != http.StatusNotFound {
			t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
		}
	})
}

// TestRefDeleteForceScrubsFrameIdentity verifies that a force delete removes
// the ref's per-frame identity directory, not just the ref config. The frame's
// /id/<ref> here is a plain directory (a unit test cannot create btrfs
// subvolumes); refid.Remove falls back to os.RemoveAll for plain dirs, which is
// enough to prove the handler now resolves the frame UUID and wires the frame
// path through to refid.Remove on force.
func TestRefDeleteForceScrubsFrameIdentity(t *testing.T) {
	stateDir := t.TempDir()
	initRefStore(stateDir)

	fsDir := t.TempDir()
	old := flagFsDir
	flagFsDir = &fsDir
	defer func() { flagFsDir = old }()

	uuid := frameid.MustNew()
	if err := refStore.Create("secret-ref", uuid); err != nil {
		t.Fatalf("create ref: %v", err)
	}

	// The conflict guard checks the state-dir id dir; populate it so a
	// non-force delete is refused.
	if err := refStore.EnsureIDDir("secret-ref"); err != nil {
		t.Fatalf("ensure state-dir id dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "id", "secret-ref", "state"), []byte("x"), 0600); err != nil {
		t.Fatalf("write state-dir id state: %v", err)
	}

	// Populate the per-frame identity dir with key material; this is what a
	// force delete must scrub via refid.Remove.
	framePath := filepath.Join(fsDir, uuid.String())
	idPath := refid.Path(framePath, "secret-ref")
	if err := os.MkdirAll(idPath, 0700); err != nil {
		t.Fatalf("mkdir id path: %v", err)
	}
	keyPath := filepath.Join(idPath, "identity.key")
	if err := os.WriteFile(keyPath, []byte("private"), 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	// Without force, a non-empty identity dir blocks deletion.
	body, _ := json.Marshal(RefRequest{Name: "secret-ref"})
	r := httptest.NewRequest(http.MethodPost, "/ref/delete", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handleRefDelete(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("non-force delete status = %d, want %d; body=%s", w.Code, http.StatusConflict, w.Body.String())
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("identity key should survive a refused delete: %v", err)
	}

	// With force, the ref and its per-frame identity dir are both gone.
	body, _ = json.Marshal(RefRequest{Name: "secret-ref", Force: true})
	r = httptest.NewRequest(http.MethodPost, "/ref/delete", bytes.NewReader(body))
	w = httptest.NewRecorder()
	handleRefDelete(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("force delete status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if refStore.Exists("secret-ref") {
		t.Error("ref still exists after force delete")
	}
	if _, err := os.Stat(idPath); !os.IsNotExist(err) {
		t.Errorf("per-frame identity dir should be scrubbed after force delete, stat err = %v", err)
	}
}

func TestRefHandlersValidation(t *testing.T) {
	dir := t.TempDir()
	initRefStore(dir)

	// Test invalid ref name
	t.Run("invalid_name", func(t *testing.T) {
		req := RefRequest{Name: "-invalid", UUID: frameid.MustNew().String()}
		body, _ := json.Marshal(req)

		r := httptest.NewRequest(http.MethodPost, "/ref/create", bytes.NewReader(body))
		w := httptest.NewRecorder()
		handleRefCreate(w, r)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	// Test invalid UUID
	t.Run("invalid_uuid", func(t *testing.T) {
		req := RefRequest{Name: "valid-name", UUID: "not-a-uuid"}
		body, _ := json.Marshal(req)

		r := httptest.NewRequest(http.MethodPost, "/ref/create", bytes.NewReader(body))
		w := httptest.NewRecorder()
		handleRefCreate(w, r)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	// Test missing required fields
	t.Run("missing_fields", func(t *testing.T) {
		req := RefRequest{Name: ""}
		body, _ := json.Marshal(req)

		r := httptest.NewRequest(http.MethodPost, "/ref/create", bytes.NewReader(body))
		w := httptest.NewRecorder()
		handleRefCreate(w, r)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	// Test wrong HTTP method
	t.Run("wrong_method", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/ref/create", nil)
		w := httptest.NewRecorder()
		handleRefCreate(w, r)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
		}
	})
}

// Ensure refs package is imported for the test
var _ = refs.ErrRefExists
