// Package bupdate provides functionality for downloading snapshots from mesh peers.
package bupdate

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DownloadSnapshotOptions configures the snapshot download.
type DownloadSnapshotOptions struct {
	// SnapshotID is the ID of the snapshot to download
	SnapshotID string
	// SnapshotsDir is the directory where snapshots are stored
	SnapshotsDir string
	// Peers is the list of mesh peers to query
	Peers []PeerInfo
	// ProgressWriter receives progress updates (can be nil)
	ProgressWriter io.Writer
	// IsTTY indicates whether to format progress for a terminal
	IsTTY bool
}

// DownloadSnapshotResult contains the result of downloading a snapshot.
type DownloadSnapshotResult struct {
	// SnapshotPath is the full path to the downloaded snapshot directory
	SnapshotPath string
	// PeerURL is the URL of the peer we downloaded from
	PeerURL string
	// PeerHostname is the hostname of the peer
	PeerHostname string
	// AlreadyExists is true if the snapshot was already present locally
	AlreadyExists bool
}

// DownloadSnapshot downloads a snapshot from mesh peers into the snapshots directory.
// It returns an error if no peer has the snapshot.
// If the snapshot already exists locally, it returns success without downloading.
func DownloadSnapshot(opts DownloadSnapshotOptions) (*DownloadSnapshotResult, error) {
	snapshotPath := filepath.Join(opts.SnapshotsDir, opts.SnapshotID)

	// Check if snapshot already exists
	if _, err := os.Stat(snapshotPath); err == nil {
		return &DownloadSnapshotResult{
			SnapshotPath:  snapshotPath,
			AlreadyExists: true,
		}, nil
	}

	// Find a peer with the snapshot
	results := CheckPeersForSnapshot(opts.Peers, opts.SnapshotID)

	var peersWithSnap []PeerResult
	for _, r := range results {
		if r.HasSnap {
			peersWithSnap = append(peersWithSnap, r)
		}
	}

	if len(peersWithSnap) == 0 {
		return nil, fmt.Errorf("no peer has snapshot %s", opts.SnapshotID)
	}

	// Sort by hostname for determinism, pick first
	sort.Slice(peersWithSnap, func(i, j int) bool {
		return peersWithSnap[i].Hostname < peersWithSnap[j].Hostname
	})

	peer := peersWithSnap[0]
	baseURL := strings.TrimSuffix(peer.PeerURL, "/")

	if opts.ProgressWriter != nil {
		fmt.Fprintf(opts.ProgressWriter, "Downloading snapshot %s from %s\n", opts.SnapshotID, peer.Hostname)
	}

	// Download to temp locations first
	tmpSnapshotDir := snapshotPath + ".tmp"
	tmpFidxPath := snapshotPath + ".fidx.tmp"
	tmpFidxFidxPath := snapshotPath + ".fidx.fidx.tmp"
	tmpStampPath := snapshotPath + ".stamp.tmp"

	finalFidxPath := snapshotPath + ".fidx"
	finalFidxFidxPath := snapshotPath + ".fidx.fidx"
	finalStampPath := snapshotPath + ".stamp"

	// Clean up temp files on error
	cleanup := func() {
		os.RemoveAll(tmpSnapshotDir)
		os.Remove(tmpFidxPath)
		os.Remove(tmpFidxFidxPath)
		os.Remove(tmpStampPath)
	}

	// Create temp snapshot directory
	if err := os.MkdirAll(tmpSnapshotDir, 0755); err != nil {
		return nil, fmt.Errorf("creating temp snapshot dir: %w", err)
	}

	// Step 1: Download the stamp file
	stampURL := baseURL + "/bupdate/" + opts.SnapshotID + ".stamp"
	stampData, err := FetchFullFile(stampURL)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("downloading stamp file: %w", err)
	}
	if err := os.WriteFile(tmpStampPath, stampData, 0644); err != nil {
		cleanup()
		return nil, fmt.Errorf("writing stamp file: %w", err)
	}

	// Step 2: Download the fidx.fidx (for bootstrapping fidx download)
	fidxFidxURL := baseURL + "/bupdate/" + opts.SnapshotID + ".fidx.fidx"
	fidxFidxData, err := FetchFullFile(fidxFidxURL)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("downloading fidx.fidx: %w", err)
	}
	if err := os.WriteFile(tmpFidxFidxPath, fidxFidxData, 0644); err != nil {
		cleanup()
		return nil, fmt.Errorf("writing fidx.fidx: %w", err)
	}

	// Step 3: Download the main fidx using bupdate
	fidxURL := baseURL + "/bupdate/" + opts.SnapshotID + ".fidx"
	if opts.ProgressWriter != nil {
		fmt.Fprintf(opts.ProgressWriter, "Downloading index file...\n")
	}

	// Load the fidx.fidx to get the structure
	fidxFidx, err := ParseFidxData(tmpFidxFidxPath, fidxFidxData)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("parsing fidx.fidx: %w", err)
	}

	// Download fidx using range requests
	if err := downloadFileWithFidx(tmpFidxPath, fidxURL, fidxFidx, opts.SnapshotsDir, opts.ProgressWriter); err != nil {
		cleanup()
		return nil, fmt.Errorf("downloading fidx: %w", err)
	}

	// Step 4: Load the main fidx and download all files
	mainFidx, err := LoadFidx(tmpFidxPath)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("loading fidx: %w", err)
	}

	if opts.ProgressWriter != nil {
		fmt.Fprintf(opts.ProgressWriter, "Downloading %d files...\n", len(mainFidx.Files))
	}

	// Download all files in the snapshot
	if err := downloadMFIDX(tmpSnapshotDir, baseURL, mainFidx, opts.SnapshotID, opts.SnapshotsDir, opts.ProgressWriter); err != nil {
		cleanup()
		return nil, fmt.Errorf("downloading snapshot files: %w", err)
	}

	// Step 5: Rename all temp files to final locations
	if err := os.Rename(tmpSnapshotDir, snapshotPath); err != nil {
		cleanup()
		return nil, fmt.Errorf("renaming snapshot dir: %w", err)
	}
	if err := os.Rename(tmpFidxPath, finalFidxPath); err != nil {
		// Snapshot already renamed, just log
	}
	if err := os.Rename(tmpFidxFidxPath, finalFidxFidxPath); err != nil {
		// Snapshot already renamed, just log
	}
	if err := os.Rename(tmpStampPath, finalStampPath); err != nil {
		// Snapshot already renamed, just log
	}

	if opts.ProgressWriter != nil {
		fmt.Fprintf(opts.ProgressWriter, "Downloaded snapshot %s\n", opts.SnapshotID)
	}

	return &DownloadSnapshotResult{
		SnapshotPath: snapshotPath,
		PeerURL:      peer.PeerURL,
		PeerHostname: peer.Hostname,
	}, nil
}

