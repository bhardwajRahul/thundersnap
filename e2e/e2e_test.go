// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

// Package e2e contains true end-to-end tests for thundersnap.
//
// These tests start a real thundersnapd process and connect to it over SSH.
// All validation is done by running commands through the SSH session.
//
// Requirements:
//   - root access (for btrfs subvolume operations)
//   - btrfs filesystem for temp directory
//   - pre-built binaries specified via environment variables:
//   - TS_BINARY: path to pre-built ts binary
//   - VSHD_BINARY: path to pre-built vshd binary
//   - THUNDERSNAPD_BINARY: path to pre-built thundersnapd binary
//
// Use "make e2e" to build binaries and run tests with the correct environment.
package e2e

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// testEnv holds paths and resources for a test environment.
type testEnv struct {
	t            *testing.T
	root         string // temp dir root
	repoRoot     string // ts2 repository root
	fsDir        string
	snapshotsDir string
	libexecDir   string
	tsBinary     string
	daemonBinary string
}

// requireBtrfsRoot fails the test if the e2e environment is not set up
// correctly (not root or not on btrfs). e2e tests must never skip: if the
// environment is misconfigured we want a hard failure, not a silent skip.
func requireBtrfsRoot(t *testing.T) string {
	t.Helper()

	if os.Getuid() != 0 {
		t.Fatal("e2e test requires root for btrfs and container ops")
	}
	if _, err := exec.LookPath("btrfs"); err != nil {
		t.Fatal("btrfs not on PATH")
	}

	root := t.TempDir()
	// Ensure absolute path (TMPDIR may be relative)
	root, _ = filepath.Abs(root)
	cmd := exec.Command("stat", "-f", "-c", "%T", root)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("stat -f failed: %v", err)
	}
	if strings.TrimSpace(string(out)) != "btrfs" {
		t.Fatalf("test dir %s not on btrfs (got %q)", root, strings.TrimSpace(string(out)))
	}

	return root
}

// findRepoRoot finds the ts2 repository root by looking for go.mod.
func findRepoRoot(t *testing.T) string {
	t.Helper()

	// Start from current working directory
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	// Walk up looking for go.mod with module github.com/tailscale/thundersnap
	for {
		gomod := filepath.Join(dir, "go.mod")
		if data, err := os.ReadFile(gomod); err == nil {
			if strings.Contains(string(data), "module github.com/tailscale/thundersnap") {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find ts2 repo root (no go.mod with thundersnap module)")
		}
		dir = parent
	}
}

// newTestEnv creates a test environment with built binaries.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	root := requireBtrfsRoot(t)
	repoRoot := findRepoRoot(t)

	env := &testEnv{
		t:            t,
		root:         root,
		repoRoot:     repoRoot,
		fsDir:        filepath.Join(root, "fs"),
		snapshotsDir: filepath.Join(root, "snapshots"),
		libexecDir:   filepath.Join(root, "libexec"),
	}

	// Create directories
	for _, d := range []string{env.fsDir, env.snapshotsDir, env.libexecDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// Get pre-built binaries from environment
	env.tsBinary = env.requireBinary("ts")
	env.daemonBinary = env.requireBinary("thundersnapd")

	// Copy ts to libexec (thundersnapd looks for it there)
	if err := copyFile(env.tsBinary, filepath.Join(env.libexecDir, "ts")); err != nil {
		t.Fatalf("copy ts to libexec: %v", err)
	}

	t.Cleanup(env.cleanup)

	return env
}

func (e *testEnv) requireBinary(name string) string {
	e.t.Helper()

	envVar := strings.ToUpper(name) + "_BINARY"
	path := os.Getenv(envVar)
	if path == "" {
		e.t.Fatalf("%s not set; use 'make e2e' to run e2e tests", envVar)
	}
	if _, err := os.Stat(path); err != nil {
		e.t.Fatalf("%s=%s but file not found: %v", envVar, path, err)
	}
	e.t.Logf("using %s from %s", name, path)
	return path
}

func (e *testEnv) cleanup() {
	// Clean up all btrfs subvolumes under the test root.
	// Walk the tree, collect all directories, then delete deepest-first.
	cleanupAllSubvolumes(e.root)
}

func cleanupAllSubvolumes(root string) {
	// Collect all directories under root (these might be subvolumes)
	var dirs []string
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // continue walking
		}
		if d.IsDir() && path != root {
			dirs = append(dirs, path)
		}
		return nil
	})

	// Sort by path length descending (deepest first)
	sort.Slice(dirs, func(i, j int) bool {
		return len(dirs[i]) > len(dirs[j])
	})

	// Try to delete each as a subvolume (make writable first for read-only snapshots)
	for _, path := range dirs {
		// Make writable in case it's a read-only snapshot
		exec.Command("btrfs", "property", "set", "-ts", path, "ro", "false").Run()
		// Try to delete as subvolume (will fail silently if not a subvolume)
		exec.Command("btrfs", "subvolume", "delete", path).Run()
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

	// Copy permissions
	info, err := in.Stat()
	if err != nil {
		return err
	}
	return os.Chmod(dst, info.Mode())
}

// vmDir returns the path to VM binaries if available, or empty string if not.
// Can be overridden with THUNDERSNAP_VM_DIR environment variable.
func vmDir() string {
	// Allow override via environment
	if dir := os.Getenv("THUNDERSNAP_VM_DIR"); dir != "" {
		chv := filepath.Join(dir, "cloud-hypervisor")
		kernel := filepath.Join(dir, "vmlinux")
		if _, err := os.Stat(chv); err == nil {
			if _, err := os.Stat(kernel); err == nil {
				return dir
			}
		}
	}

	// Check common locations for cloud-hypervisor
	candidates := []string{
		"vm",    // repo's vm/ directory (when running from repo root)
		"../vm", // when running from e2e/
		"/usr/local/lib/thundersnap",
		"/usr/lib/thundersnap",
		"/opt/thundersnap",
	}
	for _, dir := range candidates {
		chv := filepath.Join(dir, "cloud-hypervisor")
		kernel := filepath.Join(dir, "vmlinux")
		if _, err := os.Stat(chv); err == nil {
			if _, err := os.Stat(kernel); err == nil {
				return dir
			}
		}
	}
	return ""
}
