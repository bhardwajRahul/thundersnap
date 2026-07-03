// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

package e2e

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestContainerSpec describes the contents of a test container image.
type TestContainerSpec struct {
	// Files is a list of files to create in the container.
	Files []TestFile
}

// TestFile describes a file to create in the test container.
type TestFile struct {
	Path    string      // Path in the container (e.g., "etc/passwd")
	Mode    os.FileMode // File mode including type bits
	Content []byte      // File content (for regular files)
	LinkTo  string      // Target for symlinks
	UID     int         // Owner UID
	GID     int         // Owner GID
	DevMaj  int         // Major device number (for char/block devices)
	DevMin  int         // Minor device number (for char/block devices)
}

// DefaultTestContainerSpec returns a TestContainerSpec with a variety of file types
// for exercising the snapshot/restore code paths.
func DefaultTestContainerSpec() *TestContainerSpec {
	return &TestContainerSpec{
		Files: []TestFile{
			// Basic directory structure
			{Path: "bin", Mode: os.ModeDir | 0755},
			{Path: "etc", Mode: os.ModeDir | 0755},
			{Path: "home", Mode: os.ModeDir | 0755},
			{Path: "home/user", Mode: os.ModeDir | 0755, UID: 1000, GID: 1000},
			{Path: "lib", Mode: os.ModeDir | 0755},
			{Path: "proc", Mode: os.ModeDir | 0555},
			{Path: "root", Mode: os.ModeDir | 0700},
			{Path: "sys", Mode: os.ModeDir | 0755},
			{Path: "tmp", Mode: os.ModeDir | 0777 | os.ModeSticky},
			{Path: "usr", Mode: os.ModeDir | 0755},
			{Path: "usr/bin", Mode: os.ModeDir | 0755},
			{Path: "usr/lib", Mode: os.ModeDir | 0755},
			{Path: "var", Mode: os.ModeDir | 0755},
			{Path: "var/log", Mode: os.ModeDir | 0755},
			{Path: "work", Mode: os.ModeDir | 0755, UID: 1000, GID: 1000},
			{Path: "dev", Mode: os.ModeDir | 0755},

			// /etc files
			{Path: "etc/passwd", Mode: 0644, Content: []byte(
				"root:x:0:0:root:/root:/bin/sh\n" +
					"user:x:1000:1000:user:/home/user:/bin/sh\n" +
					"daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin\n" +
					"nobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin\n",
			)},
			{Path: "etc/group", Mode: 0644, Content: []byte(
				"root:x:0:\n" +
					"user:x:1000:\n" +
					"daemon:x:1:\n" +
					"nogroup:x:65534:\n",
			)},
			{Path: "etc/hostname", Mode: 0644, Content: []byte("testcontainer\n")},
			{Path: "etc/hosts", Mode: 0644, Content: []byte("127.0.0.1 localhost\n")},
			{Path: "etc/resolv.conf", Mode: 0644, Content: []byte("nameserver 8.8.8.8\n")},

			// User files with specific ownership
			{Path: "home/user/.profile", Mode: 0644, UID: 1000, GID: 1000, Content: []byte("# user profile\nexport PATH=$PATH:/usr/local/bin\n")},
			{Path: "home/user/.bashrc", Mode: 0644, UID: 1000, GID: 1000, Content: []byte("# user bashrc\n")},

			// Root files
			{Path: "root/.profile", Mode: 0644, Content: []byte("# root profile\n")},

			// Symlinks
			{Path: "lib64", Mode: os.ModeSymlink | 0777, LinkTo: "lib"},
			{Path: "usr/sbin", Mode: os.ModeSymlink | 0777, LinkTo: "../sbin"},

			// Files with various permissions
			{Path: "etc/shadow", Mode: 0640, Content: []byte("root:*:19000:0:99999:7:::\nuser:*:19000:0:99999:7:::\n")},
			{Path: "var/log/messages", Mode: 0640, Content: []byte("")},

			// Setuid/setgid files (for testing permission preservation)
			{Path: "usr/bin/sudo-test", Mode: 0755 | os.ModeSetuid, Content: []byte("#!/bin/sh\necho sudo-test\n")},
			{Path: "usr/bin/sg-test", Mode: 0755 | os.ModeSetgid, Content: []byte("#!/bin/sh\necho sg-test\n")},

			// Character device: /dev/null (major 1, minor 3)
			{Path: "dev/null", Mode: os.ModeCharDevice | 0666, DevMaj: 1, DevMin: 3},

			// Character device: /dev/zero (major 1, minor 5)
			{Path: "dev/zero", Mode: os.ModeCharDevice | 0666, DevMaj: 1, DevMin: 5},

			// Character device: /dev/random (major 1, minor 8)
			{Path: "dev/random", Mode: os.ModeCharDevice | 0666, DevMaj: 1, DevMin: 8},

			// Character device: /dev/urandom (major 1, minor 9)
			{Path: "dev/urandom", Mode: os.ModeCharDevice | 0666, DevMaj: 1, DevMin: 9},

			// Character device: /dev/tty (major 5, minor 0)
			{Path: "dev/tty", Mode: os.ModeCharDevice | 0666, DevMaj: 5, DevMin: 0},

			// Block device: /dev/loop0 (major 7, minor 0) - for testing
			{Path: "dev/loop0", Mode: os.ModeDevice | 0660, DevMaj: 7, DevMin: 0},
		},
	}
}

