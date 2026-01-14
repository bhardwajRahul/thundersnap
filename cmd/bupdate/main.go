// bupdate reconstructs files from fidx indexes by combining local and remote chunks.
// Given a remote fidx file and a directory full of existing files with their fidx indexes,
// it downloads only the chunks that don't already exist locally and reconstructs the file.
package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pborman/getopt/v2"
	"github.com/tailscale/thundersnap/bupdate"
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

	if err := runBupdate(*localDir, *remoteDir, targetFidx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// runBupdate performs the main reconstruction operation
func runBupdate(localDir, remoteDir, targetFidx string) error {
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

	remoteFidx, err := bupdate.LoadFidx(remoteFidxPath)
	if err != nil {
		return fmt.Errorf("loading remote fidx: %w", err)
	}

	// Check if we already have this index
	localFidxPath := filepath.Join(localDir, targetFidx)
	if localFidx, err := bupdate.LoadFidx(localFidxPath); err == nil {
		if bytes.Equal(localFidx.FileSHA[:], remoteFidx.FileSHA[:]) {
			fmt.Printf("  already up to date.\n")
			return nil
		}
	}

	if remoteFidx.IsMFIDX {
		// Multi-file index - extract all files
		return bupdateMFIDX(localDir, remoteDir, remoteFidx, remoteFidxPath, localFidxPath, mappings)
	}

	// Single file reconstruction
	// Determine output filename (strip .fidx extension)
	outputName := strings.TrimSuffix(targetFidx, ".fidx")
	outputPath := filepath.Join(localDir, outputName)
	tmpOutputPath := outputPath + ".tmp"

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
	if err := bupdate.CopyFile(localFidxPath, remoteFidxPath); err != nil {
		return fmt.Errorf("copying fidx: %w", err)
	}

	fmt.Printf("  successfully reconstructed: %s\n", outputPath)
	return nil
}

// bupdateMFIDX handles reconstruction of all files from a multi-file index
func bupdateMFIDX(localDir, remoteDir string, remoteFidx *bupdate.Fidx, remoteFidxPath, localFidxPath string, mappings *bupdate.FidxMappings) error {
	fmt.Printf("Multi-file index containing %d files\n", len(remoteFidx.Files))

	// Calculate total missing data across all files
	var totalMissing int64
	var totalSize int64
	for _, fileEntry := range remoteFidx.Files {
		missing, _ := predictDownloadForEntries(fileEntry.Entries, mappings)
		totalMissing += missing
		for _, ent := range fileEntry.Entries {
			totalSize += int64(ent.Size)
		}
	}

	fmt.Printf("  Total size: %d bytes\n", totalSize)
	fmt.Printf("  Need to download: %d bytes (%.1f%%)\n",
		totalMissing, float64(totalMissing)*100.0/float64(totalSize))

	// Reconstruct each file
	for _, fileEntry := range remoteFidx.Files {
		fmt.Printf("\n%s\n", fileEntry.Filename)

		// Use the filename as stored in mfidx for local output
		// This preserves directory structure
		outputPath := filepath.Join(localDir, fileEntry.Filename)
		tmpOutputPath := outputPath + ".tmp"

		// Ensure parent directory exists
		if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
			return fmt.Errorf("creating directory for %s: %w", fileEntry.Filename, err)
		}

		// Create a temporary Fidx struct for this file
		fileFidx := &bupdate.Fidx{
			Entries:  fileEntry.Entries,
			FileSize: int64(fileEntry.FileSize),
		}

		// Predict download for this specific file
		missing, chunks := predictDownloadForEntries(fileEntry.Entries, mappings)
		fmt.Printf("  need to download %d/%d bytes in %d chunks.\n",
			missing, fileEntry.FileSize, chunks)

		// Try to find the remote file using the path from mfidx
		remoteFilePath := filepath.Join(remoteDir, fileEntry.Filename)

		// Check if remote is a symlink
		remoteInfo, err := os.Lstat(remoteFilePath)
		isSymlink := err == nil && remoteInfo.Mode()&os.ModeSymlink != 0

		if isSymlink {
			// For symlinks, reconstruct the target string then create symlink
			if err := reconstructSymlinkFromMFIDX(outputPath, fileFidx, remoteFilePath, fileEntry, mappings); err != nil {
				return fmt.Errorf("reconstructing symlink %s: %w", fileEntry.Filename, err)
			}
		} else {
			// Regular file
			if err := reconstructFileFromMFIDX(tmpOutputPath, fileFidx, remoteFilePath, fileEntry, mappings); err != nil {
				os.Remove(tmpOutputPath)
				return fmt.Errorf("reconstructing %s: %w", fileEntry.Filename, err)
			}

			// Atomically rename to final location
			if err := os.Rename(tmpOutputPath, outputPath); err != nil {
				return fmt.Errorf("rename %s: %w", fileEntry.Filename, err)
			}
		}

		fmt.Printf("  successfully reconstructed: %s\n", outputPath)
	}

	// Copy the mfidx file to local
	if err := bupdate.CopyFile(localFidxPath, remoteFidxPath); err != nil {
		return fmt.Errorf("copying mfidx: %w", err)
	}

	return nil
}

