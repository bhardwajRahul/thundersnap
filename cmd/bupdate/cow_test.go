//go:build linux

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tailscale/thundersnap/bupdate"
)

// TestCOWCloneOnMatchingChecksum verifies that when restoring a file that exactly
// matches the checksum of a file in the local fidx, bupdate uses a btrfs full-file
// COW copy instead of reconstructing it one chunk at a time.
//
// This tests the single-file fidx path in runBupdate.
func TestCOWCloneOnMatchingChecksum(t *testing.T) {
	// Create temp directories using current directory (should be on btrfs)
	tmpDir, err := os.MkdirTemp(".", "cow_test")
	if err != nil {
		t.Fatalf("creating temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Check if we're on btrfs
	if !isBtrfs(tmpDir) {
		t.Skip("Skipping test: not on a btrfs filesystem")
	}

	localDir := filepath.Join(tmpDir, "local")
	remoteDir := filepath.Join(tmpDir, "remote")

	if err := os.MkdirAll(localDir, 0755); err != nil {
		t.Fatalf("creating local dir: %v", err)
	}
	if err := os.MkdirAll(remoteDir, 0755); err != nil {
		t.Fatalf("creating remote dir: %v", err)
	}

	// Create test content - large enough to have multiple chunks
	testContent := bytes.Repeat([]byte("COW clone test content for bupdate. "), 2000) // ~72KB

	// Create a source file in local directory
	sourceFile := filepath.Join(localDir, "source.bin")
	if err := os.WriteFile(sourceFile, testContent, 0644); err != nil {
		t.Fatalf("writing source file: %v", err)
	}

	// Create fidx for the source file
	sourceFidxPath := filepath.Join(localDir, "source.bin.fidx")
	if err := bupdate.CreateSingleFidx(sourceFile, sourceFidxPath, bupdate.IndexerOptions{}); err != nil {
		t.Fatalf("creating source fidx: %v", err)
	}

	// Create the "remote" directory structure for a different fidx name but identical content
	// Structure: remote/target123.fidx and remote/target123/bin
	targetHash := "target123"
	remoteFileDir := filepath.Join(remoteDir, targetHash)
	if err := os.MkdirAll(remoteFileDir, 0755); err != nil {
		t.Fatalf("creating remote file dir: %v", err)
	}

	// Write identical content to remote
	remoteFile := filepath.Join(remoteFileDir, "bin")
	if err := os.WriteFile(remoteFile, testContent, 0644); err != nil {
		t.Fatalf("writing remote file: %v", err)
	}

	// Create fidx for the remote file
	remoteFidxPath := filepath.Join(remoteDir, targetHash+".fidx")
	if err := bupdate.CreateSingleFidx(remoteFile, remoteFidxPath, bupdate.IndexerOptions{}); err != nil {
		t.Fatalf("creating remote fidx: %v", err)
	}

	// Run bupdate - it should detect that local source.bin has identical checksums
	// and use COW clone instead of chunk-by-chunk reconstruction
	done := make(chan error, 1)
	go func() {
		done <- runBupdate(localDir, remoteDir, targetHash+".fidx")
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("bupdate failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("bupdate timed out")
	}

	// Verify the output file was created
	outputFile := filepath.Join(localDir, targetHash)
	outputContent, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}

	if !bytes.Equal(outputContent, testContent) {
		t.Errorf("output content mismatch: got %d bytes, want %d bytes", len(outputContent), len(testContent))
	}

	// Verify that the output file shares extents with source.bin (proving COW clone was used)
	sourceExtents := getFileExtents(t, sourceFile)
	outputExtents := getFileExtents(t, outputFile)

	if sourceExtents == "" || outputExtents == "" {
		t.Skip("Cannot verify extents - filefrag not available")
	}

	if sourceExtents != outputExtents {
		t.Errorf("Files don't share extents - COW clone was NOT used.\nSource extents: %s\nOutput extents: %s",
			sourceExtents, outputExtents)
	} else {
		t.Logf("COW clone verified - files share extents: %s", sourceExtents)
	}
}

// TestCOWCloneOnMatchingChecksumMFIDX verifies COW clone behavior for multi-file indexes.
func TestCOWCloneOnMatchingChecksumMFIDX(t *testing.T) {
	// Create temp directories using current directory (should be on btrfs)
	tmpDir, err := os.MkdirTemp(".", "cow_mfidx_test")
	if err != nil {
		t.Fatalf("creating temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Check if we're on btrfs
	if !isBtrfs(tmpDir) {
		t.Skip("Skipping test: not on a btrfs filesystem")
	}

	localDir := filepath.Join(tmpDir, "local")
	remoteDir := filepath.Join(tmpDir, "remote")

	if err := os.MkdirAll(localDir, 0755); err != nil {
		t.Fatalf("creating local dir: %v", err)
	}
	if err := os.MkdirAll(remoteDir, 0755); err != nil {
		t.Fatalf("creating remote dir: %v", err)
	}

	// Create test content - large enough to have multiple chunks
	testContent := bytes.Repeat([]byte("MFIDX COW clone test content. "), 2000) // ~60KB

	// Create a source file in local directory with its mfidx
	sourceHash := "sourcemfidx123"
	localSourceDir := filepath.Join(localDir, sourceHash)
	if err := os.MkdirAll(localSourceDir, 0755); err != nil {
		t.Fatalf("creating local source dir: %v", err)
	}

	sourceFile := filepath.Join(localSourceDir, "data.bin")
	if err := os.WriteFile(sourceFile, testContent, 0644); err != nil {
		t.Fatalf("writing source file: %v", err)
	}

	// Create mfidx for the source file
	sourceMfidxPath := filepath.Join(localDir, sourceHash+".mfidx")
	if err := createTestMfidx(sourceFile, "data.bin", sourceMfidxPath); err != nil {
		t.Fatalf("creating source mfidx: %v", err)
	}

	// Create the "remote" directory structure with a different mfidx name but identical file content
	targetHash := "targetmfidx456"
	remoteFileDir := filepath.Join(remoteDir, targetHash)
	if err := os.MkdirAll(remoteFileDir, 0755); err != nil {
		t.Fatalf("creating remote file dir: %v", err)
	}

	// Write identical content to remote with a different filename
	remoteFile := filepath.Join(remoteFileDir, "other.bin")
	if err := os.WriteFile(remoteFile, testContent, 0644); err != nil {
		t.Fatalf("writing remote file: %v", err)
	}

	// Create mfidx for the remote file
	remoteMfidxPath := filepath.Join(remoteDir, targetHash+".mfidx")
	if err := createTestMfidx(remoteFile, "other.bin", remoteMfidxPath); err != nil {
		t.Fatalf("creating remote mfidx: %v", err)
	}

	// Run bupdate - it should detect that local data.bin has identical checksums
	// to remote other.bin and use COW clone
	done := make(chan error, 1)
	go func() {
		done <- runBupdate(localDir, remoteDir, targetHash+".mfidx")
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("bupdate failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("bupdate timed out")
	}

	// Verify the output file was created
	outputFile := filepath.Join(localDir, targetHash, "other.bin")
	outputContent, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}

	if !bytes.Equal(outputContent, testContent) {
		t.Errorf("output content mismatch: got %d bytes, want %d bytes", len(outputContent), len(testContent))
	}

	// Verify that the output file shares extents with source data.bin
	sourceExtents := getFileExtents(t, sourceFile)
	outputExtents := getFileExtents(t, outputFile)

	if sourceExtents == "" || outputExtents == "" {
		t.Skip("Cannot verify extents - filefrag not available")
	}

	if sourceExtents != outputExtents {
		t.Errorf("Files don't share extents - COW clone was NOT used.\nSource extents: %s\nOutput extents: %s",
			sourceExtents, outputExtents)
	} else {
		t.Logf("COW clone verified - files share extents: %s", sourceExtents)
	}
}

// TestCOWCloneFallsBackToReconstruction verifies that when COW clone fails
// (e.g., on a non-btrfs filesystem), bupdate falls back to chunk reconstruction.
func TestCOWCloneFallsBackToReconstruction(t *testing.T) {
	// Use /tmp which is usually tmpfs (doesn't support FICLONE)
	tmpDir, err := os.MkdirTemp("/tmp", "cow_fallback_test")
	if err != nil {
		t.Fatalf("creating temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Skip if /tmp happens to be on btrfs
	if isBtrfs(tmpDir) {
		t.Skip("Skipping test: /tmp is on btrfs, cannot test fallback")
	}

	localDir := filepath.Join(tmpDir, "local")
	remoteDir := filepath.Join(tmpDir, "remote")

	if err := os.MkdirAll(localDir, 0755); err != nil {
		t.Fatalf("creating local dir: %v", err)
	}
	if err := os.MkdirAll(remoteDir, 0755); err != nil {
		t.Fatalf("creating remote dir: %v", err)
	}

	// Create test content
	testContent := bytes.Repeat([]byte("Fallback test content. "), 1000)

	// Create a source file in local directory
	sourceFile := filepath.Join(localDir, "source.bin")
	if err := os.WriteFile(sourceFile, testContent, 0644); err != nil {
		t.Fatalf("writing source file: %v", err)
	}

	// Create fidx for the source file
	sourceFidxPath := filepath.Join(localDir, "source.bin.fidx")
	if err := bupdate.CreateSingleFidx(sourceFile, sourceFidxPath, bupdate.IndexerOptions{}); err != nil {
		t.Fatalf("creating source fidx: %v", err)
	}

	// Create remote directory structure
	targetHash := "fallback123"
	remoteFileDir := filepath.Join(remoteDir, targetHash)
	if err := os.MkdirAll(remoteFileDir, 0755); err != nil {
		t.Fatalf("creating remote file dir: %v", err)
	}

	remoteFile := filepath.Join(remoteFileDir, "bin")
	if err := os.WriteFile(remoteFile, testContent, 0644); err != nil {
		t.Fatalf("writing remote file: %v", err)
	}

	remoteFidxPath := filepath.Join(remoteDir, targetHash+".fidx")
	if err := bupdate.CreateSingleFidx(remoteFile, remoteFidxPath, bupdate.IndexerOptions{}); err != nil {
		t.Fatalf("creating remote fidx: %v", err)
	}

	// Run bupdate - COW clone will fail, but reconstruction should succeed
	done := make(chan error, 1)
	go func() {
		done <- runBupdate(localDir, remoteDir, targetHash+".fidx")
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("bupdate failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("bupdate timed out")
	}

	// Verify the output file was created with correct content
	outputFile := filepath.Join(localDir, targetHash)
	outputContent, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}

	if !bytes.Equal(outputContent, testContent) {
		t.Errorf("output content mismatch: got %d bytes, want %d bytes", len(outputContent), len(testContent))
	}

	t.Logf("Fallback reconstruction succeeded on non-btrfs filesystem")
}

// isBtrfs checks if the given path is on a btrfs filesystem
func isBtrfs(path string) bool {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return false
	}
	// btrfs magic number is 0x9123683E
	return stat.Type == 0x9123683E
}

// getFileExtents returns a string representation of file extents using filefrag
func getFileExtents(t *testing.T, path string) string {
	cmd := exec.Command("filefrag", "-v", path)
	output, err := cmd.Output()
	if err != nil {
		// filefrag might not be available, just return empty
		t.Logf("filefrag not available: %v", err)
		return ""
	}

	// Extract just the physical offset lines for comparison
	lines := strings.Split(string(output), "\n")
	var extents []string
	for _, line := range lines {
		if strings.Contains(line, "..") && strings.Contains(line, ":") {
			// This looks like an extent line
			extents = append(extents, strings.TrimSpace(line))
		}
	}
	return strings.Join(extents, "\n")
}
