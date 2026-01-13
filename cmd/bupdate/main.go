// bupdate reconstructs files from fidx indexes by combining local and remote chunks.
// Given a remote fidx file and a directory full of existing files with their fidx indexes,
// it downloads only the chunks that don't already exist locally and reconstructs the file.
package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pborman/getopt/v2"
)

const (
	// FIDX_VERSION is the fidx file format version
	FIDX_VERSION = 1
	// BLOB_MAX is the maximum chunk size
	BLOB_MAX = 8192 * 4 // 32768 bytes
)

var (
	localDir  = getopt.StringLong("local", 'l', ".", "local directory with existing files and .fidx indexes")
	remoteDir = getopt.StringLong("remote", 'r', "", "remote directory to read from")
	help      = getopt.BoolLong("help", 'h', "show help")
)

func usage() {
	getopt.PrintUsage(os.Stderr)
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Reconstructs files from fidx indexes by combining local and remote chunks.")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Example:")
	fmt.Fprintln(os.Stderr, "  bupdate --local ./cache --remote /mnt/repo file.bin.fidx")
	os.Exit(1)
}

// FidxEntry represents a single chunk in a fidx file
type FidxEntry struct {
	SHA   [20]byte // SHA-1 hash of the chunk (as git blob)
	Size  uint16   // size of chunk in bytes
	Level uint16   // hierarchical level from content-based chunking
}

// Fidx represents a parsed fidx file
type Fidx struct {
	Filename string
	Entries  []FidxEntry
	FileSHA  [20]byte // SHA-1 of the entire fidx file (excluding footer)
	FileSize int64    // total size of reconstructed file
}

// FidxMapping maps a chunk SHA to its location in a local file
type FidxMapping struct {
	SHA      [20]byte
	Filename string
	Offset   int64
	Size     uint16
}

// FidxMappings is a sorted collection of chunk mappings for fast lookup
type FidxMappings struct {
	Mappings []FidxMapping
}

func main() {
	getopt.SetParameters("<fidx-file>")
	getopt.SetUsage(usage)
	getopt.Parse()
	args := getopt.Args()

	if *help || len(args) != 1 {
		usage()
	}

	if *remoteDir == "" {
		fmt.Fprintln(os.Stderr, "error: --remote is required")
		os.Exit(1)
	}

	targetFidx := args[0]

	if err := bupdate(*localDir, *remoteDir, targetFidx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// bupdate performs the main reconstruction operation
func bupdate(localDir, remoteDir, targetFidx string) error {
	fmt.Printf("Loading local fidx files from: %s\n", localDir)

	// Load all local fidx files to build chunk mappings
	mappings, err := loadLocalMappings(localDir)
	if err != nil {
		return fmt.Errorf("loading local mappings: %w", err)
	}

	fmt.Printf("Loaded %d chunk mappings from local files.\n", len(mappings.Mappings))

	// Load the remote fidx file
	remoteFidxPath := filepath.Join(remoteDir, targetFidx)
	fmt.Printf("\nProcessing remote fidx: %s\n", remoteFidxPath)

	remoteFidx, err := loadFidx(remoteFidxPath)
	if err != nil {
		return fmt.Errorf("loading remote fidx: %w", err)
	}

	// Determine output filename (strip .fidx extension)
	outputName := strings.TrimSuffix(targetFidx, ".fidx")
	outputPath := filepath.Join(localDir, outputName)
	tmpOutputPath := outputPath + ".tmp"

	// Check if we already have this file
	localFidxPath := filepath.Join(localDir, targetFidx)
	if localFidx, err := loadFidx(localFidxPath); err == nil {
		if bytes.Equal(localFidx.FileSHA[:], remoteFidx.FileSHA[:]) {
			fmt.Printf("  already up to date.\n")
			return nil
		}
	}

	// Predict what we need to download
	missing, chunks := predictDownload(remoteFidx, mappings)
	fmt.Printf("  need to download %d/%d bytes in %d chunks.\n",
		missing, remoteFidx.FileSize, chunks)

	// Reconstruct the file
	remoteFilePath := filepath.Join(remoteDir, outputName)
	if err := reconstructFile(tmpOutputPath, remoteFidx, remoteFilePath, mappings); err != nil {
		os.Remove(tmpOutputPath)
		return fmt.Errorf("reconstructing file: %w", err)
	}

	// Atomically rename to final location
	if err := os.Rename(tmpOutputPath, outputPath); err != nil {
		return fmt.Errorf("rename: %w", err)
	}

	// Copy the fidx file to local
	if err := copyFile(localFidxPath, remoteFidxPath); err != nil {
		return fmt.Errorf("copying fidx: %w", err)
	}

	fmt.Printf("  successfully reconstructed: %s\n", outputPath)
	return nil
}

// loadLocalMappings scans the local directory for .fidx files and builds chunk mappings
func loadLocalMappings(dir string) (*FidxMappings, error) {
	var allMappings []FidxMapping

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".fidx") {
			continue
		}

		fidxPath := filepath.Join(dir, entry.Name())
		fidx, err := loadFidx(fidxPath)
		if err != nil {
			fmt.Printf("  warning: skipping %s: %v\n", entry.Name(), err)
			continue
		}

		// Get the actual file name (without .fidx)
		filename := strings.TrimSuffix(entry.Name(), ".fidx")
		filePath := filepath.Join(dir, filename)

		// Check if the file exists
		if _, err := os.Stat(filePath); err != nil {
			continue
		}

		fmt.Printf("  %s\n", entry.Name())

		// Add mappings for this file
		var offset int64
		for _, ent := range fidx.Entries {
			allMappings = append(allMappings, FidxMapping{
				SHA:      ent.SHA,
				Filename: filePath,
				Offset:   offset,
				Size:     ent.Size,
			})
			offset += int64(ent.Size)
		}
	}

	// Sort mappings by SHA for binary search
	sort.Slice(allMappings, func(i, j int) bool {
		return bytes.Compare(allMappings[i].SHA[:], allMappings[j].SHA[:]) < 0
	})

	return &FidxMappings{Mappings: allMappings}, nil
}

