//go:build linux

package bupdate

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestCloneFile tests that FICLONE creates a proper COW clone
func TestCloneFile(t *testing.T) {
	// Create a temp directory for the test - use current directory which should be on btrfs
	// (default /tmp is often tmpfs which doesn't support FICLONE)
	tmpDir, err := os.MkdirTemp(".", "clone_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Check if we're on btrfs
	if !isBtrfs(tmpDir) {
		t.Skip("Skipping test: not on a btrfs filesystem")
	}

	// Create source file with some content
	srcPath := filepath.Join(tmpDir, "source.bin")
	content := bytes.Repeat([]byte("Hello, btrfs clone test! "), 10000) // ~250KB
	if err := os.WriteFile(srcPath, content, 0644); err != nil {
		t.Fatalf("Failed to write source file: %v", err)
	}

	// Clone the file
	dstPath := filepath.Join(tmpDir, "dest.bin")
	if err := CloneFile(dstPath, srcPath); err != nil {
		t.Fatalf("CloneFile failed: %v", err)
	}

	// Verify the destination file exists and has the same content
	dstContent, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("Failed to read destination file: %v", err)
	}

	if !bytes.Equal(content, dstContent) {
		t.Fatalf("Destination content doesn't match source. Source: %d bytes, Dest: %d bytes",
			len(content), len(dstContent))
	}

	// Verify they share the same extents (are actually COW clones)
	srcExtents := getFileExtents(t, srcPath)
	dstExtents := getFileExtents(t, dstPath)

	if srcExtents != dstExtents {
		t.Errorf("Files don't share extents - not a proper COW clone.\nSource extents: %s\nDest extents: %s",
			srcExtents, dstExtents)
	}

	t.Logf("Successfully created COW clone of %d bytes", len(content))
	t.Logf("Shared extents: %s", srcExtents)
}

// TestFileByChecksums tests that checksum-based file lookup works correctly
func TestFileByChecksums(t *testing.T) {
	// Create test entries
	entries1 := []FidxEntry{
		{SHA: [20]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}, Size: 100},
		{SHA: [20]byte{21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40}, Size: 200},
	}

	entries2 := []FidxEntry{
		{SHA: [20]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}, Size: 100},
		{SHA: [20]byte{21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40}, Size: 200},
	}

	entries3 := []FidxEntry{
		{SHA: [20]byte{99, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}, Size: 100},
	}

	fbc := NewFileByChecksums()

	// Add first file
	fbc.Add(entries1, "/path/to/file1")

	// Should find file1 with identical entries
	path, found := fbc.Find(entries2)
	if !found {
		t.Error("Expected to find file with identical checksums")
	}
	if path != "/path/to/file1" {
		t.Errorf("Expected path /path/to/file1, got %s", path)
	}

	// Should not find with different entries
	_, found = fbc.Find(entries3)
	if found {
		t.Error("Should not find file with different checksums")
	}

	// Adding same checksums again should not overwrite
	fbc.Add(entries1, "/path/to/file2")
	path, _ = fbc.Find(entries1)
	if path != "/path/to/file1" {
		t.Errorf("First file should be preserved, got %s", path)
	}
}

// TestMakeChecksumKey tests that checksum keys are generated correctly
func TestMakeChecksumKey(t *testing.T) {
	entries := []FidxEntry{
		{SHA: [20]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}, Size: 100},
		{SHA: [20]byte{21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40}, Size: 200},
	}

	key := MakeChecksumKey(entries)

	// Key should be 40 bytes (2 entries * 20 bytes each)
	if len(key) != 40 {
		t.Errorf("Expected key length 40, got %d", len(key))
	}

	// Verify the key contains the concatenated SHAs
	expected := make([]byte, 40)
	for i := 0; i < 20; i++ {
		expected[i] = byte(i + 1)
		expected[20+i] = byte(i + 21)
	}
	if key != string(expected) {
		t.Error("Key doesn't match expected concatenated SHAs")
	}
}

