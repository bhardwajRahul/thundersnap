//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/tailscale/thundersnap/tsm"
)

// TestSnapshotIncrementalNoReindex is the regression test for the bug "ts snap
// is suspiciously slow for incremental updates": indexing a tree a second time,
// with no changes in between, must NOT re-read and re-hash any file. Instead the
// indexer must reuse every regular file's chunks from the parent snapshot's
// manifest.
//
// This exercises the exact production indexing path that
// thundersnapd's createSnapshotWithTaints uses (tsm.Create with the parent's
// .tsm/.tsc), on a real btrfs subvolume:
//
//  1. Create a btrfs frame from the base snapshot.
//  2. Index it once (snap1) — a full index, since there is no parent manifest.
//  3. Make a read-only btrfs snapshot of the unchanged frame and index THAT
//     against snap1's manifest, exactly as a second consecutive `ts snap`
//     would. Assert that every regular file is reused (zero re-hashed) and the
//     resulting manifest SHA is identical to snap1.
//
// Before the fix the indexer never received a parent manifest, so step 3 reused
// zero files; this test fails (reused=0) without the incremental-indexing
// change.
func TestSnapshotIncrementalNoReindex(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()
	t.Logf("base snapshot: %s", baseSnap)

	// Create a writable frame (btrfs snapshot of the base) to index.
	framePath := filepath.Join(env.fsDir, "testuser", "incrsnap")
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	basePath := filepath.Join(env.snapshotsDir, baseSnap)
	if out, err := exec.Command("btrfs", "subvolume", "snapshot", basePath, framePath).CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot frame: %v\n%s", err, out)
	}

	// Step 2: First (full) index. No parent manifest exists yet.
	snap1Base := filepath.Join(env.snapshotsDir, "incr-snap1")
	idx1 := tsm.NewIndexer(tsm.IndexerOptions{})
	if err := idx1.Index(framePath, snap1Base); err != nil {
		t.Fatalf("first index: %v", err)
	}
	stats1 := idx1.Stats()
	t.Logf("first index: files=%d reused=%d", stats1.FileCount, stats1.ReusedFiles)
	if stats1.ReusedFiles != 0 {
		t.Errorf("first index should reuse nothing, reused=%d", stats1.ReusedFiles)
	}
	if stats1.FileCount == 0 {
		t.Fatal("first index processed no files")
	}

	parentTSM, err := tsm.ReadTSM(snap1Base + ".tsm")
	if err != nil {
		t.Fatalf("read parent tsm: %v", err)
	}
	parentTSC, err := tsm.ReadTSC(snap1Base + ".tsc")
	if err != nil {
		t.Fatalf("read parent tsc: %v", err)
	}

	regularFiles := 0
	for i := range parentTSM.Entries {
		if parentTSM.Entries[i].Type == tsm.EntryTypeFile {
			regularFiles++
		}
	}
	if regularFiles == 0 {
		t.Fatal("parent manifest has no regular files; test cannot verify reuse")
	}

	// Step 3: Take a read-only btrfs snapshot of the UNCHANGED frame (this is
	// what `ts snap` does for atomicity) and index it against snap1's manifest.
	snap2Subvol := filepath.Join(env.snapshotsDir, "incr-snap2-subvol")
	if out, err := exec.Command("btrfs", "subvolume", "snapshot", "-r", framePath, snap2Subvol).CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot for second index: %v\n%s", err, out)
	}

	snap2Base := filepath.Join(env.snapshotsDir, "incr-snap2")
	idx2 := tsm.NewIndexer(tsm.IndexerOptions{
		ParentTSM: parentTSM,
		ParentTSC: parentTSC,
	})
	if err := idx2.Index(snap2Subvol, snap2Base); err != nil {
		t.Fatalf("second (incremental) index: %v", err)
	}
	stats2 := idx2.Stats()
	t.Logf("second index: files=%d reused=%d", stats2.FileCount, stats2.ReusedFiles)

	if stats2.ReusedFiles != regularFiles {
		t.Errorf("second consecutive snap reused %d of %d regular files; expected ALL files reused (no reindexing on a no-op snap)",
			stats2.ReusedFiles, regularFiles)
	}

	// The incremental manifest must be byte-identical (same SHA) to snap1.
	verifyTSM, err := tsm.ReadTSM(snap2Base + ".tsm")
	if err != nil {
		t.Fatalf("read incremental tsm: %v", err)
	}
	if verifyTSM.SHA256 != parentTSM.SHA256 {
		t.Errorf("incremental manifest SHA differs from parent: %x != %x",
			verifyTSM.SHA256, parentTSM.SHA256)
	}
}