// loadLocalMappings scans the local directory for .fidx and .mfidx files and builds chunk mappings
func loadLocalMappings(dir string) (*bupdate.FidxMappings, error) {
	var allMappings []bupdate.FidxMapping

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		if !strings.HasSuffix(entry.Name(), ".fidx") && !strings.HasSuffix(entry.Name(), ".mfidx") {
			continue
		}

		fidxPath := filepath.Join(dir, entry.Name())
		fidx, err := bupdate.LoadFidx(fidxPath)
		if err != nil {
			fmt.Printf("  warning: skipping %s: %v\n", entry.Name(), err)
			continue
		}

		fmt.Printf("  %s\n", entry.Name())

		if fidx.IsMFIDX {
			// Multi-file index - process each file within it
			for _, fileEntry := range fidx.Files {
				// Use basename to avoid path issues if mfidx has full paths
				baseName := filepath.Base(fileEntry.Filename)
				filePath := filepath.Join(dir, baseName)

				// Check if the file exists
				if _, err := os.Stat(filePath); err != nil {
					continue
				}

				// Add mappings for this file
				var offset int64
				for _, ent := range fileEntry.Entries {
					allMappings = append(allMappings, bupdate.FidxMapping{
						SHA:      ent.SHA,
						Filename: filePath,
						Offset:   offset,
						Size:     ent.Size,
					})
					offset += int64(ent.Size)
				}
			}
		} else {
			// Single-file index
			// Get the actual file name (without .fidx)
			filename := strings.TrimSuffix(entry.Name(), ".fidx")
			filePath := filepath.Join(dir, filename)

			// Check if the file exists
			if _, err := os.Stat(filePath); err != nil {
				continue
			}

			// Add mappings for this file
			var offset int64
			for _, ent := range fidx.Entries {
				allMappings = append(allMappings, bupdate.FidxMapping{
					SHA:      ent.SHA,
					Filename: filePath,
					Offset:   offset,
					Size:     ent.Size,
				})
				offset += int64(ent.Size)
			}
		}
	}

	// Sort mappings by SHA for binary search
	sort.Slice(allMappings, func(i, j int) bool {
		return bytes.Compare(allMappings[i].SHA[:], allMappings[j].SHA[:]) < 0
	})

	return &bupdate.FidxMappings{Mappings: allMappings}, nil
}




// predictDownload calculates how much data needs to be downloaded
func predictDownload(fidx *bupdate.Fidx, mappings *bupdate.FidxMappings) (missing int64, chunks int) {
	return predictDownloadForEntries(fidx.Entries, mappings)
}

// predictDownloadForEntries calculates how much data needs to be downloaded for a list of entries
func predictDownloadForEntries(entries []bupdate.FidxEntry, mappings *bupdate.FidxMappings) (missing int64, chunks int) {
	for _, ent := range entries {
		if mappings.FindMapping(ent.SHA) == nil {
			missing += int64(ent.Size)
			chunks++
		}
	}
	return
}

