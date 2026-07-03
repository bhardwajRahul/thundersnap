//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/tailscale/thundersnap/tsm"
)

// TestSnapshotIncrementalNoChanges verifies that a second snap of an unchanged
// directory shows 0 modified entries when using the first snap as parent.
// This is the basic contract for incremental indexing: if nothing changed,
// nothing should be re-indexed.
func TestSnapshotIncrementalNoChanges(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()
	t.Logf("base snapshot: %s", baseSnap)

	// Create a frame
	framePath := filepath.Join(env.fsDir, "testuser", "incr-nochange")
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Clone base to frame
	basePath := filepath.Join(env.snapshotsDir, baseSnap)
	if out, err := exec.Command("btrfs", "subvolume", "snapshot", basePath, framePath).CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot frame: %v\n%s", err, out)
	}

	// The base snapshot has /home and /work as regular directories.
	// Convert them to nested btrfs subvolumes.
	homePath := filepath.Join(framePath, "home")
	workPath := filepath.Join(framePath, "work")

	if err := os.RemoveAll(homePath); err != nil {
		t.Fatalf("remove home dir: %v", err)
	}
	if err := os.RemoveAll(workPath); err != nil {
		t.Fatalf("remove work dir: %v", err)
	}

	if out, err := exec.Command("btrfs", "subvolume", "create", homePath).CombinedOutput(); err != nil {
		t.Fatalf("btrfs create home subvol: %v\n%s", err, out)
	}
	if out, err := exec.Command("btrfs", "subvolume", "create", workPath).CombinedOutput(); err != nil {
		t.Fatalf("btrfs create work subvol: %v\n%s", err, out)
	}

	// Write some content to home
	if err := os.WriteFile(filepath.Join(homePath, "file1.txt"), []byte("hello world\n"), 0644); err != nil {
		t.Fatalf("write file1.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "file2.txt"), []byte("goodbye world\n"), 0644); err != nil {
		t.Fatalf("write file2.txt: %v", err)
	}

	// Wait past the racy-ctime window so that ctime comparisons work correctly.
	time.Sleep(1200 * time.Millisecond)

	// First snap - no parent
	snap1Path := filepath.Join(env.snapshotsDir, "incr-nochange-snap1")
	idx1 := tsm.NewIndexer(tsm.IndexerOptions{})
	if err := idx1.Index(homePath, snap1Path); err != nil {
		t.Fatalf("first index: %v", err)
	}
	stats1 := idx1.Stats()
	t.Logf("first snap: unmodified=%d, modified=%d", stats1.UnmodifiedEntries, stats1.ModifiedEntries)

	// Without a parent, everything is "modified" (new)
	if stats1.UnmodifiedEntries != 0 {
		t.Errorf("first snap should have 0 unmodified (no parent), got %d", stats1.UnmodifiedEntries)
	}
	// Should have 3 entries: root dir + 2 files
	if stats1.ModifiedEntries != 3 {
		t.Errorf("first snap should have 3 modified entries (root + 2 files), got %d", stats1.ModifiedEntries)
	}

	// Read snap1's manifests for use as parent
	snap1TSM, err := tsm.ReadTSM(snap1Path + ".tsm")
	if err != nil {
		t.Fatalf("read snap1 tsm: %v", err)
	}
	snap1TSC, err := tsm.ReadTSC(snap1Path + ".tsc")
	if err != nil {
		t.Fatalf("read snap1 tsc: %v", err)
	}

	// Second snap - with snap1 as parent, no changes made
	snap2Path := filepath.Join(env.snapshotsDir, "incr-nochange-snap2")
	idx2 := tsm.NewIndexer(tsm.IndexerOptions{
		ParentTSM: snap1TSM,
		ParentTSC: snap1TSC,
	})
	if err := idx2.Index(homePath, snap2Path); err != nil {
		t.Fatalf("second index: %v", err)
	}
	stats2 := idx2.Stats()
	t.Logf("second snap (with parent, no changes): unmodified=%d, modified=%d", stats2.UnmodifiedEntries, stats2.ModifiedEntries)

	// The key assertion: with no changes, everything should be unmodified
	if stats2.ModifiedEntries != 0 {
		t.Errorf("second snap with no changes should have 0 modified entries, got %d", stats2.ModifiedEntries)
	}
	if stats2.UnmodifiedEntries != 3 {
		t.Errorf("second snap with no changes should have 3 unmodified entries (root + 2 files), got %d", stats2.UnmodifiedEntries)
	}
}
