// Package btrfsutil centralizes the handful of "btrfs" subcommand invocations
// used across the daemon and the leaf packages (snapsubdir, refid). Every call
// previously open-coded exec.Command("btrfs", ...).CombinedOutput() with its
// own ad-hoc error wrapping; routing them through one package gives a single
// error-formatting convention and one place to mock for tests.
package btrfsutil

import (
	"fmt"
	"os/exec"
	"strings"
)

// Run executes a "btrfs" subcommand, wrapping any failure with the joined
// arguments and the command's combined output for diagnosis.
func Run(args ...string) error {
	out, err := exec.Command("btrfs", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("btrfs %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return nil
}

// IsSubvolume reports whether path is a btrfs subvolume.
//
// It treats ANY error from "btrfs subvolume show" as "not a subvolume",
// including the btrfs binary being absent, a permission error, or the path not
// existing. This conflation is intentional: callers use IsSubvolume only to
// decide whether a btrfs delete (vs a plain RemoveAll) is appropriate, and in
// every such case the safe fallback is to treat the path as a plain directory.
func IsSubvolume(path string) bool {
	return exec.Command("btrfs", "subvolume", "show", path).Run() == nil
}

// CreateSubvol creates a new empty btrfs subvolume at path.
func CreateSubvol(path string) error {
	return Run("subvolume", "create", path)
}

// DeleteSubvol deletes the btrfs subvolume at path. The caller is responsible
// for having established that path is a subvolume.
func DeleteSubvol(path string) error {
	return Run("subvolume", "delete", path)
}

// Snapshot creates a btrfs snapshot of src at dst. When readonly is true the
// snapshot is created with -r (used for the immutable snaps-dir entries).
func Snapshot(src, dst string, readonly bool) error {
	args := []string{"subvolume", "snapshot"}
	if readonly {
		args = append(args, "-r")
	}
	args = append(args, src, dst)
	return Run(args...)
}
