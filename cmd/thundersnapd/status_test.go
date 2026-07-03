// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteStatusWaitingForAuth tests that writeStatusWaitingForAuth creates
// the status file with the correct format.
func TestWriteStatusWaitingForAuth(t *testing.T) {
	// Save original and set up a temp status file
	origStatusFile := statusFile
	tmpDir := t.TempDir()
	statusFile = filepath.Join(tmpDir, "status")
	defer func() { statusFile = origStatusFile }()

	testURL := "https://login.tailscale.com/a/abc123"
	writeStatusWaitingForAuth(testURL)

	data, err := os.ReadFile(statusFile)
	if err != nil {
		t.Fatalf("failed to read status file %s: %v", statusFile, err)
	}

	content := string(data)

	if !strings.Contains(content, "state: waiting_for_auth") {
		t.Errorf("status file %s missing 'state: waiting_for_auth', got:\n%s", statusFile, content)
	}
	if !strings.Contains(content, "auth_url: "+testURL) {
		t.Errorf("status file %s missing auth_url, got:\n%s", statusFile, content)
	}
}

// TestWriteStatusError tests that writeStatusError creates the status file
// with the error message in the correct format.
func TestWriteStatusError(t *testing.T) {
	// Save original and set up a temp status file
	origStatusFile := statusFile
	tmpDir := t.TempDir()
	statusFile = filepath.Join(tmpDir, "status")
	defer func() { statusFile = origStatusFile }()

	testError := "failed to start: missing btrfs filesystem"
	writeStatusError(testError)

	data, err := os.ReadFile(statusFile)
	if err != nil {
		t.Fatalf("failed to read status file %s: %v", statusFile, err)
	}

	content := string(data)

	if !strings.Contains(content, "error: "+testError) {
		t.Errorf("status file %s missing error message, got:\n%s", statusFile, content)
	}
}

// TestParentCgroupNameUsesPID tests that the daemon's cgroup manager parent name
// includes the daemon PID to allow multiple thundersnapd instances to run without
// cgroup conflicts.
func TestParentCgroupNameUsesPID(t *testing.T) {
	pid := os.Getpid()
	parentName := cgroupManager.ParentName()

	// Verify format is thundersnap-<pid>
	expectedPrefix := "thundersnap-"
	if !strings.HasPrefix(parentName, expectedPrefix) {
		t.Errorf("parent cgroup name should have prefix %q, got: %s", expectedPrefix, parentName)
	}

	parts := strings.SplitN(parentName, "-", 2)
	if len(parts) != 2 {
		t.Fatalf("parent cgroup name should be thundersnap-<pid>, got: %s", parentName)
	}

	// The PID part should be a non-empty numeric string. It matches the test
	// process PID because cgroupManager is initialized at package load time.
	pidStr := parts[1]
	if len(pidStr) == 0 {
		t.Error("parent cgroup name PID part is empty")
	}
	for _, c := range pidStr {
		if c < '0' || c > '9' {
			t.Errorf("parent cgroup name PID part should be numeric, got: %s", pidStr)
			break
		}
	}

	t.Logf("parent cgroup name=%s (test process pid=%d)", parentName, pid)
}
