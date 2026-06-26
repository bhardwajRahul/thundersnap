package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tailscale/thundersnap/frameid"
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
