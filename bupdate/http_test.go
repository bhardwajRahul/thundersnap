package bupdate

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestHTTPRangeRequests(t *testing.T) {
	// Create a temporary directory with test files
	tmpDir, err := os.MkdirTemp("", "bupdate-http-test")
	if err != nil {
		t.Fatalf("creating temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create a test file with known content
	testContent := make([]byte, 100000)
	for i := range testContent {
		testContent[i] = byte(i % 256)
	}
	testFile := filepath.Join(tmpDir, "testfile.bin")
	if err := os.WriteFile(testFile, testContent, 0644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	// Start the file server
	server, err := NewFileServer(tmpDir)
	if err != nil {
		t.Fatalf("creating file server: %v", err)
	}
	addr, err := server.Start()
	if err != nil {
		t.Fatalf("starting server: %v", err)
	}
	defer server.Close()

	t.Logf("Server listening on %s", addr)

	// Create HTTP reader
	fileURL := "http://" + addr + "/testfile.bin"
	reader, err := NewHTTPReader(fileURL)
	if err != nil {
		t.Fatalf("creating HTTP reader: %v", err)
	}
	defer reader.Close()

	// Test single range request
	t.Run("single range", func(t *testing.T) {
		data, err := reader.ReadRange(0, 100)
		if err != nil {
			t.Fatalf("reading range: %v", err)
		}
		if !bytes.Equal(data, testContent[:100]) {
			t.Errorf("data mismatch: got %d bytes", len(data))
		}
	})

	// Test middle range
	t.Run("middle range", func(t *testing.T) {
		data, err := reader.ReadRange(5000, 1000)
		if err != nil {
			t.Fatalf("reading range: %v", err)
		}
		if !bytes.Equal(data, testContent[5000:6000]) {
			t.Errorf("data mismatch at offset 5000")
		}
	})

	// Test end of file
	t.Run("end range", func(t *testing.T) {
		data, err := reader.ReadRange(99900, 100)
		if err != nil {
			t.Fatalf("reading range: %v", err)
		}
		if !bytes.Equal(data, testContent[99900:100000]) {
			t.Errorf("data mismatch at end")
		}
	})
}

func TestHTTPPipelining(t *testing.T) {
	// Create a temporary directory with test files
	tmpDir, err := os.MkdirTemp("", "bupdate-http-test")
	if err != nil {
		t.Fatalf("creating temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create a test file with known content
	testContent := make([]byte, 100000)
	for i := range testContent {
		testContent[i] = byte(i % 256)
	}
	testFile := filepath.Join(tmpDir, "testfile.bin")
	if err := os.WriteFile(testFile, testContent, 0644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	// Start the file server
	server, err := NewFileServer(tmpDir)
	if err != nil {
		t.Fatalf("creating file server: %v", err)
	}
	addr, err := server.Start()
	if err != nil {
		t.Fatalf("starting server: %v", err)
	}
	defer server.Close()

	// Create HTTP reader
	fileURL := "http://" + addr + "/testfile.bin"
	reader, err := NewHTTPReader(fileURL)
	if err != nil {
		t.Fatalf("creating HTTP reader: %v", err)
	}
	defer reader.Close()

	// Test pipelined requests
	t.Run("pipelined requests", func(t *testing.T) {
		requests := []RangeRequest{
			{Offset: 0, Size: 100},
			{Offset: 1000, Size: 200},
			{Offset: 5000, Size: 500},
			{Offset: 10000, Size: 1000},
			{Offset: 50000, Size: 2000},
		}

		results, err := reader.ReadRanges(requests)
		if err != nil {
			t.Fatalf("reading pipelined ranges: %v", err)
		}

		if len(results) != len(requests) {
			t.Fatalf("expected %d results, got %d", len(requests), len(results))
		}

		// Verify each result
		for i, req := range requests {
			expected := testContent[req.Offset : req.Offset+req.Size]
			if !bytes.Equal(results[i], expected) {
				t.Errorf("result %d mismatch: offset=%d size=%d", i, req.Offset, req.Size)
			}
		}
	})

	// Test many pipelined requests to verify connection stays open
	t.Run("many pipelined requests", func(t *testing.T) {
		var requests []RangeRequest
		chunkSize := int64(1000)
		for offset := int64(0); offset < 50000; offset += chunkSize {
			requests = append(requests, RangeRequest{Offset: offset, Size: chunkSize})
		}

		results, err := reader.ReadRanges(requests)
		if err != nil {
			t.Fatalf("reading many pipelined ranges: %v", err)
		}

		// Verify all results
		for i, req := range requests {
			expected := testContent[req.Offset : req.Offset+req.Size]
			if !bytes.Equal(results[i], expected) {
				t.Errorf("result %d mismatch", i)
			}
		}
	})
}

func TestIsHTTPURL(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"http://example.com/file", true},
		{"https://example.com/file", true},
		{"ftp://example.com/file", true},
		{"/local/path/file", false},
		{"./relative/path", false},
		{"file.txt", false},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			result := IsHTTPURL(tc.input)
			if result != tc.expected {
				t.Errorf("IsHTTPURL(%q) = %v, want %v", tc.input, result, tc.expected)
			}
		})
	}
}

// TestFileServerSymlinks tests that symlinks return readlink() content
func TestFileServerSymlinks(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "bupdate-symlink-test")
	if err != nil {
		t.Fatalf("creating temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create a symlink
	symlinkPath := filepath.Join(tmpDir, "mylink")
	symlinkTarget := "/some/target/path"
	if err := os.Symlink(symlinkTarget, symlinkPath); err != nil {
		t.Fatalf("creating symlink: %v", err)
	}

	server, err := NewFileServer(tmpDir)
	if err != nil {
		t.Fatalf("creating file server: %v", err)
	}
	addr, err := server.Start()
	if err != nil {
		t.Fatalf("starting server: %v", err)
	}
	defer server.Close()

	// Fetch the symlink - should get the target path as content
	fileURL := "http://" + addr + "/mylink"
	reader, err := NewHTTPReader(fileURL)
	if err != nil {
		t.Fatalf("creating reader: %v", err)
	}
	defer reader.Close()

	data, err := reader.ReadRange(0, int64(len(symlinkTarget)))
	if err != nil {
		t.Fatalf("reading symlink: %v", err)
	}

	if string(data) != symlinkTarget {
		t.Errorf("symlink content: got %q, want %q", string(data), symlinkTarget)
	}

	// Test range request on symlink
	data, err = reader.ReadRange(6, 6)
	if err != nil {
		t.Fatalf("reading symlink range: %v", err)
	}
	if string(data) != "target" {
		t.Errorf("symlink range: got %q, want %q", string(data), "target")
	}
}

// TestFileServerRejectsSpecialFiles tests that directories and other special files are rejected
func TestFileServerRejectsSpecialFiles(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "bupdate-special-test")
	if err != nil {
		t.Fatalf("creating temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create a subdirectory
	subdir := filepath.Join(tmpDir, "subdir")
	if err := os.Mkdir(subdir, 0755); err != nil {
		t.Fatalf("creating subdir: %v", err)
	}

	server, err := NewFileServer(tmpDir)
	if err != nil {
		t.Fatalf("creating file server: %v", err)
	}
	addr, err := server.Start()
	if err != nil {
		t.Fatalf("starting server: %v", err)
	}
	defer server.Close()

	// Try to fetch the directory - should fail
	fileURL := "http://" + addr + "/subdir"
	reader, err := NewHTTPReader(fileURL)
	if err != nil {
		t.Fatalf("creating reader: %v", err)
	}
	defer reader.Close()

	_, err = reader.ReadRange(0, 100)
	if err == nil {
		t.Errorf("expected error when fetching directory, got none")
	}
}

func TestParseRangeHeader(t *testing.T) {
	fileSize := int64(10000)

	tests := []struct {
		name      string
		header    string
		wantStart int64
		wantEnd   int64
		wantErr   bool
	}{
		{"simple range", "bytes=0-99", 0, 99, false},
		{"middle range", "bytes=500-999", 500, 999, false},
		{"open end", "bytes=5000-", 5000, 9999, false},
		{"suffix range", "bytes=-500", 9500, 9999, false},
		{"single byte", "bytes=100-100", 100, 100, false},
		{"past end gets truncated", "bytes=9000-20000", 9000, 9999, false},
		{"invalid prefix", "range=0-99", 0, 0, true},
		{"start beyond file", "bytes=20000-20100", 0, 0, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			start, end, err := ParseRangeHeader(tc.header, fileSize)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got none")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if start != tc.wantStart || end != tc.wantEnd {
				t.Errorf("got [%d-%d], want [%d-%d]", start, end, tc.wantStart, tc.wantEnd)
			}
		})
	}
}
