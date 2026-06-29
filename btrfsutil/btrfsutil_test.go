package btrfsutil

import (
	"strings"
	"testing"
)

// TestIsSubvolumeNonExistent confirms IsSubvolume reports false for a path that
// is not a subvolume (and conflates any error, including a missing btrfs binary,
// into "not a subvolume"). A non-existent path makes "btrfs subvolume show" fail
// regardless of the environment, so this is deterministic without root/btrfs.
func TestIsSubvolumeNonExistent(t *testing.T) {
	if IsSubvolume("/nonexistent/definitely/not/a/subvolume") {
		t.Error("IsSubvolume(non-existent) = true, want false")
	}
}

// TestRunWrapsError confirms Run surfaces the joined subcommand in its error so
// failures are diagnosable. An unknown subcommand fails on any btrfs build; if
// btrfs is entirely absent the exec error is still wrapped and non-nil.
func TestRunWrapsError(t *testing.T) {
	err := Run("this-is-not-a-real-btrfs-subcommand")
	if err == nil {
		t.Fatal("Run(bogus subcommand) = nil, want error")
	}
	if !strings.Contains(err.Error(), "btrfs this-is-not-a-real-btrfs-subcommand") {
		t.Errorf("error %q does not mention the joined args", err.Error())
	}
}
