// Package e2e contains end-to-end tests for thundersnap TSM/TSC format.
package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tailscale/thundersnap/tsm"
)

// TestTSMFormatBasic tests that snapshot creation produces valid TSM/TSC files:
// 1. Create snapshot
// 2. Verify .tsm and .tsc files exist
// 3. Verify TSM has valid structure (can be parsed)
// 4. Verify TSC has valid structure
func TestTSMFormatBasic(t *testing.T) {
	env := newTestEnv(t)

	// Create a base snapshot
	baseSnap := env.createBaseSnapshot()
	t.Logf("Created base snapshot: %s", baseSnap)

	snapPath := filepath.Join(env.snapshotsDir, baseSnap)

	// Generate TSM/TSC files for the snapshot
	// In the real system, thundersnapd does this during createSnapshot.
	// For this test, we'll use the tsm package directly.
	tsmPath := snapPath + ".tsm"
	tscPath := snapPath + ".tsc"

	// Create the TSM/TSC files using the tsm package
	err := generateTSMFiles(t, snapPath, tsmPath, tscPath)
	if err != nil {
		t.Fatalf("generate TSM files: %v", err)
	}

	// Step 1: Verify .tsm file exists
	tsmInfo, err := os.Stat(tsmPath)
	if err != nil {
		t.Fatalf(".tsm file not found: %v", err)
	}
	t.Logf(".tsm file exists: %d bytes", tsmInfo.Size())

	// Step 2: Verify .tsc file exists
	tscInfo, err := os.Stat(tscPath)
	if err != nil {
		t.Fatalf(".tsc file not found: %v", err)
	}
	t.Logf(".tsc file exists: %d bytes", tscInfo.Size())

	// Step 3: Verify TSM can be parsed
	tsmReader, err := tsm.ReadTSM(tsmPath)
	if err != nil {
		t.Fatalf("open TSM file: %v", err)
	}

	t.Logf("TSM header: files=%d totalSize=%d", tsmReader.Header.FileCount, tsmReader.Header.TotalSize)

	if tsmReader.Header.FileCount == 0 {
		t.Error("TSM has no files")
	}
	if tsmReader.Header.TotalSize == 0 {
		t.Error("TSM totalSize is 0")
	}

	// Iterate through some entries to verify structure
	entryCount := len(tsmReader.Entries)
	for i, entry := range tsmReader.Entries {
		if i < 5 {
			t.Logf("  Entry %d: path=%s mode=%o size=%d", i+1, entry.Path, entry.Mode, entry.Size)
		}
	}
	t.Logf("TSM contains %d entries", entryCount)

	// Step 4: Verify TSC structure
	// TSC is a sorted list of chunks - we just verify it's not empty
	// and has reasonable size (should have content for the test files)
	if tscInfo.Size() < 64 {
		t.Errorf(".tsc file too small: %d bytes", tscInfo.Size())
	}

	// Verify TSC can be opened
	tscReader, err := tsm.ReadTSC(tscPath)
	if err != nil {
		t.Fatalf("open TSC file: %v", err)
	}

	chunkCount := tscReader.Header.ChunkCount
	t.Logf("TSC contains %d chunks", chunkCount)

	if chunkCount == 0 {
		t.Error("TSC has no chunks")
	}
}

// generateTSMFiles creates TSM/TSC files for a snapshot directory.
// This mirrors what thundersnapd does during createSnapshot.
func generateTSMFiles(t *testing.T, snapPath, tsmPath, tscPath string) error {
	t.Helper()

	// The tsm.Create function takes rootPath and output base path
	// It generates both .tsm and .tsc files
	outBase := tsmPath[:len(tsmPath)-4] // Strip .tsm extension
	err := tsm.Create(snapPath, outBase, tsm.IndexerOptions{})
	if err != nil {
		return err
	}
	t.Logf("Generated TSM/TSC for snapshot")
	return nil
}