// downloadFileWithFidx downloads a file using its fidx to fetch chunks
func downloadFileWithFidx(outputPath, fileURL string, fidx *Fidx, localDir string, progress io.Writer) error {
	outf, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer outf.Close()

	// Pre-allocate file
	if err := outf.Truncate(fidx.FileSize); err != nil {
		return fmt.Errorf("truncating file: %w", err)
	}

	// Load local mappings for deduplication
	mappings, err := loadLocalMappings(localDir)
	if err != nil {
		mappings = &FidxMappings{}
	}

	// Create HTTP reader for the file
	reader, err := NewHTTPReader(fileURL)
	if err != nil {
		return fmt.Errorf("creating HTTP reader: %w", err)
	}
	defer reader.Close()

	// Build list of chunks we need to download
	type chunkInfo struct {
		ent          FidxEntry
		localMapping *FidxMapping
		remoteOffset int64
		outputIdx    int
	}

	var chunks []chunkInfo
	var remoteChunks []chunkInfo
	var remoteOffset int64

	for i, ent := range fidx.Entries {
		mapping := mappings.FindMapping(ent.SHA)
		ci := chunkInfo{
			ent:          ent,
			localMapping: mapping,
			remoteOffset: remoteOffset,
			outputIdx:    i,
		}
		chunks = append(chunks, ci)
		remoteOffset += int64(ent.Size)

		if mapping == nil {
			// Skip zero blocks
			if ent.Size == BLOB_MAX && ent.SHA == ZeroBlockSHA {
				continue
			}
			remoteChunks = append(remoteChunks, ci)
		}
	}

	// Fetch remote chunks
	var remoteData map[int][]byte
	if len(remoteChunks) > 0 {
		remoteData = make(map[int][]byte)

		// Batch requests for pipelining
		const batchSize = 16
		for i := 0; i < len(remoteChunks); i += batchSize {
			end := i + batchSize
			if end > len(remoteChunks) {
				end = len(remoteChunks)
			}
			batch := remoteChunks[i:end]

			requests := make([]RangeRequest, len(batch))
			for j, ci := range batch {
				requests[j] = RangeRequest{
					Offset: ci.remoteOffset,
					Size:   int64(ci.ent.Size),
				}
			}

			results, err := reader.ReadRanges(requests)
			if err != nil {
				return fmt.Errorf("reading ranges: %w", err)
			}

			for j, data := range results {
				ci := batch[j]
				// Verify SHA
				computedSHA := BlobSHA(data)
				if computedSHA != ci.ent.SHA {
					return fmt.Errorf("checksum mismatch at offset %d", ci.remoteOffset)
				}
				remoteData[ci.outputIdx] = data
			}
		}
	}

	// Write all chunks in order
	for i, ci := range chunks {
		chunkSize := int64(ci.ent.Size)

		// Zero block - leave a hole
		if ci.ent.Size == BLOB_MAX && ci.ent.SHA == ZeroBlockSHA {
			if _, err := outf.Seek(chunkSize, io.SeekCurrent); err != nil {
				return fmt.Errorf("seeking past zero block: %w", err)
			}
			continue
		}

		var data []byte

		if ci.localMapping != nil {
			// Read from local
			data, err = ReadChunk(ci.localMapping.Filename, ci.localMapping.Offset, int64(ci.localMapping.Size))
			if err != nil {
				// Fall back to remote
				if rd, ok := remoteData[i]; ok {
					data = rd
				} else {
					return fmt.Errorf("reading local chunk: %w", err)
				}
			} else {
				// Verify SHA
				computedSHA := BlobSHA(data)
				if computedSHA != ci.ent.SHA {
					if rd, ok := remoteData[i]; ok {
						data = rd
					} else {
						return fmt.Errorf("local chunk checksum mismatch")
					}
				}
			}
		} else {
			// Get from remote
			var ok bool
			data, ok = remoteData[i]
			if !ok {
				return fmt.Errorf("remote chunk not available for chunk %d", i)
			}
		}

		if _, err := outf.Write(data); err != nil {
			return fmt.Errorf("writing chunk: %w", err)
		}
	}

	return nil
}

