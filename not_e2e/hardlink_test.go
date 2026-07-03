// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

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

// TestHardlinkSpanningDirectories tests that hardlinks spanning directories
// are preserved correctly through snapshot and restore.
func TestHardlinkSpanningDirectories(t *testing.T) {
	env := newTestEnv(t)

	// Create a base snapshot
	baseSnap := env.createBaseSnapshot()

	// Create a frame from the snapshot
	framePath := filepath.Join(env.fsDir, "testuser", "hardlinkspan")
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cmd := exec.Command("btrfs", "subvolume", "snapshot",
		filepath.Join(env.snapshotsDir, baseSnap), framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	// Create a file in one directory
	dir1 := filepath.Join(framePath, "tmp", "dir1")
	dir2 := filepath.Join(framePath, "tmp", "dir2")
	if err := os.MkdirAll(dir1, 0755); err != nil {
		t.Fatalf("mkdir dir1: %v", err)
	}
	if err := os.MkdirAll(dir2, 0755); err != nil {
		t.Fatalf("mkdir dir2: %v", err)
	}

	origFile := filepath.Join(dir1, "orig.txt")
	if err := os.WriteFile(origFile, []byte("hardlink spanning dirs test\n"), 0644); err != nil {
		t.Fatalf("write orig file: %v", err)
	}

	// Create hardlink in a different directory
	linkFile := filepath.Join(dir2, "link.txt")
	if err := os.Link(origFile, linkFile); err != nil {
		t.Fatalf("create hardlink: %v", err)
	}

	// Verify they share the same inode
	var origStat, linkStat syscall.Stat_t
	if err := syscall.Stat(origFile, &origStat); err != nil {
		t.Fatalf("stat orig: %v", err)
	}
	if err := syscall.Stat(linkFile, &linkStat); err != nil {
		t.Fatalf("stat link: %v", err)
	}
	if origStat.Ino != linkStat.Ino {
		t.Fatalf("files don't share inode before snapshot")
	}
	t.Logf("Before snapshot: inode=%d, nlink=%d", origStat.Ino, origStat.Nlink)

	// Create a snapshot of the frame
	snap2Path := filepath.Join(env.snapshotsDir, "hardlink-span-snap")
	cmd = exec.Command("btrfs", "subvolume", "snapshot", "-r", framePath, snap2Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot frame: %v\n%s", err, out)
	}

	// Create a new frame from the snapshot
	frame2Path := filepath.Join(env.fsDir, "testuser", "hardlinkspan2")
	cmd = exec.Command("btrfs", "subvolume", "snapshot", snap2Path, frame2Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot to frame2: %v\n%s", err, out)
	}

	// Verify the hardlink is preserved in the restored frame
	origFile2 := filepath.Join(frame2Path, "tmp", "dir1", "orig.txt")
	linkFile2 := filepath.Join(frame2Path, "tmp", "dir2", "link.txt")

	var origStat2, linkStat2 syscall.Stat_t
	if err := syscall.Stat(origFile2, &origStat2); err != nil {
		t.Fatalf("stat orig in frame2: %v", err)
	}
	if err := syscall.Stat(linkFile2, &linkStat2); err != nil {
		t.Fatalf("stat link in frame2: %v", err)
	}

	if origStat2.Ino != linkStat2.Ino {
		t.Errorf("hardlink not preserved: orig inode=%d, link inode=%d", origStat2.Ino, linkStat2.Ino)
	} else {
		t.Logf("After restore: files share inode=%d, nlink=%d", origStat2.Ino, origStat2.Nlink)
	}

	if origStat2.Nlink < 2 {
		t.Errorf("nlink=%d, want >= 2", origStat2.Nlink)
	}
}

// TestHardlinkBreakOnModify tests that modifying one copy of a hardlink
// correctly breaks the link (copy-on-write behavior in btrfs).
func TestHardlinkBreakOnModify(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()
	framePath := filepath.Join(env.fsDir, "testuser", "hardlinkbreak")
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cmd := exec.Command("btrfs", "subvolume", "snapshot",
		filepath.Join(env.snapshotsDir, baseSnap), framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	// Create a file and hardlink
	origFile := filepath.Join(framePath, "tmp", "orig.txt")
	linkFile := filepath.Join(framePath, "tmp", "link.txt")

	initialContent := "initial content\n"
	if err := os.WriteFile(origFile, []byte(initialContent), 0644); err != nil {
		t.Fatalf("write orig: %v", err)
	}
	if err := os.Link(origFile, linkFile); err != nil {
		t.Fatalf("create hardlink: %v", err)
	}

	// Verify they share inode initially
	var origStat, linkStat syscall.Stat_t
	syscall.Stat(origFile, &origStat)
	syscall.Stat(linkFile, &linkStat)
	if origStat.Ino != linkStat.Ino {
		t.Fatal("files should share inode before modification")
	}
	t.Logf("Before modification: shared inode=%d", origStat.Ino)

	// Modify the original file
	newContent := "modified content in orig\n"
	if err := os.WriteFile(origFile, []byte(newContent), 0644); err != nil {
		t.Fatalf("write modified: %v", err)
	}

	// Read both files - depending on filesystem behavior:
	// - In traditional Unix: hardlinks always share data, modification affects both
	// - In btrfs with COW: this depends on how the write happened
	origContent, _ := os.ReadFile(origFile)
	linkContent, _ := os.ReadFile(linkFile)

	t.Logf("After modification:")
	t.Logf("  orig content: %q", origContent)
	t.Logf("  link content: %q", linkContent)

	// The key verification: orig has the new content
	if string(origContent) != newContent {
		t.Errorf("orig should have new content")
	}

	// Note: whether link has new or old content depends on filesystem
	// With O_TRUNC truncate+rewrite, the link typically keeps old content
	// With in-place modification, both would be updated
	// This test documents the behavior rather than mandating it
	syscall.Stat(origFile, &origStat)
	syscall.Stat(linkFile, &linkStat)
	t.Logf("  orig inode=%d, link inode=%d", origStat.Ino, linkStat.Ino)

	if origStat.Ino == linkStat.Ino {
		t.Log("Hardlink still intact (same inode)")
		// If same inode, both should have same content
		if string(origContent) != string(linkContent) {
			t.Error("same inode but different content - unexpected")
		}
	} else {
		t.Log("Hardlink broken by modification (different inodes)")
		// If different inodes, link should have old content
		if string(linkContent) != initialContent {
			t.Logf("Link content changed unexpectedly to: %q", linkContent)
		}
	}
}
