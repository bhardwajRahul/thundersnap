//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/tailscale/thundersnap/refid"
)

// TestIdRefSubvolumeMoveSequence is the regression test for "the /id workspace
// for refs is not getting initialized as expected". It exercises the real
// production refid package (refid.Ensure / refid.Move, the same calls the
// daemon's ref create/move handlers make) against real btrfs frames and covers
// the whole sequence:
//
//   - Ensure creates a per-ref identity subvolume at <frameA>/id/<ref> (the
//     parent /id is itself a subvolume), with its private state preserved.
//   - A read-only snapshot of frameA excludes the per-ref subvolume's contents
//     (btrfs never captures nested subvolumes), so ref state never leaks into a
//     snap.
//   - Move relocates the per-ref subvolume (and its contents) from frameA to
//     frameB, leaving nothing behind in frameA's /id.
func TestIdRefSubvolumeMoveSequence(t *testing.T) {
	env := newTestEnv(t)

	// Two frames are just two btrfs subvolumes on the same filesystem, matching
	// the on-disk <fs-dir>/<frame>/ layout the daemon uses.
	frameA := filepath.Join(env.fsDir, "frameA")
	frameB := filepath.Join(env.fsDir, "frameB")
	for _, f := range []string{frameA, frameB} {
		if out, err := exec.Command("btrfs", "subvolume", "create", f).CombinedOutput(); err != nil {
			t.Fatalf("btrfs subvolume create %s: %v\n%s", f, err, out)
		}
		defer exec.Command("btrfs", "subvolume", "delete", f).Run()
	}

	const refName = "myref"

	// Step 1: Ensure the ref's identity subvolume in frameA.
	if err := refid.Ensure(frameA, refName); err != nil {
		t.Fatalf("refid.Ensure(frameA): %v", err)
	}

	idDirA := refid.IDDir(frameA)
	refPathA := refid.Path(frameA, refName)

	// /id is a subvolume.
	if err := exec.Command("btrfs", "subvolume", "show", idDirA).Run(); err != nil {
		t.Fatalf("frameA /id should be a btrfs subvolume: %v", err)
	}
	// The per-ref dir is itself a subvolume.
	if err := exec.Command("btrfs", "subvolume", "show", refPathA).Run(); err != nil {
		t.Fatalf("frameA /id/%s should be a btrfs subvolume: %v", refName, err)
	}
	t.Logf("frameA /id and /id/%s are subvolumes", refName)

	// Step 2: Write private state into the per-ref subvolume.
	secretPath := filepath.Join(refPathA, "identity.key")
	secretContent := []byte("ref-private-state-7f3a")
	if err := os.WriteFile(secretPath, secretContent, 0600); err != nil {
		t.Fatalf("write ref identity state: %v", err)
	}

	// Step 3: A read-only snapshot of frameA must exclude the per-ref
	// subvolume's contents (nested subvolumes are never captured).
	snapA := filepath.Join(env.snapshotsDir, "snapA")
	if out, err := exec.Command("btrfs", "subvolume", "snapshot", "-r", frameA, snapA).CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot frameA: %v\n%s", err, out)
	}
	defer exec.Command("btrfs", "subvolume", "delete", snapA).Run()

	// btrfs excludes nested subvolumes from a read-only snapshot: the per-ref
	// subvolume is not recreated, so its private state must not be present. The
	// mount point may be absent entirely or present as an empty stub directory;
	// either way the ref's identity.key must not appear.
	snapRefDir := filepath.Join(snapA, "id", refName)
	if entries, err := os.ReadDir(snapRefDir); err == nil {
		for _, e := range entries {
			t.Logf("unexpected file leaked into snapshot: %s", e.Name())
		}
		if len(entries) != 0 {
			t.Fatalf("snapshot /id/%s should be empty (nested subvol excluded), has %d entries", refName, len(entries))
		}
	} else if !os.IsNotExist(err) {
		t.Fatalf("read snapshot /id/%s: %v", refName, err)
	}
	t.Logf("ref identity state was NOT captured in the snapshot (correct)")

	// Step 4: Move the ref to frameB. Its subvolume and contents must follow.
	if err := refid.Move(frameA, frameB, refName); err != nil {
		t.Fatalf("refid.Move(frameA -> frameB): %v", err)
	}

	refPathB := refid.Path(frameB, refName)

	// The per-ref subvolume now lives in frameB and is still a subvolume.
	if err := exec.Command("btrfs", "subvolume", "show", refPathB).Run(); err != nil {
		t.Fatalf("frameB /id/%s should be a btrfs subvolume after move: %v", refName, err)
	}
	// Its private state moved with it.
	got, err := os.ReadFile(filepath.Join(refPathB, "identity.key"))
	if err != nil {
		t.Fatalf("read moved ref identity state in frameB: %v", err)
	}
	if string(got) != string(secretContent) {
		t.Fatalf("moved ref identity state = %q, want %q", got, secretContent)
	}
	t.Logf("ref identity subvolume + state moved to frameB")

	// Step 5: Nothing is left behind in frameA's /id.
	if _, err := os.Stat(refPathA); !os.IsNotExist(err) {
		t.Fatalf("frameA /id/%s should be gone after move, stat err = %v", refName, err)
	}
	t.Logf("frameA /id/%s removed after move (correct)", refName)
}

