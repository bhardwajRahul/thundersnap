// download.go provides functionality for downloading snapshots using the
// TSM/TSC format from mesh peers. (See types.go for the package doc.)
package tsm

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DownloadOptions configures the TSM/TSC-based snapshot download.
type DownloadOptions struct {
	// SnapshotID is the ID of the snapshot to download
	SnapshotID string
	// SnapsDir is the directory where snapshots are stored
	SnapsDir string
	// BaseURL is the base URL of the peer's HTTP server (e.g., "http://host:7575")
	BaseURL string
	// ProgressWriter receives progress updates (can be nil)
	ProgressWriter io.Writer

	// CreateTargetDir is called to create the target directory for the snapshot.
	// parentStamp is the parent snapshot ID from the stamp file (may be empty).
	// If nil, os.MkdirAll is used.
	CreateTargetDir func(path, parentStamp string) error

	// CleanupTargetDir is called to clean up the target directory on error.
	// If nil, os.RemoveAll is used.
	CleanupTargetDir func(path string)

	// PrepareForFiles is called after the target dir is created but before
	// downloading files. fileList contains all files that will be in the snapshot.
	// This can be used to delete files that exist in a cloned parent but are
	// not present in the new snapshot.
	// If nil, no preparation is done.
	PrepareForFiles func(targetDir string, fileList []string) error

	// HTTPClient is the HTTP client to use. If nil, http.DefaultClient is used.
	HTTPClient *http.Client
}

// DownloadResult contains the result of downloading a snapshot.
type DownloadResult struct {
	// SnapshotPath is the full path to the downloaded snapshot directory
	SnapshotPath string
	// AlreadyExists is true if the snapshot was already present locally
	AlreadyExists bool
}

