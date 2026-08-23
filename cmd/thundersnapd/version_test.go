// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tailscale/thundersnap/version"
)

// TestHandleVersion covers the /version control endpoint: it returns the
// daemon's build version for a well-formed request, 400 for a wrong command,
// and 405 for a non-POST request.
func TestHandleVersion(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		body, _ := json.Marshal(ControlRequest{Command: "version"})
		r := httptest.NewRequest(http.MethodPost, "/version", bytes.NewReader(body))
		w := httptest.NewRecorder()
		handleVersion(w, r)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
		}
		var resp ControlResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Status != "ok" {
			t.Errorf("status = %q, want %q", resp.Status, "ok")
		}
		if resp.Version != version.String() {
			t.Errorf("version = %q, want %q", resp.Version, version.String())
		}
	})

	t.Run("wrong_command", func(t *testing.T) {
		body, _ := json.Marshal(ControlRequest{Command: "ping"})
		r := httptest.NewRequest(http.MethodPost, "/version", bytes.NewReader(body))
		w := httptest.NewRecorder()
		handleVersion(w, r)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusBadRequest, w.Body.String())
		}
	})

	t.Run("method_not_allowed", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/version", nil)
		w := httptest.NewRecorder()
		handleVersion(w, r)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
		}
	})
}