// CreateOnDisk creates the test container filesystem on disk at the given path.
func (spec *TestContainerSpec) CreateOnDisk(t *testing.T, dir string) {
	t.Helper()

	// First pass: create all directories
	for _, f := range spec.Files {
		if f.Mode&os.ModeDir != 0 {
			path := filepath.Join(dir, f.Path)
			if err := os.MkdirAll(path, f.Mode.Perm()); err != nil {
				t.Fatalf("mkdir %s: %v", f.Path, err)
			}
		}
	}

	// Second pass: create files, symlinks, devices
	for _, f := range spec.Files {
		path := filepath.Join(dir, f.Path)

		switch {
		case f.Mode&os.ModeDir != 0:
			// Already created, just set ownership and sticky bit if needed
			if err := os.Chmod(path, f.Mode.Perm()|f.Mode&os.ModeSticky); err != nil {
				t.Fatalf("chmod %s: %v", f.Path, err)
			}
			if f.UID != 0 || f.GID != 0 {
				if err := os.Lchown(path, f.UID, f.GID); err != nil {
					t.Fatalf("chown %s: %v", f.Path, err)
				}
			}

		case f.Mode&os.ModeSymlink != 0:
			// Ensure parent directory exists
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatalf("mkdir parent of %s: %v", f.Path, err)
			}
			if err := os.Symlink(f.LinkTo, path); err != nil {
				t.Fatalf("symlink %s -> %s: %v", f.Path, f.LinkTo, err)
			}

		case f.Mode&os.ModeCharDevice != 0 || f.Mode&os.ModeDevice != 0:
			// Character or block device - requires root
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatalf("mkdir parent of %s: %v", f.Path, err)
			}
			dev := unix.Mkdev(uint32(f.DevMaj), uint32(f.DevMin))
			mode := uint32(f.Mode.Perm())
			if f.Mode&os.ModeCharDevice != 0 {
				mode |= unix.S_IFCHR
			} else {
				mode |= unix.S_IFBLK
			}
			if err := unix.Mknod(path, mode, int(dev)); err != nil {
				t.Fatalf("mknod %s: %v", f.Path, err)
			}
			if err := os.Chmod(path, f.Mode.Perm()); err != nil {
				t.Fatalf("chmod %s: %v", f.Path, err)
			}

		default:
			// Regular file
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatalf("mkdir parent of %s: %v", f.Path, err)
			}
			if err := os.WriteFile(path, f.Content, f.Mode.Perm()); err != nil {
				t.Fatalf("write %s: %v", f.Path, err)
			}
			// Set special mode bits (setuid, setgid, sticky)
			fullMode := f.Mode.Perm() | f.Mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)
			if err := os.Chmod(path, fullMode); err != nil {
				t.Fatalf("chmod %s: %v", f.Path, err)
			}
			if f.UID != 0 || f.GID != 0 {
				if err := os.Lchown(path, f.UID, f.GID); err != nil {
					t.Fatalf("chown %s: %v", f.Path, err)
				}
			}
		}
	}
}

// CreateTestContainer creates a test container filesystem on disk.
// If tsBinaryPath is non-empty, the ts binary is copied to /bin/ts and
// /bin/sh is hardlinked to it (ts acts as a shell when invoked as "sh").
func CreateTestContainer(t *testing.T, dir string, tsBinaryPath string) {
	t.Helper()

	spec := DefaultTestContainerSpec()
	spec.CreateOnDisk(t, dir)

	// Copy ts binary if provided
	if tsBinaryPath != "" {
		tsDst := filepath.Join(dir, "bin/ts")
		if err := copyFilePreserveMode(tsBinaryPath, tsDst); err != nil {
			t.Fatalf("copy ts binary: %v", err)
		}
		// Create /bin/sh as hardlink to ts - ts acts as a shell when invoked as "sh"
		shDst := filepath.Join(dir, "bin/sh")
		if err := os.Link(tsDst, shDst); err != nil {
			t.Fatalf("link bin/sh to bin/ts: %v", err)
		}
	}
}

func copyFilePreserveMode(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