// TestTSMMetadataCorrectness verifies TSM contains correct file metadata.
func TestTSMMetadataCorrectness(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()
	snapPath := filepath.Join(env.snapshotsDir, baseSnap)
	tsmPath := snapPath + ".tsm"
	tscPath := snapPath + ".tsc"

	err := generateTSMFiles(t, snapPath, tsmPath, tscPath)
	if err != nil {
		t.Fatalf("generate TSM files: %v", err)
	}

	tsmReader, err := tsm.ReadTSM(tsmPath)
	if err != nil {
		t.Fatalf("open TSM: %v", err)
	}

	// Find specific entries and verify their metadata
	expectedEntries := map[string]struct {
		isDir bool
		mode  uint32 // just the permission bits
		uid   uint32
		gid   uint32
	}{
		"etc/passwd": {isDir: false, mode: 0644, uid: 0, gid: 0},
		"home/user":  {isDir: true, mode: 0755, uid: 1000, gid: 1000},
		"tmp":        {isDir: true, mode: 0777, uid: 0, gid: 0}, // sticky bit checked separately
	}

	found := make(map[string]bool)
	for _, entry := range tsmReader.Entries {
		expected, ok := expectedEntries[entry.Path]
		if !ok {
			continue
		}
		found[entry.Path] = true

		// Check if directory
		isDir := entry.Type == tsm.EntryTypeDir
		if isDir != expected.isDir {
			t.Errorf("%s: isDir=%v, want %v", entry.Path, isDir, expected.isDir)
		}

		// Check mode (lower 9 bits)
		gotMode := entry.Mode & 0777
		if gotMode != expected.mode {
			t.Errorf("%s: mode=%o, want %o", entry.Path, gotMode, expected.mode)
		}

		// Check UID/GID
		if entry.UID != expected.uid {
			t.Errorf("%s: uid=%d, want %d", entry.Path, entry.UID, expected.uid)
		}
		if entry.GID != expected.gid {
			t.Errorf("%s: gid=%d, want %d", entry.Path, entry.GID, expected.gid)
		}

		t.Logf("%s: verified (mode=%o uid=%d gid=%d)", entry.Path, gotMode, entry.UID, entry.GID)
	}

	// Verify all expected entries were found
	for path := range expectedEntries {
		if !found[path] {
			t.Errorf("entry not found in TSM: %s", path)
		}
	}
}

// TestTSMSymlinkStorage verifies symlinks are stored correctly.
func TestTSMSymlinkStorage(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()
	snapPath := filepath.Join(env.snapshotsDir, baseSnap)
	tsmPath := snapPath + ".tsm"
	tscPath := snapPath + ".tsc"

	err := generateTSMFiles(t, snapPath, tsmPath, tscPath)
	if err != nil {
		t.Fatalf("generate TSM files: %v", err)
	}

	tsmReader, err := tsm.ReadTSM(tsmPath)
	if err != nil {
		t.Fatalf("open TSM: %v", err)
	}

	// Look for the lib64 -> lib symlink from fixtures
	foundSymlink := false
	for _, entry := range tsmReader.Entries {
		if entry.Path == "lib64" {
			foundSymlink = true
			if entry.Type != tsm.EntryTypeSymlink {
				t.Errorf("lib64 not marked as symlink, type=%d", entry.Type)
			}
			if entry.LinkTarget != "lib" {
				t.Errorf("lib64 target=%q, want 'lib'", entry.LinkTarget)
			}
			t.Logf("Symlink verified: lib64 -> %s", entry.LinkTarget)
			break
		}
	}

	if !foundSymlink {
		t.Error("lib64 symlink not found in TSM")
	}
}

