package tsm

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ChunkLocation maps a chunk SHA-256 to its location in a local file.
// This is the TSC-based equivalent of bupdate.FidxMapping.
type ChunkLocation struct {
	SHA256   [32]byte
	Filename string // Full path to the file containing this chunk
	Offset   int64  // Byte offset within the file
	Size     uint32 // Chunk size
}

// ReadData reads the chunk data from its file location.
// Returns the data and nil on success, or nil and an error on failure.
func (loc *ChunkLocation) ReadData() ([]byte, error) {
	f, err := os.Open(loc.Filename)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", loc.Filename, err)
	}
	defer f.Close()

	if _, err := f.Seek(loc.Offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seeking to offset %d: %w", loc.Offset, err)
	}

	data := make([]byte, loc.Size)
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, fmt.Errorf("reading %d bytes: %w", loc.Size, err)
	}

	return data, nil
}

// VerifyAndRead reads the chunk data and verifies its SHA-256.
// Returns the data if the hash matches, or an error if it doesn't.
func (loc *ChunkLocation) VerifyAndRead() ([]byte, error) {
	data, err := loc.ReadData()
	if err != nil {
		return nil, err
	}

	computed := BlobSHA256(data)
	if computed != loc.SHA256 {
		return nil, fmt.Errorf("chunk hash mismatch: expected %x, got %x", loc.SHA256, computed)
	}

	return data, nil
}

// ChunkMap is a sorted collection of chunk locations for fast lookup.
// It provides O(log n) lookup by SHA-256.
type ChunkMap struct {
	Locations []ChunkLocation
}

// FindChunk looks up a chunk by SHA-256 using binary search.
// Returns the location and true if found, or nil and false if not found.
func (m *ChunkMap) FindChunk(sha [32]byte) (*ChunkLocation, bool) {
	if len(m.Locations) == 0 {
		return nil, false
	}

	idx := sort.Search(len(m.Locations), func(i int) bool {
		return bytes.Compare(m.Locations[i].SHA256[:], sha[:]) >= 0
	})

	if idx < len(m.Locations) && bytes.Equal(m.Locations[idx].SHA256[:], sha[:]) {
		return &m.Locations[idx], true
	}
	return nil, false
}

// LoadLocalChunkMap scans a directory for .tsc files and builds a mapping
// from SHA-256 hashes to file locations. This enables local chunk deduplication
// when downloading new snapshots.
//
// For each .tsc file found, it:
// 1. Parses the TSC to get the chunk list
// 2. Looks for the corresponding .tsm file to get file-to-chunk mappings
// 3. For each file in the TSM, calculates chunk offsets and adds to the map
//
// This is the TSM/TSC-based equivalent of bupdate.loadLocalMappings.
func LoadLocalChunkMap(snapshotsDir string) (*ChunkMap, error) {
	entries, err := os.ReadDir(snapshotsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return &ChunkMap{}, nil
		}
		return nil, err
	}

	var allLocations []ChunkLocation

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		// Only process .tsc files
		if !strings.HasSuffix(entry.Name(), ".tsc") {
			continue
		}

		tscPath := filepath.Join(snapshotsDir, entry.Name())
		baseName := strings.TrimSuffix(entry.Name(), ".tsc")
		tsmPath := filepath.Join(snapshotsDir, baseName+".tsm")
		snapDir := filepath.Join(snapshotsDir, baseName)

		// Both TSM and snapshot directory must exist
		if _, err := os.Stat(tsmPath); err != nil {
			continue
		}
		if _, err := os.Stat(snapDir); err != nil {
			continue
		}

		// Parse TSC and TSM
		tscReader, err := ReadTSC(tscPath)
		if err != nil {
			continue // skip invalid TSC files
		}

		tsmReader, err := ReadTSM(tsmPath)
		if err != nil {
			continue // skip invalid TSM files
		}

		// For each file entry in TSM, compute chunk offsets
		for _, fileEntry := range tsmReader.Entries {
			if fileEntry.Type != EntryTypeFile || fileEntry.ChunkCount == 0 {
				continue
			}

			filePath := filepath.Join(snapDir, fileEntry.Path)
			if _, err := os.Stat(filePath); err != nil {
				continue // file doesn't exist
			}

			// Calculate offsets for each chunk
			var offset int64
			for _, tscIdx := range fileEntry.ChunkRefs {
				if int(tscIdx) >= len(tscReader.Entries) {
					break // invalid reference
				}

				chunk := tscReader.Entries[tscIdx]
				allLocations = append(allLocations, ChunkLocation{
					SHA256:   chunk.SHA256,
					Filename: filePath,
					Offset:   offset,
					Size:     chunk.Size,
				})
				offset += int64(chunk.Size)
			}
		}
	}

	// Sort by SHA-256 for binary search
	sort.Slice(allLocations, func(i, j int) bool {
		return bytes.Compare(allLocations[i].SHA256[:], allLocations[j].SHA256[:]) < 0
	})

	return &ChunkMap{Locations: allLocations}, nil
}
