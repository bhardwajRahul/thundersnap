// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestFixtureCreatesAllFileTypes verifies that the fixture generator creates
// all the expected file types: directories, regular files, symlinks, hardlinks,
// and device nodes.
func TestFixtureCreatesAllFileTypes(t *testing.T) {
	if os.Getuid() != 0 {
		t.Fatal("fixture test requires root for device nodes")
	}

	dir := t.TempDir()
	spec := DefaultTestContainerSpec()
	spec.CreateOnDisk(t, dir)
	CreateHardlinkTest(t, dir)

	// Test directories exist with correct permissions
	testCases := []struct {
		path string
		mode os.FileMode
		uid  uint32
		gid  uint32
	}{
		{"bin", os.ModeDir | 0755, 0, 0},
		{"etc", os.ModeDir | 0755, 0, 0},
		{"home/user", os.ModeDir | 0755, 1000, 1000},
		{"tmp", os.ModeDir | 0777 | os.ModeSticky, 0, 0},
		{"work", os.ModeDir | 0755, 1000, 1000},
	}

	for _, tc := range testCases {
		path := filepath.Join(dir, tc.path)
		info, err := os.Lstat(path)
		if err != nil {
			t.Errorf("path %s: %v", tc.path, err)
			continue
		}

		// Check mode (including directory bit and sticky bit)
		wantPerm := tc.mode.Perm()
		gotPerm := info.Mode().Perm()
		if tc.mode&os.ModeSticky != 0 {
			// Check sticky bit separately
			stat := info.Sys().(*syscall.Stat_t)
			if stat.Mode&01000 == 0 {
				t.Errorf("path %s: expected sticky bit", tc.path)
			}
		}
		if gotPerm != wantPerm {
			t.Errorf("path %s: mode = %o, want %o", tc.path, gotPerm, wantPerm)
		}

		// Check ownership
		stat := info.Sys().(*syscall.Stat_t)
		if stat.Uid != tc.uid || stat.Gid != tc.gid {
			t.Errorf("path %s: uid:gid = %d:%d, want %d:%d", tc.path, stat.Uid, stat.Gid, tc.uid, tc.gid)
		}
	}

	// Test symlinks
	symlinkTests := []struct {
		path   string
		target string
	}{
		{"lib64", "lib"},
		{"usr/sbin", "../sbin"},
	}

	for _, tc := range symlinkTests {
		path := filepath.Join(dir, tc.path)
		target, err := os.Readlink(path)
		if err != nil {
			t.Errorf("symlink %s: %v", tc.path, err)
			continue
		}
		if target != tc.target {
			t.Errorf("symlink %s: target = %q, want %q", tc.path, target, tc.target)
		}
	}

	// Test regular files
	regularFiles := []struct {
		path string
		mode os.FileMode
	}{
		{"etc/passwd", 0644},
		{"etc/hostname", 0644},
		{"etc/shadow", 0640},
		{"home/user/.profile", 0644},
	}

	for _, tc := range regularFiles {
		path := filepath.Join(dir, tc.path)
		info, err := os.Lstat(path)
		if err != nil {
			t.Errorf("file %s: %v", tc.path, err)
			continue
		}
		if info.Mode().Perm() != tc.mode {
			t.Errorf("file %s: mode = %o, want %o", tc.path, info.Mode().Perm(), tc.mode)
		}
	}

	// Test setuid/setgid files
	setuidTest := filepath.Join(dir, "usr/bin/sudo-test")
	info, err := os.Lstat(setuidTest)
	if err != nil {
		t.Errorf("setuid file: %v", err)
	} else if info.Mode()&os.ModeSetuid == 0 {
		t.Error("usr/bin/sudo-test should have setuid bit")
	}

	setgidTest := filepath.Join(dir, "usr/bin/sg-test")
	info, err = os.Lstat(setgidTest)
	if err != nil {
		t.Errorf("setgid file: %v", err)
	} else if info.Mode()&os.ModeSetgid == 0 {
		t.Error("usr/bin/sg-test should have setgid bit")
	}

	// Test character devices
	charDevices := []struct {
		path string
		maj  uint32
		min  uint32
	}{
		{"dev/null", 1, 3},
		{"dev/zero", 1, 5},
		{"dev/random", 1, 8},
		{"dev/urandom", 1, 9},
		{"dev/tty", 5, 0},
	}

	for _, tc := range charDevices {
		path := filepath.Join(dir, tc.path)
		info, err := os.Lstat(path)
		if err != nil {
			t.Errorf("chardev %s: %v", tc.path, err)
			continue
		}
		if info.Mode()&os.ModeCharDevice == 0 {
			t.Errorf("chardev %s: not a character device", tc.path)
			continue
		}
		stat := info.Sys().(*syscall.Stat_t)
		maj := uint32(stat.Rdev >> 8)
		min := uint32(stat.Rdev & 0xff)
		if maj != tc.maj || min != tc.min {
			t.Errorf("chardev %s: dev = %d:%d, want %d:%d", tc.path, maj, min, tc.maj, tc.min)
		}
	}

	// Test block device
	blockDev := filepath.Join(dir, "dev/loop0")
	info, err = os.Lstat(blockDev)
	if err != nil {
		t.Errorf("blockdev: %v", err)
	} else if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
		t.Errorf("dev/loop0: not a block device (mode=%v)", info.Mode())
	}

	// Test hardlinks
	orig := filepath.Join(dir, "var/log/original.log")
	link := filepath.Join(dir, "var/log/hardlink.log")

	origInfo, err := os.Lstat(orig)
	if err != nil {
		t.Errorf("hardlink original: %v", err)
	}
	linkInfo, err := os.Lstat(link)
	if err != nil {
		t.Errorf("hardlink: %v", err)
	}

	if origInfo != nil && linkInfo != nil {
		origStat := origInfo.Sys().(*syscall.Stat_t)
		linkStat := linkInfo.Sys().(*syscall.Stat_t)
		if origStat.Ino != linkStat.Ino {
			t.Errorf("hardlink inode mismatch: orig=%d, link=%d", origStat.Ino, linkStat.Ino)
		}
		if origStat.Nlink < 2 {
			t.Errorf("hardlink nlink = %d, want >= 2", origStat.Nlink)
		}
	}
}

// TestDefaultTestContainerSpecCompleteness verifies that the spec contains
// all the file types we claim to support.
func TestDefaultTestContainerSpecCompleteness(t *testing.T) {
	spec := DefaultTestContainerSpec()

	var hasDir, hasFile, hasSymlink, hasCharDev, hasBlockDev bool

	for _, f := range spec.Files {
		switch {
		case f.Mode&os.ModeDir != 0:
			hasDir = true
		case f.Mode&os.ModeSymlink != 0:
			hasSymlink = true
		case f.Mode&os.ModeCharDevice != 0:
			hasCharDev = true
		case f.Mode&os.ModeDevice != 0:
			hasBlockDev = true
		default:
			hasFile = true
		}
	}

	if !hasDir {
		t.Error("spec missing directories")
	}
	if !hasFile {
		t.Error("spec missing regular files")
	}
	if !hasSymlink {
		t.Error("spec missing symlinks")
	}
	if !hasCharDev {
		t.Error("spec missing character devices")
	}
	if !hasBlockDev {
		t.Error("spec missing block devices")
	}
}
