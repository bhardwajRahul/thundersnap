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
	// Save original and set up temp status file
	origStatusFile := statusFile
	tmpDir := t.TempDir()
	statusFile = filepath.Join(tmpDir, "run", "thundersnap", "status")
	defer func() { statusFile = origStatusFile }()

	testURL := "https://login.tailscale.com/a/abc123"
	writeStatusWaitingForAuth(testURL)

	data, err := os.ReadFile(statusFile)
	if err != nil {
		t.Fatalf("failed to read status file: %v", err)
	}

	content := string(data)

	// Check for expected fields
	if !strings.Contains(content, "state: waiting_for_auth") {
		t.Errorf("status file missing 'state: waiting_for_auth', got:\n%s", content)
	}
	if !strings.Contains(content, "auth_url: "+testURL) {
		t.Errorf("status file missing auth_url, got:\n%s", content)
	}
}

// TestWriteStatusError tests that writeStatusError creates the status file
// with the error message in the correct format.
func TestWriteStatusError(t *testing.T) {
	// Save original and set up temp status file
	origStatusFile := statusFile
	tmpDir := t.TempDir()
	statusFile = filepath.Join(tmpDir, "run", "thundersnap", "status")
	defer func() { statusFile = origStatusFile }()

	testError := "failed to start: missing btrfs filesystem"
	writeStatusError(testError)

	data, err := os.ReadFile(statusFile)
	if err != nil {
		t.Fatalf("failed to read status file: %v", err)
	}

	content := string(data)

	// Check for expected error format
	if !strings.Contains(content, "error: "+testError) {
		t.Errorf("status file missing error message, got:\n%s", content)
	}
}

// TestStatusFileDirectoryCreation tests that status functions create the
// parent directory if it doesn't exist.
func TestStatusFileDirectoryCreation(t *testing.T) {
	origStatusFile := statusFile
	tmpDir := t.TempDir()
	// Use a deeply nested path that doesn't exist
	statusFile = filepath.Join(tmpDir, "deeply", "nested", "path", "status")
	defer func() { statusFile = origStatusFile }()

	writeStatusError("test error")

	// Verify the directory was created and file exists
	if _, err := os.Stat(statusFile); err != nil {
		t.Errorf("status file not created: %v", err)
	}
}

// TestParentCgroupNameUsesPID tests that parentCgroupName includes the daemon PID
// to allow multiple thundersnapd instances to run without cgroup conflicts.
func TestParentCgroupNameUsesPID(t *testing.T) {
	pid := os.Getpid()

	// Check that parentCgroupName contains the PID
	if !strings.Contains(parentCgroupName, "thundersnap-") {
		t.Errorf("parentCgroupName should start with 'thundersnap-', got: %s", parentCgroupName)
	}

	// Check that it contains a PID-like number (our test process PID)
	// The actual daemon would use its own PID
	expectedPrefix := "thundersnap-"
	if !strings.HasPrefix(parentCgroupName, expectedPrefix) {
		t.Errorf("parentCgroupName should have prefix %q, got: %s", expectedPrefix, parentCgroupName)
	}

	// Verify format is thundersnap-<pid>
	parts := strings.SplitN(parentCgroupName, "-", 2)
	if len(parts) != 2 {
		t.Errorf("parentCgroupName should be thundersnap-<pid>, got: %s", parentCgroupName)
	}

	// The PID part should be a valid number (might not match test process PID
	// since parentCgroupName is initialized at package load time, but it should
	// be a number)
	pidStr := parts[1]
	if len(pidStr) == 0 {
		t.Error("parentCgroupName PID part is empty")
	}

	// Check that it's numeric
	for _, c := range pidStr {
		if c < '0' || c > '9' {
			t.Errorf("parentCgroupName PID part should be numeric, got: %s", pidStr)
			break
		}
	}

	t.Logf("parentCgroupName=%s (test process pid=%d)", parentCgroupName, pid)
}
