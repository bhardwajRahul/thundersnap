package tsm

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestTSCWriteRead(t *testing.T) {
	tmpDir := t.TempDir()
	tscPath := filepath.Join(tmpDir, "test.tsc")

	// Create TSC with some test chunks
	writer := NewTSCWriter()

	// Add some test chunks
	sha1 := sha256.Sum256([]byte("chunk1"))
	sha2 := sha256.Sum256([]byte("chunk2"))
	sha3 := sha256.Sum256([]byte("chunk3"))

	idx1 := writer.AddChunk(sha1, 100, 1, 0)
	idx2 := writer.AddChunk(sha2, 200, 2, 0)
	idx3 := writer.AddChunk(sha3, 300, 3, TSCEntryFlagZeroBlock)

	// Test deduplication
	idx1dup := writer.AddChunk(sha1, 100, 1, 0)
	if idx1dup != idx1 {
		t.Errorf("deduplication failed: got index %d, want %d", idx1dup, idx1)
	}

	if writer.ChunkCount() != 3 {
		t.Errorf("chunk count: got %d, want 3", writer.ChunkCount())
	}

	// Write the file
	fileSHA, indexMap, err := writer.Write(tscPath)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	if fileSHA == [32]byte{} {
		t.Error("file SHA is zero")
	}

	// Verify index map exists
	if len(indexMap) != 3 {
		t.Errorf("index map length: got %d, want 3", len(indexMap))
	}

	// Read it back
	reader, err := ReadTSC(tscPath)
	if err != nil {
		t.Fatalf("ReadTSC failed: %v", err)
	}

	if reader.Header.ChunkCount != 3 {
		t.Errorf("header chunk count: got %d, want 3", reader.Header.ChunkCount)
	}

	if reader.Header.TotalChunkSize != 600 {
		t.Errorf("total chunk size: got %d, want 600", reader.Header.TotalChunkSize)
	}

	if len(reader.Entries) != 3 {
		t.Errorf("entries count: got %d, want 3", len(reader.Entries))
	}

	// Verify entries are sorted by SHA
	for i := 1; i < len(reader.Entries); i++ {
		if bytes.Compare(reader.Entries[i-1].SHA256[:], reader.Entries[i].SHA256[:]) >= 0 {
			t.Error("entries not sorted by SHA")
		}
	}

	// Test lookup
	entry, found := reader.LookupChunk(sha2)
	if !found {
		t.Error("LookupChunk: sha2 not found")
	}
	if entry.Size != 200 {
		t.Errorf("LookupChunk: size got %d, want 200", entry.Size)
	}

	// Test lookup for non-existent chunk
	nonExistent := sha256.Sum256([]byte("nonexistent"))
	_, found = reader.LookupChunk(nonExistent)
	if found {
		t.Error("LookupChunk: found non-existent chunk")
	}

	_ = idx2
	_ = idx3
}

func TestTSMWriteRead(t *testing.T) {
	tmpDir := t.TempDir()
	tsmPath := filepath.Join(tmpDir, "test.tsm")

	// Create a fake TSC SHA for reference
	tscSHA := sha256.Sum256([]byte("fake tsc"))

	// Create TSM with test entries
	writer := NewTSMWriter()

	writer.AddEntry(TSMEntry{
		Path:  "dir",
		Type:  EntryTypeDir,
		Mode:  0755 | uint32(os.ModeDir),
		UID:   1000,
		GID:   1000,
		Mtime: 1234567890000000000,
		Ctime: 1234567890000000000,
		Atime: 1234567890000000000,
	})

	writer.AddEntry(TSMEntry{
		Path:       "dir/file.txt",
		Type:       EntryTypeFile,
		Mode:       0644,
		UID:        1000,
		GID:        1000,
		Size:       1024,
		Mtime:      1234567890000000000,
		Ctime:      1234567890000000000,
		Atime:      1234567890000000000,
		ChunkRefs:  []uint32{0, 1, 2, 3, 4},
		ChunkCount: 5,
	})

	writer.AddEntry(TSMEntry{
		Path:       "link",
		Type:       EntryTypeSymlink,
		Mode:       0777 | uint32(os.ModeSymlink),
		UID:        1000,
		GID:        1000,
		LinkTarget: "dir/file.txt",
		Mtime:      1234567890000000000,
		Ctime:      1234567890000000000,
		Atime:      1234567890000000000,
	})

	if writer.EntryCount() != 3 {
		t.Errorf("entry count: got %d, want 3", writer.EntryCount())
	}

	// Write the file
	fileSHA, err := writer.Write(tsmPath, tscSHA, nil)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	if fileSHA == [32]byte{} {
		t.Error("file SHA is zero")
	}

	// Read it back
	reader, err := ReadTSM(tsmPath)
	if err != nil {
		t.Fatalf("ReadTSM failed: %v", err)
	}

	if reader.Header.FileCount != 3 {
		t.Errorf("header file count: got %d, want 3", reader.Header.FileCount)
	}

	if len(reader.Entries) != 3 {
		t.Errorf("entries count: got %d, want 3", len(reader.Entries))
	}

	// Verify entries are sorted by path
	for i := 1; i < len(reader.Entries); i++ {
		if reader.Entries[i-1].Path >= reader.Entries[i].Path {
			t.Errorf("entries not sorted: %q >= %q", reader.Entries[i-1].Path, reader.Entries[i].Path)
		}
	}

	// Check the file entry
	fileEntry, found := reader.LookupPath("dir/file.txt")
	if !found {
		t.Error("LookupPath: dir/file.txt not found")
	}
	if fileEntry.Type != EntryTypeFile {
		t.Errorf("file type: got %v, want file", fileEntry.Type)
	}
	if fileEntry.Size != 1024 {
		t.Errorf("file size: got %d, want 1024", fileEntry.Size)
	}
	if fileEntry.ChunkCount != 5 {
		t.Errorf("chunk count: got %d, want 5", fileEntry.ChunkCount)
	}

	// Check the symlink entry
	linkEntry, found := reader.LookupPath("link")
	if !found {
		t.Error("LookupPath: link not found")
	}
	if linkEntry.Type != EntryTypeSymlink {
		t.Errorf("link type: got %v, want symlink", linkEntry.Type)
	}
	if linkEntry.LinkTarget != "dir/file.txt" {
		t.Errorf("link target: got %q, want %q", linkEntry.LinkTarget, "dir/file.txt")
	}

	// Verify TSC SHA is stored
	if reader.TSCSHA != tscSHA {
		t.Error("TSC SHA mismatch")
	}
}

