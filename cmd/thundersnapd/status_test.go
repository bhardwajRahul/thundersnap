package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteStatusWaitingForAuth tests that writeStatusWaitingForAuth creates
// the status files with the correct format in both locations.
func TestWriteStatusWaitingForAuth(t *testing.T) {
	// Save original and set up temp status files
	origStatusFiles := statusFiles
	tmpDir := t.TempDir()
	statusFiles = []string{
		filepath.Join(tmpDir, "run", "thundersnap", "status"),
		filepath.Join(tmpDir, "var", "lib", "thundersnap", "status"),
	}
	defer func() { statusFiles = origStatusFiles }()

	testURL := "https://login.tailscale.com/a/abc123"
	writeStatusWaitingForAuth(testURL)

	// Verify both files were created with correct content
	for _, path := range statusFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read status file %s: %v", path, err)
		}

		content := string(data)

		if !strings.Contains(content, "state: waiting_for_auth") {
			t.Errorf("status file %s missing 'state: waiting_for_auth', got:\n%s", path, content)
		}
		if !strings.Contains(content, "auth_url: "+testURL) {
			t.Errorf("status file %s missing auth_url, got:\n%s", path, content)
		}
	}
}

// TestWriteStatusError tests that writeStatusError creates the status files
// with the error message in the correct format in both locations.
func TestWriteStatusError(t *testing.T) {
	// Save original and set up temp status files
	origStatusFiles := statusFiles
	tmpDir := t.TempDir()
	statusFiles = []string{
		filepath.Join(tmpDir, "run", "thundersnap", "status"),
		filepath.Join(tmpDir, "var", "lib", "thundersnap", "status"),
	}
	defer func() { statusFiles = origStatusFiles }()

	testError := "failed to start: missing btrfs filesystem"
	writeStatusError(testError)

	// Verify both files were created with correct content
	for _, path := range statusFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read status file %s: %v", path, err)
		}

		content := string(data)

		if !strings.Contains(content, "error: "+testError) {
			t.Errorf("status file %s missing error message, got:\n%s", path, content)
		}
	}
}

// TestStatusFileDirectoryCreation tests that status functions create the
// parent directories if they don't exist.
func TestStatusFileDirectoryCreation(t *testing.T) {
	origStatusFiles := statusFiles
	tmpDir := t.TempDir()
	// Use deeply nested paths that don't exist
	statusFiles = []string{
		filepath.Join(tmpDir, "deeply", "nested", "path1", "status"),
		filepath.Join(tmpDir, "another", "nested", "path2", "status"),
	}
	defer func() { statusFiles = origStatusFiles }()

	writeStatusError("test error")

	// Verify all directories were created and files exist
	for _, path := range statusFiles {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("status file not created at %s: %v", path, err)
		}
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
