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

// Make sure tsm package constants exist
func init() {
	// Verify the tsm package is importable
	_ = tsm.EntryTypeFile
}