// downloadMFIDX downloads all files from a multi-file index
func downloadMFIDX(localDir, baseURL string, fidx *Fidx, snapshotID, snapshotsDir string, progress io.Writer) error {
	// Load local mappings for deduplication
	mappings, err := loadLocalMappings(snapshotsDir)
	if err != nil {
		mappings = &FidxMappings{}
	}

	// Create a shared HTTP reader for better connection reuse
	reader, err := NewHTTPReaderForHost(baseURL)
	if err != nil {
		return fmt.Errorf("creating HTTP reader: %w", err)
	}
	defer reader.Close()

	for i, fileEntry := range fidx.Files {
		outputPath := filepath.Join(localDir, fileEntry.Filename)
		tmpOutputPath := outputPath + ".tmp"

		// Ensure parent directory exists
		if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
			return fmt.Errorf("creating directory for %s: %w", fileEntry.Filename, err)
		}

		// Create a temporary Fidx for this file
		fileFidx := &Fidx{
			Entries:  fileEntry.Entries,
			FileSize: int64(fileEntry.FileSize),
		}

		remotePath := "/bupdate/" + snapshotID + "/" + fileEntry.Filename
		if err := downloadFileWithFidxReader(tmpOutputPath, reader, remotePath, fileFidx, mappings); err != nil {
			os.Remove(tmpOutputPath)
			return fmt.Errorf("downloading %s: %w", fileEntry.Filename, err)
		}

		if err := os.Rename(tmpOutputPath, outputPath); err != nil {
			return fmt.Errorf("renaming %s: %w", fileEntry.Filename, err)
		}

		if progress != nil && (i+1)%100 == 0 {
			fmt.Fprintf(progress, "  %d/%d files\n", i+1, len(fidx.Files))
		}
	}

	return nil
}

