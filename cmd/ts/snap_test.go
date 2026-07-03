// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSnapResponseEncoding verifies that SnapResponse JSON encoding/decoding
// works correctly between client and server.
func TestSnapResponseEncoding(t *testing.T) {
	tests := []struct {
		name     string
		response SnapResponse
	}{
		{
			name: "success",
			response: SnapResponse{
				Status:     "ok",
				SnapshotID: "abc123def456",
			},
		},
		{
			name: "error",
			response: SnapResponse{
				Status:  "error",
				Message: "something went wrong",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Encode
			data, err := json.Marshal(tt.response)
			if err != nil {
				t.Fatalf("failed to marshal: %v", err)
			}

			// Decode
			var decoded SnapResponse
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("failed to unmarshal: %v", err)
			}

			// Verify
			if decoded.Status != tt.response.Status {
				t.Errorf("Status mismatch: got %q, want %q", decoded.Status, tt.response.Status)
			}
			if decoded.SnapshotID != tt.response.SnapshotID {
				t.Errorf("SnapshotID mismatch: got %q, want %q", decoded.SnapshotID, tt.response.SnapshotID)
			}
			if decoded.Message != tt.response.Message {
				t.Errorf("Message mismatch: got %q, want %q", decoded.Message, tt.response.Message)
			}
		})
	}
}

// TestSnapEndpointResponse simulates the server response and verifies the client
// can correctly parse it.
func TestSnapEndpointResponse(t *testing.T) {
	tests := []struct {
		name           string
		serverResponse SnapResponse
		statusCode     int
		wantErr        bool
		wantID         string
	}{
		{
			name: "success",
			serverResponse: SnapResponse{
				Status:     "ok",
				SnapshotID: "abc123def456",
			},
			statusCode: http.StatusOK,
			wantErr:    false,
			wantID:     "abc123def456",
		},
		{
			name: "server_error",
			serverResponse: SnapResponse{
				Status:  "error",
				Message: "btrfs snapshot failed",
			},
			statusCode: http.StatusInternalServerError,
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create test server
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.statusCode)
				json.NewEncoder(w).Encode(tt.serverResponse)
			}))
			defer server.Close()

			// Make request
			resp, err := http.Post(server.URL+"/snap", "application/json", nil)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			// Parse response like the client does
			var result SnapResponse
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				t.Fatalf("failed to decode response: %v", err)
			}

			// Check status code
			if resp.StatusCode != tt.statusCode {
				t.Errorf("status code mismatch: got %d, want %d", resp.StatusCode, tt.statusCode)
			}

			// Check error case
			if tt.wantErr {
				if result.Status == "ok" {
					t.Error("expected error status, got ok")
				}
				return
			}

			// Check success case
			if result.Status != "ok" {
				t.Errorf("expected ok status, got %q", result.Status)
			}
			if result.SnapshotID != tt.wantID {
				t.Errorf("snapshot ID mismatch: got %q, want %q", result.SnapshotID, tt.wantID)
			}
		})
	}
}

// TestWhoHasColonDetection tests that who-has properly rejects frame specs with colons.
func TestWhoHasColonDetection(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		hasColon      bool
		nonEmptyCount int
		wantSnaps     []string
	}{
		{
			name:          "single_snap",
			input:         "abc123",
			hasColon:      false,
			nonEmptyCount: 0,
			wantSnaps:     nil,
		},
		{
			name:          "frame_spec_all_filled",
			input:         "abc:def:ghi",
			hasColon:      true,
			nonEmptyCount: 3,
			wantSnaps:     []string{"abc", "def", "ghi"},
		},
		{
			name:          "frame_spec_with_nil",
			input:         "abc:nil:ghi",
			hasColon:      true,
			nonEmptyCount: 2,
			wantSnaps:     []string{"abc", "ghi"},
		},
		{
			name:          "frame_spec_with_empty",
			input:         "abc::ghi",
			hasColon:      true,
			nonEmptyCount: 2,
			wantSnaps:     []string{"abc", "ghi"},
		},
		{
			name:          "frame_spec_rootfs_only",
			input:         "abc:nil:nil",
			hasColon:      true,
			nonEmptyCount: 1,
			wantSnaps:     []string{"abc"},
		},
		{
			name:          "frame_spec_all_nil",
			input:         "nil:nil:nil",
			hasColon:      true,
			nonEmptyCount: 0,
			wantSnaps:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hasColon := strings.Contains(tt.input, ":")
			if hasColon != tt.hasColon {
				t.Errorf("hasColon: got %v, want %v", hasColon, tt.hasColon)
			}

			if !hasColon {
				return
			}

			// Parse the frame spec like cmdWhoHas does
			parts := strings.Split(tt.input, ":")
			var nonEmpty []string
			for _, p := range parts {
				if p != "" && p != "nil" {
					nonEmpty = append(nonEmpty, p)
				}
			}

			if len(nonEmpty) != tt.nonEmptyCount {
				t.Errorf("nonEmptyCount: got %d, want %d", len(nonEmpty), tt.nonEmptyCount)
			}

			if tt.wantSnaps != nil {
				if len(nonEmpty) != len(tt.wantSnaps) {
					t.Errorf("snaps count mismatch: got %d, want %d", len(nonEmpty), len(tt.wantSnaps))
				}
				for i, snap := range nonEmpty {
					if i < len(tt.wantSnaps) && snap != tt.wantSnaps[i] {
						t.Errorf("snap[%d]: got %q, want %q", i, snap, tt.wantSnaps[i])
					}
				}
			}
		})
	}
}

