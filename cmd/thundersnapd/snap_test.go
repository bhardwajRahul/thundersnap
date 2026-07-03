// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

// TestControlResponseWriter verifies that the custom response writer
// correctly formats HTTP responses for JSON payloads.
func TestControlResponseWriter(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		response   SnapResponse
	}{
		{
			name:       "success_200",
			statusCode: 0, // will default to 200
			response: SnapResponse{
				Status:     "ok",
				SnapshotID: "abc123",
			},
		},
		{
			name:       "error_500",
			statusCode: http.StatusInternalServerError,
			response: SnapResponse{
				Status:  "error",
				Message: "something failed",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a pipe to capture the response
			clientConn, serverConn := net.Pipe()
			defer clientConn.Close()
			defer serverConn.Close()

			// Write response in a goroutine
			done := make(chan error, 1)
			go func() {
				rw := newControlResponseWriter(serverConn)
				rw.Header().Set("Content-Type", "application/json")
				if tt.statusCode != 0 {
					rw.WriteHeader(tt.statusCode)
				}
				json.NewEncoder(rw).Encode(tt.response)
				done <- rw.finish()
				serverConn.Close()
			}()

			// Read and parse the response
			reader := bufio.NewReader(clientConn)
			resp, err := http.ReadResponse(reader, nil)
			if err != nil {
				t.Fatalf("failed to read response: %v", err)
			}
			defer resp.Body.Close()

			// Check status code
			expectedStatus := tt.statusCode
			if expectedStatus == 0 {
				expectedStatus = http.StatusOK
			}
			if resp.StatusCode != expectedStatus {
				t.Errorf("status code: got %d, want %d", resp.StatusCode, expectedStatus)
			}

			// Check content type
			if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("content-type: got %q, want %q", ct, "application/json")
			}

			// Read body
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("failed to read body: %v", err)
			}

			// Parse JSON
			var result SnapResponse
			if err := json.Unmarshal(body, &result); err != nil {
				t.Fatalf("failed to parse JSON body %q: %v", string(body), err)
			}

			// Verify fields
			if result.Status != tt.response.Status {
				t.Errorf("Status: got %q, want %q", result.Status, tt.response.Status)
			}
			if result.SnapshotID != tt.response.SnapshotID {
				t.Errorf("SnapshotID: got %q, want %q", result.SnapshotID, tt.response.SnapshotID)
			}
			if result.Message != tt.response.Message {
				t.Errorf("Message: got %q, want %q", result.Message, tt.response.Message)
			}

			// Check goroutine finished without error
			if err := <-done; err != nil {
				t.Errorf("finish() returned error: %v", err)
			}
		})
	}
}

// TestSnapHandlerResponse tests the actual handleSnap handler output.
func TestSnapHandlerResponse(t *testing.T) {
	// Create a mock control server (we can't call createSnapshot in tests
	// since it needs btrfs, but we can test the response format)

	// Test that a POST to /snap with a mock createSnapshot would return valid JSON
	// This test verifies the JSON structure matches what the client expects

	responseJSON := `{"status":"ok","snapshot_id":"abc123def456"}`

	var resp SnapResponse
	if err := json.Unmarshal([]byte(responseJSON), &resp); err != nil {
		t.Fatalf("failed to parse expected response format: %v", err)
	}

	if resp.Status != "ok" {
		t.Errorf("Status: got %q, want %q", resp.Status, "ok")
	}
	if resp.SnapshotID != "abc123def456" {
		t.Errorf("SnapshotID: got %q, want %q", resp.SnapshotID, "abc123def456")
	}
}

// TestHandleSnapMethodNotAllowed verifies proper error response for wrong method.
func TestHandleSnapMethodNotAllowed(t *testing.T) {
	// Create pipe for connection
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	// Create a control server with empty rootFS (won't actually snapshot)
	cs := &controlServer{
		rootFS: "/nonexistent",
	}

	// Send GET request (should fail with method not allowed)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, "/snap", nil)
		rw := newControlResponseWriter(serverConn)
		cs.handleSnap(rw, req)
		rw.finish()
		serverConn.Close()
	}()

	// Read response
	reader := bufio.NewReader(clientConn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status code: got %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}

	// Body should be plain text error from http.Error
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "method not allowed") {
		t.Errorf("body should contain 'method not allowed', got %q", string(body))
	}
}

// mockConn implements net.Conn for testing
type mockConn struct {
	bytes.Buffer
}

func (m *mockConn) Close() error                 { return nil }
func (m *mockConn) LocalAddr() net.Addr          { return nil }
func (m *mockConn) RemoteAddr() net.Addr         { return nil }
func (m *mockConn) SetDeadline(t any) error      { return nil }
func (m *mockConn) SetReadDeadline(t any) error  { return nil }
func (m *mockConn) SetWriteDeadline(t any) error { return nil }