// TestTSMDeviceNodeStorage verifies device nodes are stored correctly.
func TestTSMDeviceNodeStorage(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()
	snapPath := filepath.Join(env.snapshotsDir, baseSnap)
	tsmPath := snapPath + ".tsm"
	tscPath := snapPath + ".tsc"

	err := generateTSMFiles(t, snapPath, tsmPath, tscPath)
	if err != nil {
		t.Fatalf("generate TSM files: %v", err)
	}

	tsmReader, err := tsm.ReadTSM(tsmPath)
	if err != nil {
		t.Fatalf("open TSM: %v", err)
	}

	// Look for dev/null (char device 1,3)
	foundNull := false
	for _, entry := range tsmReader.Entries {
		if entry.Path == "dev/null" {
			foundNull = true
			if entry.Type != tsm.EntryTypeCharDev {
				t.Errorf("dev/null not marked as char device, type=%d", entry.Type)
			}
			if entry.DevMajor != 1 || entry.DevMinor != 3 {
				t.Errorf("dev/null device=%d:%d, want 1:3", entry.DevMajor, entry.DevMinor)
			}
			t.Logf("Device node verified: dev/null (char %d:%d)", entry.DevMajor, entry.DevMinor)
			break
		}
	}

	if !foundNull {
		t.Error("dev/null not found in TSM")
	}
}

// TestTSCSortedSHA256 verifies TSC chunks are sorted by SHA-256 (binary searchable).
func TestTSCSortedSHA256(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()
	snapPath := filepath.Join(env.snapshotsDir, baseSnap)
	tsmPath := snapPath + ".tsm"
	tscPath := snapPath + ".tsc"

	err := generateTSMFiles(t, snapPath, tsmPath, tscPath)
	if err != nil {
		t.Fatalf("generate TSM files: %v", err)
	}

	tscReader, err := tsm.ReadTSC(tscPath)
	if err != nil {
		t.Fatalf("open TSC: %v", err)
	}

	if len(tscReader.Entries) < 2 {
		t.Skip("not enough chunks to verify sorting")
	}

	// Verify each entry's SHA is >= the previous
	for i := 1; i < len(tscReader.Entries); i++ {
		prev := tscReader.Entries[i-1].SHA256[:]
		curr := tscReader.Entries[i].SHA256[:]

		cmp := 0
		for j := 0; j < 32; j++ {
			if prev[j] < curr[j] {
				cmp = -1
				break
			} else if prev[j] > curr[j] {
				cmp = 1
				break
			}
		}

		if cmp > 0 {
			t.Errorf("TSC not sorted at index %d: %x > %x", i, prev, curr)
		}
	}

	t.Logf("Verified %d TSC chunks are sorted by SHA-256", len(tscReader.Entries))
}

