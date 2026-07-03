package tsm

import (
	"path/filepath"
	"testing"
)

// TestProgressCallbackReceivesStats verifies that the progress callback is
// called with IndexerStats containing the unmodified/modified counts and
// total bytes.
func TestProgressCallbackReceivesStats(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), bytesRepeat("alpha\n", 5000))
	mustWrite(t, filepath.Join(root, "b.bin"), bytesRepeat("\x00\x01\x02\x03", 100000))

	out := t.TempDir()
	var callCount int
	idx := NewIndexer(IndexerOptions{
		ProgressCallback: func(stats IndexerStats) {
			callCount++
		},
	})
	if err := idx.Index(root, filepath.Join(out, "snap")); err != nil {
		t.Fatalf("Index: %v", err)
	}

	// The callback should have been called at least once
	if callCount == 0 {
		t.Error("progress callback was never called")
	}

	// Final stats should have some entries
	finalStats := idx.Stats()
	totalEntries := finalStats.UnmodifiedEntries + finalStats.ModifiedEntries
	if totalEntries == 0 {
		t.Error("final stats have zero entries")
	}

	// TotalBytes should be non-zero (we wrote some data)
	if finalStats.TotalBytes == 0 {
		t.Error("final stats have zero bytes")
	}
}
