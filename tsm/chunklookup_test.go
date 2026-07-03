// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package tsm

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestChunkMapFindChunk(t *testing.T) {
	sha1 := [32]byte{0x01}
	sha2 := [32]byte{0x02}
	sha3 := [32]byte{0x03}

	m := &ChunkMap{
		Locations: []ChunkLocation{
			{SHA256: sha1, Filename: "file1", Offset: 0, Size: 100},
			{SHA256: sha2, Filename: "file2", Offset: 100, Size: 200},
			{SHA256: sha3, Filename: "file3", Offset: 300, Size: 300},
		},
	}

	// Test finding existing chunks
	if loc, found := m.FindChunk(sha1); !found {
		t.Error("sha1 not found")
	} else if loc.Filename != "file1" {
		t.Errorf("sha1 filename = %s, want file1", loc.Filename)
	}

	if loc, found := m.FindChunk(sha2); !found {
		t.Error("sha2 not found")
	} else if loc.Size != 200 {
		t.Errorf("sha2 size = %d, want 200", loc.Size)
	}

	if loc, found := m.FindChunk(sha3); !found {
		t.Error("sha3 not found")
	} else if loc.Offset != 300 {
		t.Errorf("sha3 offset = %d, want 300", loc.Offset)
	}

	// Test not finding non-existent chunk
	sha4 := [32]byte{0x04}
	if _, found := m.FindChunk(sha4); found {
		t.Error("sha4 should not be found")
	}

	// Test empty map
	empty := &ChunkMap{}
	if _, found := empty.FindChunk(sha1); found {
		t.Error("empty map should not find anything")
	}
}

func TestLoadLocalChunkMap(t *testing.T) {
	// Create a temp directory with test TSC/TSM files
	tmpDir := t.TempDir()

	// Create a minimal snapshot structure:
	// tmpDir/
	//   snap1.tsc
	//   snap1.tsm
	//   snap1/
	//     testfile.txt

	snapName := "snap1"
	snapDir := filepath.Join(tmpDir, snapName)
	if err := os.MkdirAll(snapDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a test file
	testContent := []byte("Hello, World! This is test content for chunking.\n")
	testFile := filepath.Join(snapDir, "testfile.txt")
	if err := os.WriteFile(testFile, testContent, 0644); err != nil {
		t.Fatal(err)
	}

	// Generate TSM/TSC for the snapshot
	outBase := filepath.Join(tmpDir, snapName)
	if err := Create(snapDir, outBase, IndexerOptions{}); err != nil {
		t.Fatal(err)
	}

	// Verify files were created
	tscPath := outBase + ".tsc"
	tsmPath := outBase + ".tsm"
	if _, err := os.Stat(tscPath); err != nil {
		t.Fatalf("TSC not created: %v", err)
	}
	if _, err := os.Stat(tsmPath); err != nil {
		t.Fatalf("TSM not created: %v", err)
	}

	// Load the chunk map
	m, err := LoadLocalChunkMap(tmpDir)
	if err != nil {
		t.Fatalf("LoadLocalChunkMap: %v", err)
	}

	if len(m.Locations) == 0 {
		t.Error("no chunks found")
	}

	t.Logf("Found %d chunk locations", len(m.Locations))

	// Verify chunks are sorted
	for i := 1; i < len(m.Locations); i++ {
		if bytes.Compare(m.Locations[i-1].SHA256[:], m.Locations[i].SHA256[:]) > 0 {
			t.Errorf("chunks not sorted at index %d", i)
		}
	}

	// Verify we can find a chunk
	if len(m.Locations) > 0 {
		sha := m.Locations[0].SHA256
		if loc, found := m.FindChunk(sha); !found {
			t.Error("first chunk not found")
		} else {
			t.Logf("Found chunk: file=%s offset=%d size=%d", loc.Filename, loc.Offset, loc.Size)
		}
	}
}

func TestLoadLocalChunkMapEmptyDir(t *testing.T) {
	tmpDir := t.TempDir()

	m, err := LoadLocalChunkMap(tmpDir)
	if err != nil {
		t.Fatalf("LoadLocalChunkMap on empty dir: %v", err)
	}

	if len(m.Locations) != 0 {
		t.Errorf("expected 0 locations, got %d", len(m.Locations))
	}
}

func TestLoadLocalChunkMapNonExistentDir(t *testing.T) {
	m, err := LoadLocalChunkMap("/nonexistent/path")
	if err != nil {
		t.Fatalf("LoadLocalChunkMap on nonexistent dir: %v", err)
	}

	if len(m.Locations) != 0 {
		t.Errorf("expected 0 locations, got %d", len(m.Locations))
	}
}