// downloadFileWithFidxReader downloads a file using an existing HTTP reader
func downloadFileWithFidxReader(outputPath string, reader *HTTPReader, remotePath string, fidx *Fidx, mappings *FidxMappings) error {
	outf, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer outf.Close()

	// Pre-allocate file
	if err := outf.Truncate(fidx.FileSize); err != nil {
		return fmt.Errorf("truncating file: %w", err)
	}

	// Build list of chunks we need to download
	type chunkInfo struct {
		ent          FidxEntry
		localMapping *FidxMapping
		remoteOffset int64
		outputIdx    int
	}

	var chunks []chunkInfo
	var remoteChunks []chunkInfo
	var remoteOffset int64

	for i, ent := range fidx.Entries {
		mapping := mappings.FindMapping(ent.SHA)
		ci := chunkInfo{
			ent:          ent,
			localMapping: mapping,
			remoteOffset: remoteOffset,
			outputIdx:    i,
		}
		chunks = append(chunks, ci)
		remoteOffset += int64(ent.Size)

		if mapping == nil {
			// Skip zero blocks
			if ent.Size == BLOB_MAX && ent.SHA == ZeroBlockSHA {
				continue
			}
			remoteChunks = append(remoteChunks, ci)
		}
	}

	// Fetch remote chunks
	var remoteData map[int][]byte
	if len(remoteChunks) > 0 {
		remoteData = make(map[int][]byte)

		// Batch requests for pipelining
		const batchSize = 16
		for i := 0; i < len(remoteChunks); i += batchSize {
			end := i + batchSize
			if end > len(remoteChunks) {
				end = len(remoteChunks)
			}
			batch := remoteChunks[i:end]

			requests := make([]RangeRequest, len(batch))
			for j, ci := range batch {
				requests[j] = RangeRequest{
					Offset: ci.remoteOffset,
					Size:   int64(ci.ent.Size),
				}
			}

			results, err := reader.ReadRangesFromPath(remotePath, requests)
			if err != nil {
				return fmt.Errorf("reading ranges: %w", err)
			}

			for j, data := range results {
				ci := batch[j]
				// Verify SHA
				computedSHA := BlobSHA(data)
				if computedSHA != ci.ent.SHA {
					return fmt.Errorf("checksum mismatch at offset %d", ci.remoteOffset)
				}
				remoteData[ci.outputIdx] = data
			}
		}
	}

	// Write all chunks in order
	for i, ci := range chunks {
		chunkSize := int64(ci.ent.Size)

		// Zero block - leave a hole
		if ci.ent.Size == BLOB_MAX && ci.ent.SHA == ZeroBlockSHA {
			if _, err := outf.Seek(chunkSize, io.SeekCurrent); err != nil {
				return fmt.Errorf("seeking past zero block: %w", err)
			}
			continue
		}

		var data []byte

		if ci.localMapping != nil {
			// Read from local
			data, err = ReadChunk(ci.localMapping.Filename, ci.localMapping.Offset, int64(ci.localMapping.Size))
			if err != nil {
				// Fall back to remote
				if rd, ok := remoteData[i]; ok {
					data = rd
				} else {
					return fmt.Errorf("reading local chunk: %w", err)
				}
			} else {
				// Verify SHA
				computedSHA := BlobSHA(data)
				if computedSHA != ci.ent.SHA {
					if rd, ok := remoteData[i]; ok {
						data = rd
					} else {
						return fmt.Errorf("local chunk checksum mismatch")
					}
				}
			}
		} else {
			// Get from remote
			var ok bool
			data, ok = remoteData[i]
			if !ok {
				return fmt.Errorf("remote chunk not available for chunk %d", i)
			}
		}

		if _, err := outf.Write(data); err != nil {
			return fmt.Errorf("writing chunk: %w", err)
		}
	}

	return nil
}

// loadLocalMappings scans a directory for .fidx and .mfidx files and builds a mapping
func loadLocalMappings(dir string) (*FidxMappings, error) {
	var allMappings []FidxMapping

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return &FidxMappings{}, nil
		}
		return nil, err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		if !strings.HasSuffix(entry.Name(), ".fidx") && !strings.HasSuffix(entry.Name(), ".mfidx") {
			continue
		}

		// Skip fidx.fidx files (they're metadata, not data)
		if strings.HasSuffix(entry.Name(), ".fidx.fidx") {
			continue
		}

		fidxPath := filepath.Join(dir, entry.Name())
		fidx, err := LoadFidx(fidxPath)
		if err != nil {
			continue // skip invalid fidx files
		}

		if fidx.IsMFIDX {
			// Multi-file index
			for _, fileEntry := range fidx.Files {
				filePath := filepath.Join(dir, strings.TrimSuffix(entry.Name(), ".fidx"), fileEntry.Filename)
				if _, err := os.Lstat(filePath); err != nil {
					continue
				}

				var offset int64
				for _, ent := range fileEntry.Entries {
					allMappings = append(allMappings, FidxMapping{
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
			filename := strings.TrimSuffix(entry.Name(), ".fidx")
			filePath := filepath.Join(dir, filename)

			if _, err := os.Stat(filePath); err != nil {
				continue
			}

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
	}

	// Sort by SHA for binary search
	sort.Slice(allMappings, func(i, j int) bool {
		return string(allMappings[i].SHA[:]) < string(allMappings[j].SHA[:])
	})

	return &FidxMappings{Mappings: allMappings}, nil
}