// Download downloads a snapshot from a mesh peer using TSM/TSC format.
// This is the TSM/TSC-based equivalent of bupdate.DownloadSnapshot.
//
// The download process:
// 1. Download .stamp file (parent reference)
// 2. Download .tsm file (manifest with file metadata and chunk refs)
// 3. Download .tsc file (chunk index)
// 4. For each file, fetch chunks via HTTP range requests (with local dedup)
func Download(opts DownloadOptions) (*DownloadResult, error) {
	snapshotPath := filepath.Join(opts.SnapsDir, opts.SnapshotID)

	// Check if snapshot already exists
	if _, err := os.Stat(snapshotPath); err == nil {
		return &DownloadResult{
			SnapshotPath:  snapshotPath,
			AlreadyExists: true,
		}, nil
	}

	baseURL := strings.TrimSuffix(opts.BaseURL, "/")
	client := opts.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	// Determine cleanup functions
	cleanupDir := opts.CleanupTargetDir
	if cleanupDir == nil {
		cleanupDir = func(path string) { os.RemoveAll(path) }
	}

	// Temp paths
	tmpSnapshotDir := snapshotPath + ".tmp"
	tmpTSMPath := snapshotPath + ".tsm.tmp"
	tmpTSCPath := snapshotPath + ".tsc.tmp"
	tmpStampPath := snapshotPath + ".stamp.tmp"

	finalTSMPath := snapshotPath + ".tsm"
	finalTSCPath := snapshotPath + ".tsc"
	finalStampPath := snapshotPath + ".stamp"

	cleanup := func() {
		cleanupDir(tmpSnapshotDir)
		os.Remove(tmpTSMPath)
		os.Remove(tmpTSCPath)
		os.Remove(tmpStampPath)
	}

	// Step 1: Download the stamp file
	if opts.ProgressWriter != nil {
		fmt.Fprintf(opts.ProgressWriter, "Downloading stamp...\n")
	}
	stampURL := baseURL + "/bupdate/" + opts.SnapshotID + ".stamp"
	stampData, err := fetchFullFile(client, stampURL)
	if err != nil {
		return nil, fmt.Errorf("downloading stamp: %w", err)
	}
	parentStamp := strings.TrimSpace(string(stampData))

	// Step 2: Download the TSM file
	if opts.ProgressWriter != nil {
		fmt.Fprintf(opts.ProgressWriter, "Downloading manifest...\n")
	}
	tsmURL := baseURL + "/bupdate/" + opts.SnapshotID + ".tsm"
	tsmData, err := fetchFullFile(client, tsmURL)
	if err != nil {
		return nil, fmt.Errorf("downloading tsm: %w", err)
	}

	// Parse TSM
	tsmReader, err := ParseTSM(tsmData)
	if err != nil {
		return nil, fmt.Errorf("parsing tsm: %w", err)
	}

	// Step 3: Download the TSC file
	if opts.ProgressWriter != nil {
		fmt.Fprintf(opts.ProgressWriter, "Downloading chunk index...\n")
	}
	tscURL := baseURL + "/bupdate/" + opts.SnapshotID + ".tsc"
	tscData, err := fetchFullFile(client, tscURL)
	if err != nil {
		return nil, fmt.Errorf("downloading tsc: %w", err)
	}

	// Parse TSC
	tscReader, err := ParseTSC(tscData)
	if err != nil {
		return nil, fmt.Errorf("parsing tsc: %w", err)
	}

	// Step 4: Create target directory
	if opts.CreateTargetDir != nil {
		if err := opts.CreateTargetDir(tmpSnapshotDir, parentStamp); err != nil {
			return nil, fmt.Errorf("creating target dir: %w", err)
		}
	} else {
		if err := os.MkdirAll(tmpSnapshotDir, 0755); err != nil {
			return nil, fmt.Errorf("creating temp snapshot dir: %w", err)
		}
	}

	// Step 5: Prepare for files
	if opts.PrepareForFiles != nil {
		fileList := make([]string, 0, len(tsmReader.Entries))
		for _, e := range tsmReader.Entries {
			if e.Type == EntryTypeFile {
				fileList = append(fileList, e.Path)
			}
		}
		if err := opts.PrepareForFiles(tmpSnapshotDir, fileList); err != nil {
			cleanup()
			return nil, fmt.Errorf("preparing for files: %w", err)
		}
	}

	// Step 6: Load local chunk map for deduplication
	localChunks, err := LoadLocalChunkMap(opts.SnapsDir)
	if err != nil {
		// Non-fatal: just won't deduplicate
		localChunks = &ChunkMap{}
	}

	// Step 7: Download files
	if opts.ProgressWriter != nil {
		fileCount := 0
		for _, e := range tsmReader.Entries {
			if e.Type == EntryTypeFile {
				fileCount++
			}
		}
		fmt.Fprintf(opts.ProgressWriter, "Downloading %d files...\n", fileCount)
	}

	if err := downloadFiles(downloadFilesOpts{
		targetDir:   tmpSnapshotDir,
		baseURL:     baseURL,
		snapshotID:  opts.SnapshotID,
		tsm:         tsmReader,
		tsc:         tscReader,
		localChunks: localChunks,
		client:      client,
		progress:    opts.ProgressWriter,
	}); err != nil {
		cleanup()
		return nil, fmt.Errorf("downloading files: %w", err)
	}

	// Step 8: Create non-file entries (directories, symlinks, devices)
	if err := createNonFileEntries(tmpSnapshotDir, tsmReader); err != nil {
		cleanup()
		return nil, fmt.Errorf("creating non-file entries: %w", err)
	}

	// Write temporary metadata files
	if err := os.WriteFile(tmpStampPath, stampData, 0644); err != nil {
		cleanup()
		return nil, fmt.Errorf("writing stamp: %w", err)
	}
	if err := os.WriteFile(tmpTSMPath, tsmData, 0644); err != nil {
		cleanup()
		return nil, fmt.Errorf("writing tsm: %w", err)
	}
	if err := os.WriteFile(tmpTSCPath, tscData, 0644); err != nil {
		cleanup()
		return nil, fmt.Errorf("writing tsc: %w", err)
	}

	// Step 9: Atomic rename
	if err := os.Rename(tmpSnapshotDir, snapshotPath); err != nil {
		cleanup()
		return nil, fmt.Errorf("renaming snapshot dir: %w", err)
	}
	os.Rename(tmpStampPath, finalStampPath)
	os.Rename(tmpTSMPath, finalTSMPath)
	os.Rename(tmpTSCPath, finalTSCPath)

	if opts.ProgressWriter != nil {
		fmt.Fprintf(opts.ProgressWriter, "Downloaded snapshot %s\n", opts.SnapshotID)
	}

	return &DownloadResult{
		SnapshotPath: snapshotPath,
	}, nil
}

type downloadFilesOpts struct {
	targetDir   string
	baseURL     string
	snapshotID  string
	tsm         *TSMReader
	tsc         *TSCReader
	localChunks *ChunkMap
	client      *http.Client
	progress    io.Writer
}