// loadFidx reads and parses a fidx file
func loadFidx(path string) (*Fidx, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	if len(data) < 8+20 { // header + footer
		return nil, fmt.Errorf("file too short")
	}

	// Check header
	if string(data[0:4]) != "FIDX" {
		return nil, fmt.Errorf("invalid FIDX magic")
	}

	version := binary.BigEndian.Uint32(data[4:8])
	if version != FIDX_VERSION {
		return nil, fmt.Errorf("unsupported version: %d", version)
	}

	// Extract footer (last 20 bytes = SHA-1 of everything before it)
	footerSHA := data[len(data)-20:]
	data = data[:len(data)-20] // Remove footer from data

	// Verify SHA-1 checksum
	h := sha1.New()
	h.Write(data)
	computedSHA := h.Sum(nil)
	if !bytes.Equal(computedSHA, footerSHA) {
		return nil, fmt.Errorf("fidx checksum mismatch")
	}

	// Parse entries (skip 8-byte header)
	entryData := data[8:]
	if len(entryData)%24 != 0 {
		return nil, fmt.Errorf("invalid entry data length")
	}

	numEntries := len(entryData) / 24
	entries := make([]FidxEntry, numEntries)
	var fileSize int64

	for i := 0; i < numEntries; i++ {
		offset := i * 24
		var ent FidxEntry
		copy(ent.SHA[:], entryData[offset:offset+20])
		ent.Size = binary.BigEndian.Uint16(entryData[offset+20 : offset+22])
		ent.Level = binary.BigEndian.Uint16(entryData[offset+22 : offset+24])
		entries[i] = ent
		fileSize += int64(ent.Size)
	}

	fidx := &Fidx{
		Filename: path,
		Entries:  entries,
		FileSize: fileSize,
	}
	copy(fidx.FileSHA[:], computedSHA)

	return fidx, nil
}

