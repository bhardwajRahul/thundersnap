// Package e2e contains end-to-end tests for thundersnap hardlink handling.
package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// TestHardlinkHandlingBasic tests that hardlinks are preserved through snapshot and restore.
// It creates a test environment with hardlinks, creates a frame from a snapshot,
// and verifies that both paths point to the same inode with nlink >= 2.
func TestHardlinkHandlingBasic(t *testing.T) {
	env := newTestEnv(t)

	// Create a base snapshot (which includes hardlinks from CreateHardlinkTest)
	baseSnap := env.createBaseSnapshot()
	t.Logf("Created base snapshot: %s", baseSnap)

	// Create a frame from the snapshot
	frameName := "hardlinktest"
	framePath := filepath.Join(env.fsDir, "testuser", frameName)

	// Create the frame directory structure
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir frame parent: %v", err)
	}

	// Clone snapshot to frame using btrfs
	cmd := exec.Command("btrfs", "subvolume", "snapshot",
		filepath.Join(env.snapshotsDir, baseSnap), framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	// Verify the hardlinks at var/log/original.log and var/log/hardlink.log
	originalPath := filepath.Join(framePath, "var/log/original.log")
	hardlinkPath := filepath.Join(framePath, "var/log/hardlink.log")

	// Get stat info for original
	var originalStat syscall.Stat_t
	if err := syscall.Stat(originalPath, &originalStat); err != nil {
		t.Fatalf("stat original: %v", err)
	}

	// Get stat info for hardlink
	var hardlinkStat syscall.Stat_t
	if err := syscall.Stat(hardlinkPath, &hardlinkStat); err != nil {
		t.Fatalf("stat hardlink: %v", err)
	}

	// Verify both files have the same inode
	if originalStat.Ino != hardlinkStat.Ino {
		t.Errorf("inode mismatch: original=%d, hardlink=%d (expected same inode)",
			originalStat.Ino, hardlinkStat.Ino)
	} else {
		t.Logf("Verified hardlinks share inode %d", originalStat.Ino)
	}

	// Verify nlink >= 2 for original
	if originalStat.Nlink < 2 {
		t.Errorf("original nlink=%d, want >= 2", originalStat.Nlink)
	} else {
		t.Logf("Original file has nlink=%d", originalStat.Nlink)
	}

	// Verify nlink >= 2 for hardlink
	if hardlinkStat.Nlink < 2 {
		t.Errorf("hardlink nlink=%d, want >= 2", hardlinkStat.Nlink)
	} else {
		t.Logf("Hardlink file has nlink=%d", hardlinkStat.Nlink)
	}
}