func TestBlobSHA256(t *testing.T) {
	// Test that BlobSHA256 produces consistent output
	data := []byte("hello world")
	sha1 := BlobSHA256(data)
	sha2 := BlobSHA256(data)

	if sha1 != sha2 {
		t.Error("BlobSHA256 not deterministic")
	}

	// Different data should produce different SHA
	sha3 := BlobSHA256([]byte("different"))
	if sha1 == sha3 {
		t.Error("different data produced same SHA")
	}
}

func TestChunkData(t *testing.T) {
	// Test chunking a small piece of data
	data := []byte("small data that won't be split")
	var chunks []struct {
		sha  [32]byte
		size uint32
	}

	err := ChunkData(data, func(sha [32]byte, size uint32, level uint16) error {
		chunks = append(chunks, struct {
			sha  [32]byte
			size uint32
		}{sha, size})
		return nil
	})
	if err != nil {
		t.Fatalf("ChunkData failed: %v", err)
	}

	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk, got %d", len(chunks))
	}

	if chunks[0].size != uint32(len(data)) {
		t.Errorf("chunk size: got %d, want %d", chunks[0].size, len(data))
	}

	// Verify SHA matches
	expectedSHA := BlobSHA256(data)
	if chunks[0].sha != expectedSHA {
		t.Error("chunk SHA mismatch")
	}
}

func TestChunkLargeData(t *testing.T) {
	// Test chunking data larger than BLOB_MAX
	data := make([]byte, BLOB_MAX*3+1000)
	for i := range data {
		data[i] = byte(i % 256)
	}

	var totalSize uint32
	var chunkCount int

	err := ChunkData(data, func(sha [32]byte, size uint32, level uint16) error {
		totalSize += size
		chunkCount++
		return nil
	})
	if err != nil {
		t.Fatalf("ChunkData failed: %v", err)
	}

	if totalSize != uint32(len(data)) {
		t.Errorf("total size: got %d, want %d", totalSize, len(data))
	}

	// Should have at least 4 chunks (3 BLOB_MAX + remainder, possibly more due to content-defined boundaries)
	if chunkCount < 4 {
		t.Errorf("expected at least 4 chunks, got %d", chunkCount)
	}
}

