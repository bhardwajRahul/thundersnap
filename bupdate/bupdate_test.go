package bupdate

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"os"
	"testing"
)

func TestChunkOverflow(t *testing.T) {
	// This test uses a synthetic file (100KB of zeros) that never triggers
	// natural chunk boundaries, forcing the chunker to split at BLOB_MAX (32768).
	// This tests the fix for the bug where files would be truncated when
	// no natural split points were found.
	//
	// IMPORTANT: This test explicitly verifies the chunk sizes and count.
	// If the chunking algorithm changes (e.g., different BLOB_MAX, different
	// boundary detection), this test MUST fail to alert you to update the
	// test data. Do not simply change the expected values without understanding
	// why the chunking behavior changed.

	// Generate synthetic test file
	testFile := "chunk_overflow.bin"
	originalData := make([]byte, 100000) // 100KB of zeros
	if err := os.WriteFile(testFile, originalData, 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	defer os.Remove(testFile)

	// Create a temporary mfidx file
	mfidxPath := "test_overflow.mfidx"
	defer os.Remove(mfidxPath)

	// Write the mfidx
	outf, err := os.Create(mfidxPath)
	if err != nil {
		t.Fatalf("Failed to create mfidx: %v", err)
	}

	fidxHash := sha1.New()

	// Write header
	header := make([]byte, 8)
	copy(header[0:4], "FIDX")
	binary.BigEndian.PutUint32(header[4:8], FIDX_VERSION)
	outf.Write(header)
	fidxHash.Write(header)

	// Write file separator
	sep := FileSeparator{
		Filename: testFile,
		FileSize: uint64(len(originalData)),
		Mtime:    0,
	}

	var sepBuf []byte
	sepWriter := &bytesWriter{buf: &sepBuf}
	WriteFileSeparator(sepWriter, sep)
	outf.Write(sepBuf)
	fidxHash.Write(sepBuf)

	// Chunk the file
	var totalChunked int64
	err = ChunkFile(testFile, func(entry FidxEntry) error {
		entryData := make([]byte, 24)
		copy(entryData[0:20], entry.SHA[:])
		binary.BigEndian.PutUint16(entryData[20:22], entry.Size)
		binary.BigEndian.PutUint16(entryData[22:24], entry.Level)
		outf.Write(entryData)
		fidxHash.Write(entryData)
		totalChunked += int64(entry.Size)
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("Failed to chunk file: %v", err)
	}

	// Write checksum
	checksum := fidxHash.Sum(nil)
	outf.Write(checksum)
	outf.Close()

	// Verify total chunked size matches original
	if totalChunked != int64(len(originalData)) {
		t.Fatalf("Chunked size %d doesn't match original size %d", totalChunked, len(originalData))
	}

	// Load the mfidx
	fidx, err := LoadFidx(mfidxPath)
	if err != nil {
		t.Fatalf("Failed to load mfidx: %v", err)
	}

	if !fidx.IsMFIDX {
		t.Fatal("Expected mfidx format")
	}

	if len(fidx.Files) != 1 {
		t.Fatalf("Expected 1 file, got %d", len(fidx.Files))
	}

	file := fidx.Files[0]
	if file.Filename != testFile {
		t.Errorf("Expected filename %s, got %s", testFile, file.Filename)
	}

	if file.FileSize != uint64(len(originalData)) {
		t.Errorf("Expected file size %d, got %d", len(originalData), file.FileSize)
	}

	// Verify we hit BLOB_MAX forced splits (this is the critical test)
	// The test file is designed to never trigger natural boundaries,
	// so all chunks except the last should be exactly BLOB_MAX bytes.
	expectedChunks := 4 // 3 full BLOB_MAX chunks + 1 partial
	if len(file.Entries) != expectedChunks {
		t.Fatalf("Expected %d chunks (testing BLOB_MAX forcing), got %d. "+
			"If the chunking algorithm changed, update the test data.", expectedChunks, len(file.Entries))
	}

	// Verify first 3 chunks are BLOB_MAX sized (forced splits)
	for i := 0; i < 3; i++ {
		if file.Entries[i].Size != BLOB_MAX {
			t.Errorf("Chunk %d: expected size %d (BLOB_MAX), got %d. "+
				"This suggests the chunking algorithm isn't forcing splits at BLOB_MAX.",
				i, BLOB_MAX, file.Entries[i].Size)
		}
		if file.Entries[i].Level != 0 {
			t.Errorf("Chunk %d: expected level 0 (forced split), got %d", i, file.Entries[i].Level)
		}
	}

	// Last chunk should be the remainder
	expectedLastSize := len(originalData) - (3 * BLOB_MAX)
	if file.Entries[3].Size != uint16(expectedLastSize) {
		t.Errorf("Last chunk: expected size %d, got %d", expectedLastSize, file.Entries[3].Size)
	}

	// Reconstruct from chunks
	var reconstructed []byte
	for _, entry := range file.Entries {
		// Find chunk in original file by scanning
		found := false
		for offset := 0; offset <= len(originalData)-int(entry.Size); offset++ {
			chunk := originalData[offset : offset+int(entry.Size)]
			computedSHA := BlobSHA(chunk)
			if bytes.Equal(computedSHA[:], entry.SHA[:]) {
				reconstructed = append(reconstructed, chunk...)
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("Could not find chunk with SHA %x", entry.SHA)
		}
	}

	// Verify reconstructed data matches original
	if !bytes.Equal(reconstructed, originalData) {
		t.Fatalf("Reconstructed data doesn't match original. Original: %d bytes, Reconstructed: %d bytes",
			len(originalData), len(reconstructed))
	}

	t.Logf("Successfully chunked and reconstructed %d bytes in %d chunks",
		len(originalData), len(file.Entries))
}

// bytesWriter wraps a byte slice for io.Writer compatibility
type bytesWriter struct {
	buf *[]byte
}

func (bw *bytesWriter) Write(p []byte) (n int, err error) {
	*bw.buf = append(*bw.buf, p...)
	return len(p), nil
}
