package tsm

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// TestProgressReportsCountsAndBytes verifies that a non-TTY progress consumer
// receives at least one structured progress line reporting the number of files
// indexed, how many were already indexed (reused from the parent), and the
// total bytes indexed.
func TestProgressReportsCountsAndBytes(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), bytesRepeat("alpha\n", 5000))
	mustWrite(t, filepath.Join(root, "b.bin"), bytesRepeat("\x00\x01\x02\x03", 100000))

	out := t.TempDir()
	var buf bytes.Buffer
	idx := NewIndexer(IndexerOptions{
		ProgressWriter: &buf,
		IsTTY:          false,
	})
	if err := idx.Index(root, filepath.Join(out, "snap")); err != nil {
		t.Fatalf("Index: %v", err)
	}

	got := buf.String()
	// The periodic update line and the final summary both report counts.
	if !strings.Contains(got, "files") {
		t.Errorf("progress output missing file count: %q", got)
	}
	if !strings.Contains(got, "already indexed") {
		t.Errorf("progress output missing reused/already-indexed count: %q", got)
	}
	if !strings.Contains(got, "MB") {
		t.Errorf("progress output missing byte total: %q", got)
	}
}