func TestIndexerBasic(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a test directory structure
	testDir := filepath.Join(tmpDir, "testroot")
	if err := os.MkdirAll(filepath.Join(testDir, "subdir"), 0755); err != nil {
		t.Fatal(err)
	}

	// Create a regular file
	if err := os.WriteFile(filepath.Join(testDir, "file.txt"), []byte("hello world"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a file in subdir
	if err := os.WriteFile(filepath.Join(testDir, "subdir", "nested.txt"), []byte("nested content"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a symlink
	if err := os.Symlink("file.txt", filepath.Join(testDir, "link")); err != nil {
		t.Fatal(err)
	}

	// Index the directory
	outBase := filepath.Join(tmpDir, "output")
	err := Create(testDir, outBase, IndexerOptions{})
	if err != nil {
		t.Fatalf("Index failed: %v", err)
	}

	// Verify output files exist
	if _, err := os.Stat(outBase + ".tsm"); err != nil {
		t.Errorf("TSM file not created: %v", err)
	}
	if _, err := os.Stat(outBase + ".tsc"); err != nil {
		t.Errorf("TSC file not created: %v", err)
	}

	// Read and verify TSM
	tsm, err := ReadTSM(outBase + ".tsm")
	if err != nil {
		t.Fatalf("ReadTSM failed: %v", err)
	}

	// Should have: root dir, subdir, file.txt, nested.txt, link = 5 entries
	if len(tsm.Entries) != 5 {
		t.Errorf("expected 5 entries, got %d", len(tsm.Entries))
		for _, e := range tsm.Entries {
			t.Logf("  %s (%v)", e.Path, e.Type)
		}
	}

	// Check file entry
	fileEntry, found := tsm.LookupPath("file.txt")
	if !found {
		t.Error("file.txt not found")
	} else {
		if fileEntry.Type != EntryTypeFile {
			t.Errorf("file.txt type: got %v, want file", fileEntry.Type)
		}
		if fileEntry.Size != 11 {
			t.Errorf("file.txt size: got %d, want 11", fileEntry.Size)
		}
	}

	// Check symlink entry
	linkEntry, found := tsm.LookupPath("link")
	if !found {
		t.Error("link not found")
	} else {
		if linkEntry.Type != EntryTypeSymlink {
			t.Errorf("link type: got %v, want symlink", linkEntry.Type)
		}
		if linkEntry.LinkTarget != "file.txt" {
			t.Errorf("link target: got %q, want %q", linkEntry.LinkTarget, "file.txt")
		}
	}

	// Read and verify TSC
	tsc, err := ReadTSC(outBase + ".tsc")
	if err != nil {
		t.Fatalf("ReadTSC failed: %v", err)
	}

	// Should have chunks for the two regular files
	if tsc.Header.ChunkCount == 0 {
		t.Error("TSC has no chunks")
	}
}

func TestIndexerHardlinks(t *testing.T) {
	tmpDir := t.TempDir()
	testDir := filepath.Join(tmpDir, "testroot")
	if err := os.MkdirAll(testDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a file and a hardlink to it
	originalFile := filepath.Join(testDir, "original.txt")
	hardlinkFile := filepath.Join(testDir, "hardlink.txt")

	if err := os.WriteFile(originalFile, []byte("shared content"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.Link(originalFile, hardlinkFile); err != nil {
		t.Fatal(err)
	}

	// Index
	outBase := filepath.Join(tmpDir, "output")
	err := Create(testDir, outBase, IndexerOptions{})
	if err != nil {
		t.Fatalf("Index failed: %v", err)
	}

	// Read TSM
	tsm, err := ReadTSM(outBase + ".tsm")
	if err != nil {
		t.Fatalf("ReadTSM failed: %v", err)
	}

	// Find the hardlink entry
	var hardlinkEntry *TSMEntry
	var fileEntry *TSMEntry
	for i := range tsm.Entries {
		if tsm.Entries[i].Path == "hardlink.txt" {
			hardlinkEntry = &tsm.Entries[i]
		}
		if tsm.Entries[i].Path == "original.txt" {
			fileEntry = &tsm.Entries[i]
		}
	}

	// One should be a regular file, one should be a hardlink
	// (order depends on sorting)
	if hardlinkEntry == nil || fileEntry == nil {
		t.Fatal("missing entries")
	}

	// The first one (alphabetically) gets to be the file
	// hardlink.txt < original.txt, so hardlink.txt is first
	if hardlinkEntry.Type != EntryTypeFile {
		t.Errorf("hardlink.txt should be the file (first alphabetically), got %v", hardlinkEntry.Type)
	}
	if fileEntry.Type != EntryTypeHardlink {
		t.Errorf("original.txt should be the hardlink, got %v", fileEntry.Type)
	}
}

func TestIndexerEmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	testDir := filepath.Join(tmpDir, "testroot")
	if err := os.MkdirAll(testDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create an empty file
	if err := os.WriteFile(filepath.Join(testDir, "empty.txt"), []byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	// Index
	outBase := filepath.Join(tmpDir, "output")
	err := Create(testDir, outBase, IndexerOptions{})
	if err != nil {
		t.Fatalf("Index failed: %v", err)
	}

	// Read TSM
	tsm, err := ReadTSM(outBase + ".tsm")
	if err != nil {
		t.Fatalf("ReadTSM failed: %v", err)
	}

	// Find the empty file
	entry, found := tsm.LookupPath("empty.txt")
	if !found {
		t.Fatal("empty.txt not found")
	}

	if entry.Size != 0 {
		t.Errorf("empty file size: got %d, want 0", entry.Size)
	}
	if entry.ChunkCount != 0 {
		t.Errorf("empty file chunk count: got %d, want 0", entry.ChunkCount)
	}
}

func TestIndexerDeviceNodes(t *testing.T) {
	// Skip if not running as root (can't create device nodes)
	if os.Getuid() != 0 {
		t.Skip("skipping device node test: not running as root")
	}

	tmpDir := t.TempDir()
	testDir := filepath.Join(tmpDir, "testroot")
	if err := os.MkdirAll(testDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a character device (null device clone)
	nullDev := filepath.Join(testDir, "null")
	// major 1, minor 3 = /dev/null
	if err := syscall.Mknod(nullDev, syscall.S_IFCHR|0666, int(unix.Mkdev(1, 3))); err != nil {
		t.Fatalf("mknod failed: %v", err)
	}

	// Index
	outBase := filepath.Join(tmpDir, "output")
	err := Create(testDir, outBase, IndexerOptions{})
	if err != nil {
		t.Fatalf("Index failed: %v", err)
	}

	// Read TSM
	tsm, err := ReadTSM(outBase + ".tsm")
	if err != nil {
		t.Fatalf("ReadTSM failed: %v", err)
	}

	// Find the device
	entry, found := tsm.LookupPath("null")
	if !found {
		t.Fatal("null device not found")
	}

	if entry.Type != EntryTypeCharDev {
		t.Errorf("device type: got %v, want chardev", entry.Type)
	}
	if entry.DevMajor != 1 {
		t.Errorf("device major: got %d, want 1", entry.DevMajor)
	}
	if entry.DevMinor != 3 {
		t.Errorf("device minor: got %d, want 3", entry.DevMinor)
	}
}

func TestIndexerFifo(t *testing.T) {
	tmpDir := t.TempDir()
	testDir := filepath.Join(tmpDir, "testroot")
	if err := os.MkdirAll(testDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a named pipe (FIFO)
	fifoPath := filepath.Join(testDir, "fifo")
	if err := syscall.Mkfifo(fifoPath, 0644); err != nil {
		t.Fatalf("mkfifo failed: %v", err)
	}

	// Index
	outBase := filepath.Join(tmpDir, "output")
	err := Create(testDir, outBase, IndexerOptions{})
	if err != nil {
		t.Fatalf("Index failed: %v", err)
	}

	// Read TSM
	tsm, err := ReadTSM(outBase + ".tsm")
	if err != nil {
		t.Fatalf("ReadTSM failed: %v", err)
	}

	// Find the fifo
	entry, found := tsm.LookupPath("fifo")
	if !found {
		t.Fatal("fifo not found")
	}

	if entry.Type != EntryTypeFifo {
		t.Errorf("fifo type: got %v, want fifo", entry.Type)
	}
}

func TestChunkOverflow(t *testing.T) {
	// Test with data that doesn't trigger natural chunk boundaries
	// This forces splits at BLOB_MAX
	data := make([]byte, 100000) // 100KB of zeros

	var totalSize uint32
	var chunks []uint32

	err := ChunkData(data, func(sha [32]byte, size uint32, level uint16) error {
		totalSize += size
		chunks = append(chunks, size)
		return nil
	})
	if err != nil {
		t.Fatalf("ChunkData failed: %v", err)
	}

	if totalSize != uint32(len(data)) {
		t.Errorf("total size: got %d, want %d", totalSize, len(data))
	}

	// With 100KB of zeros and BLOB_MAX=32768:
	// 100000 / 32768 = 3 full chunks + 1696 remainder
	// So we expect 4 chunks
	expectedChunks := 4
	if len(chunks) != expectedChunks {
		t.Errorf("chunk count: got %d, want %d", len(chunks), expectedChunks)
	}

	// Verify chunk sizes
	for i := 0; i < 3; i++ {
		if chunks[i] != BLOB_MAX {
			t.Errorf("chunk %d size: got %d, want %d", i, chunks[i], BLOB_MAX)
		}
	}

	expectedLast := 100000 - (3 * BLOB_MAX)
	if chunks[3] != uint32(expectedLast) {
		t.Errorf("last chunk size: got %d, want %d", chunks[3], expectedLast)
	}
}

func TestTSMRoundtrip(t *testing.T) {
	// Test that we can write and read back all entry types
	tmpDir := t.TempDir()
	tsmPath := filepath.Join(tmpDir, "test.tsm")
	tscSHA := sha256.Sum256([]byte("fake tsc"))

	writer := NewTSMWriter()

	// Add all entry types
	entries := []TSMEntry{
		{
			Path:  "dir",
			Type:  EntryTypeDir,
			Mode:  0755,
			UID:   1000,
			GID:   1000,
			Mtime: 1000000000,
			Ctime: 1000000000,
			Atime: 1000000000,
		},
		{
			Path:       "file",
			Type:       EntryTypeFile,
			Mode:       0644,
			UID:        1000,
			GID:        1000,
			Size:       12345,
			Mtime:      1000000000,
			Ctime:      1000000000,
			Atime:      1000000000,
			ChunkRefs:  []uint32{10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29},
			ChunkCount: 20,
		},
		{
			Path:       "symlink",
			Type:       EntryTypeSymlink,
			Mode:       0777,
			UID:        1000,
			GID:        1000,
			LinkTarget: "/some/target/path",
			Mtime:      1000000000,
			Ctime:      1000000000,
			Atime:      1000000000,
		},
		{
			Path:      "hardlink",
			Type:      EntryTypeHardlink,
			Mode:      0644,
			UID:       1000,
			GID:       1000,
			LinkIndex: 1,
			Mtime:     1000000000,
			Ctime:     1000000000,
			Atime:     1000000000,
		},
		{
			Path:     "blockdev",
			Type:     EntryTypeBlockDev,
			Mode:     0660,
			UID:      0,
			GID:      6,
			DevMajor: 8,
			DevMinor: 0,
			Mtime:    1000000000,
			Ctime:    1000000000,
			Atime:    1000000000,
		},
		{
			Path:     "chardev",
			Type:     EntryTypeCharDev,
			Mode:     0666,
			UID:      0,
			GID:      0,
			DevMajor: 1,
			DevMinor: 3,
			Mtime:    1000000000,
			Ctime:    1000000000,
			Atime:    1000000000,
		},
		{
			Path:  "fifo",
			Type:  EntryTypeFifo,
			Mode:  0644,
			UID:   1000,
			GID:   1000,
			Mtime: 1000000000,
			Ctime: 1000000000,
			Atime: 1000000000,
		},
		{
			Path:  "socket",
			Type:  EntryTypeSocket,
			Mode:  0755,
			UID:   1000,
			GID:   1000,
			Mtime: 1000000000,
			Ctime: 1000000000,
			Atime: 1000000000,
		},
	}

	for _, e := range entries {
		writer.AddEntry(e)
	}

	_, err := writer.Write(tsmPath, tscSHA, nil)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	reader, err := ReadTSM(tsmPath)
	if err != nil {
		t.Fatalf("ReadTSM failed: %v", err)
	}

	if len(reader.Entries) != len(entries) {
		t.Fatalf("entry count: got %d, want %d", len(reader.Entries), len(entries))
	}

	// Verify each entry (note: entries are sorted by path)
	for _, original := range entries {
		found, ok := reader.LookupPath(original.Path)
		if !ok {
			t.Errorf("entry %q not found", original.Path)
			continue
		}

		if found.Type != original.Type {
			t.Errorf("%s: type got %v, want %v", original.Path, found.Type, original.Type)
		}
		if found.Mode != original.Mode {
			t.Errorf("%s: mode got %o, want %o", original.Path, found.Mode, original.Mode)
		}
		if found.UID != original.UID {
			t.Errorf("%s: UID got %d, want %d", original.Path, found.UID, original.UID)
		}
		if found.GID != original.GID {
			t.Errorf("%s: GID got %d, want %d", original.Path, found.GID, original.GID)
		}

		switch original.Type {
		case EntryTypeFile:
			if found.Size != original.Size {
				t.Errorf("%s: size got %d, want %d", original.Path, found.Size, original.Size)
			}
			if found.ChunkCount != original.ChunkCount {
				t.Errorf("%s: chunk count got %d, want %d", original.Path, found.ChunkCount, original.ChunkCount)
			}
			// Verify chunk refs match (values should be same since no index mapping)
			if len(found.ChunkRefs) != len(original.ChunkRefs) {
				t.Errorf("%s: chunk refs length got %d, want %d", original.Path, len(found.ChunkRefs), len(original.ChunkRefs))
			} else {
				for j, ref := range found.ChunkRefs {
					if ref != original.ChunkRefs[j] {
						t.Errorf("%s: chunk ref[%d] got %d, want %d", original.Path, j, ref, original.ChunkRefs[j])
					}
				}
			}
		case EntryTypeSymlink:
			if found.LinkTarget != original.LinkTarget {
				t.Errorf("%s: link target got %q, want %q", original.Path, found.LinkTarget, original.LinkTarget)
			}
		case EntryTypeHardlink:
			if found.LinkIndex != original.LinkIndex {
				t.Errorf("%s: link index got %d, want %d", original.Path, found.LinkIndex, original.LinkIndex)
			}
		case EntryTypeBlockDev, EntryTypeCharDev:
			if found.DevMajor != original.DevMajor {
				t.Errorf("%s: dev major got %d, want %d", original.Path, found.DevMajor, original.DevMajor)
			}
			if found.DevMinor != original.DevMinor {
				t.Errorf("%s: dev minor got %d, want %d", original.Path, found.DevMinor, original.DevMinor)
			}
		}
	}
}

func TestChunkRefTableRoundtrip(t *testing.T) {
	// Simulate a full indexer flow: create TSC + TSM, verify that
	// chunk references survive the TSC sort + index remapping.
	tmpDir := t.TempDir()

	// Create test files with known content
	testDir := filepath.Join(tmpDir, "testroot")
	if err := os.MkdirAll(testDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create two files with different content
	file1Data := []byte("first file content with some unique data AAAA")
	file2Data := []byte("second file with different unique data BBBB")
	if err := os.WriteFile(filepath.Join(testDir, "file1.txt"), file1Data, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(testDir, "file2.txt"), file2Data, 0644); err != nil {
		t.Fatal(err)
	}

	// Index
	outBase := filepath.Join(tmpDir, "output")
	err := Create(testDir, outBase, IndexerOptions{})
	if err != nil {
		t.Fatalf("Index failed: %v", err)
	}

	// Read both files back
	tsmReader, err := ReadTSM(outBase + ".tsm")
	if err != nil {
		t.Fatalf("ReadTSM failed: %v", err)
	}
	tscReader, err := ReadTSC(outBase + ".tsc")
	if err != nil {
		t.Fatalf("ReadTSC failed: %v", err)
	}

	// Verify TSC SHA reference matches
	if tsmReader.TSCSHA != tscReader.SHA256 {
		t.Error("TSC SHA mismatch between TSM footer and TSC file")
	}

	// For each file, verify that the chunk refs point to valid TSC entries,
	// and that the chunk SHAs match what we'd compute directly.
	for _, entry := range tsmReader.Entries {
		if entry.Type != EntryTypeFile || entry.ChunkCount == 0 {
			continue
		}

		// Get the file's chunk SHAs via the ref table
		shas := GetFileChunkSHAs(&entry, tscReader)
		if len(shas) != int(entry.ChunkCount) {
			t.Errorf("%s: expected %d chunk SHAs, got %d", entry.Path, entry.ChunkCount, len(shas))
			continue
		}

		// Read the actual file and chunk it to get expected SHAs
		filePath := filepath.Join(testDir, entry.Path)
		var expectedSHAs [][32]byte
		err := ChunkFile(filePath, func(sha [32]byte, size uint32, level uint16) error {
			expectedSHAs = append(expectedSHAs, sha)
			return nil
		}, nil)
		if err != nil {
			t.Fatalf("%s: ChunkFile failed: %v", entry.Path, err)
		}

		if len(shas) != len(expectedSHAs) {
			t.Errorf("%s: chunk count mismatch: ref table has %d, direct chunking has %d",
				entry.Path, len(shas), len(expectedSHAs))
			continue
		}

		for i := range shas {
			if shas[i] != expectedSHAs[i] {
				t.Errorf("%s: chunk[%d] SHA mismatch: ref table=%x, direct=%x",
					entry.Path, i, shas[i], expectedSHAs[i])
			}
		}
	}
}

func TestIndexerRootTimestampsZeroed(t *testing.T) {
	// Test that the root entry has zeroed timestamps. This ensures that
	// two empty directories produce identical hashes regardless of when
	// they were created (e.g., /home and /work subvolumes).
	tmpDir := t.TempDir()

	// Create two empty directories
	dir1 := filepath.Join(tmpDir, "dir1")
	dir2 := filepath.Join(tmpDir, "dir2")

	if err := os.MkdirAll(dir1, 0755); err != nil {
		t.Fatal(err)
	}
	// Sleep briefly to ensure different timestamps
	time.Sleep(10 * time.Millisecond)
	if err := os.MkdirAll(dir2, 0755); err != nil {
		t.Fatal(err)
	}

	// Index both directories
	out1 := filepath.Join(tmpDir, "out1")
	out2 := filepath.Join(tmpDir, "out2")

	if err := Create(dir1, out1, IndexerOptions{}); err != nil {
		t.Fatalf("Index dir1 failed: %v", err)
	}
	if err := Create(dir2, out2, IndexerOptions{}); err != nil {
		t.Fatalf("Index dir2 failed: %v", err)
	}

	// Read both TSMs
	tsm1, err := ReadTSM(out1 + ".tsm")
	if err != nil {
		t.Fatalf("ReadTSM dir1 failed: %v", err)
	}
	tsm2, err := ReadTSM(out2 + ".tsm")
	if err != nil {
		t.Fatalf("ReadTSM dir2 failed: %v", err)
	}

	// Verify the root entry has zeroed timestamps
	root1, found := tsm1.LookupPath("")
	if !found {
		t.Fatal("root entry not found in dir1 TSM")
	}
	if root1.Mtime != 0 || root1.Ctime != 0 || root1.Atime != 0 {
		t.Errorf("root entry should have zeroed timestamps, got mtime=%d ctime=%d atime=%d",
			root1.Mtime, root1.Ctime, root1.Atime)
	}

	root2, found := tsm2.LookupPath("")
	if !found {
		t.Fatal("root entry not found in dir2 TSM")
	}
	if root2.Mtime != 0 || root2.Ctime != 0 || root2.Atime != 0 {
		t.Errorf("root entry should have zeroed timestamps, got mtime=%d ctime=%d atime=%d",
			root2.Mtime, root2.Ctime, root2.Atime)
	}

	// Two empty directories should produce identical TSM hashes since root
	// timestamps are zeroed
	if tsm1.SHA256 != tsm2.SHA256 {
		t.Errorf("TSM SHA256 mismatch: two empty directories should have identical hashes\n"+
			"dir1: %x\ndir2: %x", tsm1.SHA256, tsm2.SHA256)
	}
}

// TestIndexerCtimeAtimeStored verifies that ctime and atime are stored in TSM
// entries (for change detection), but the TSM hash excludes them (for reproducibility).
func TestIndexerCtimeAtimeStored(t *testing.T) {
	tmpDir := t.TempDir()
	testDir := filepath.Join(tmpDir, "testroot")
	if err := os.MkdirAll(testDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a directory with some files
	subdir := filepath.Join(testDir, "subdir")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(testDir, "file.txt"), []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "nested.txt"), []byte("nested"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file.txt", filepath.Join(testDir, "link")); err != nil {
		t.Fatal(err)
	}

	// Index the directory
	outBase := filepath.Join(tmpDir, "out")
	if err := Create(testDir, outBase, IndexerOptions{}); err != nil {
		t.Fatalf("Index failed: %v", err)
	}

	// Read the TSM
	tsm, err := ReadTSM(outBase + ".tsm")
	if err != nil {
		t.Fatalf("ReadTSM failed: %v", err)
	}

	// Verify non-root entries have non-zero ctime (stored for change detection)
	// Root entry should have zeroed timestamps
	for _, entry := range tsm.Entries {
		if entry.Path == "" {
			// Root entry should have zeroed timestamps
			if entry.Ctime != 0 || entry.Atime != 0 || entry.Mtime != 0 {
				t.Errorf("root entry should have zeroed timestamps, got mtime=%d ctime=%d atime=%d",
					entry.Mtime, entry.Ctime, entry.Atime)
			}
		} else {
			// Non-root entries should have real ctime/atime (for change detection)
			if entry.Ctime == 0 {
				t.Errorf("entry %q has zero ctime, expected non-zero (stored for change detection)", entry.Path)
			}
		}
	}
}

// TestIndexerReproducibility verifies that indexing the same directory content
// at different times produces identical TSM files. This specifically tests
// that non-reproducible timestamps (ctime, atime) don't affect the hash.
func TestIndexerReproducibility(t *testing.T) {
	tmpDir := t.TempDir()

	// Create two directories with identical content at different times
	dir1 := filepath.Join(tmpDir, "dir1")
	dir2 := filepath.Join(tmpDir, "dir2")

	createTestDir := func(root string) {
		os.MkdirAll(filepath.Join(root, "subdir"), 0755)
		os.WriteFile(filepath.Join(root, "file.txt"), []byte("content"), 0644)
		os.WriteFile(filepath.Join(root, "subdir", "nested.txt"), []byte("nested"), 0644)
		os.Symlink("file.txt", filepath.Join(root, "link"))
	}

	createTestDir(dir1)
	// Sleep to ensure different ctime/atime on the filesystem
	time.Sleep(50 * time.Millisecond)
	createTestDir(dir2)

	// Set identical mtimes (the one timestamp we DO preserve)
	// Note: os.Chtimes follows symlinks, so we need unix.Lutimes for symlinks
	fixedTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	setMtimeRecursive := func(root string) {
		filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				// Use Lutimes for symlinks (Chtimes follows symlinks)
				tv := []unix.Timeval{
					{Sec: fixedTime.Unix(), Usec: 0},
					{Sec: fixedTime.Unix(), Usec: 0},
				}
				unix.Lutimes(path, tv)
			} else {
				os.Chtimes(path, fixedTime, fixedTime)
			}
			return nil
		})
	}
	setMtimeRecursive(dir1)
	setMtimeRecursive(dir2)

	// Index both
	out1 := filepath.Join(tmpDir, "out1")
	out2 := filepath.Join(tmpDir, "out2")
	if err := Create(dir1, out1, IndexerOptions{}); err != nil {
		t.Fatalf("Index dir1 failed: %v", err)
	}
	if err := Create(dir2, out2, IndexerOptions{}); err != nil {
		t.Fatalf("Index dir2 failed: %v", err)
	}

	// Read both TSMs
	tsm1, err := ReadTSM(out1 + ".tsm")
	if err != nil {
		t.Fatalf("ReadTSM dir1 failed: %v", err)
	}
	tsm2, err := ReadTSM(out2 + ".tsm")
	if err != nil {
		t.Fatalf("ReadTSM dir2 failed: %v", err)
	}

	// The TSM files should have identical hashes
	if tsm1.SHA256 != tsm2.SHA256 {
		t.Errorf("TSM SHA256 mismatch: identical content should produce identical hashes\n"+
			"dir1: %x\ndir2: %x", tsm1.SHA256, tsm2.SHA256)
	}
}

func TestChunkRefTableWithDedup(t *testing.T) {
	// Test that deduplicated chunks (same content in different files)
	// correctly reference the same TSC entry.
	tmpDir := t.TempDir()
	testDir := filepath.Join(tmpDir, "testroot")
	if err := os.MkdirAll(testDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create two files with identical content
	sharedContent := []byte("identical content in both files")
	if err := os.WriteFile(filepath.Join(testDir, "copy1.txt"), sharedContent, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(testDir, "copy2.txt"), sharedContent, 0644); err != nil {
		t.Fatal(err)
	}

	// Index
	outBase := filepath.Join(tmpDir, "output")
	err := Create(testDir, outBase, IndexerOptions{})
	if err != nil {
		t.Fatalf("Index failed: %v", err)
	}

	// Read both files back
	tsmReader, err := ReadTSM(outBase + ".tsm")
	if err != nil {
		t.Fatalf("ReadTSM failed: %v", err)
	}
	tscReader, err := ReadTSC(outBase + ".tsc")
	if err != nil {
		t.Fatalf("ReadTSC failed: %v", err)
	}

	// The TSC should have only 1 unique chunk (since both files are identical)
	if tscReader.Header.ChunkCount != 1 {
		t.Errorf("expected 1 unique chunk, got %d", tscReader.Header.ChunkCount)
	}

	// Both files should reference the same TSC entry
	copy1, found1 := tsmReader.LookupPath("copy1.txt")
	copy2, found2 := tsmReader.LookupPath("copy2.txt")
	if !found1 || !found2 {
		t.Fatal("missing file entries")
	}

	if copy1.ChunkCount != 1 || copy2.ChunkCount != 1 {
		t.Errorf("expected 1 chunk each, got %d and %d", copy1.ChunkCount, copy2.ChunkCount)
	}

	if len(copy1.ChunkRefs) != 1 || len(copy2.ChunkRefs) != 1 {
		t.Errorf("expected 1 chunk ref each, got %d and %d",
			len(copy1.ChunkRefs), len(copy2.ChunkRefs))
	} else if copy1.ChunkRefs[0] != copy2.ChunkRefs[0] {
		t.Errorf("dedup failed: copy1 ref=%d, copy2 ref=%d (should be same)",
			copy1.ChunkRefs[0], copy2.ChunkRefs[0])
	}

	// Verify the chunk SHAs match
	shas1 := GetFileChunkSHAs(copy1, tscReader)
	shas2 := GetFileChunkSHAs(copy2, tscReader)
	if len(shas1) != 1 || len(shas2) != 1 {
		t.Fatal("expected 1 SHA each")
	}
	if shas1[0] != shas2[0] {
		t.Errorf("chunk SHA mismatch: %x vs %x", shas1[0], shas2[0])
	}

	// Verify the SHA matches direct computation
	expectedSHA := BlobSHA256(sharedContent)
	if shas1[0] != expectedSHA {
		t.Errorf("chunk SHA doesn't match direct computation: %x vs %x", shas1[0], expectedSHA)
	}
}

func TestIndexerLargeFileChunkRefs(t *testing.T) {
	// Test that a file split into multiple chunks has correct refs after TSC sort.
	tmpDir := t.TempDir()
	testDir := filepath.Join(tmpDir, "testroot")
	if err := os.MkdirAll(testDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a large file that will have multiple chunks
	data := make([]byte, BLOB_MAX*3+5000)
	for i := range data {
		data[i] = byte(i % 251) // Use prime to avoid patterns
	}
	if err := os.WriteFile(filepath.Join(testDir, "large.bin"), data, 0644); err != nil {
		t.Fatal(err)
	}

	// Index
	outBase := filepath.Join(tmpDir, "output")
	err := Create(testDir, outBase, IndexerOptions{})
	if err != nil {
		t.Fatalf("Index failed: %v", err)
	}

	// Read back
	tsmReader, err := ReadTSM(outBase + ".tsm")
	if err != nil {
		t.Fatalf("ReadTSM failed: %v", err)
	}
	tscReader, err := ReadTSC(outBase + ".tsc")
	if err != nil {
		t.Fatalf("ReadTSC failed: %v", err)
	}

	// Find the large file
	entry, found := tsmReader.LookupPath("large.bin")
	if !found {
		t.Fatal("large.bin not found")
	}

	if entry.ChunkCount < 4 {
		t.Errorf("expected at least 4 chunks, got %d", entry.ChunkCount)
	}

	// Verify chunk refs point to valid TSC entries
	for i, ref := range entry.ChunkRefs {
		if int(ref) >= len(tscReader.Entries) {
			t.Errorf("chunk ref[%d]=%d out of range (TSC has %d entries)",
				i, ref, len(tscReader.Entries))
		}
	}

	// Get SHAs via ref table and compare with direct chunking
	shas := GetFileChunkSHAs(entry, tscReader)
	var expectedSHAs [][32]byte
	err = ChunkFile(filepath.Join(testDir, "large.bin"), func(sha [32]byte, size uint32, level uint16) error {
		expectedSHAs = append(expectedSHAs, sha)
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("ChunkFile failed: %v", err)
	}

	if len(shas) != len(expectedSHAs) {
		t.Fatalf("chunk count mismatch: ref table=%d, direct=%d", len(shas), len(expectedSHAs))
	}
	for i := range shas {
		if shas[i] != expectedSHAs[i] {
			t.Errorf("chunk[%d] SHA mismatch", i)
		}
	}

	// Also verify total size
	var totalSize uint64
	for _, ref := range entry.ChunkRefs {
		totalSize += uint64(tscReader.Entries[ref].Size)
	}
	if totalSize != uint64(len(data)) {
		t.Errorf("total chunk size %d != file size %d", totalSize, len(data))
	}
}

// TestEmptyDirectoryHashDeterministic verifies that two empty directories with the same
// attributes produce identical TSM hashes. This is important because the TSM indexer
// zeros root entry timestamps, making empty directories produce identical hashes
// regardless of creation time.
func TestEmptyDirectoryHashDeterministic(t *testing.T) {
	tmpDir := t.TempDir()

	// Create two empty directories
	emptyDir1 := filepath.Join(tmpDir, "empty1")
	emptyDir2 := filepath.Join(tmpDir, "empty2")
	if err := os.MkdirAll(emptyDir1, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(emptyDir2, 0755); err != nil {
		t.Fatal(err)
	}

	// Index both
	outPath1 := filepath.Join(tmpDir, "out1")
	outPath2 := filepath.Join(tmpDir, "out2")
	if err := Create(emptyDir1, outPath1, IndexerOptions{}); err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if err := Create(emptyDir2, outPath2, IndexerOptions{}); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Read both TSMs
	tsmReader1, err := ReadTSM(outPath1 + ".tsm")
	if err != nil {
		t.Fatalf("ReadTSM failed: %v", err)
	}
	tsmReader2, err := ReadTSM(outPath2 + ".tsm")
	if err != nil {
		t.Fatalf("ReadTSM failed: %v", err)
	}

	// Verify they have identical hashes
	if tsmReader1.SHA256 != tsmReader2.SHA256 {
		t.Errorf("Two empty directories should have identical hashes:\n  dir1: %x\n  dir2: %x",
			tsmReader1.SHA256, tsmReader2.SHA256)
	}

	// Log the hash for reference
	t.Logf("Empty directory hash (UID=%d): %x", os.Getuid(), tsmReader1.SHA256)
}