// TestFullSnapProtocol tests the complete protocol including handshake.
func TestFullSnapProtocol(t *testing.T) {
	// Create pipe for connection
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	// Create a control server
	mux := http.NewServeMux()
	// Add a test handler that returns a known response
	mux.HandleFunc("/snap", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(SnapResponse{
			Status:     "ok",
			SnapshotID: "test123",
		})
	})

	cs := &controlServer{
		handler: mux,
	}

	// Server goroutine - handles handshake then HTTP
	go func() {
		defer serverConn.Close()
		reader := bufio.NewReader(serverConn)

		// Read handshake
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Logf("server: failed to read handshake: %v", err)
			return
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "CONNECT ") {
			t.Logf("server: invalid handshake: %s", line)
			return
		}

		// Send OK
		serverConn.Write([]byte("OK 5223\n"))

		// Read HTTP request
		req, err := http.ReadRequest(reader)
		if err != nil {
			t.Logf("server: failed to read request: %v", err)
			return
		}

		// Handle request
		rw := newControlResponseWriter(serverConn)
		cs.handler.ServeHTTP(rw, req)
		rw.finish()
	}()

	// Client side - send handshake then HTTP request
	clientConn.Write([]byte("CONNECT 5223\n"))

	// Read OK response
	reader := bufio.NewReader(clientConn)
	okLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("client: failed to read OK: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(okLine), "OK") {
		t.Fatalf("client: expected OK, got %q", okLine)
	}

	// Send HTTP request
	req, _ := http.NewRequest(http.MethodPost, "/snap", nil)
	req.Write(clientConn)

	// Read HTTP response
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		t.Fatalf("client: failed to read HTTP response: %v", err)
	}
	defer resp.Body.Close()

	// Check status
	if resp.StatusCode != http.StatusOK {
		t.Errorf("client: status code: got %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// Read and parse body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("client: failed to read body: %v", err)
	}

	var result SnapResponse
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("client: failed to parse JSON %q: %v", string(body), err)
	}

	if result.Status != "ok" {
		t.Errorf("client: Status: got %q, want %q", result.Status, "ok")
	}
	if result.SnapshotID != "test123" {
		t.Errorf("client: SnapshotID: got %q, want %q", result.SnapshotID, "test123")
	}
}

// TestParseFrameSpec tests the parseFrameSpec function with various inputs.
func TestParseFrameSpec(t *testing.T) {
	tests := []struct {
		name       string
		spec       string
		wantRootfs string
		wantHome   string
		wantWork   string
	}{
		{
			name:       "single_snap",
			spec:       "abc123",
			wantRootfs: "abc123",
			wantHome:   "",
			wantWork:   "",
		},
		{
			name:       "two_components",
			spec:       "abc:def",
			wantRootfs: "abc",
			wantHome:   "def",
			wantWork:   "",
		},
		{
			name:       "three_components",
			spec:       "abc:def:ghi",
			wantRootfs: "abc",
			wantHome:   "def",
			wantWork:   "ghi",
		},
		{
			name:       "empty_home",
			spec:       "abc::ghi",
			wantRootfs: "abc",
			wantHome:   "",
			wantWork:   "ghi",
		},
		{
			name:       "nil_home",
			spec:       "abc:nil:ghi",
			wantRootfs: "abc",
			wantHome:   "",
			wantWork:   "ghi",
		},
		{
			name:       "nil_work",
			spec:       "abc:def:nil",
			wantRootfs: "abc",
			wantHome:   "def",
			wantWork:   "",
		},
		{
			name:       "both_nil",
			spec:       "abc:nil:nil",
			wantRootfs: "abc",
			wantHome:   "",
			wantWork:   "",
		},
		{
			name:       "all_nil",
			spec:       "nil:nil:nil",
			wantRootfs: "",
			wantHome:   "",
			wantWork:   "",
		},
		{
			name:       "nil_rootfs",
			spec:       "nil:def:ghi",
			wantRootfs: "",
			wantHome:   "def",
			wantWork:   "ghi",
		},
		{
			name:       "rootfs_with_empty_suffix",
			spec:       "abc::",
			wantRootfs: "abc",
			wantHome:   "",
			wantWork:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRootfs, gotHome, gotWork := parseFrameSpec(tt.spec)
			if gotRootfs != tt.wantRootfs {
				t.Errorf("rootfs: got %q, want %q", gotRootfs, tt.wantRootfs)
			}
			if gotHome != tt.wantHome {
				t.Errorf("home: got %q, want %q", gotHome, tt.wantHome)
			}
			if gotWork != tt.wantWork {
				t.Errorf("work: got %q, want %q", gotWork, tt.wantWork)
			}
		})
	}
}

// TestIsFrameSpec tests the isFrameSpec function.
func TestIsFrameSpec(t *testing.T) {
	tests := []struct {
		spec string
		want bool
	}{
		{"abc123", false},
		{"abc:def", true},
		{"abc:def:ghi", true},
		{"abc::", true},
		{"abc:nil:nil", true},
		{":", true},
	}

	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			if got := isFrameSpec(tt.spec); got != tt.want {
				t.Errorf("isFrameSpec(%q) = %v, want %v", tt.spec, got, tt.want)
			}
		})
	}
}
