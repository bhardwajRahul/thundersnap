// Package refid manages the per-ref identity subvolumes that live under a
// frame's /id directory.
//
// Inside a frame, /id is its own btrfs subvolume that is never captured in a
// snapshot (btrfs excludes nested subvolumes, and the snapshot indexer skips
// across filesystem boundaries). Each subdirectory of /id is itself a btrfs
// subvolume corresponding to one ref that points at this frame, holding that
// ref's private state (keys, tsnet identity, etc.).
//
// When a ref moves from one frame to another, its identity subvolume is moved
// with it: a plain rename across the two frames' /id subvolumes preserves the
// nested subvolume and its contents (both frames live on the same btrfs
// filesystem under the data dir).
package refid

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// idDirName is the per-frame directory holding per-ref identity subvolumes.
const idDirName = "id"

// isSubvolume reports whether path is a btrfs subvolume.
func isSubvolume(path string) bool {
	return exec.Command("btrfs", "subvolume", "show", path).Run() == nil
}

// IDDir returns the path to a frame's /id directory.
func IDDir(framePath string) string {
	return filepath.Join(framePath, idDirName)
}

// Path returns the path to a ref's identity subvolume within a frame, i.e.
// <framePath>/id/<refName>.
func Path(framePath, refName string) string {
	return filepath.Join(IDDir(framePath), refName)
}

// ensureIDSubvol makes sure <framePath>/id exists and is a btrfs subvolume,
// creating it (0700) if necessary. A plain directory left over from a snapshot
// is replaced with a fresh subvolume.
func ensureIDSubvol(framePath string) error {
	idPath := IDDir(framePath)
	if fi, err := os.Stat(idPath); err == nil && fi.IsDir() && !isSubvolume(idPath) {
		if err := os.RemoveAll(idPath); err != nil {
			return fmt.Errorf("remove non-subvolume id dir: %w", err)
		}
	}
	if !isSubvolume(idPath) {
		if out, err := exec.Command("btrfs", "subvolume", "create", idPath).CombinedOutput(); err != nil {
			return fmt.Errorf("create id subvolume %s: %w\n%s", idPath, err, out)
		}
		if err := os.Chmod(idPath, 0700); err != nil {
			return fmt.Errorf("chmod id subvolume: %w", err)
		}
	}
	return nil
}

// Ensure creates the identity subvolume for refName in framePath if it does
// not already exist. It is idempotent: an existing subvolume is left untouched
// (its contents are preserved). The parent /id is created as a subvolume too if
// needed.
func Ensure(framePath, refName string) error {
	if err := ensureIDSubvol(framePath); err != nil {
		return err
	}
	refPath := Path(framePath, refName)
	if isSubvolume(refPath) {
		return nil
	}
	// A leftover plain directory (e.g. from an older layout) is replaced so the
	// ref state is always a real subvolume that snapshots exclude.
	if fi, err := os.Stat(refPath); err == nil && fi.IsDir() {
		if err := os.RemoveAll(refPath); err != nil {
			return fmt.Errorf("remove non-subvolume ref id dir: %w", err)
		}
	}
	if out, err := exec.Command("btrfs", "subvolume", "create", refPath).CombinedOutput(); err != nil {
		return fmt.Errorf("create ref id subvolume %s: %w\n%s", refPath, err, out)
	}
	if err := os.Chmod(refPath, 0700); err != nil {
		return fmt.Errorf("chmod ref id subvolume: %w", err)
	}
	return nil
}

// Move relocates refName's identity subvolume from srcFramePath to
// dstFramePath, preserving its contents. If the source subvolume does not
// exist, Move ensures a fresh empty one at the destination instead (the ref had
// no prior identity state). If the destination already holds a subvolume for
// this ref, it is removed first so the moved one takes its place.
func Move(srcFramePath, dstFramePath, refName string) error {
	if err := ensureIDSubvol(dstFramePath); err != nil {
		return err
	}
	src := Path(srcFramePath, refName)
	dst := Path(dstFramePath, refName)

	if !isSubvolume(src) {
		// Nothing to move; make sure the destination has an identity subvolume.
		return Ensure(dstFramePath, refName)
	}

	// Clear any existing destination subvolume so the rename can land.
	if isSubvolume(dst) {
		if err := Remove(dstFramePath, refName); err != nil {
			return err
		}
	} else if fi, err := os.Stat(dst); err == nil && fi.IsDir() {
		if err := os.RemoveAll(dst); err != nil {
			return fmt.Errorf("remove dst id dir: %w", err)
		}
	}

	// A rename across two parent subvolumes on the same btrfs filesystem keeps
	// the nested subvolume (and its contents) intact.
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("move ref id subvolume %s -> %s: %w", src, dst, err)
	}
	return nil
}

// Remove deletes refName's identity subvolume from framePath, if present.
func Remove(framePath, refName string) error {
	refPath := Path(framePath, refName)
	if !isSubvolume(refPath) {
		// Fall back to removing a plain directory if one exists.
		if err := os.RemoveAll(refPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove ref id dir: %w", err)
		}
		return nil
	}
	if out, err := exec.Command("btrfs", "subvolume", "delete", refPath).CombinedOutput(); err != nil {
		return fmt.Errorf("delete ref id subvolume %s: %w\n%s", refPath, err, out)
	}
	return nil
}
