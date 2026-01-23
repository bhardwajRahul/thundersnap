package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tailscale/thundersnap/bupdate"
)

// TestPipelineSmallBatchNoDeadlock verifies that the pipeline correctly handles
// files with fewer chunks than the HTTP batch size (64). Before the fix, this
// would deadlock because:
//
//  1. processFile() queues HTTP requests for remote chunks
//  2. httpFetcher() batches requests and only flushes when batch is full (64) or channel closes
//  3. fileWriter() waits for chunks to arrive on the results channel
//  4. The main loop calls fw.wait() which blocks until fileWriter completes
//  5. pipe.stop() (which closes the channel) is deferred until after fw.wait() returns
//
// With fewer than 64 chunks, the batch never fills, stop() never runs, and we deadlock.
// The fix makes httpFetcher flush incomplete batches when the request channel is temporarily empty.
func TestPipelineSmallBatchNoDeadlock(t *testing.T) {
	// Create temp directories
	remoteDir, err := os.MkdirTemp("", "bupdate-pipeline-remote")
	if err != nil {
		t.Fatalf("creating remote dir: %v", err)
	}
	defer os.RemoveAll(remoteDir)

	localDir, err := os.MkdirTemp("", "bupdate-pipeline-local")
	if err != nil {
		t.Fatalf("creating local dir: %v", err)
	}
	defer os.RemoveAll(localDir)

	// Create a small test file - size chosen to produce fewer than 64 chunks
	// With BLOB_MAX of 32768, a 100KB file produces ~4 chunks
	testContent := make([]byte, 100000)
	for i := range testContent {
		testContent[i] = byte(i % 251)
	}

	// Create remote directory structure: <hash>/filename
	fileHash := "deadbeef12345678"
	remoteFileDir := filepath.Join(remoteDir, fileHash)
	if err := os.MkdirAll(remoteFileDir, 0755); err != nil {
		t.Fatalf("creating remote file dir: %v", err)
	}

	testFile := filepath.Join(remoteFileDir, "testfile.bin")
	if err := os.WriteFile(testFile, testContent, 0644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	// Create mfidx for the test file
	mfidxPath := filepath.Join(remoteDir, fileHash+".mfidx")
	if err := createTestMfidx(testFile, "testfile.bin", mfidxPath); err != nil {
		t.Fatalf("creating mfidx: %v", err)
	}

	// Start HTTP server
	server, err := bupdate.NewFileServer(remoteDir)
	if err != nil {
		t.Fatalf("creating file server: %v", err)
	}
	addr, err := server.Start()
	if err != nil {
		t.Fatalf("starting server: %v", err)
	}
	defer server.Close()

	// Run bupdate with a timeout to detect deadlock
	done := make(chan error, 1)
	go func() {
		done <- runBupdate(localDir, "http://"+addr, fileHash+".mfidx")
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("bupdate failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("DEADLOCK DETECTED: bupdate did not complete within 10 seconds. " +
			"This indicates the pipeline batching bug where incomplete batches are not flushed.")
	}

	// Verify the output file was created correctly
	outputFile := filepath.Join(localDir, fileHash, "testfile.bin")
	outputContent, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}

	if !bytes.Equal(outputContent, testContent) {
		t.Errorf("output content mismatch: got %d bytes, want %d bytes", len(outputContent), len(testContent))
	}

	t.Logf("Successfully processed file with %d bytes (fewer than batch size chunks)", len(testContent))
}

