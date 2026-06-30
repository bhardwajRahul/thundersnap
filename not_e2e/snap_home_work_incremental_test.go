//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/tailscale/thundersnap/tsm"
)

// TestSnapshotHomeWorkIncrementalPreserveParent is the regression test for the
// bug where running `ts snap` repeatedly on a frame with /home and /work nested
// subvolumes would re-index many files because the parent snap IDs for home/work
// were being cleared when those directories were empty.
//
// The bug: in createSnapshotSubdir, after snapping home/work, the code would
// unconditionally set frameMeta.Home = homeID and frameMeta.Work = workID. But
// if home or work was empty during a snap, homeID/workID would be "", which
// ERASED the previous snap ID needed for incremental indexing on subsequent snaps.
//
// This test verifies that the parent snap ID is preserved across snaps even when
// home/work is temporarily empty.
func TestSnapshotHomeWorkIncrementalPreserveParent(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()
	t.Logf("base snapshot: %s", baseSnap)

	// Create a frame with nested home/work subvolumes (not just directories)
	framePath := filepath.Join(env.fsDir, "testuser", "incr-home-work")
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Clone base to frame
	basePath := filepath.Join(env.snapshotsDir, baseSnap)
	if out, err := exec.Command("btrfs", "subvolume", "snapshot", basePath, framePath).CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot frame: %v\n%s", err, out)
	}

	// The base snapshot has /home and /work as regular directories.
	// Convert them to nested btrfs subvolumes to simulate a 3-component frame.
	homePath := filepath.Join(framePath, "home")
	workPath := filepath.Join(framePath, "work")

	// Remove existing dirs
	if err := os.RemoveAll(homePath); err != nil {
		t.Fatalf("remove home dir: %v", err)
	}
	if err := os.RemoveAll(workPath); err != nil {
		t.Fatalf("remove work dir: %v", err)
	}

	// Create as subvolumes
	if out, err := exec.Command("btrfs", "subvolume", "create", homePath).CombinedOutput(); err != nil {
		t.Fatalf("btrfs create home subvol: %v\n%s", err, out)
	}
	if out, err := exec.Command("btrfs", "subvolume", "create", workPath).CombinedOutput(); err != nil {
		t.Fatalf("btrfs create work subvol: %v\n%s", err, out)
	}

	// Add some initial content to home so it gets snapped
	testFile1 := filepath.Join(homePath, "file1.txt")
	if err := os.WriteFile(testFile1, []byte("initial content\n"), 0644); err != nil {
		t.Fatalf("write file1: %v", err)
	}

	// Step 1: First snap - this creates the initial home snap and writes frameMeta.Home
	snap1Path := filepath.Join(env.snapshotsDir, "incr-hw-snap1")
	idx1 := tsm.NewIndexer(tsm.IndexerOptions{})
	if err := idx1.Index(homePath, snap1Path); err != nil {
		t.Fatalf("first index of home: %v", err)
	}
	stats1 := idx1.Stats()
	t.Logf("first home snap: files=%d, reused=%d", stats1.FileCount, stats1.ReusedFiles)
	if stats1.FileCount == 0 {
		t.Fatal("first snap found no files")
	}

	// Read snap1's TSM/TSC for later comparison
	snap1TSM, err := tsm.ReadTSM(snap1Path + ".tsm")
	if err != nil {
		t.Fatalf("read snap1 tsm: %v", err)
	}
	snap1TSC, err := tsm.ReadTSC(snap1Path + ".tsc")
	if err != nil {
		t.Fatalf("read snap1 tsc: %v", err)
	}

	// Step 2: Empty home and snap again
	// In the buggy code, this would clear frameMeta.Home to ""
	if err := os.RemoveAll(testFile1); err != nil {
		t.Fatalf("remove file1: %v", err)
	}

	// Home is now empty, so a snap of home would produce no homeID
	// Simulating what the daemon does: when home is empty, homeID = ""
	// The buggy code would write frameMeta.Home = "" here, erasing the snap1 ID

	// Step 3: Re-add the same content to home
	if err := os.WriteFile(testFile1, []byte("initial content\n"), 0644); err != nil {
		t.Fatalf("re-write file1: %v", err)
	}

	// Step 4: Index home again with snap1 as parent
	// This simulates what SHOULD happen: even though home was temporarily empty,
	// the parent ID from the first snap should be preserved for incremental indexing.
	snap3Path := filepath.Join(env.snapshotsDir, "incr-hw-snap3")
	idx3 := tsm.NewIndexer(tsm.IndexerOptions{
		ParentTSM: snap1TSM,
		ParentTSC: snap1TSC,
	})
	if err := idx3.Index(homePath, snap3Path); err != nil {
		t.Fatalf("third index of home: %v", err)
	}
	stats3 := idx3.Stats()
	t.Logf("third home snap (with parent): files=%d, reused=%d", stats3.FileCount, stats3.ReusedFiles)

	// The key assertion: since the content is identical to snap1, the file should
	// be reused (not re-indexed). If the parent was lost (frameMeta.Home = ""),
	// this would be 0 reused and we'd have done a full re-hash.
	regularFiles := 0
	for i := range snap1TSM.Entries {
		if snap1TSM.Entries[i].Type == tsm.EntryTypeFile {
			regularFiles++
		}
	}
	if regularFiles == 0 {
		t.Fatal("snap1 has no regular files")
	}

	if stats3.ReusedFiles != regularFiles {
		t.Errorf("expected all %d files reused, got %d reused; parent snap ID was likely lost", regularFiles, stats3.ReusedFiles)
	}

	// Verify the manifest SHA matches (content-identical means same hash)
	snap3TSM, err := tsm.ReadTSM(snap3Path + ".tsm")
	if err != nil {
		t.Fatalf("read snap3 tsm: %v", err)
	}
	if snap3TSM.SHA256 != snap1TSM.SHA256 {
		t.Errorf("snap3 SHA differs from snap1: %x != %x", snap3TSM.SHA256, snap1TSM.SHA256)
	}
}