// downloadFiles downloads all file entries in the TSM.
func downloadFiles(opts downloadFilesOpts) error {
	fileCount := 0
	for _, entry := range opts.tsm.Entries {
		if entry.Type != EntryTypeFile {
			continue
		}

		outputPath := filepath.Join(opts.targetDir, entry.Path)
		tmpPath := outputPath + ".tmp"

		// Ensure parent directory exists
		if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
			return fmt.Errorf("creating directory for %s: %w", entry.Path, err)
		}

		// Download the file
		if err := downloadFile(downloadFileOpts{
			outputPath:  tmpPath,
			remotePath:  "/bupdate/" + opts.snapshotID + "/" + entry.Path,
			entry:       &entry,
			tsc:         opts.tsc,
			localChunks: opts.localChunks,
			baseURL:     opts.baseURL,
			client:      opts.client,
		}); err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("downloading %s: %w", entry.Path, err)
		}

		// Set permissions (include setuid/setgid/sticky bits). Best-effort:
		// permission/ownership may legitimately fail without root.
		_ = os.Chmod(tmpPath, os.FileMode(entry.Mode&0xFFF))
		_ = os.Lchown(tmpPath, int(entry.UID), int(entry.GID))

		// Restore the original mtime recorded in the manifest. This is purely
		// for display/POSIX-semantics fidelity: mtime is not trusted as a
		// change-detection signal (see indexer.go's reuseParentChunks, which
		// relies on ctime instead, precisely because mtime can be freely set
		// by any process and so must not be trusted for that purpose). ctime
		// itself cannot be, and is not, restored here - it is intentionally
		// left as whatever the kernel stamps during this extraction, since
		// that is the receiving host's own tamper-resistant record of when
		// the file was actually written locally.
		mtime := time.Unix(0, entry.Mtime)
		_ = os.Chtimes(tmpPath, mtime, mtime)

		// Rename to final location
		if err := os.Rename(tmpPath, outputPath); err != nil {
			return fmt.Errorf("renaming %s: %w", entry.Path, err)
		}

		fileCount++
		if opts.progress != nil && fileCount%100 == 0 {
			fmt.Fprintf(opts.progress, "  %d files\n", fileCount)
		}
	}

	return nil
}

type downloadFileOpts struct {
	outputPath  string
	remotePath  string
	entry       *TSMEntry
	tsc         *TSCReader
	localChunks *ChunkMap
	baseURL     string
	client      *http.Client
}

