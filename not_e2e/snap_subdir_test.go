//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tailscale/thundersnap/snapsubdir"
	"github.com/tailscale/thundersnap/tsm"
)

// TestSnapshotSubdir is the regression test for "ts snap <path>": snapping a
// subdir of a frame must produce a snapshot whose manifest contains ONLY that
// subtree, re-rooted to the snapshot root, with everything outside it pruned.
//
// It exercises the real production subdir pipeline (snapsubdir.Snapshot, which
// thundersnapd's createSnapshotSubdir uses, followed by tsm.Create) on a real
// btrfs subvolume:
//
//  1. Create a btrfs frame from the base snapshot and lay down a recognizable
//     tree: keep/ (the subtree to snap, with a nested file) and drop/ plus a
//     top-level file that must NOT survive.
//  2. Run snapsubdir.Snapshot(frame, "keep", out) and index the result.
//  3. Assert the manifest contains keep's contents at the root (e.g. "inner.txt")
//     and does NOT contain the pruned "drop" or top-level "toplevel.txt".
func TestSnapshotSubdir(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()
	t.Logf("base snapshot: %s", baseSnap)

	// Writable frame (btrfs snapshot of the base) to populate and snap.
	framePath := filepath.Join(env.fsDir, "testuser", "subdirsnap")
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	basePath := filepath.Join(env.snapshotsDir, baseSnap)
	if out, err := exec.Command("btrfs", "subvolume", "snapshot", basePath, framePath).CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot frame: %v\n%s", err, out)
	}

	// Lay down a recognizable tree.
	keepDir := filepath.Join(framePath, "keep")
	keepSub := filepath.Join(keepDir, "nested")
	dropDir := filepath.Join(framePath, "drop")
	for _, d := range []string{keepSub, dropDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	mustWriteFile(t, filepath.Join(keepDir, "inner.txt"), "keep-me\n")
	mustWriteFile(t, filepath.Join(keepSub, "deep.txt"), "deep-keep\n")
	mustWriteFile(t, filepath.Join(dropDir, "junk.txt"), "drop-me\n")
	mustWriteFile(t, filepath.Join(framePath, "toplevel.txt"), "drop-me-too\n")

	// Run the real subdir reduction pipeline against a fresh destination.
	subSnapPath := filepath.Join(env.snapshotsDir, "subdir-reduced")
	if err := snapsubdir.Snapshot(framePath, "keep", subSnapPath); err != nil {
		t.Fatalf("snapsubdir.Snapshot: %v", err)
	}
	defer exec.Command("btrfs", "subvolume", "delete", subSnapPath).Run()

	// The reduced subvolume must be read-only.
	if out, err := exec.Command("btrfs", "property", "get", subSnapPath, "ro").CombinedOutput(); err != nil {
		t.Fatalf("btrfs property get ro: %v\n%s", err, out)
	} else if !strings.Contains(string(out), "ro=true") {
		t.Errorf("reduced subvolume is not read-only: %q", string(out))
	}

	// Index the reduced subvolume and inspect its manifest.
	outBase := filepath.Join(env.snapshotsDir, "subdir-snap")
	idx := tsm.NewIndexer(tsm.IndexerOptions{})
	if err := idx.Index(subSnapPath, outBase); err != nil {
		t.Fatalf("index reduced subvolume: %v", err)
	}
	r, err := tsm.ReadTSM(outBase + ".tsm")
	if err != nil {
		t.Fatalf("ReadTSM: %v", err)
	}

	paths := map[string]bool{}
	for i := range r.Entries {
		paths[r.Entries[i].Path] = true
	}

	// keep/'s contents must be promoted to the root.
	if !paths["inner.txt"] {
		t.Errorf("subdir snap missing promoted file inner.txt; paths=%v", keys(paths))
	}
	if !paths["nested/deep.txt"] {
		t.Errorf("subdir snap missing promoted nested file nested/deep.txt; paths=%v", keys(paths))
	}
	// The "keep" wrapper directory itself must be gone (contents re-rooted).
	if paths["keep/inner.txt"] || paths["keep"] {
		t.Errorf("subdir snap should re-root keep/ contents, but keep/ survived; paths=%v", keys(paths))
	}
	// Pruned siblings must not appear.
	if paths["toplevel.txt"] {
		t.Errorf("subdir snap leaked pruned top-level file toplevel.txt")
	}
	for p := range paths {
		if p == "drop" || p == "drop/junk.txt" {
			t.Errorf("subdir snap leaked pruned sibling %q", p)
		}
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
