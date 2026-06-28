package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestParseRangeHeader(t *testing.T) {
	const fileSize = 1000

	tests := []struct {
		name      string
		header    string
		fileSize  int64
		wantStart int64
		wantEnd   int64
		wantErr   bool
	}{
		{name: "simple", header: "bytes=0-99", fileSize: fileSize, wantStart: 0, wantEnd: 99},
		{name: "mid range", header: "bytes=100-199", fileSize: fileSize, wantStart: 100, wantEnd: 199},
		{name: "open ended", header: "bytes=500-", fileSize: fileSize, wantStart: 500, wantEnd: 999},
		{name: "suffix", header: "bytes=-500", fileSize: fileSize, wantStart: 500, wantEnd: 999},
		{name: "suffix larger than file clamps to 0", header: "bytes=-5000", fileSize: fileSize, wantStart: 0, wantEnd: 999},
		{name: "end clamps to last byte", header: "bytes=0-5000", fileSize: fileSize, wantStart: 0, wantEnd: 999},
		{name: "single byte zero", header: "bytes=0-0", fileSize: fileSize, wantStart: 0, wantEnd: 0},
		{name: "whitespace tolerated", header: "bytes= 10 - 20 ", fileSize: fileSize, wantStart: 10, wantEnd: 20},

		{name: "missing prefix", header: "0-99", fileSize: fileSize, wantErr: true},
		{name: "multiple ranges", header: "bytes=0-1,2-3", fileSize: fileSize, wantErr: true},
		{name: "malformed no dash", header: "bytes=100", fileSize: fileSize, wantErr: true},
		{name: "empty dash", header: "bytes=-", fileSize: fileSize, wantErr: true},
		{name: "non-numeric start", header: "bytes=abc-10", fileSize: fileSize, wantErr: true},
		{name: "non-numeric end", header: "bytes=0-abc", fileSize: fileSize, wantErr: true},
		{name: "non-numeric suffix", header: "bytes=-abc", fileSize: fileSize, wantErr: true},
		{name: "start at fileSize", header: "bytes=1000-1001", fileSize: fileSize, wantErr: true},
		{name: "start beyond fileSize", header: "bytes=2000-3000", fileSize: fileSize, wantErr: true},
		{name: "start > end", header: "bytes=50-10", fileSize: fileSize, wantErr: true},
		{name: "empty file", header: "bytes=0-0", fileSize: 0, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, err := parseRangeHeader(tt.header, tt.fileSize)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseRangeHeader(%q, %d) = (%d,%d,nil), want error",
						tt.header, tt.fileSize, start, end)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRangeHeader(%q, %d) = error %v, want (%d,%d)",
					tt.header, tt.fileSize, err, tt.wantStart, tt.wantEnd)
			}
			if start != tt.wantStart || end != tt.wantEnd {
				t.Errorf("parseRangeHeader(%q, %d) = (%d,%d), want (%d,%d)",
					tt.header, tt.fileSize, start, end, tt.wantStart, tt.wantEnd)
			}
		})
	}
}

func TestServeHTTPTraversal(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	fs := &fileServer{root: root}

	tests := []struct {
		name     string
		path     string
		wantCode int
	}{
		{name: "normal file", path: "/ok.txt", wantCode: http.StatusOK},
		// An absolute "/../" path is collapsed by filepath.Clean back to a
		// root-relative path ("/../ok.txt" -> "/ok.txt"), so it is neutralized
		// (served from root), not rejected — it cannot escape.
		{name: "rooted parent traversal neutralized", path: "/../ok.txt", wantCode: http.StatusOK},
		{name: "rooted deep traversal neutralized", path: "/../../etc/passwd", wantCode: http.StatusNotFound},
		// A non-rooted path that Clean leaves with a leading ".." trips the
		// explicit guard and is rejected outright.
		{name: "relative parent traversal rejected", path: "../ok.txt", wantCode: http.StatusBadRequest},
		{name: "missing file", path: "/nope.txt", wantCode: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set URL.Path directly: httptest.NewRequest would otherwise
			// resolve/validate the request URI before we see it.
			req := httptest.NewRequest("GET", "/", nil)
			req.URL.Path = tt.path
			rec := httptest.NewRecorder()
			fs.ServeHTTP(rec, req)
			if rec.Code != tt.wantCode {
				t.Errorf("GET %s = %d, want %d (body %q)", tt.path, rec.Code, tt.wantCode, rec.Body.String())
			}
		})
	}
}