// downloadFile downloads a single file using its chunk references.
func downloadFile(opts downloadFileOpts) error {
	outf, err := os.Create(opts.outputPath)
	if err != nil {
		return err
	}
	defer outf.Close()

	// Pre-allocate file
	if err := outf.Truncate(int64(opts.entry.Size)); err != nil {
		return fmt.Errorf("truncating: %w", err)
	}

	if opts.entry.ChunkCount == 0 {
		// Empty file
		return nil
	}

	// Build chunk info with offsets
	type chunkInfo struct {
		sha          [32]byte
		size         uint32
		remoteOffset int64
		localLoc     *ChunkLocation
	}

	chunks := make([]chunkInfo, len(opts.entry.ChunkRefs))
	var remoteOffset int64

	for i, tscIdx := range opts.entry.ChunkRefs {
		if int(tscIdx) >= len(opts.tsc.Entries) {
			return fmt.Errorf("invalid chunk ref %d", tscIdx)
		}

		tscEntry := opts.tsc.Entries[tscIdx]
		localLoc, _ := opts.localChunks.FindChunk(tscEntry.SHA256)

		chunks[i] = chunkInfo{
			sha:          tscEntry.SHA256,
			size:         tscEntry.Size,
			remoteOffset: remoteOffset,
			localLoc:     localLoc,
		}
		remoteOffset += int64(tscEntry.Size)
	}

	// Identify chunks needing remote fetch
	var remoteChunks []int // indices into chunks
	for i, ci := range chunks {
		if ci.localLoc == nil {
			// Skip zero blocks
			if ci.size == BLOB_MAX && ci.sha == ZeroBlockSHA {
				continue
			}
			remoteChunks = append(remoteChunks, i)
		}
	}

	// Fetch remote chunks in batches
	remoteData := make(map[int][]byte)
	if len(remoteChunks) > 0 {
		const batchSize = 16
		for i := 0; i < len(remoteChunks); i += batchSize {
			end := i + batchSize
			if end > len(remoteChunks) {
				end = len(remoteChunks)
			}
			batch := remoteChunks[i:end]

			// Build range request
			ranges := make([]rangeSpec, len(batch))
			for j, idx := range batch {
				ci := chunks[idx]
				ranges[j] = rangeSpec{
					offset: ci.remoteOffset,
					size:   int64(ci.size),
				}
			}

			results, err := fetchRanges(opts.client, opts.baseURL+opts.remotePath, ranges)
			if err != nil {
				return fmt.Errorf("fetching ranges: %w", err)
			}

			for j, data := range results {
				idx := batch[j]
				ci := chunks[idx]

				// Verify SHA
				computed := BlobSHA256(data)
				if computed != ci.sha {
					return fmt.Errorf("checksum mismatch at offset %d", ci.remoteOffset)
				}
				remoteData[idx] = data
			}
		}
	}

	// Write all chunks in order
	for i, ci := range chunks {
		// Zero block - leave a hole
		if ci.size == BLOB_MAX && ci.sha == ZeroBlockSHA {
			if _, err := outf.Seek(int64(ci.size), io.SeekCurrent); err != nil {
				return fmt.Errorf("seeking past zero block: %w", err)
			}
			continue
		}

		var data []byte

		if ci.localLoc != nil {
			// Read from local
			data, err = ci.localLoc.VerifyAndRead()
			if err != nil {
				// Fall back to remote
				if rd, ok := remoteData[i]; ok {
					data = rd
				} else {
					return fmt.Errorf("reading local chunk: %w", err)
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

// createNonFileEntries creates directories and symlinks. Device nodes are NOT
// created (see the CharDev/BlockDev case below); regular files are handled
// separately by downloadFiles.
func createNonFileEntries(targetDir string, tsm *TSMReader) error {
	// Sort by path length so a parent is always created before its children.
	// Path length is a valid stand-in for depth here because any child path is
	// strictly longer than its own parent ("a" < "a/b" < "a/b/c"); it is not a
	// general topological sort, but it does not need to be.
	entries := make([]TSMEntry, len(tsm.Entries))
	copy(entries, tsm.Entries)
	sort.Slice(entries, func(i, j int) bool {
		return len(entries[i].Path) < len(entries[j].Path)
	})

	for _, entry := range entries {
		fullPath := filepath.Join(targetDir, entry.Path)

		switch entry.Type {
		case EntryTypeDir:
			if err := os.MkdirAll(fullPath, os.FileMode(entry.Mode&0777)); err != nil {
				return fmt.Errorf("mkdir %s: %w", entry.Path, err)
			}
			// Set ownership (best effort)
			os.Chown(fullPath, int(entry.UID), int(entry.GID))

		case EntryTypeSymlink:
			// Remove existing (might be from parent clone)
			os.Remove(fullPath)
			if err := os.Symlink(entry.LinkTarget, fullPath); err != nil {
				return fmt.Errorf("symlink %s: %w", entry.Path, err)
			}
			// Note: symlink ownership set via Lchown (not Chown)
			os.Lchown(fullPath, int(entry.UID), int(entry.GID))

		case EntryTypeCharDev, EntryTypeBlockDev:
			// Device nodes are intentionally not recreated on download: mknod
			// requires CAP_MKNOD and downloads are not assumed to run as root.
			// The entry's metadata is preserved in the manifest, but no device
			// node is materialized.

		case EntryTypeFile:
			// Already handled in downloadFiles
		}
	}

	return nil
}

// fetchFullFile downloads an entire file from a URL.
func fetchFullFile(client *http.Client, url string) ([]byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

type rangeSpec struct {
	offset int64
	size   int64
}

// fetchRanges fetches multiple ranges from a URL using HTTP Range header.
func fetchRanges(client *http.Client, url string, ranges []rangeSpec) ([][]byte, error) {
	if len(ranges) == 0 {
		return nil, nil
	}

	// For simplicity, fetch each range separately
	// (Could be optimized with multipart ranges later)
	results := make([][]byte, len(ranges))

	for i, r := range ranges {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", r.offset, r.offset+r.size-1))

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			resp.Body.Close()
			return nil, fmt.Errorf("HTTP %d for range %d-%d", resp.StatusCode, r.offset, r.offset+r.size-1)
		}

		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}

		results[i] = data
	}

	return results, nil
}