// TestIdRefMoveNoPriorSubvolume guards the Move fast-path: when the source ref
// has no identity subvolume yet, Move must NOT attempt an os.Rename (which only
// works when the source is precisely a subvolume root). Instead it ensures a
// fresh empty subvolume at the destination. This pins the invariant documented
// in refid.Move so nobody "simplifies" it into an unconditional rename and
// reintroduces the EXDEV trap for plain directories.
func TestIdRefMoveNoPriorSubvolume(t *testing.T) {
	env := newTestEnv(t)

	frameA := filepath.Join(env.fsDir, "frameA")
	frameB := filepath.Join(env.fsDir, "frameB")
	for _, f := range []string{frameA, frameB} {
		if out, err := exec.Command("btrfs", "subvolume", "create", f).CombinedOutput(); err != nil {
			t.Fatalf("btrfs subvolume create %s: %v\n%s", f, err, out)
		}
		defer exec.Command("btrfs", "subvolume", "delete", f).Run()
	}

	const refName = "newref"

	// No refid.Ensure on frameA: the source /id/<ref> subvolume never exists.
	if err := refid.Move(frameA, frameB, refName); err != nil {
		t.Fatalf("refid.Move with no prior source subvolume: %v", err)
	}

	// The destination gets a fresh, empty identity subvolume.
	refPathB := refid.Path(frameB, refName)
	if err := exec.Command("btrfs", "subvolume", "show", refPathB).Run(); err != nil {
		t.Fatalf("frameB /id/%s should be a fresh subvolume after move: %v", refName, err)
	}
	entries, err := os.ReadDir(refPathB)
	if err != nil {
		t.Fatalf("read frameB /id/%s: %v", refName, err)
	}
	if len(entries) != 0 {
		t.Fatalf("frameB /id/%s should be empty, has %d entries", refName, len(entries))
	}
	t.Logf("Move with no prior source subvolume produced a fresh empty dst subvolume (correct)")
}

// TestIdRefForceDeleteScrubsSubvolume verifies the production refid.Remove call
// the daemon's force ref delete makes: it deletes the per-frame identity
// subvolume (and its private contents) so a force delete never leaves key
// material orphaned on the frame.
func TestIdRefForceDeleteScrubsSubvolume(t *testing.T) {
	env := newTestEnv(t)

	frame := filepath.Join(env.fsDir, "frame")
	if out, err := exec.Command("btrfs", "subvolume", "create", frame).CombinedOutput(); err != nil {
		t.Fatalf("btrfs subvolume create %s: %v\n%s", frame, err, out)
	}
	defer exec.Command("btrfs", "subvolume", "delete", frame).Run()

	const refName = "secretref"
	if err := refid.Ensure(frame, refName); err != nil {
		t.Fatalf("refid.Ensure: %v", err)
	}
	refPath := refid.Path(frame, refName)
	if err := os.WriteFile(filepath.Join(refPath, "identity.key"), []byte("k"), 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	if err := refid.Remove(frame, refName); err != nil {
		t.Fatalf("refid.Remove: %v", err)
	}
	if _, err := os.Stat(refPath); !os.IsNotExist(err) {
		t.Fatalf("identity subvolume should be gone after Remove, stat err = %v", err)
	}
	t.Logf("identity subvolume scrubbed (correct)")
}