// TestPipelineMultipleSmallFiles tests multiple small files that each have
// fewer chunks than the batch size. This more closely matches the real-world
// scenario where many small files are processed through the pipeline.
func TestPipelineMultipleSmallFiles(t *testing.T) {
	remoteDir, err := os.MkdirTemp("", "bupdate-pipeline-multi-remote")
	if err != nil {
		t.Fatalf("creating remote dir: %v", err)
	}
	defer os.RemoveAll(remoteDir)

	localDir, err := os.MkdirTemp("", "bupdate-pipeline-multi-local")
	if err != nil {
		t.Fatalf("creating local dir: %v", err)
	}
	defer os.RemoveAll(localDir)

	// Create multiple small files of varying sizes
	fileSizes := []int{1000, 5000, 10000, 30000, 50000}
	fileContents := make(map[string][]byte)

	fileHash := "multifile12345678"
	remoteFileDir := filepath.Join(remoteDir, fileHash)
	if err := os.MkdirAll(remoteFileDir, 0755); err != nil {
		t.Fatalf("creating remote file dir: %v", err)
	}

	for i, size := range fileSizes {
		content := make([]byte, size)
		for j := range content {
			content[j] = byte((i*17 + j) % 256)
		}
		filename := filepath.Join(remoteFileDir, string(rune('a'+i))+".bin")
		if err := os.WriteFile(filename, content, 0644); err != nil {
			t.Fatalf("writing test file %d: %v", i, err)
		}
		fileContents[string(rune('a'+i))+".bin"] = content
	}

	// Create mfidx for all files
	mfidxPath := filepath.Join(remoteDir, fileHash+".mfidx")
	if err := createTestMfidxMultiple(remoteFileDir, fileContents, mfidxPath); err != nil {
		t.Fatalf("creating mfidx: %v", err)
	}

	// Start HTTP server
	server, err := bupdate.NewFileServer(remoteDir)
	if err != nil {
		t.Fatalf("creating file server: %v", err)
	}
	addr, err := server.Start()
	if err != nil {
		t.Fatalf("starting server: %v", err)
	}
	defer server.Close()

	// Run bupdate with a timeout
	done := make(chan error, 1)
	go func() {
		done <- runBupdate(localDir, "http://"+addr, fileHash+".mfidx")
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("bupdate failed: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("DEADLOCK DETECTED: bupdate did not complete within 30 seconds")
	}

	// Verify all output files
	for filename, expectedContent := range fileContents {
		outputFile := filepath.Join(localDir, fileHash, filename)
		outputContent, err := os.ReadFile(outputFile)
		if err != nil {
			t.Fatalf("reading output file %s: %v", filename, err)
		}

		if !bytes.Equal(outputContent, expectedContent) {
			t.Errorf("output content mismatch for %s: got %d bytes, want %d bytes",
				filename, len(outputContent), len(expectedContent))
		}
	}

	t.Logf("Successfully processed %d small files", len(fileSizes))
}

// TestPipelineSingleChunkFile tests the edge case of a file with exactly one chunk,
// which is the minimal case that would trigger the deadlock.
func TestPipelineSingleChunkFile(t *testing.T) {
	remoteDir, err := os.MkdirTemp("", "bupdate-pipeline-single-remote")
	if err != nil {
		t.Fatalf("creating remote dir: %v", err)
	}
	defer os.RemoveAll(remoteDir)

	localDir, err := os.MkdirTemp("", "bupdate-pipeline-single-local")
	if err != nil {
		t.Fatalf("creating local dir: %v", err)
	}
	defer os.RemoveAll(localDir)

	// Create a tiny file that will be a single chunk
	testContent := []byte("This is a small file that fits in one chunk.")

	fileHash := "singlechunk12345"
	remoteFileDir := filepath.Join(remoteDir, fileHash)
	if err := os.MkdirAll(remoteFileDir, 0755); err != nil {
		t.Fatalf("creating remote file dir: %v", err)
	}

	testFile := filepath.Join(remoteFileDir, "tiny.txt")
	if err := os.WriteFile(testFile, testContent, 0644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	// Create mfidx
	mfidxPath := filepath.Join(remoteDir, fileHash+".mfidx")
	if err := createTestMfidx(testFile, "tiny.txt", mfidxPath); err != nil {
		t.Fatalf("creating mfidx: %v", err)
	}

	// Start HTTP server
	server, err := bupdate.NewFileServer(remoteDir)
	if err != nil {
		t.Fatalf("creating file server: %v", err)
	}
	addr, err := server.Start()
	if err != nil {
		t.Fatalf("starting server: %v", err)
	}
	defer server.Close()

	// Run bupdate with a short timeout - single chunk should be very fast
	done := make(chan error, 1)
	go func() {
		done <- runBupdate(localDir, "http://"+addr, fileHash+".mfidx")
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("bupdate failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("DEADLOCK DETECTED: single-chunk file did not complete within 5 seconds")
	}

	// Verify output
	outputFile := filepath.Join(localDir, fileHash, "tiny.txt")
	outputContent, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}

	if !bytes.Equal(outputContent, testContent) {
		t.Errorf("output content mismatch")
	}

	t.Logf("Successfully processed single-chunk file")
}

// createTestMfidx creates an mfidx file for a single file
func createTestMfidx(srcPath, filename, mfidxPath string) error {
	outf, err := os.Create(mfidxPath)
	if err != nil {
		return err
	}
	defer outf.Close()

	fidxHash := sha1.New()

	// Write header
	header := make([]byte, 8)
	copy(header[0:4], "FIDX")
	binary.BigEndian.PutUint32(header[4:8], bupdate.FIDX_VERSION)
	outf.Write(header)
	fidxHash.Write(header)

	// Get file info
	info, err := os.Stat(srcPath)
	if err != nil {
		return err
	}

	// Write file separator
	sep := bupdate.FileSeparator{
		Filename: filename,
		FileSize: uint64(info.Size()),
		Mtime:    0,
	}

	var sepBuf bytes.Buffer
	bupdate.WriteFileSeparator(&sepBuf, sep)
	outf.Write(sepBuf.Bytes())
	fidxHash.Write(sepBuf.Bytes())

	// Chunk the file
	err = bupdate.ChunkFile(srcPath, func(entry bupdate.FidxEntry) error {
		entryData := make([]byte, 24)
		copy(entryData[0:20], entry.SHA[:])
		binary.BigEndian.PutUint16(entryData[20:22], entry.Size)
		binary.BigEndian.PutUint16(entryData[22:24], entry.Level)
		outf.Write(entryData)
		fidxHash.Write(entryData)
		return nil
	}, nil)
	if err != nil {
		return err
	}

	// Write checksum
	checksum := fidxHash.Sum(nil)
	outf.Write(checksum)

	return nil
}

// createTestMfidxMultiple creates an mfidx file for multiple files
func createTestMfidxMultiple(srcDir string, files map[string][]byte, mfidxPath string) error {
	outf, err := os.Create(mfidxPath)
	if err != nil {
		return err
	}
	defer outf.Close()

	fidxHash := sha1.New()

	// Write header
	header := make([]byte, 8)
	copy(header[0:4], "FIDX")
	binary.BigEndian.PutUint32(header[4:8], bupdate.FIDX_VERSION)
	outf.Write(header)
	fidxHash.Write(header)

	// Process each file
	for filename, content := range files {
		srcPath := filepath.Join(srcDir, filename)

		// Write file separator
		sep := bupdate.FileSeparator{
			Filename: filename,
			FileSize: uint64(len(content)),
			Mtime:    0,
		}

		var sepBuf bytes.Buffer
		bupdate.WriteFileSeparator(&sepBuf, sep)
		outf.Write(sepBuf.Bytes())
		fidxHash.Write(sepBuf.Bytes())

		// Chunk the file
		err = bupdate.ChunkFile(srcPath, func(entry bupdate.FidxEntry) error {
			entryData := make([]byte, 24)
			copy(entryData[0:20], entry.SHA[:])
			binary.BigEndian.PutUint16(entryData[20:22], entry.Size)
			binary.BigEndian.PutUint16(entryData[22:24], entry.Level)
			outf.Write(entryData)
			fidxHash.Write(entryData)
			return nil
		}, nil)
		if err != nil {
			return err
		}
	}

	// Write checksum
	checksum := fidxHash.Sum(nil)
	outf.Write(checksum)

	return nil
}