// findMapping performs binary search to find a chunk by SHA
func (m *FidxMappings) findMapping(sha [20]byte) *FidxMapping {
	i := sort.Search(len(m.Mappings), func(i int) bool {
		return bytes.Compare(m.Mappings[i].SHA[:], sha[:]) >= 0
	})

	if i < len(m.Mappings) && bytes.Equal(m.Mappings[i].SHA[:], sha[:]) {
		return &m.Mappings[i]
	}
	return nil
}

// predictDownload calculates how much data needs to be downloaded
func predictDownload(fidx *Fidx, mappings *FidxMappings) (missing int64, chunks int) {
	for _, ent := range fidx.Entries {
		if mappings.findMapping(ent.SHA) == nil {
			missing += int64(ent.Size)
			chunks++
		}
	}
	return
}

// reconstructFile rebuilds the output file by combining local and remote chunks
func reconstructFile(outputPath string, fidx *Fidx, remoteFilePath string, mappings *FidxMappings) error {
	outf, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer outf.Close()

	// Open remote file for reading chunks we don't have locally
	remotef, err := os.Open(remoteFilePath)
	if err != nil {
		return fmt.Errorf("opening remote file: %w", err)
	}
	defer remotef.Close()

	var remoteOffset int64
	got := int64(0)
	missing := int64(0)

	// Calculate total missing for progress
	for _, ent := range fidx.Entries {
		if mappings.findMapping(ent.SHA) == nil {
			missing += int64(ent.Size)
		}
	}

	// Process each chunk
	for _, ent := range fidx.Entries {
		chunkSize := int64(ent.Size)
		mapping := mappings.findMapping(ent.SHA)

		if mapping != nil {
			// We have this chunk locally - read and verify it
			localData, err := readChunk(mapping.Filename, mapping.Offset, int64(mapping.Size))
			if err != nil {
				// Failed to read local chunk, fall back to remote
				mapping = nil
			} else {
				// Verify SHA matches (as git blob)
				computedSHA := blobSHA(localData)
				if bytes.Equal(computedSHA[:], ent.SHA[:]) {
					// Write verified local chunk
					if _, err := outf.Write(localData); err != nil {
						return fmt.Errorf("writing local chunk: %w", err)
					}
				} else {
					// Checksum mismatch, fall back to remote
					fmt.Printf("    checksum mismatch in local file\n")
					mapping = nil
				}
			}
		}

		if mapping == nil {
			// Need to fetch from remote
			remoteData, err := readChunk(remoteFilePath, remoteOffset, chunkSize)
			if err != nil {
				return fmt.Errorf("reading remote chunk at offset %d: %w", remoteOffset, err)
			}

			// Verify remote chunk
			computedSHA := blobSHA(remoteData)
			if !bytes.Equal(computedSHA[:], ent.SHA[:]) {
				return fmt.Errorf("remote chunk checksum mismatch at offset %d", remoteOffset)
			}

			if _, err := outf.Write(remoteData); err != nil {
				return fmt.Errorf("writing remote chunk: %w", err)
			}

			got += chunkSize
			if missing > 0 {
				pct := (got * 100) / missing
				fmt.Printf("\r  Downloading... %d%% (%d/%d bytes)", pct, got, missing)
			}
		}

		remoteOffset += chunkSize
	}

	if missing > 0 {
		fmt.Println() // newline after progress
	}

	return nil
}

// readChunk reads a chunk from a file at the specified offset
func readChunk(filename string, offset, size int64) ([]byte, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if _, err := f.Seek(offset, 0); err != nil {
		return nil, err
	}

	data := make([]byte, size)
	n, err := io.ReadFull(f, data)
	if err != nil {
		return nil, err
	}
	if int64(n) != size {
		return nil, fmt.Errorf("short read: expected %d, got %d", size, n)
	}

	return data, nil
}

// blobSHA computes the git blob SHA-1 of data
func blobSHA(data []byte) [20]byte {
	h := sha1.New()
	// Git blob format: "blob <size>\0<data>"
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	var result [20]byte
	copy(result[:], h.Sum(nil))
	return result
}

// copyFile copies a file from src to dst
func copyFile(dst, src string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0644)
}
