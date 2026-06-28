//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tailscale/thundersnap/tsm"
)

// TestSnapshotProgressReporting is the regression test for "ts snap needs to
// report progress as it runs": the indexer must emit periodic progress lines
// reporting the total number of files indexed, how many were already indexed
// (reused from the parent .tsm), and the total bytes indexed.
//
// It exercises the real production indexing path (tsm.Create with a
// ProgressWriter, exactly as thundersnapd's createSnapshotWithTaints does on a
// streaming snap) on a real btrfs subvolume:
//
//  1. Index a tree once with a progress writer; assert the progress output
//     reports file count and bytes (and "0 already indexed" since there is no
//     parent).
//  2. Index the same unchanged tree again against the first snapshot's
//     manifest; assert the progress output now reports a non-zero
//     "already indexed" count, proving incremental reuse is surfaced to the
//     user.
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

	// Step 1: first (full) index with progress capture.
	var prog1 bytes.Buffer
	snap1Base := filepath.Join(env.snapshotsDir, "prog-snap1")
	idx1 := tsm.NewIndexer(tsm.IndexerOptions{
		ProgressWriter: &prog1,
		IsTTY:          false,
	})
	if err := idx1.Index(framePath, snap1Base); err != nil {
		t.Fatalf("first index: %v", err)
	}
	out1 := prog1.String()
	t.Logf("first index progress:\n%s", out1)

	// The progress output must include at least one periodic "Indexing:" line
	// (emitted live as the walk proceeds) reporting the running file count,
	// already-indexed count, and bytes.
	if !strings.Contains(out1, "Indexing:") {
		t.Errorf("progress missing periodic Indexing line: %q", out1)
	}
	if !strings.Contains(out1, "files") {
		t.Errorf("progress missing file count: %q", out1)
	}
	if !strings.Contains(out1, "MB") {
		t.Errorf("progress missing byte total: %q", out1)
	}
	if !strings.Contains(out1, "already indexed") {
		t.Errorf("progress missing already-indexed count: %q", out1)
	}
	// On a full index nothing is reused: the final summary reports 0 already
	// indexed.
	if got := alreadyIndexedFromSummary(out1); got != 0 {
		t.Errorf("first index should report 0 already indexed in summary, got %d: %q", got, out1)
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
	// parent manifest. Progress must now report a non-zero already-indexed
	// count.
	snap2Subvol := filepath.Join(env.snapshotsDir, "prog-snap2-subvol")
	if out, err := exec.Command("btrfs", "subvolume", "snapshot", "-r", framePath, snap2Subvol).CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot for second index: %v\n%s", err, out)
	}

	var prog2 bytes.Buffer
	snap2Base := filepath.Join(env.snapshotsDir, "prog-snap2")
	idx2 := tsm.NewIndexer(tsm.IndexerOptions{
		ProgressWriter: &prog2,
		IsTTY:          false,
		ParentTSM:      parentTSM,
		ParentTSC:      parentTSC,
	})
	if err := idx2.Index(snap2Subvol, snap2Base); err != nil {
		t.Fatalf("second index: %v", err)
	}
	out2 := prog2.String()
	t.Logf("second index progress:\n%s", out2)

	stats2 := idx2.Stats()
	if stats2.ReusedFiles == 0 {
		t.Fatalf("second index reused no files; cannot verify already-indexed progress")
	}
	// The summary line must surface the non-zero already-indexed count to the
	// user, matching the indexer's reuse stats.
	if got := alreadyIndexedFromSummary(out2); got != stats2.ReusedFiles {
		t.Errorf("second index progress summary reports %d already indexed, want %d: %q",
			got, stats2.ReusedFiles, out2)
	}
}

// alreadyIndexedFromSummary parses the "Indexed N files (R already indexed), ..."
// summary line and returns R, or -1 if no summary line is present.
func alreadyIndexedFromSummary(progress string) int {
	for _, line := range strings.Split(progress, "\n") {
		if !strings.HasPrefix(line, "Indexed ") {
			continue
		}
		open := strings.Index(line, "(")
		marker := strings.Index(line, " already indexed)")
		if open < 0 || marker < 0 || marker < open {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(line[open+1:marker+1], "%d", &n); err == nil {
			return n
		}
	}
	return -1
}
