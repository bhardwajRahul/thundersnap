package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
