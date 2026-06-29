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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tailscale/thundersnap/btrfsutil"
)

// idDirName is the per-frame directory holding per-ref identity subvolumes.
const idDirName = "id"

// ErrInvalidRefName is returned when a ref name cannot be used as a single
// path component under a frame's /id directory.
var ErrInvalidRefName = errors.New("invalid ref name")

// validateRefName rejects ref names that would escape or not resolve to a
// single child of the /id directory. Callers (the daemon) already validate ref
// names via refs.ValidateName, but refid is an importable package operating on
// real filesystem paths, so it guards itself: "", ".", "..", and any name
// containing a path separator (e.g. "../escape" or "a/b") are rejected.
func validateRefName(refName string) error {
	if refName == "" || refName == "." || refName == ".." {
		return fmt.Errorf("%w: %q", ErrInvalidRefName, refName)
	}
	if strings.ContainsRune(refName, filepath.Separator) || strings.ContainsRune(refName, '/') {
		return fmt.Errorf("%w: %q contains a path separator", ErrInvalidRefName, refName)
	}
	return nil
}

// createSubvol creates a btrfs subvolume at path with 0700 permissions. The
// 0700 chmod is what distinguishes it from btrfsutil.CreateSubvol: identity
// subvolumes hold per-ref private state and must not be world-readable.
func createSubvol(path string) error {
	if err := btrfsutil.CreateSubvol(path); err != nil {
		return err
	}
	if err := os.Chmod(path, 0700); err != nil {
		return fmt.Errorf("chmod subvolume %s: %w", path, err)
	}
	return nil
}

// removeIfPlainDir removes path if it exists as a plain (non-subvolume)
// directory. Such leftovers can appear because a read-only btrfs snapshot does
// not recreate nested subvolumes, leaving an empty or plain directory in their
// place. A path that is already a subvolume, or does not exist, is left alone.
func removeIfPlainDir(path string) error {
	if btrfsutil.IsSubvolume(path) {
		return nil
	}
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove non-subvolume dir %s: %w", path, err)
		}
	}
	return nil
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
	if btrfsutil.IsSubvolume(idPath) {
		return nil
	}
	// Not (yet) a subvolume: drop any leftover plain directory, then create it.
	if err := removeIfPlainDir(idPath); err != nil {
		return err
	}
	return createSubvol(idPath)
}

// Ensure creates the identity subvolume for refName in framePath if it does
// not already exist. It is idempotent: an existing subvolume is left untouched
// (its contents are preserved). The parent /id is created as a subvolume too if
// needed.
func Ensure(framePath, refName string) error {
	if err := validateRefName(refName); err != nil {
		return err
	}
	if err := ensureIDSubvol(framePath); err != nil {
		return err
	}
	refPath := Path(framePath, refName)
	if btrfsutil.IsSubvolume(refPath) {
		return nil
	}
	// A leftover plain directory (e.g. from an older layout) is replaced so the
	// ref state is always a real subvolume that snapshots exclude.
	if err := removeIfPlainDir(refPath); err != nil {
		return err
	}
	return createSubvol(refPath)
}

// Move relocates refName's identity subvolume from srcFramePath to
// dstFramePath, preserving its contents. If the source subvolume does not
// exist, Move ensures a fresh empty one at the destination instead (the ref had
// no prior identity state). If the destination already holds a subvolume for
// this ref, it is removed first so the moved one takes its place.
func Move(srcFramePath, dstFramePath, refName string) error {
	if err := validateRefName(refName); err != nil {
		return err
	}
	if err := ensureIDSubvol(dstFramePath); err != nil {
		return err
	}
	src := Path(srcFramePath, refName)
	dst := Path(dstFramePath, refName)

	if !btrfsutil.IsSubvolume(src) {
		// The ref had no prior identity state (its source subvolume was never
		// created, e.g. the ref was only ever attached to an empty frame), so
		// there is nothing to relocate. Give the destination a fresh, empty
		// identity subvolume so callers can rely on it existing afterwards.
		return Ensure(dstFramePath, refName)
	}

	// Clear any existing destination so the rename can land. Delete it as a
	// subvolume if it is one, otherwise drop a leftover plain directory.
	if btrfsutil.IsSubvolume(dst) {
		if err := btrfsutil.DeleteSubvol(dst); err != nil {
			return err
		}
	} else if err := removeIfPlainDir(dst); err != nil {
		return err
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
	if err := validateRefName(refName); err != nil {
		return err
	}
	refPath := Path(framePath, refName)
	if !btrfsutil.IsSubvolume(refPath) {
		// Fall back to removing a plain directory if one exists.
		if err := os.RemoveAll(refPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove ref id dir: %w", err)
		}
		return nil
	}
	return btrfsutil.DeleteSubvol(refPath)
}