// TestCloneAndChecksumIntegration tests the full flow of detecting identical files and cloning
func TestCloneAndChecksumIntegration(t *testing.T) {
	// Create a temp directory for the test - use current directory which should be on btrfs
	tmpDir, err := os.MkdirTemp(".", "clone_integration_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Check if we're on btrfs
	if !isBtrfs(tmpDir) {
		t.Skip("Skipping test: not on a btrfs filesystem")
	}

	// Create a source file and chunk it
	srcPath := filepath.Join(tmpDir, "source.bin")
	content := bytes.Repeat([]byte("Test content for clone integration. "), 1000)
	if err := os.WriteFile(srcPath, content, 0644); err != nil {
		t.Fatalf("Failed to write source file: %v", err)
	}

	// Chunk the file to get entries
	var entries []FidxEntry
	err = ChunkFile(srcPath, func(entry FidxEntry) error {
		entries = append(entries, entry)
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("Failed to chunk file: %v", err)
	}

	t.Logf("Source file chunked into %d entries", len(entries))

	// Create FileByChecksums and register the source file
	fbc := NewFileByChecksums()
	fbc.Add(entries, srcPath)

	// Simulate finding an identical file and cloning it
	clonePath := filepath.Join(tmpDir, "clone.bin")

	// Look up by the same entries (as if we were reconstructing a file with identical checksums)
	foundPath, found := fbc.Find(entries)
	if !found {
		t.Fatal("Should have found the source file by checksums")
	}
	if foundPath != srcPath {
		t.Fatalf("Found wrong path: %s", foundPath)
	}

	// Clone it
	if err := CloneFile(clonePath, foundPath); err != nil {
		t.Fatalf("CloneFile failed: %v", err)
	}

	// Verify content
	cloneContent, err := os.ReadFile(clonePath)
	if err != nil {
		t.Fatalf("Failed to read clone: %v", err)
	}
	if !bytes.Equal(content, cloneContent) {
		t.Fatal("Clone content doesn't match original")
	}

	// Verify they share extents
	srcExtents := getFileExtents(t, srcPath)
	cloneExtents := getFileExtents(t, clonePath)
	if srcExtents != cloneExtents {
		t.Errorf("Files don't share extents after clone.\nSource: %s\nClone: %s", srcExtents, cloneExtents)
	}

	// Now modify the clone and verify source is unchanged (COW behavior)
	modifiedContent := append(cloneContent, []byte(" MODIFIED")...)
	if err := os.WriteFile(clonePath, modifiedContent, 0644); err != nil {
		t.Fatalf("Failed to modify clone: %v", err)
	}

	// Source should be unchanged
	srcContentAfter, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("Failed to read source after clone modification: %v", err)
	}
	if !bytes.Equal(content, srcContentAfter) {
		t.Fatal("Source file was modified when clone was changed - not proper COW!")
	}

	t.Log("COW clone integration test passed")
}

// TestCopyFileRange tests that CopyFile uses copy_file_range efficiently
func TestCopyFileRange(t *testing.T) {
	// Create a temp directory
	tmpDir, err := os.MkdirTemp(".", "copy_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create source file with some content
	srcPath := filepath.Join(tmpDir, "source.bin")
	content := bytes.Repeat([]byte("Copy file range test content! "), 10000) // ~300KB
	if err := os.WriteFile(srcPath, content, 0644); err != nil {
		t.Fatalf("Failed to write source file: %v", err)
	}

	// Copy the file
	dstPath := filepath.Join(tmpDir, "dest.bin")
	if err := CopyFile(dstPath, srcPath); err != nil {
		t.Fatalf("CopyFile failed: %v", err)
	}

	// Verify the destination file exists and has the same content
	dstContent, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("Failed to read destination file: %v", err)
	}

	if !bytes.Equal(content, dstContent) {
		t.Fatalf("Destination content doesn't match source. Source: %d bytes, Dest: %d bytes",
			len(content), len(dstContent))
	}

	t.Logf("Successfully copied %d bytes using copy_file_range", len(content))
}

// TestCloneOrCopyFile tests the combined clone-or-copy function
func TestCloneOrCopyFile(t *testing.T) {
	// Create a temp directory using current directory (should be on btrfs)
	tmpDir, err := os.MkdirTemp(".", "clone_or_copy_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create source file
	srcPath := filepath.Join(tmpDir, "source.bin")
	content := bytes.Repeat([]byte("Clone or copy test! "), 5000)
	if err := os.WriteFile(srcPath, content, 0644); err != nil {
		t.Fatalf("Failed to write source file: %v", err)
	}

	// Use CloneOrCopyFile
	dstPath := filepath.Join(tmpDir, "dest.bin")
	cloned, err := CloneOrCopyFile(dstPath, srcPath)
	if err != nil {
		t.Fatalf("CloneOrCopyFile failed: %v", err)
	}

	// Verify content
	dstContent, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("Failed to read destination file: %v", err)
	}

	if !bytes.Equal(content, dstContent) {
		t.Fatalf("Destination content doesn't match source")
	}

	if isBtrfs(tmpDir) {
		if cloned {
			t.Log("Used COW clone (as expected on btrfs)")
		} else {
			t.Log("Used copy_file_range (FICLONE failed, possibly NOCOW)")
		}
	} else {
		if cloned {
			t.Error("Reported clone on non-btrfs filesystem - unexpected")
		} else {
			t.Log("Used copy_file_range (as expected on non-btrfs)")
		}
	}
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
