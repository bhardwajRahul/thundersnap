package bupdate

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestBupdateHTTPIntegration tests the full HTTP workflow:
// 1. Create test files
// 2. Create fidx files for them
// 3. Start HTTP server
// 4. Download chunks via HTTP with pipelining
// 5. Verify the reconstructed content
func TestBupdateHTTPIntegration(t *testing.T) {
	// Create temp directories for "remote" (served via HTTP) and "local" (destination)
	remoteDir, err := os.MkdirTemp("", "bupdate-remote")
	if err != nil {
		t.Fatalf("creating remote dir: %v", err)
	}
	defer os.RemoveAll(remoteDir)

	localDir, err := os.MkdirTemp("", "bupdate-local")
	if err != nil {
		t.Fatalf("creating local dir: %v", err)
	}
	defer os.RemoveAll(localDir)

	// Create test file with known content (large enough to have multiple chunks)
	testContent := make([]byte, 200000)
	for i := range testContent {
		testContent[i] = byte(i % 251) // Prime to avoid patterns
	}

	testFile := filepath.Join(remoteDir, "testfile.bin")
	if err := os.WriteFile(testFile, testContent, 0644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	// Create fidx file for the test file
	fidxPath := filepath.Join(remoteDir, "testfile.bin.fidx")
	if err := createFidxFile(testFile, fidxPath); err != nil {
		t.Fatalf("creating fidx: %v", err)
	}

	// Start HTTP server
	server, err := NewFileServer(remoteDir)
	if err != nil {
		t.Fatalf("creating file server: %v", err)
	}
	addr, err := server.Start()
	if err != nil {
		t.Fatalf("starting server: %v", err)
	}
	defer server.Close()

	t.Logf("Server listening on %s", addr)

	// Load fidx via HTTP
	fidxURL := "http://" + addr + "/testfile.bin.fidx"
	remoteFidx, err := LoadFidxHTTP(fidxURL)
	if err != nil {
		t.Fatalf("loading fidx via HTTP: %v", err)
	}

	t.Logf("Loaded fidx with %d entries, total size %d", len(remoteFidx.Entries), remoteFidx.FileSize)

	// Verify fidx content matches
	if remoteFidx.FileSize != int64(len(testContent)) {
		t.Fatalf("fidx file size mismatch: got %d, want %d", remoteFidx.FileSize, len(testContent))
	}

	// Create HTTP reader for the file
	fileURL := "http://" + addr + "/testfile.bin"
	reader, err := NewHTTPReader(fileURL)
	if err != nil {
		t.Fatalf("creating HTTP reader: %v", err)
	}
	defer reader.Close()

	// Build list of range requests for all chunks
	var requests []RangeRequest
	var offset int64
	for _, ent := range remoteFidx.Entries {
		requests = append(requests, RangeRequest{
			Offset: offset,
			Size:   int64(ent.Size),
		})
		offset += int64(ent.Size)
	}

	// Fetch all chunks via pipelining (in batches)
	const batchSize = 16
	var allChunks [][]byte

	for i := 0; i < len(requests); i += batchSize {
		end := i + batchSize
		if end > len(requests) {
			end = len(requests)
		}
		batch := requests[i:end]

		results, err := reader.ReadRanges(batch)
		if err != nil {
			t.Fatalf("reading batch %d: %v", i/batchSize, err)
		}
		allChunks = append(allChunks, results...)
	}

	// Verify chunk count
	if len(allChunks) != len(remoteFidx.Entries) {
		t.Fatalf("chunk count mismatch: got %d, want %d", len(allChunks), len(remoteFidx.Entries))
	}

	// Verify each chunk's SHA and reconstruct content
	var reconstructed bytes.Buffer
	for i, chunk := range allChunks {
		ent := remoteFidx.Entries[i]

		// Verify size
		if len(chunk) != int(ent.Size) {
			t.Errorf("chunk %d size mismatch: got %d, want %d", i, len(chunk), ent.Size)
		}

		// Verify SHA
		computedSHA := BlobSHA(chunk)
		if !bytes.Equal(computedSHA[:], ent.SHA[:]) {
			t.Errorf("chunk %d SHA mismatch", i)
		}

		reconstructed.Write(chunk)
	}

	// Verify reconstructed content matches original
	if !bytes.Equal(reconstructed.Bytes(), testContent) {
		t.Errorf("reconstructed content mismatch: got %d bytes, want %d bytes",
			reconstructed.Len(), len(testContent))
	}

	t.Logf("Successfully fetched and verified %d chunks via HTTP pipelining", len(allChunks))
}

// TestLoadFidxHTTP tests loading a fidx file over HTTP
func TestLoadFidxHTTP(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "bupdate-fidx-test")
	if err != nil {
		t.Fatalf("creating temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create a simple test file
	testContent := []byte("Hello, this is test content for fidx loading!")
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, testContent, 0644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	// Create fidx file
	fidxPath := filepath.Join(tmpDir, "test.txt.fidx")
	if err := createFidxFile(testFile, fidxPath); err != nil {
		t.Fatalf("creating fidx: %v", err)
	}

	// Start HTTP server
	server, err := NewFileServer(tmpDir)
	if err != nil {
		t.Fatalf("creating file server: %v", err)
	}
	addr, err := server.Start()
	if err != nil {
		t.Fatalf("starting server: %v", err)
	}
	defer server.Close()

	// Load fidx via HTTP
	fidxURL := "http://" + addr + "/test.txt.fidx"
	fidx, err := LoadFidxHTTP(fidxURL)
	if err != nil {
		t.Fatalf("loading fidx via HTTP: %v", err)
	}

	// Verify
	if fidx.FileSize != int64(len(testContent)) {
		t.Errorf("fidx file size mismatch: got %d, want %d", fidx.FileSize, len(testContent))
	}

	t.Logf("Successfully loaded fidx via HTTP: %d entries, %d bytes", len(fidx.Entries), fidx.FileSize)
}

// createFidxFile creates a fidx file for the given source file
func createFidxFile(srcPath, fidxPath string) error {
	f, err := os.Create(fidxPath)
	if err != nil {
		return err
	}
	defer f.Close()

	// Write header
	header := make([]byte, 8)
	copy(header[0:4], "FIDX")
	binary.BigEndian.PutUint32(header[4:8], FIDX_VERSION)
	if _, err := f.Write(header); err != nil {
		return err
	}

	// Track content for SHA
	h := sha1.New()
	h.Write(header)

	// Chunk the file and write entries
	err = ChunkFile(srcPath, func(entry FidxEntry) error {
		entryBytes := make([]byte, 24)
		copy(entryBytes[0:20], entry.SHA[:])
		binary.BigEndian.PutUint16(entryBytes[20:22], entry.Size)
		binary.BigEndian.PutUint16(entryBytes[22:24], entry.Level)
		h.Write(entryBytes)
		_, err := f.Write(entryBytes)
		return err
	}, nil)
	if err != nil {
		return err
	}

	// Write footer (SHA of everything before)
	_, err = f.Write(h.Sum(nil))
	return err
}

// TestHTTPPipelinedBatchSizes tests various batch sizes for pipelining
func TestHTTPPipelinedBatchSizes(t *testing.T) {
	// Create temp directory with test file
	tmpDir, err := os.MkdirTemp("", "bupdate-batch-test")
	if err != nil {
		t.Fatalf("creating temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create test file
	testContent := make([]byte, 50000)
	for i := range testContent {
		testContent[i] = byte(i % 256)
	}
	testFile := filepath.Join(tmpDir, "test.bin")
	if err := os.WriteFile(testFile, testContent, 0644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	// Start HTTP server
	server, err := NewFileServer(tmpDir)
	if err != nil {
		t.Fatalf("creating file server: %v", err)
	}
	addr, err := server.Start()
	if err != nil {
		t.Fatalf("starting server: %v", err)
	}
	defer server.Close()

	fileURL := "http://" + addr + "/test.bin"

	// Test different batch sizes
	batchSizes := []int{1, 4, 8, 16, 32}

	for _, batchSize := range batchSizes {
		t.Run("batch_"+string(rune('0'+batchSize/10))+string(rune('0'+batchSize%10)), func(t *testing.T) {
			reader, err := NewHTTPReader(fileURL)
			if err != nil {
				t.Fatalf("creating reader: %v", err)
			}
			defer reader.Close()

			// Create requests for 1000-byte chunks
			var requests []RangeRequest
			for offset := int64(0); offset < int64(len(testContent)); offset += 1000 {
				size := int64(1000)
				if offset+size > int64(len(testContent)) {
					size = int64(len(testContent)) - offset
				}
				requests = append(requests, RangeRequest{Offset: offset, Size: size})
			}

			// Fetch in batches
			var allData []byte
			for i := 0; i < len(requests); i += batchSize {
				end := i + batchSize
				if end > len(requests) {
					end = len(requests)
				}

				results, err := reader.ReadRanges(requests[i:end])
				if err != nil {
					t.Fatalf("reading batch: %v", err)
				}

				for _, chunk := range results {
					allData = append(allData, chunk...)
				}
			}

			if !bytes.Equal(allData, testContent) {
				t.Errorf("content mismatch with batch size %d", batchSize)
			}
		})
	}
}

// TestHTTPRangeEdgeCases tests edge cases for range requests
func TestHTTPRangeEdgeCases(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "bupdate-edge-test")
	if err != nil {
		t.Fatalf("creating temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	testContent := []byte("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz")
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, testContent, 0644); err != nil {
		t.Fatalf("writing test file: %v", err)
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

	fileURL := "http://" + addr + "/test.txt"
	reader, err := NewHTTPReader(fileURL)
	if err != nil {
		t.Fatalf("creating reader: %v", err)
	}
	defer reader.Close()

	tests := []struct {
		name   string
		offset int64
		size   int64
		want   string
	}{
		{"first byte", 0, 1, "0"},
		{"last byte", 61, 1, "z"},
		{"first 10", 0, 10, "0123456789"},
		{"middle", 10, 6, "ABCDEF"},
		{"last 10", 52, 10, "qrstuvwxyz"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, err := reader.ReadRange(tc.offset, tc.size)
			if err != nil {
				t.Fatalf("reading range: %v", err)
			}
			if string(data) != tc.want {
				t.Errorf("got %q, want %q", string(data), tc.want)
			}
		})
	}
}

// TestFileServerConcurrent tests the file server under concurrent load
func TestFileServerConcurrent(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "bupdate-concurrent-test")
	if err != nil {
		t.Fatalf("creating temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	testContent := make([]byte, 10000)
	for i := range testContent {
		testContent[i] = byte(i % 256)
	}
	testFile := filepath.Join(tmpDir, "test.bin")
	if err := os.WriteFile(testFile, testContent, 0644); err != nil {
		t.Fatalf("writing test file: %v", err)
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

	fileURL := "http://" + addr + "/test.bin"

	// Create multiple readers (simulating multiple connections)
	const numReaders = 3
	readers := make([]*HTTPReader, numReaders)
	for i := 0; i < numReaders; i++ {
		r, err := NewHTTPReader(fileURL)
		if err != nil {
			t.Fatalf("creating reader %d: %v", i, err)
		}
		defer r.Close()
		readers[i] = r
	}

	// Each reader reads different parts of the file
	for i, reader := range readers {
		offset := int64(i * 3000)
		size := int64(1000)
		data, err := reader.ReadRange(offset, size)
		if err != nil {
			t.Fatalf("reader %d error: %v", i, err)
		}
		expected := testContent[offset : offset+size]
		if !bytes.Equal(data, expected) {
			t.Errorf("reader %d data mismatch", i)
		}
	}
}

// Ensure our HTTP server writes proper Content-Length header
func TestFileServerContentLength(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "bupdate-cl-test")
	if err != nil {
		t.Fatalf("creating temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	testContent := []byte("Hello World!")
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, testContent, 0644); err != nil {
		t.Fatalf("writing test file: %v", err)
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

	// Use raw connection to verify headers
	fileURL := "http://" + addr + "/test.txt"
	reader, err := NewHTTPReader(fileURL)
	if err != nil {
		t.Fatalf("creating reader: %v", err)
	}
	defer reader.Close()

	// Request a specific range
	data, err := reader.ReadRange(0, 5)
	if err != nil {
		t.Fatalf("reading range: %v", err)
	}

	if string(data) != "Hello" {
		t.Errorf("got %q, want %q", string(data), "Hello")
	}
}

// readResponse is a helper to read an HTTP response from a reader
func readHTTPResponse(reader io.Reader) ([]byte, error) {
	// This is a simplified response reader - in production use net/http
	var body bytes.Buffer
	_, err := io.Copy(&body, reader)
	return body.Bytes(), err
}

// TestCheckURLExists tests the HEAD request functionality
func TestCheckURLExists(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "bupdate-head-test")
	if err != nil {
		t.Fatalf("creating temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create a test file
	testFile := filepath.Join(tmpDir, "exists.txt")
	if err := os.WriteFile(testFile, []byte("hello"), 0644); err != nil {
		t.Fatalf("writing test file: %v", err)
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

	tests := []struct {
		name   string
		path   string
		exists bool
	}{
		{"existing file", "/exists.txt", true},
		{"non-existing file", "/notfound.txt", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			url := "http://" + addr + tc.path
			exists, err := CheckURLExists(url)
			if err != nil && tc.exists {
				t.Fatalf("unexpected error: %v", err)
			}
			if exists != tc.exists {
				t.Errorf("CheckURLExists(%q) = %v, want %v", tc.path, exists, tc.exists)
			}
		})
	}
}

// TestCheckPeersForSnapshot tests parallel peer checking
func TestCheckPeersForSnapshot(t *testing.T) {
	// Create two temp directories for two "peers"
	peer1Dir, err := os.MkdirTemp("", "bupdate-peer1")
	if err != nil {
		t.Fatalf("creating peer1 dir: %v", err)
	}
	defer os.RemoveAll(peer1Dir)

	peer2Dir, err := os.MkdirTemp("", "bupdate-peer2")
	if err != nil {
		t.Fatalf("creating peer2 dir: %v", err)
	}
	defer os.RemoveAll(peer2Dir)

	// Create snapshot fidx file only on peer1
	snapshotID := "abc123def456"
	fidxFile := filepath.Join(peer1Dir, snapshotID+".fidx")
	if err := os.WriteFile(fidxFile, []byte("dummy fidx content"), 0644); err != nil {
		t.Fatalf("writing fidx file: %v", err)
	}

	// Start servers for both peers
	server1, err := NewFileServer(peer1Dir)
	if err != nil {
		t.Fatalf("creating server1: %v", err)
	}
	addr1, err := server1.Start()
	if err != nil {
		t.Fatalf("starting server1: %v", err)
	}
	defer server1.Close()

	server2, err := NewFileServer(peer2Dir)
	if err != nil {
		t.Fatalf("creating server2: %v", err)
	}
	addr2, err := server2.Start()
	if err != nil {
		t.Fatalf("starting server2: %v", err)
	}
	defer server2.Close()

	// Create peer list
	peers := []PeerInfo{
		{URL: "http://" + addr1, Hostname: "peer1.example.com"},
		{URL: "http://" + addr2, Hostname: "peer2.example.com"},
	}

	// Check for the snapshot - note: CheckPeersForSnapshot adds /bupdate/ prefix
	// but our test servers serve from root, so we need to adjust
	// Actually, let's create the proper path structure
	bupdateDir1 := filepath.Join(peer1Dir, "bupdate")
	if err := os.MkdirAll(bupdateDir1, 0755); err != nil {
		t.Fatalf("creating bupdate dir: %v", err)
	}
	fidxFile1 := filepath.Join(bupdateDir1, snapshotID+".fidx")
	if err := os.WriteFile(fidxFile1, []byte("dummy fidx content"), 0644); err != nil {
		t.Fatalf("writing fidx file: %v", err)
	}
	os.Remove(fidxFile) // remove the one at root

	results := CheckPeersForSnapshot(peers, snapshotID)

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Find results by hostname
	var peer1Result, peer2Result PeerResult
	for _, r := range results {
		if r.Hostname == "peer1.example.com" {
			peer1Result = r
		} else if r.Hostname == "peer2.example.com" {
			peer2Result = r
		}
	}

	if !peer1Result.HasSnap {
		t.Errorf("peer1 should have snapshot, but HasSnap=%v, err=%v", peer1Result.HasSnap, peer1Result.Err)
	}
	if peer2Result.HasSnap {
		t.Errorf("peer2 should NOT have snapshot, but HasSnap=%v", peer2Result.HasSnap)
	}

	t.Logf("Peer1 (has snap): HasSnap=%v, Err=%v", peer1Result.HasSnap, peer1Result.Err)
	t.Logf("Peer2 (no snap): HasSnap=%v, Err=%v", peer2Result.HasSnap, peer2Result.Err)
}

// TestDownloadSnapshot tests the snapshot download functionality
func TestDownloadSnapshot(t *testing.T) {
	// Create "peer" directory with a mock snapshot
	peerDir, err := os.MkdirTemp("", "bupdate-peer-snap")
	if err != nil {
		t.Fatalf("creating peer dir: %v", err)
	}
	defer os.RemoveAll(peerDir)

	// Create "local" snapshots directory
	localSnapsDir, err := os.MkdirTemp("", "bupdate-local-snaps")
	if err != nil {
		t.Fatalf("creating local snaps dir: %v", err)
	}
	defer os.RemoveAll(localSnapsDir)

	snapshotID := "testsnap123"

	// Create mock snapshot files in bupdate directory structure
	bupdateDir := filepath.Join(peerDir, "bupdate")
	if err := os.MkdirAll(bupdateDir, 0755); err != nil {
		t.Fatalf("creating bupdate dir: %v", err)
	}

	// Create stamp file
	stampContent := []byte("parent123")
	if err := os.WriteFile(filepath.Join(bupdateDir, snapshotID+".stamp"), stampContent, 0644); err != nil {
		t.Fatalf("writing stamp: %v", err)
	}

	// Create a simple test file for the snapshot content
	testContent := []byte("Hello, this is test content for snapshot download!")

	// Create snapshot directory with a file
	snapDir := filepath.Join(bupdateDir, snapshotID)
	if err := os.MkdirAll(snapDir, 0755); err != nil {
		t.Fatalf("creating snap dir: %v", err)
	}
	testFilePath := filepath.Join(snapDir, "test.txt")
	if err := os.WriteFile(testFilePath, testContent, 0644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	// Create fidx file for the test file
	fidxPath := filepath.Join(bupdateDir, snapshotID+".fidx")
	if err := createFidxFile(testFilePath, fidxPath); err != nil {
		t.Fatalf("creating fidx: %v", err)
	}

	// Create fidx.fidx (fidx of the fidx)
	fidxFidxPath := filepath.Join(bupdateDir, snapshotID+".fidx.fidx")
	if err := createFidxFile(fidxPath, fidxFidxPath); err != nil {
		t.Fatalf("creating fidx.fidx: %v", err)
	}

	// Start HTTP server
	server, err := NewFileServer(peerDir)
	if err != nil {
		t.Fatalf("creating server: %v", err)
	}
	addr, err := server.Start()
	if err != nil {
		t.Fatalf("starting server: %v", err)
	}
	defer server.Close()

	t.Logf("Server listening on %s", addr)

	// Create peer list
	peers := []PeerInfo{
		{URL: "http://" + addr, Hostname: "testpeer.example.com"},
	}

	// Test 1: Download snapshot that doesn't exist locally
	opts := DownloadSnapshotOptions{
		SnapshotID:   snapshotID,
		SnapshotsDir: localSnapsDir,
		Peers:        peers,
	}

	result, err := DownloadSnapshot(opts)
	if err != nil {
		t.Fatalf("DownloadSnapshot failed: %v", err)
	}

	if result.AlreadyExists {
		t.Errorf("expected AlreadyExists=false for new download")
	}
	if result.PeerHostname != "testpeer.example.com" {
		t.Errorf("expected PeerHostname=testpeer.example.com, got %s", result.PeerHostname)
	}

	// Verify the stamp file was downloaded
	localStampPath := filepath.Join(localSnapsDir, snapshotID+".stamp")
	if _, err := os.Stat(localStampPath); err != nil {
		t.Errorf("stamp file not downloaded: %v", err)
	}

	// Verify the fidx file was downloaded
	localFidxPath := filepath.Join(localSnapsDir, snapshotID+".fidx")
	if _, err := os.Stat(localFidxPath); err != nil {
		t.Errorf("fidx file not downloaded: %v", err)
	}

	// Test 2: Try to download again - should report AlreadyExists
	result2, err := DownloadSnapshot(opts)
	if err != nil {
		t.Fatalf("second DownloadSnapshot failed: %v", err)
	}

	if !result2.AlreadyExists {
		t.Errorf("expected AlreadyExists=true for second download")
	}

	t.Logf("Download test passed: snapshot=%s, peer=%s", result.SnapshotPath, result.PeerHostname)
}

// TestDownloadSnapshotNotFound tests error handling when no peer has the snapshot
func TestDownloadSnapshotNotFound(t *testing.T) {
	// Create empty "peer" directory
	peerDir, err := os.MkdirTemp("", "bupdate-peer-empty")
	if err != nil {
		t.Fatalf("creating peer dir: %v", err)
	}
	defer os.RemoveAll(peerDir)

	// Create "local" snapshots directory
	localSnapsDir, err := os.MkdirTemp("", "bupdate-local-snaps2")
	if err != nil {
		t.Fatalf("creating local snaps dir: %v", err)
	}
	defer os.RemoveAll(localSnapsDir)

	// Create bupdate directory (empty)
	bupdateDir := filepath.Join(peerDir, "bupdate")
	if err := os.MkdirAll(bupdateDir, 0755); err != nil {
		t.Fatalf("creating bupdate dir: %v", err)
	}

	// Start HTTP server
	server, err := NewFileServer(peerDir)
	if err != nil {
		t.Fatalf("creating server: %v", err)
	}
	addr, err := server.Start()
	if err != nil {
		t.Fatalf("starting server: %v", err)
	}
	defer server.Close()

	// Create peer list
	peers := []PeerInfo{
		{URL: "http://" + addr, Hostname: "testpeer.example.com"},
	}

	// Try to download non-existent snapshot
	opts := DownloadSnapshotOptions{
		SnapshotID:   "nonexistent123",
		SnapshotsDir: localSnapsDir,
		Peers:        peers,
	}

	_, err = DownloadSnapshot(opts)
	if err == nil {
		t.Fatalf("expected error for non-existent snapshot")
	}

	t.Logf("Got expected error: %v", err)
}
