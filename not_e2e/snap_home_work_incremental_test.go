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
// This test operates directly at the tsm indexing layer (below the daemon's
// frameMeta bookkeeping) to verify the underlying contract that bookkeeping
// depends on: given a *preserved* parent manifest, re-indexing a directory
// whose files are otherwise untouched must reuse their chunks rather than
// re-hashing them. It uses a "persistent" file (never touched, to prove reuse
// works when the parent is preserved) alongside a "transient" file that is
// removed and recreated (to simulate home being temporarily empty).
//
// Note on the transient file: reuse is keyed on ctime (see indexer.go's
// reuseParentChunks), which is kernel-controlled and always changes when a
// file is removed and recreated - even with byte-identical content. So the
// recreated file is *expected* to be re-hashed, not reused; what matters is
// that this is done safely (correct content) and does not affect the
// persistent file's ability to be reused via the preserved parent.
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

	// persistentFile is never touched after this initial write, so its
	// ctime never changes; it's the file that should demonstrate reuse.
	// transientFile simulates content that briefly disappears (home going
	// empty) and comes back with identical bytes but a new ctime.
	persistentFile := filepath.Join(homePath, "persistent.txt")
	transientFile := filepath.Join(homePath, "transient.txt")
	transientContent := []byte("transient content\n")

	if err := os.WriteFile(persistentFile, []byte("persistent content\n"), 0644); err != nil {
		t.Fatalf("write persistent.txt: %v", err)
	}
	if err := os.WriteFile(transientFile, transientContent, 0644); err != nil {
		t.Fatalf("write transient.txt: %v", err)
	}

	// Reuse is keyed on ctime (see indexer.go's reuseParentChunks), and a
	// ctime observed within the racy-ctime window of an indexing run's
	// start is deliberately treated as unsafe to trust (the "racy git"
	// technique - see racyAdjustedCtime). Sleep past that window before the
	// first snap so persistent.txt's recorded ctime is trustworthy and
	// reuse can happen on the very next snap.
	time.Sleep(1200 * time.Millisecond)

	// Step 1: First snap - this creates the initial home snap and writes frameMeta.Home
	snap1Path := filepath.Join(env.snapshotsDir, "incr-hw-snap1")
	idx1 := tsm.NewIndexer(tsm.IndexerOptions{})
	if err := idx1.Index(homePath, snap1Path); err != nil {
		t.Fatalf("first index of home: %v", err)
	}
	stats1 := idx1.Stats()
	t.Logf("first home snap: unmodified=%d, modified=%d", stats1.UnmodifiedEntries, stats1.ModifiedEntries)
	totalEntries1 := stats1.UnmodifiedEntries + stats1.ModifiedEntries
	if totalEntries1 == 0 {
		t.Fatal("first snap found no entries")
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

	// Step 2: Simulate the reported bug scenario - transient content goes
	// away (home would look "empty" of it) and comes back with the same
	// bytes. In the buggy daemon code, any snap of home while it lacked
	// content would erase frameMeta.Home, losing the parent ID needed for
	// incremental indexing on the next real snap.
	if err := os.RemoveAll(transientFile); err != nil {
		t.Fatalf("remove transient.txt: %v", err)
	}
	if err := os.WriteFile(transientFile, transientContent, 0644); err != nil {
		t.Fatalf("re-write transient.txt: %v", err)
	}

	// Step 3: Index home again with snap1 as parent. This simulates what
	// SHOULD happen: even though transient.txt briefly disappeared, the
	// parent ID from the first snap is preserved and passed through for
	// incremental indexing.
	snap3Path := filepath.Join(env.snapshotsDir, "incr-hw-snap3")
	idx3 := tsm.NewIndexer(tsm.IndexerOptions{
		ParentTSM: snap1TSM,
		ParentTSC: snap1TSC,
	})
	if err := idx3.Index(homePath, snap3Path); err != nil {
		t.Fatalf("third index of home: %v", err)
	}
	stats3 := idx3.Stats()
	t.Logf("third home snap (with parent): unmodified=%d, modified=%d", stats3.UnmodifiedEntries, stats3.ModifiedEntries)

	snap3TSM, err := tsm.ReadTSM(snap3Path + ".tsm")
	if err != nil {
		t.Fatalf("read snap3 tsm: %v", err)
	}
	snap3TSC, err := tsm.ReadTSC(snap3Path + ".tsc")
	if err != nil {
		t.Fatalf("read snap3 tsc: %v", err)
	}

	// Check regular-file count from the TSM entries.
	regularFiles := 0
	for i := range snap3TSM.Entries {
		if snap3TSM.Entries[i].Type == tsm.EntryTypeFile {
			regularFiles++
		}
	}
	if regularFiles != 2 {
		t.Fatalf("expected 2 regular files (persistent.txt, transient.txt), got %d", regularFiles)
	}

	// The key assertion for the original bug: persistent.txt was never
	// touched, so with the parent snap ID correctly preserved, it must be
	// marked as unmodified. If the parent had been lost (frameMeta.Home = ""),
	// all entries would be modified instead.
	// Expected: root dir unmodified, persistent.txt unmodified, transient.txt modified (was deleted/recreated)
	if stats3.UnmodifiedEntries != 2 {
		t.Errorf("expected 2 unmodified entries (root dir + persistent.txt), got %d; parent snap ID was likely lost", stats3.UnmodifiedEntries)
	}
	if stats3.ModifiedEntries != 1 {
		t.Errorf("expected 1 modified entry (transient.txt), got %d", stats3.ModifiedEntries)
	}

	persistentEntry, ok := snap3TSM.LookupPath("persistent.txt")
	if !ok {
		t.Fatal("snap3 has no entry for persistent.txt")
	}
	parentPersistentEntry, ok := snap1TSM.LookupPath("persistent.txt")
	if !ok {
		t.Fatal("snap1 has no entry for persistent.txt")
	}
	if len(persistentEntry.ChunkRefs) == 0 || len(persistentEntry.ChunkRefs) != len(parentPersistentEntry.ChunkRefs) {
		t.Fatalf("persistent.txt chunk count mismatch: snap1=%d snap3=%d",
			len(parentPersistentEntry.ChunkRefs), len(persistentEntry.ChunkRefs))
	}
	parentSHAs := tsm.GetFileChunkSHAs(parentPersistentEntry, snap1TSC)
	gotSHAs := tsm.GetFileChunkSHAs(persistentEntry, snap3TSC)
	for i := range parentSHAs {
		if parentSHAs[i] != gotSHAs[i] {
			t.Errorf("persistent.txt chunk %d SHA differs between snap1 and snap3: %x != %x", i, parentSHAs[i], gotSHAs[i])
		}
	}

	// transient.txt was removed and recreated, so its ctime necessarily
	// changed: it must NOT be falsely reused (that would be indistinguishable
	// from silently keeping stale/wrong content), but it must still be
	// correctly re-indexed with the right content.
	transientEntry, ok := snap3TSM.LookupPath("transient.txt")
	if !ok {
		t.Fatal("snap3 has no entry for transient.txt")
	}
	transientSHAs := tsm.GetFileChunkSHAs(transientEntry, snap3TSC)
	wantSHA := tsm.BlobSHA256(transientContent)
	if len(transientSHAs) != 1 || transientSHAs[0] != wantSHA {
		t.Errorf("transient.txt content incorrect after recreation: got chunk SHAs %x, want [%x]", transientSHAs, wantSHA)
	}
}