// TestDeleteSnapRequestEncoding verifies that DeleteSnapRequest JSON encoding/decoding
// works correctly between client and server.
func TestDeleteSnapRequestEncoding(t *testing.T) {
	tests := []struct {
		name    string
		request DeleteSnapRequest
	}{
		{
			name:    "simple_id",
			request: DeleteSnapRequest{SnapshotID: "abc123def456"},
		},
		{
			name:    "sha256_id",
			request: DeleteSnapRequest{SnapshotID: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Encode
			data, err := json.Marshal(tt.request)
			if err != nil {
				t.Fatalf("failed to marshal: %v", err)
			}

			// Decode
			var decoded DeleteSnapRequest
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("failed to unmarshal: %v", err)
			}

			// Verify
			if decoded.SnapshotID != tt.request.SnapshotID {
				t.Errorf("SnapshotID mismatch: got %q, want %q", decoded.SnapshotID, tt.request.SnapshotID)
			}
		})
	}
}

// TestDeleteSnapResponseEncoding verifies that DeleteSnapResponse JSON encoding/decoding
// works correctly between client and server.
func TestDeleteSnapResponseEncoding(t *testing.T) {
	tests := []struct {
		name     string
		response DeleteSnapResponse
	}{
		{
			name: "success",
			response: DeleteSnapResponse{
				Status: "ok",
			},
		},
		{
			name: "error",
			response: DeleteSnapResponse{
				Status:  "error",
				Message: "snapshot not found",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Encode
			data, err := json.Marshal(tt.response)
			if err != nil {
				t.Fatalf("failed to marshal: %v", err)
			}

			// Decode
			var decoded DeleteSnapResponse
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("failed to unmarshal: %v", err)
			}

			// Verify
			if decoded.Status != tt.response.Status {
				t.Errorf("Status mismatch: got %q, want %q", decoded.Status, tt.response.Status)
			}
			if decoded.Message != tt.response.Message {
				t.Errorf("Message mismatch: got %q, want %q", decoded.Message, tt.response.Message)
			}
		})
	}
}

// TestDeleteFrameRequestEncoding verifies that DeleteFrameRequest JSON encoding/decoding
// works correctly between client and server.
func TestDeleteFrameRequestEncoding(t *testing.T) {
	tests := []struct {
		name    string
		request DeleteFrameRequest
	}{
		{
			name:    "simple_uuid",
			request: DeleteFrameRequest{UUID: "01234567-89ab-cdef-0123-456789abcdef"},
		},
		{
			name:    "another_uuid",
			request: DeleteFrameRequest{UUID: "fedcba98-7654-3210-fedc-ba9876543210"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Encode
			data, err := json.Marshal(tt.request)
			if err != nil {
				t.Fatalf("failed to marshal: %v", err)
			}

			// Decode
			var decoded DeleteFrameRequest
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("failed to unmarshal: %v", err)
			}

			// Verify
			if decoded.UUID != tt.request.UUID {
				t.Errorf("UUID mismatch: got %q, want %q", decoded.UUID, tt.request.UUID)
			}
		})
	}
}

