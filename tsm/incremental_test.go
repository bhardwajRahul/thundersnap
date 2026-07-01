package tsm

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// indexTree indexes rootPath into outBase.{tsm,tsc} using the given parent
// manifests (which may be nil) and returns the indexer's stats plus the
// resulting TSM reader.
func indexTree(t *testing.T, rootPath, outBase string, parentTSM *TSMReader, parentTSC *TSCReader) (IndexerStats, *TSMReader) {
	t.Helper()
	idx := NewIndexer(IndexerOptions{
		ParentTSM: parentTSM,
		ParentTSC: parentTSC,
	})
	if err := idx.Index(rootPath, outBase); err != nil {
		t.Fatalf("Index: %v", err)
	}
	r, err := ReadTSM(outBase + ".tsm")
	if err != nil {
		t.Fatalf("ReadTSM: %v", err)
	}
	return idx.Stats(), r
}

// TestIncrementalReuseUnchangedTree verifies that re-indexing an unchanged
// tree against its own parent manifest reuses every file's chunks (no
// re-hashing) and produces a byte-for-byte identical snapshot ID (TSM SHA).
//
// This is the unit-level guard for the bug "ts snap is suspiciously slow for
// incremental updates": a second consecutive snap of an unchanged tree must
// reuse all chunks from the parent rather than re-reading file content.
func TestIncrementalReuseUnchangedTree(t *testing.T) {
	root := t.TempDir()
	// A few regular files large enough to chunk, a subdir, and a symlink.
	mustWrite(t, filepath.Join(root, "a.txt"), bytesRepeat("alpha\n", 5000))
	mustWrite(t, filepath.Join(root, "b.bin"), bytesRepeat("\x00\x01\x02\x03", 100000))
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "sub", "c.txt"), bytesRepeat("gamma\n", 1000))
	if err := os.Symlink("a.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	// Reuse is keyed on ctime (see indexer.go's reuseParentChunks), and a
	// ctime observed within racyCtimeWindow of an indexing run's start is
	// deliberately treated as unsafe to trust (the "racy git" technique).
	// Sleep past that window before the first index so the ctimes recorded
	// in the parent manifest are trustworthy, letting the second index
	// reuse them immediately rather than needing an extra rehash cycle to
	// escape raciness.
	time.Sleep(1200 * time.Millisecond)

	out := t.TempDir()
	base1 := filepath.Join(out, "snap1")

	// First (full) index — no parent.
	stats1, tsm1 := indexTree(t, root, base1, nil, nil)
	if stats1.ReusedFiles != 0 {
		t.Errorf("first index should reuse nothing, reused=%d", stats1.ReusedFiles)
	}
	if stats1.FileCount == 0 {
		t.Fatal("first index processed no files")
	}

	parentTSM, err := ReadTSM(base1 + ".tsm")
	if err != nil {
		t.Fatalf("ReadTSM parent: %v", err)
	}
	parentTSC, err := ReadTSC(base1 + ".tsc")
	if err != nil {
		t.Fatalf("ReadTSC parent: %v", err)
	}

	// Second index against the parent — the tree is unchanged, so every
	// regular file's chunks must be reused.
	base2 := filepath.Join(out, "snap2")
	stats2, tsm2 := indexTree(t, root, base2, parentTSM, parentTSC)

	// Count regular files (only those carry chunks / can be reused).
	regularFiles := 0
	for i := range parentTSM.Entries {
		if parentTSM.Entries[i].Type == EntryTypeFile {
			regularFiles++
		}
	}
	if stats2.ReusedFiles != regularFiles {
		t.Errorf("second index reused %d files, want all %d regular files re-used",
			stats2.ReusedFiles, regularFiles)
	}

	// The snapshot ID (TSM SHA) must be identical: incremental reuse must not
	// change the content-addressed result.
	if tsm1.SHA256 != tsm2.SHA256 {
		t.Errorf("incremental snap produced a different TSM SHA: %x vs %x",
			tsm1.SHA256, tsm2.SHA256)
	}
}

// TestIncrementalRehashChangedFile verifies that a file whose content (and
// thus size/mtime) changed is NOT reused from the parent and is re-hashed,
// while unchanged siblings are still reused.
func TestIncrementalRehashChangedFile(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "keep.txt"), bytesRepeat("keep\n", 5000))
	mustWrite(t, filepath.Join(root, "change.txt"), bytesRepeat("before\n", 5000))

	// See the comment in TestIncrementalReuseUnchangedTree: sleep past the
	// racy-ctime window before the first index so keep.txt's recorded ctime
	// is trustworthy for the second index's reuse check.
	time.Sleep(1200 * time.Millisecond)

	out := t.TempDir()
	base1 := filepath.Join(out, "snap1")
	if _, tsm1 := indexTree(t, root, base1, nil, nil); tsm1 == nil {
		t.Fatal("nil tsm1")
	}
	parentTSM, err := ReadTSM(base1 + ".tsm")
	if err != nil {
		t.Fatal(err)
	}
	parentTSC, err := ReadTSC(base1 + ".tsc")
	if err != nil {
		t.Fatal(err)
	}

	// Modify one file (different content + size => different mtime as well).
	mustWrite(t, filepath.Join(root, "change.txt"), bytesRepeat("after-the-change\n", 9000))

	base2 := filepath.Join(out, "snap2")
	stats2, _ := indexTree(t, root, base2, parentTSM, parentTSC)

	// Exactly one regular file (keep.txt) should be reused; change.txt must
	// be re-hashed.
	if stats2.ReusedFiles != 1 {
		t.Errorf("expected exactly 1 reused file (keep.txt), got %d", stats2.ReusedFiles)
	}
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func bytesRepeat(s string, n int) []byte {
	b := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		b = append(b, s...)
	}
	return b
}