// TestTSCDeduplication verifies same content across files = single chunk entry.
func TestTSCDeduplication(t *testing.T) {
	env := newTestEnv(t)

	// Create a snapshot with duplicate content
	baseSnap := env.createBaseSnapshot()
	snapPath := filepath.Join(env.snapshotsDir, baseSnap)

	// The base snapshot has bin/sh and bin/ts which are the same file (both busybox)
	// Let's create additional duplicate files to be sure
	dupContent := []byte("This is duplicate content for testing TSC deduplication.\n")
	dup1Path := filepath.Join(snapPath, "home", "user", "dup1.txt")
	dup2Path := filepath.Join(snapPath, "home", "user", "dup2.txt")
	dup3Path := filepath.Join(snapPath, "home", "user", "dup3.txt")

	for _, p := range []string{dup1Path, dup2Path, dup3Path} {
		if err := os.WriteFile(p, dupContent, 0644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	tsmPath := snapPath + ".tsm"
	tscPath := snapPath + ".tsc"

	err := generateTSMFiles(t, snapPath, tsmPath, tscPath)
	if err != nil {
		t.Fatalf("generate TSM files: %v", err)
	}

	tsmReader, err := tsm.ReadTSM(tsmPath)
	if err != nil {
		t.Fatalf("open TSM: %v", err)
	}

	tscReader, err := tsm.ReadTSC(tscPath)
	if err != nil {
		t.Fatalf("open TSC: %v", err)
	}

	// Find the dup files and verify they reference the same TSC chunk
	var dupEntries []*tsm.TSMEntry
	for i := range tsmReader.Entries {
		e := &tsmReader.Entries[i]
		if e.Path == "home/user/dup1.txt" || e.Path == "home/user/dup2.txt" || e.Path == "home/user/dup3.txt" {
			dupEntries = append(dupEntries, e)
		}
	}

	if len(dupEntries) != 3 {
		t.Fatalf("expected 3 dup entries, found %d", len(dupEntries))
	}

	// All three files should reference the same chunk(s)
	if dupEntries[0].ChunkCount == 0 {
		t.Fatal("dup1 has no chunks")
	}

	// Get the chunk SHA for each file
	chunkSHAs := make(map[string][][32]byte)
	for _, e := range dupEntries {
		shas := tsm.GetFileChunkSHAs(e, tscReader)
		chunkSHAs[e.Path] = shas
		t.Logf("%s: %d chunks", e.Path, len(shas))
	}

	// Verify all files have the same chunk SHA(s)
	ref := chunkSHAs["home/user/dup1.txt"]
	for path, shas := range chunkSHAs {
		if len(shas) != len(ref) {
			t.Errorf("%s: chunk count %d != ref %d", path, len(shas), len(ref))
			continue
		}
		for i := range shas {
			if shas[i] != ref[i] {
				t.Errorf("%s: chunk %d SHA mismatch", path, i)
			}
		}
	}

	t.Logf("Verified deduplication: 3 identical files share the same %d chunk(s)", len(ref))
}

// TestTSMChunkReferenceResolution verifies TSM chunk refs resolve to valid TSC entries.
func TestTSMChunkReferenceResolution(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()
	snapPath := filepath.Join(env.snapshotsDir, baseSnap)
	tsmPath := snapPath + ".tsm"
	tscPath := snapPath + ".tsc"

	err := generateTSMFiles(t, snapPath, tsmPath, tscPath)
	if err != nil {
		t.Fatalf("generate TSM files: %v", err)
	}

	tsmReader, err := tsm.ReadTSM(tsmPath)
	if err != nil {
		t.Fatalf("open TSM: %v", err)
	}

	tscReader, err := tsm.ReadTSC(tscPath)
	if err != nil {
		t.Fatalf("open TSC: %v", err)
	}

	// Find a file with chunks (bin/sh is large)
	var fileEntry *tsm.TSMEntry
	for i := range tsmReader.Entries {
		e := &tsmReader.Entries[i]
		if e.Type == tsm.EntryTypeFile && e.ChunkCount > 0 {
			fileEntry = e
			break
		}
	}

	if fileEntry == nil {
		t.Fatal("no file with chunks found")
	}

	t.Logf("Testing chunk resolution for %s (%d chunks)", fileEntry.Path, fileEntry.ChunkCount)

	// Verify each chunk reference is valid
	totalChunkSize := uint64(0)
	for i, tscIdx := range fileEntry.ChunkRefs {
		if int(tscIdx) >= len(tscReader.Entries) {
			t.Errorf("chunk ref %d: index %d out of range (TSC has %d entries)",
				i, tscIdx, len(tscReader.Entries))
			continue
		}

		chunk := tscReader.Entries[tscIdx]
		if chunk.Size == 0 {
			t.Errorf("chunk ref %d: zero size", i)
		}

		totalChunkSize += uint64(chunk.Size)

		if i < 3 {
			t.Logf("  Chunk %d: TSC[%d] SHA=%x... size=%d",
				i, tscIdx, chunk.SHA256[:8], chunk.Size)
		}
	}

	// The total chunk size should approximately equal the file size
	// (may be slightly larger due to chunking overhead)
	if totalChunkSize < fileEntry.Size {
		t.Errorf("total chunk size %d < file size %d", totalChunkSize, fileEntry.Size)
	}

	t.Logf("Verified %d chunk refs resolve correctly (total chunk size: %d, file size: %d)",
		len(fileEntry.ChunkRefs), totalChunkSize, fileEntry.Size)
}

// Make sure tsm package constants exist
func init() {
	// Verify the tsm package is importable
	_ = tsm.EntryTypeFile
}