// reconstructSymlinkFromMFIDX reconstructs a symlink by getting its target string
func reconstructSymlinkFromMFIDX(outputPath string, fidx *bupdate.Fidx, remoteFilePath string, fileEntry bupdate.FileEntry, mappings *bupdate.FidxMappings) error {
	// Remove existing file/symlink if it exists
	os.Remove(outputPath)

	// Try to read the symlink target from remote (may not exist)
	symlinkTarget, remoteErr := os.Readlink(remoteFilePath)
	hasRemote := remoteErr == nil

	// Reconstruct the target string from chunks (should be a single chunk)
	var reconstructedTarget []byte
	got := int64(0)
	missing := int64(0)

	// Calculate missing for progress
	for _, ent := range fidx.Entries {
		if mappings.FindMapping(ent.SHA) == nil {
			missing += int64(ent.Size)
		}
	}

	for _, ent := range fidx.Entries {
		chunkSize := int64(ent.Size)
		mapping := mappings.FindMapping(ent.SHA)

		var chunkData []byte
		if mapping != nil {
			// Check if the local mapping file is a symlink
			localInfo, err := os.Lstat(mapping.Filename)
			isLocalSymlink := err == nil && localInfo.Mode()&os.ModeSymlink != 0

			if isLocalSymlink {
				// For symlinks, read the target directly
				target, err := os.Readlink(mapping.Filename)
				if err != nil {
					mapping = nil
				} else {
					chunkData = []byte(target)
					// Verify SHA
					computedSHA := bupdate.BlobSHA(chunkData)
					if !bytes.Equal(computedSHA[:], ent.SHA[:]) {
						mapping = nil
					}
				}
			} else {
				// Regular file - read chunk at offset
				chunkData, err = bupdate.ReadChunk(mapping.Filename, mapping.Offset, int64(mapping.Size))
				if err != nil {
					mapping = nil
				} else {
					// Verify SHA
					computedSHA := bupdate.BlobSHA(chunkData)
					if !bytes.Equal(computedSHA[:], ent.SHA[:]) {
						mapping = nil
					}
				}
			}
		}

		if mapping == nil {
			// Need to get from remote
			if !hasRemote {
				return fmt.Errorf("remote symlink not available and chunk not found locally")
			}

			// Use the remote symlink target
			chunkData = []byte(symlinkTarget)
			if int64(len(chunkData)) != chunkSize {
				return fmt.Errorf("symlink target size mismatch: expected %d, got %d", chunkSize, len(chunkData))
			}

			// Verify SHA
			computedSHA := bupdate.BlobSHA(chunkData)
			if !bytes.Equal(computedSHA[:], ent.SHA[:]) {
				return fmt.Errorf("symlink target checksum mismatch")
			}

			got += chunkSize
			if missing > 0 {
				pct := (got * 100) / missing
				fmt.Printf("\r  Downloading... %d%% (%d/%d bytes)", pct, got, missing)
			}
		}

		reconstructedTarget = append(reconstructedTarget, chunkData...)
	}

	if missing > 0 {
		fmt.Println() // newline after progress
	}

	// Create the symlink with the reconstructed target
	targetStr := string(reconstructedTarget)
	if err := os.Symlink(targetStr, outputPath); err != nil {
		return fmt.Errorf("creating symlink: %w", err)
	}

	return nil
}

// reconstructFileFromMFIDX rebuilds a file from an mfidx by combining local and remote chunks
func reconstructFileFromMFIDX(outputPath string, fidx *bupdate.Fidx, remoteFilePath string, fileEntry bupdate.FileEntry, mappings *bupdate.FidxMappings) error {
	outf, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer outf.Close()

	// Try to open remote file if it exists
	var remotef *os.File
	remotef, err = os.Open(remoteFilePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("opening remote file: %w", err)
	}
	if remotef != nil {
		defer remotef.Close()
	}

	var remoteOffset int64
	got := int64(0)
	missing := int64(0)

	// Calculate total missing for progress
	for _, ent := range fidx.Entries {
		if mappings.FindMapping(ent.SHA) == nil {
			missing += int64(ent.Size)
		}
	}

	// Process each chunk
	for _, ent := range fidx.Entries {
		chunkSize := int64(ent.Size)
		mapping := mappings.FindMapping(ent.SHA)

		if mapping != nil {
			// We have this chunk locally - read and verify it
			localData, err := bupdate.ReadChunk(mapping.Filename, mapping.Offset, int64(mapping.Size))
			if err != nil {
				// Failed to read local chunk, fall back to remote
				mapping = nil
			} else {
				// Verify SHA matches (as git blob)
				computedSHA := bupdate.BlobSHA(localData)
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
			if remotef == nil {
				return fmt.Errorf("remote file not available and chunk not found locally")
			}

			remoteData, err := bupdate.ReadChunk(remoteFilePath, remoteOffset, chunkSize)
			if err != nil {
				return fmt.Errorf("reading remote chunk at offset %d: %w", remoteOffset, err)
			}

			// Verify remote chunk
			computedSHA := bupdate.BlobSHA(remoteData)
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

// reconstructFile rebuilds the output file by combining local and remote chunks
func reconstructFile(outputPath string, fidx *bupdate.Fidx, remoteFilePath string, mappings *bupdate.FidxMappings) error {
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
		if mappings.FindMapping(ent.SHA) == nil {
			missing += int64(ent.Size)
		}
	}

	// Process each chunk
	for _, ent := range fidx.Entries {
		chunkSize := int64(ent.Size)
		mapping := mappings.FindMapping(ent.SHA)

		if mapping != nil {
			// We have this chunk locally - read and verify it
			localData, err := bupdate.ReadChunk(mapping.Filename, mapping.Offset, int64(mapping.Size))
			if err != nil {
				// Failed to read local chunk, fall back to remote
				mapping = nil
			} else {
				// Verify SHA matches (as git blob)
				computedSHA := bupdate.BlobSHA(localData)
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
			remoteData, err := bupdate.ReadChunk(remoteFilePath, remoteOffset, chunkSize)
			if err != nil {
				return fmt.Errorf("reading remote chunk at offset %d: %w", remoteOffset, err)
			}

			// Verify remote chunk
			computedSHA := bupdate.BlobSHA(remoteData)
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