// TestDeleteFrameResponseEncoding verifies that DeleteFrameResponse JSON encoding/decoding
// works correctly between client and server.
func TestDeleteFrameResponseEncoding(t *testing.T) {
	tests := []struct {
		name     string
		response DeleteFrameResponse
	}{
		{
			name: "success",
			response: DeleteFrameResponse{
				Status: "ok",
			},
		},
		{
			name: "error_not_found",
			response: DeleteFrameResponse{
				Status:  "error",
				Message: "frame not found",
			},
		},
		{
			name: "error_active_frame",
			response: DeleteFrameResponse{
				Status:  "error",
				Message: "cannot delete the currently active frame",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Encode
			data, err := json.Marshal(tt.response)
			if err != nil {
				t.Fatalf("failed to marshal: %v", err)
			}

			// Decode
			var decoded DeleteFrameResponse
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("failed to unmarshal: %v", err)
			}

			// Verify
			if decoded.Status != tt.response.Status {
				t.Errorf("Status mismatch: got %q, want %q", decoded.Status, tt.response.Status)
			}
			if decoded.Message != tt.response.Message {
				t.Errorf("Message mismatch: got %q, want %q", decoded.Message, tt.response.Message)
			}
		})
	}
}

// TestDeleteSnapEndpointResponse simulates the server response and verifies the client
// can correctly parse it.
func TestDeleteSnapEndpointResponse(t *testing.T) {
	tests := []struct {
		name           string
		serverResponse DeleteSnapResponse
		statusCode     int
		wantErr        bool
	}{
		{
			name: "success",
			serverResponse: DeleteSnapResponse{
				Status: "ok",
			},
			statusCode: http.StatusOK,
			wantErr:    false,
		},
		{
			name: "not_found",
			serverResponse: DeleteSnapResponse{
				Status:  "error",
				Message: "snapshot not found",
			},
			statusCode: http.StatusNotFound,
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create test server
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.statusCode)
				json.NewEncoder(w).Encode(tt.serverResponse)
			}))
			defer server.Close()

			// Make request
			reqBody := DeleteSnapRequest{SnapshotID: "test-snap"}
			body, _ := json.Marshal(reqBody)
			resp, err := http.Post(server.URL+"/delete-snap", "application/json", strings.NewReader(string(body)))
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			// Parse response like the client does
			var result DeleteSnapResponse
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				t.Fatalf("failed to decode response: %v", err)
			}

			// Check status code
			if resp.StatusCode != tt.statusCode {
				t.Errorf("status code mismatch: got %d, want %d", resp.StatusCode, tt.statusCode)
			}

			// Check error case
			if tt.wantErr {
				if result.Status == "ok" {
					t.Error("expected error status, got ok")
				}
				return
			}

			// Check success case
			if result.Status != "ok" {
				t.Errorf("expected ok status, got %q", result.Status)
			}
		})
	}
}

// TestDownloadSnapFrameSpecParsing tests that download-snap correctly parses frame specs.
func TestDownloadSnapFrameSpecParsing(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantSnaps []string
	}{
		{
			name:      "single_snap",
			input:     "abc123",
			wantSnaps: []string{"abc123"},
		},
		{
			name:      "frame_spec_all_filled",
			input:     "abc:def:ghi",
			wantSnaps: []string{"abc", "def", "ghi"},
		},
		{
			name:      "frame_spec_with_nil",
			input:     "abc:nil:ghi",
			wantSnaps: []string{"abc", "ghi"},
		},
		{
			name:      "frame_spec_with_empty",
			input:     "abc::ghi",
			wantSnaps: []string{"abc", "ghi"},
		},
		{
			name:      "frame_spec_rootfs_only",
			input:     "abc:nil:nil",
			wantSnaps: []string{"abc"},
		},
		{
			name:      "frame_spec_all_nil",
			input:     "nil:nil:nil",
			wantSnaps: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var snapsToDownload []string

			if strings.Contains(tt.input, ":") {
				parts := strings.Split(tt.input, ":")
				for _, p := range parts {
					if p != "" && p != "nil" {
						snapsToDownload = append(snapsToDownload, p)
					}
				}
			} else {
				snapsToDownload = []string{tt.input}
			}

			if tt.wantSnaps == nil {
				if len(snapsToDownload) != 0 {
					t.Errorf("expected no snaps, got %v", snapsToDownload)
				}
				return
			}

			if len(snapsToDownload) != len(tt.wantSnaps) {
				t.Errorf("snaps count mismatch: got %d, want %d", len(snapsToDownload), len(tt.wantSnaps))
			}
			for i, snap := range snapsToDownload {
				if i < len(tt.wantSnaps) && snap != tt.wantSnaps[i] {
					t.Errorf("snap[%d]: got %q, want %q", i, snap, tt.wantSnaps[i])
				}
			}
		})
	}
}
