// Package e2e provides test fixtures for end-to-end testing.
package e2e

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
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
// This is used when we need to create a btrfs subvolume directly.
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

// CreateDockerTarball creates a Docker-compatible image tarball.
// The tarball contains:
// - manifest.json with image metadata
// - A single layer tarball with the filesystem contents
// - config.json with image configuration
//
// This can be loaded with "docker load" or extracted manually.
func (spec *TestContainerSpec) CreateDockerTarball(t *testing.T, w io.Writer) {
	t.Helper()

	// Create the outer tar (Docker image format)
	tw := tar.NewWriter(w)
	defer tw.Close()

	// Create layer tarball in memory
	layerTarGz, layerDigest := spec.createLayerTarball(t)

	// Write layer tarball
	layerPath := layerDigest + "/layer.tar"
	if err := tw.WriteHeader(&tar.Header{
		Name: layerDigest + "/",
		Mode: 0755,
		Size: 0,
		Typeflag: tar.TypeDir,
	}); err != nil {
		t.Fatalf("write layer dir header: %v", err)
	}

	if err := tw.WriteHeader(&tar.Header{
		Name: layerPath,
		Mode: 0644,
		Size: int64(len(layerTarGz)),
	}); err != nil {
		t.Fatalf("write layer tar header: %v", err)
	}
	if _, err := tw.Write(layerTarGz); err != nil {
		t.Fatalf("write layer tar: %v", err)
	}

	// Create config.json
	config := map[string]interface{}{
		"architecture": "amd64",
		"os":           "linux",
		"config": map[string]interface{}{
			"Env":        []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
			"WorkingDir": "/",
		},
		"rootfs": map[string]interface{}{
			"type":     "layers",
			"diff_ids": []string{"sha256:" + layerDigest},
		},
	}
	configBytes, _ := json.Marshal(config)
	configDigest := "testconfig123"

	if err := tw.WriteHeader(&tar.Header{
		Name: configDigest + ".json",
		Mode: 0644,
		Size: int64(len(configBytes)),
	}); err != nil {
		t.Fatalf("write config header: %v", err)
	}
	if _, err := tw.Write(configBytes); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Create manifest.json
	manifest := []map[string]interface{}{
		{
			"Config":   configDigest + ".json",
			"RepoTags": []string{"test:latest"},
			"Layers":   []string{layerPath},
		},
	}
	manifestBytes, _ := json.Marshal(manifest)

	if err := tw.WriteHeader(&tar.Header{
		Name: "manifest.json",
		Mode: 0644,
		Size: int64(len(manifestBytes)),
	}); err != nil {
		t.Fatalf("write manifest header: %v", err)
	}
	if _, err := tw.Write(manifestBytes); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// createLayerTarball creates a gzipped tar of the layer contents.
func (spec *TestContainerSpec) createLayerTarball(t *testing.T) ([]byte, string) {
	t.Helper()

	// Create uncompressed tar first (Docker expects uncompressed layer.tar)
	var buf []byte
	tw := tar.NewWriter(writerFunc(func(p []byte) (int, error) {
		buf = append(buf, p...)
		return len(p), nil
	}))

	for _, f := range spec.Files {
		hdr := &tar.Header{
			Name: f.Path,
			Uid:  f.UID,
			Gid:  f.GID,
		}

		switch {
		case f.Mode&os.ModeDir != 0:
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = int64(f.Mode.Perm())
			if f.Mode&os.ModeSticky != 0 {
				hdr.Mode |= 01000
			}

		case f.Mode&os.ModeSymlink != 0:
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = f.LinkTo
			hdr.Mode = 0777

		case f.Mode&os.ModeCharDevice != 0:
			hdr.Typeflag = tar.TypeChar
			hdr.Devmajor = int64(f.DevMaj)
			hdr.Devminor = int64(f.DevMin)
			hdr.Mode = int64(f.Mode.Perm())

		case f.Mode&os.ModeDevice != 0:
			hdr.Typeflag = tar.TypeBlock
			hdr.Devmajor = int64(f.DevMaj)
			hdr.Devminor = int64(f.DevMin)
			hdr.Mode = int64(f.Mode.Perm())

		default:
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(f.Content))
			hdr.Mode = int64(f.Mode.Perm())
			if f.Mode&os.ModeSetuid != 0 {
				hdr.Mode |= 04000
			}
			if f.Mode&os.ModeSetgid != 0 {
				hdr.Mode |= 02000
			}
		}

		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header for %s: %v", f.Path, err)
		}

		if hdr.Typeflag == tar.TypeReg && len(f.Content) > 0 {
			if _, err := tw.Write(f.Content); err != nil {
				t.Fatalf("write tar content for %s: %v", f.Path, err)
			}
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("close layer tar: %v", err)
	}

	// Return uncompressed tar (Docker layer.tar is not gzipped)
	return buf, "abc123testlayer"
}

// writerFunc adapts a function to io.Writer.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) {
	return f(p)
}

// CreateHardlinkTest creates a test file and a hardlink to it.
// This is separate because hardlinks require the target to exist first.
func CreateHardlinkTest(t *testing.T, dir string) {
	t.Helper()

	// Create original file
	original := filepath.Join(dir, "var/log/original.log")
	if err := os.MkdirAll(filepath.Dir(original), 0755); err != nil {
		t.Fatalf("mkdir for hardlink test: %v", err)
	}
	if err := os.WriteFile(original, []byte("hardlink test content\n"), 0644); err != nil {
		t.Fatalf("write original for hardlink: %v", err)
	}

	// Create hardlink
	hardlink := filepath.Join(dir, "var/log/hardlink.log")
	if err := os.Link(original, hardlink); err != nil {
		t.Fatalf("create hardlink: %v", err)
	}
}

// CreateTestContainer creates a test container filesystem on disk and returns the path.
// The caller should have already created the parent directory.
// If tsBinaryPath is non-empty, the ts binary is copied to /bin/ts and
// /bin/sh is hardlinked to it (ts acts as a shell when invoked as "sh").
func CreateTestContainer(t *testing.T, dir string, tsBinaryPath string) {
	t.Helper()

	spec := DefaultTestContainerSpec()
	spec.CreateOnDisk(t, dir)

	// Add hardlink test
	CreateHardlinkTest(t, dir)

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

// unused, silence linter
var _ = gzip.NewWriter
