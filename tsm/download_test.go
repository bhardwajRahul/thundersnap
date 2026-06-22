package tsm

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFetchFullFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("test content"))
	}))
	defer server.Close()

	data, err := fetchFullFile(http.DefaultClient, server.URL)
	if err != nil {
		t.Fatalf("fetchFullFile: %v", err)
	}
	if string(data) != "test content" {
		t.Errorf("got %q, want %q", data, "test content")
	}
}

func TestFetchRanges(t *testing.T) {
	content := "Hello, World! This is test content."
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHeader := r.Header.Get("Range")
		if rangeHeader == "" {
			w.Write([]byte(content))
			return
		}

		// Parse range header (simple version)
		var start, end int64
		fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end)
		if start < 0 || end >= int64(len(content)) || start > end {
			http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}

		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte(content[start : end+1]))
	}))
	defer server.Close()

	ranges := []rangeSpec{
		{offset: 0, size: 5},  // "Hello"
		{offset: 7, size: 6},  // "World!"
	}

	results, err := fetchRanges(http.DefaultClient, server.URL, ranges)
	if err != nil {
		t.Fatalf("fetchRanges: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}

	if string(results[0]) != "Hello" {
		t.Errorf("range 0: got %q, want %q", results[0], "Hello")
	}
	if string(results[1]) != "World!" {
		t.Errorf("range 1: got %q, want %q", results[1], "World!")
	}
}

func TestDownloadIntegration(t *testing.T) {
	// Create a temp directory structure simulating a peer's snapshots
	peerDir := t.TempDir()
	snapName := "testsnap123"
	snapDir := filepath.Join(peerDir, snapName)

	// Create snapshot content
	if err := os.MkdirAll(filepath.Join(snapDir, "subdir"), 0755); err != nil {
		t.Fatal(err)
	}

	testContent := []byte("This is test file content for the download test.\n")
	if err := os.WriteFile(filepath.Join(snapDir, "testfile.txt"), testContent, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapDir, "subdir", "nested.txt"), []byte("nested content\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create symlink
	if err := os.Symlink("testfile.txt", filepath.Join(snapDir, "link.txt")); err != nil {
		t.Fatal(err)
	}

	// Generate TSM/TSC
	outBase := filepath.Join(peerDir, snapName)
	if err := Create(snapDir, outBase, IndexerOptions{}); err != nil {
		t.Fatal(err)
	}

	// Create stamp file
	if err := os.WriteFile(outBase+".stamp", []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	// Create HTTP server serving the peer's files
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/bupdate/")
		fullPath := filepath.Join(peerDir, path)

		// Handle range requests
		rangeHeader := r.Header.Get("Range")
		if rangeHeader != "" {
			var start, end int64
			fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end)

			f, err := os.Open(fullPath)
			if err != nil {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			defer f.Close()

			stat, _ := f.Stat()
			if end >= stat.Size() {
				end = stat.Size() - 1
			}

			f.Seek(start, 0)
			data := make([]byte, end-start+1)
			f.Read(data)

			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, stat.Size()))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(data)
			return
		}

		// Serve full file
		http.ServeFile(w, r, fullPath)
	}))
	defer server.Close()

	// Download to a new directory
	localDir := t.TempDir()

	result, err := Download(DownloadOptions{
		SnapshotID:   snapName,
		SnapshotsDir: localDir,
		BaseURL:      server.URL,
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	if result.AlreadyExists {
		t.Error("expected AlreadyExists=false")
	}

	// Verify downloaded content
	downloadedPath := filepath.Join(localDir, snapName)
	if _, err := os.Stat(downloadedPath); err != nil {
		t.Fatalf("snapshot dir not created: %v", err)
	}

	// Check testfile.txt
	data, err := os.ReadFile(filepath.Join(downloadedPath, "testfile.txt"))
	if err != nil {
		t.Fatalf("reading testfile.txt: %v", err)
	}
	if string(data) != string(testContent) {
		t.Errorf("testfile.txt content mismatch")
	}

	// Check nested file
	data, err = os.ReadFile(filepath.Join(downloadedPath, "subdir", "nested.txt"))
	if err != nil {
		t.Fatalf("reading nested.txt: %v", err)
	}
	if string(data) != "nested content\n" {
		t.Errorf("nested.txt content mismatch")
	}

	// Check symlink
	target, err := os.Readlink(filepath.Join(downloadedPath, "link.txt"))
	if err != nil {
		t.Fatalf("reading link.txt: %v", err)
	}
	if target != "testfile.txt" {
		t.Errorf("symlink target=%q, want testfile.txt", target)
	}

	// Check metadata files
	if _, err := os.Stat(filepath.Join(localDir, snapName+".tsm")); err != nil {
		t.Errorf("TSM file not saved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(localDir, snapName+".tsc")); err != nil {
		t.Errorf("TSC file not saved: %v", err)
	}

	t.Log("Download integration test passed")
}

func TestDownloadAlreadyExists(t *testing.T) {
	tmpDir := t.TempDir()
	snapName := "existing"

	// Create an existing snapshot
	if err := os.MkdirAll(filepath.Join(tmpDir, snapName), 0755); err != nil {
		t.Fatal(err)
	}

	result, err := Download(DownloadOptions{
		SnapshotID:   snapName,
		SnapshotsDir: tmpDir,
		BaseURL:      "http://localhost:9999", // Won't be used
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	if !result.AlreadyExists {
		t.Error("expected AlreadyExists=true")
	}
}
