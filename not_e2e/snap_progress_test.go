// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

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

// TestSnapshotProgressReporting is the regression test for "ts snap needs to
// report progress as it runs": the indexer must call the progress callback
// with stats during indexing, and the second snap must show entries as
// unmodified (not re-hashed).
//
// It exercises the real production indexing path (tsm.Create with a
// ProgressCallback) on a real btrfs subvolume:
//
//  1. Index a tree once with progress tracking; assert we get callback calls.
//  2. Index the same unchanged tree again against the first snapshot's
//     manifest; assert all entries are unmodified.
func TestSnapshotProgressReporting(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()
	t.Logf("base snapshot: %s", baseSnap)

	// Writable frame (btrfs snapshot of the base) to index.
	framePath := filepath.Join(env.fsDir, "testuser", "progsnap")
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	basePath := filepath.Join(env.snapshotsDir, baseSnap)
	if out, err := exec.Command("btrfs", "subvolume", "snapshot", basePath, framePath).CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot frame: %v\n%s", err, out)
	}

	// Reuse is keyed on ctime (see indexer.go's reuseParentChunks), and a
	// ctime observed within the racy-ctime window of an indexing run's start
	// is deliberately treated as unsafe to trust (the "racy git" technique -
	// see racyAdjustedCtime). The frame's files were just written moments
	// ago, so sleep past that window before the first index so their
	// recorded ctimes are trustworthy and reuse can happen on the very next
	// index.
	time.Sleep(1200 * time.Millisecond)

	// Step 1: first (full) index with progress tracking.
	var callCount1 int
	var lastStats1 tsm.IndexerStats
	snap1Base := filepath.Join(env.snapshotsDir, "prog-snap1")
	idx1 := tsm.NewIndexer(tsm.IndexerOptions{
		ProgressCallback: func(stats tsm.IndexerStats) {
			callCount1++
			lastStats1 = stats
		},
	})
	if err := idx1.Index(framePath, snap1Base); err != nil {
		t.Fatalf("first index: %v", err)
	}
	stats1 := idx1.Stats()
	t.Logf("first index: callback called %d times, final stats: unmodified=%d modified=%d bytes=%d",
		callCount1, stats1.UnmodifiedEntries, stats1.ModifiedEntries, stats1.TotalBytes)

	// Progress callback should be called at least once
	if callCount1 == 0 {
		t.Error("first index: progress callback never called")
	}

	// On first index, everything should be modified (no parent)
	if stats1.UnmodifiedEntries != 0 {
		t.Errorf("first index should have 0 unmodified entries, got %d", stats1.UnmodifiedEntries)
	}

	parentTSM, err := tsm.ReadTSM(snap1Base + ".tsm")
	if err != nil {
		t.Fatalf("read parent tsm: %v", err)
	}
	parentTSC, err := tsm.ReadTSC(snap1Base + ".tsc")
	if err != nil {
		t.Fatalf("read parent tsc: %v", err)
	}

	// Step 2: read-only snapshot of the unchanged frame, indexed against the
	// parent manifest. All entries should now be unmodified.
	snap2Subvol := filepath.Join(env.snapshotsDir, "prog-snap2-subvol")
	if out, err := exec.Command("btrfs", "subvolume", "snapshot", "-r", framePath, snap2Subvol).CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot for second index: %v\n%s", err, out)
	}

	var callCount2 int
	snap2Base := filepath.Join(env.snapshotsDir, "prog-snap2")
	idx2 := tsm.NewIndexer(tsm.IndexerOptions{
		ProgressCallback: func(stats tsm.IndexerStats) {
			callCount2++
		},
		ParentTSM: parentTSM,
		ParentTSC: parentTSC,
	})
	if err := idx2.Index(snap2Subvol, snap2Base); err != nil {
		t.Fatalf("second index: %v", err)
	}

	stats2 := idx2.Stats()
	t.Logf("second index: callback called %d times, final stats: unmodified=%d modified=%d bytes=%d",
		callCount2, stats2.UnmodifiedEntries, stats2.ModifiedEntries, stats2.TotalBytes)

	// On second index with unchanged tree, everything should be unmodified
	if stats2.UnmodifiedEntries == 0 {
		t.Fatal("second index: no entries marked as unmodified; incremental reuse not working")
	}

	// No entries should be modified (tree is unchanged)
	if stats2.ModifiedEntries != 0 {
		t.Errorf("second index: %d entries modified, expected 0 for unchanged tree",
			stats2.ModifiedEntries)
	}
}
