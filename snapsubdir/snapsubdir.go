// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// Package snapsubdir implements the btrfs snapshot reduction used by
// "ts snap <path>": it takes a writable snapshot of an entire subvolume (for
// atomicity), then reduces it to just a requested subtree by deleting
// everything else and promoting the subtree's contents to the snapshot root.
// The resulting subvolume is made read-only, ready to be indexed into a snap
// whose content hash reflects only that subtree.
package snapsubdir

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tailscale/thundersnap/btrfsutil"
)

// promoteDirName is the temporary directory the requested subtree is moved to
// at the snapshot root while sibling entries are pruned.
const promoteDirName = ".ts-subdir-promote"

// Validate cleans and validates a caller-supplied subdir path. It returns the
// cleaned slash-relative path (no leading slash, no "." or "..") or an error if
// the path is empty or the root.
//
// Anchoring with filepath.Clean("/"+subdir) means any ".." segments collapse
// against the leading "/" and can never escape: an escaping input like ".." or
// "a/../.." resolves to "/" and, after TrimPrefix, to "" — caught by the
// root-error branch below. The cleaned path therefore can never retain a
// leading "..", so no separate traversal check is needed.
func Validate(subdir string) (string, error) {
	clean := filepath.Clean("/" + subdir) // anchor at root, collapse .. that would escape
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" || clean == "." {
		return "", fmt.Errorf("subdir resolves to the subvolume root; snap the whole frame instead")
	}
	return clean, nil
}

// removeChildren recursively removes every entry within dir (handling nested
// subvolumes). It does not remove dir itself.
func removeChildren(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := removePathRecursive(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// removePathRecursive removes path, deleting any nested btrfs subvolumes it
// encounters with "btrfs subvolume delete" (plain os.RemoveAll cannot remove a
// subvolume). path may itself be a subvolume, a directory, or a file.
func removePathRecursive(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.IsDir() {
		// Both branches remove all children first (handling nested subvolumes);
		// they differ only in how the now-empty directory itself is removed.
		if err := removeChildren(path); err != nil {
			return err
		}
		if btrfsutil.IsSubvolume(path) {
			return btrfsutil.DeleteSubvol(path)
		}
		return os.Remove(path)
	}
	return os.Remove(path)
}

// Snapshot creates a writable btrfs snapshot of source at dstPath, then reduces
// it to just the subtree at subdir: everything outside subdir is removed,
// subdir's contents are promoted to the snapshot root, and the subvolume is
// made read-only. subdir is validated/cleaned internally; pass a path relative
// to the source subvolume root (with or without a leading slash).
func Snapshot(source, subdir, dstPath string) error {
	clean, err := Validate(subdir)
	if err != nil {
		return err
	}

	// Writable snapshot of the whole subvolume for atomicity.
	if err := btrfsutil.Snapshot(source, dstPath, false); err != nil {
		return err
	}

	srcSub := filepath.Join(dstPath, clean)
	info, err := os.Lstat(srcSub)
	if err != nil {
		return fmt.Errorf("subdir %q not found in frame: %w", subdir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("subdir %q is not a directory", subdir)
	}

	// Promote the subtree to a unique name at the snapshot root so it can't
	// collide with sibling entries we are about to delete. Clear any stale
	// promote dir first: it can only exist if the source frame itself happened
	// to contain a ".ts-subdir-promote" entry, which the snapshot copied in.
	promote := filepath.Join(dstPath, promoteDirName)
	if err := os.RemoveAll(promote); err != nil {
		return fmt.Errorf("clear promote dir: %w", err)
	}
	// srcSub and promote are both inside dstPath (the same btrfs subvolume), so
	// this rename is a cheap in-fs move, not a copy.
	if err := os.Rename(srcSub, promote); err != nil {
		return fmt.Errorf("promote subdir: %w", err)
	}

	// Delete every original top-level entry except the promoted subtree.
	entries, err := os.ReadDir(dstPath)
	if err != nil {
		return fmt.Errorf("read snapshot root: %w", err)
	}
	for _, e := range entries {
		if e.Name() == promoteDirName {
			continue
		}
		if err := removePathRecursive(filepath.Join(dstPath, e.Name())); err != nil {
			return fmt.Errorf("prune %s: %w", e.Name(), err)
		}
	}

	// Move the promoted subtree's contents up into the snapshot root.
	promoted, err := os.ReadDir(promote)
	if err != nil {
		return fmt.Errorf("read promote dir: %w", err)
	}
	for _, e := range promoted {
		from := filepath.Join(promote, e.Name())
		to := filepath.Join(dstPath, e.Name())
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("move %s to root: %w", e.Name(), err)
		}
	}
	if err := os.Remove(promote); err != nil {
		return fmt.Errorf("remove promote dir: %w", err)
	}

	// Make the resulting subvolume read-only before indexing.
	if err := btrfsutil.Run("property", "set", dstPath, "ro", "true"); err != nil {
		return err
	}
	return nil
}
