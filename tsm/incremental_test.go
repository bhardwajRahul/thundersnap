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
	if stats1.UnmodifiedEntries != 0 {
		t.Errorf("first index should reuse nothing, unmodified=%d", stats1.UnmodifiedEntries)
	}
	totalEntries := stats1.UnmodifiedEntries + stats1.ModifiedEntries
	if totalEntries == 0 {
		t.Fatal("first index processed no entries")
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

	// All entries should be unmodified on the second pass.
	totalEntries2 := stats2.UnmodifiedEntries + stats2.ModifiedEntries
	if stats2.UnmodifiedEntries != totalEntries2 {
		t.Errorf("second index: unmodified=%d, modified=%d, want all unmodified",
			stats2.UnmodifiedEntries, stats2.ModifiedEntries)
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

	// There are 3 entries total: root dir, keep.txt, change.txt.
	// The root dir and keep.txt should be unmodified; change.txt should be modified.
	if stats2.ModifiedEntries != 1 {
		t.Errorf("expected exactly 1 modified entry (change.txt), got %d", stats2.ModifiedEntries)
	}
}

// TestRacyAdjustedCtime verifies the "racy git" adjustment: a ctime observed
// within racyCtimeWindow of the reference time (the start of the indexing/
// extraction run) is decremented by one nanosecond so that it can never
// match a genuine future observation of the same real ctime value, while a
// ctime safely outside the window is stored unmodified.
func TestRacyAdjustedCtime(t *testing.T) {
	ref := time.Unix(1000, 0)

	// Well outside the window: stored as-is.
	old := ref.Add(-10 * time.Second).UnixNano()
	if got := racyAdjustedCtime(old, ref); got != old {
		t.Errorf("ctime outside racy window: got %d, want unmodified %d", got, old)
	}

	// Exactly at the edge of the window (racyCtimeWindow before ref): the
	// comparison is a strict "<", so this is not racy and should be
	// unmodified.
	edge := ref.Add(-racyCtimeWindow).UnixNano()
	if got := racyAdjustedCtime(edge, ref); got != edge {
		t.Errorf("ctime at racy window edge: got %d, want unmodified %d", got, edge)
	}

	// Inside the window (including exactly at ref, i.e. zero delta): stored
	// decremented by one nanosecond.
	for _, delta := range []time.Duration{0, 1 * time.Millisecond, racyCtimeWindow - 1} {
		racy := ref.Add(-delta).UnixNano()
		want := racy - 1
		if got := racyAdjustedCtime(racy, ref); got != want {
			t.Errorf("ctime %v inside racy window: got %d, want decremented %d", delta, got, want)
		}
	}
}

// TestReuseParentChunksRejectsRacyCollision is a white-box regression test
// for the scenario racyAdjustedCtime exists to guard against: two distinct
// writes to the same path, at the same size, whose *real* ctimes are
// identical due to coarse filesystem/clock timestamp resolution (a "racy"
// collision). If reuse were keyed on the raw ctime value, the second write's
// content would be indistinguishable from the first and would be wrongly
// reused.
//
// This is tested deterministically (without depending on actual filesystem
// timing) by directly constructing a parent manifest whose stored ctime is
// the racy-adjusted value (realCtime-1, as racyAdjustedCtime would have
// produced when that entry was originally indexed within the racy window),
// and calling the unexported reuseParentChunks with a fresh entry carrying
// the colliding real ctime and identical size. The mismatch (realCtime-1 !=
// realCtime) must force a rehash rather than a false reuse.
func TestReuseParentChunksRejectsRacyCollision(t *testing.T) {
	const path = "colliding.txt"
	const size = uint64(4096)
	const realCtime = int64(1_700_000_000_000_000_000) // arbitrary Unix nanos

	parentTSC := &TSCReader{
		Entries: []TSCEntry{
			{SHA256: BlobSHA256([]byte("first content")), Size: 32},
		},
	}
	parentTSM := &TSMReader{
		Entries: []TSMEntry{
			{
				Path:       path,
				Type:       EntryTypeFile,
				Size:       size,
				Ctime:      realCtime - 1, // as racyAdjustedCtime would have stored
				ChunkStart: 0,
				ChunkCount: 1,
				ChunkRefs:  []uint32{0},
			},
		},
	}

	idx := NewIndexer(IndexerOptions{
		ParentTSM: parentTSM,
		ParentTSC: parentTSC,
	})

	// Fresh entry: same path and size, but the real (colliding) ctime -
	// simulating a second, different write that the coarse clock stamped
	// with the identical timestamp as the first.
	entry := &TSMEntry{
		Path:  path,
		Type:  EntryTypeFile,
		Size:  size,
		Ctime: realCtime,
	}

	if _, ok := idx.reuseParentChunks(entry); ok {
		t.Fatal("reuseParentChunks falsely reused chunks across a racy ctime collision")
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
