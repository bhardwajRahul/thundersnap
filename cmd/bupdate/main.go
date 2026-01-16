// bupdate reconstructs files from fidx indexes by combining local and remote chunks.
// Given a remote fidx file and a directory full of existing files with their fidx indexes,
// it downloads only the chunks that don't already exist locally and reconstructs the file.
package main

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pborman/getopt/v2"
	"github.com/tailscale/thundersnap/bupdate"
)

// remoteSource abstracts access to remote files, either via filesystem or HTTP.
type remoteSource struct {
	isHTTP     bool
	baseURL    string              // for HTTP: base URL without trailing slash
	basePath   string              // for filesystem: base directory path
	httpReader *bupdate.HTTPReader // reused HTTP reader for chunk fetches
}

// newRemoteSource creates a remoteSource from a remote path or URL.
func newRemoteSource(remote string) (*remoteSource, error) {
	if bupdate.IsHTTPURL(remote) {
		return &remoteSource{
			isHTTP:  true,
			baseURL: strings.TrimSuffix(remote, "/"),
		}, nil
	}
	return &remoteSource{
		isHTTP:   false,
		basePath: remote,
	}, nil
}

// loadFidx loads a fidx file from the remote source.
func (r *remoteSource) loadFidx(relativePath string) (*bupdate.Fidx, error) {
	if r.isHTTP {
		fullURL := r.baseURL + "/" + relativePath
		return bupdate.LoadFidxHTTP(fullURL)
	}
	fullPath := filepath.Join(r.basePath, relativePath)
	return bupdate.LoadFidx(fullPath)
}

// copyFidx copies a fidx file from remote to local.
func (r *remoteSource) copyFidx(localPath, relativePath string) error {
	if r.isHTTP {
		// For HTTP, we need to download the fidx file
		fullURL := r.baseURL + "/" + relativePath
		fidx, err := bupdate.LoadFidxHTTP(fullURL)
		if err != nil {
			return err
		}
		// Re-download the raw data to save it locally
		// (We could optimize by caching, but this is simple)
		u, _ := url.Parse(fullURL)
		host := u.Host
		if !strings.Contains(host, ":") {
			host = host + ":80"
		}
		reader, err := bupdate.NewHTTPReader(fullURL)
		if err != nil {
			return err
		}
		defer reader.Close()
		// Get the file size by requesting the whole file
		// Actually, let's just re-serialize the fidx... but that's complex.
		// Instead, let's download it fresh.
		_ = fidx // suppress unused warning
		// Simple approach: fetch the whole file via a full GET
		return downloadFile(fullURL, localPath)
	}
	remotePath := filepath.Join(r.basePath, relativePath)
	return bupdate.CopyFile(localPath, remotePath)
}

// downloadFile downloads a file from an HTTP URL to a local path.
func downloadFile(rawURL, localPath string) error {
	data, err := bupdate.FetchFullFile(rawURL)
	if err != nil {
		return err
	}
	return os.WriteFile(localPath, data, 0644)
}

// getHTTPReaderForFile gets an HTTP reader for a specific file path.
func (r *remoteSource) getHTTPReaderForFile(relativePath string) (*bupdate.HTTPReader, error) {
	if !r.isHTTP {
		return nil, fmt.Errorf("not an HTTP source")
	}
	fullURL := r.baseURL + "/" + relativePath
	return bupdate.NewHTTPReader(fullURL)
}

// filePath returns the full path for a file in the remote source (filesystem only).
func (r *remoteSource) filePath(relativePath string) string {
	return filepath.Join(r.basePath, relativePath)
}

// close releases any resources held by the remote source.
func (r *remoteSource) close() {
	if r.httpReader != nil {
		r.httpReader.Close()
		r.httpReader = nil
	}
}

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

	// Create remote source (filesystem or HTTP)
	remote, err := newRemoteSource(remoteDir)
	if err != nil {
		return fmt.Errorf("creating remote source: %w", err)
	}
	defer remote.close()

	// Load the remote fidx file
	fmt.Printf("\nProcessing remote fidx: %s/%s\n", remoteDir, targetFidx)

	remoteFidx, err := remote.loadFidx(targetFidx)
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
		return bupdateMFIDX(localDir, remote, remoteFidx, targetFidx, localFidxPath, mappings)
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
	if err := reconstructFile(tmpOutputPath, remoteFidx, remote, outputName, mappings); err != nil {
		os.Remove(tmpOutputPath)
		return fmt.Errorf("reconstructing file: %w", err)
	}

	// Atomically rename to final location
	if err := os.Rename(tmpOutputPath, outputPath); err != nil {
		return fmt.Errorf("rename: %w", err)
	}

	// Copy the fidx file to local
	if err := remote.copyFidx(localFidxPath, targetFidx); err != nil {
		return fmt.Errorf("copying fidx: %w", err)
	}

	fmt.Printf("  successfully reconstructed: %s\n", outputPath)
	return nil
}

// bupdateMFIDX handles reconstruction of all files from a multi-file index
func bupdateMFIDX(localDir string, remote *remoteSource, remoteFidx *bupdate.Fidx, targetFidx, localFidxPath string, mappings *bupdate.FidxMappings) error {
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

		// Check if remote is a symlink (only possible for filesystem sources)
		isSymlink := false
		if !remote.isHTTP {
			remoteFilePath := remote.filePath(fileEntry.Filename)
			remoteInfo, err := os.Lstat(remoteFilePath)
			isSymlink = err == nil && remoteInfo.Mode()&os.ModeSymlink != 0
		}

		if isSymlink {
			// For symlinks, reconstruct the target string then create symlink
			remoteFilePath := remote.filePath(fileEntry.Filename)
			if err := reconstructSymlinkFromMFIDX(outputPath, fileFidx, remoteFilePath, fileEntry, mappings); err != nil {
				return fmt.Errorf("reconstructing symlink %s: %w", fileEntry.Filename, err)
			}
		} else {
			// Regular file
			if err := reconstructFileFromMFIDX(tmpOutputPath, fileFidx, remote, fileEntry, mappings); err != nil {
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
	if err := remote.copyFidx(localFidxPath, targetFidx); err != nil {
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
				// Use the full path from the mfidx, preserving directory structure
				filePath := filepath.Join(dir, fileEntry.Filename)

				// Check if the file exists (use Lstat to handle symlinks)
				stat, err := os.Lstat(filePath)
				if err != nil {
					continue
				}

				// Handle symlinks specially - their content is the target path
				if stat.Mode()&os.ModeSymlink != 0 {
					target, err := os.Readlink(filePath)
					if err != nil {
						continue
					}
					// For symlinks, we treat the entire target as a single chunk
					// The offset is always 0 and size is len(target)
					for _, ent := range fileEntry.Entries {
						allMappings = append(allMappings, bupdate.FidxMapping{
							SHA:      ent.SHA,
							Filename: filePath,
							Offset:   0,
							Size:     ent.Size,
						})
					}
					_ = target // used for documentation
					continue
				}

				// Regular file - add mappings for each chunk
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
func reconstructFileFromMFIDX(outputPath string, fidx *bupdate.Fidx, remote *remoteSource, fileEntry bupdate.FileEntry, mappings *bupdate.FidxMappings) error {
	outf, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer outf.Close()

	// Build list of chunks we need, marking which are local vs remote
	var chunks []chunkInfo
	var remoteOffset int64
	var missing int64

	for i, ent := range fidx.Entries {
		mapping := mappings.FindMapping(ent.SHA)
		ci := chunkInfo{
			ent:          ent,
			localMapping: mapping,
			remoteOffset: remoteOffset,
			outputIdx:    i,
		}
		if mapping == nil {
			missing += int64(ent.Size)
		}
		chunks = append(chunks, ci)
		remoteOffset += int64(ent.Size)
	}

	// Collect remote chunks we need to fetch
	var remoteChunks []chunkInfo
	for i := range chunks {
		if chunks[i].localMapping == nil {
			remoteChunks = append(remoteChunks, chunks[i])
		}
	}

	// Fetch remote chunks (using pipelining for HTTP)
	var remoteData map[int][]byte
	if len(remoteChunks) > 0 {
		if remote.isHTTP {
			// Use HTTP pipelining
			httpReader, err := remote.getHTTPReaderForFile(fileEntry.Filename)
			if err != nil {
				return fmt.Errorf("creating HTTP reader: %w", err)
			}
			defer httpReader.Close()

			remoteData, err = fetchChunksHTTP(httpReader, remoteChunks, missing)
			if err != nil {
				return fmt.Errorf("fetching remote chunks: %w", err)
			}
		} else {
			// Use filesystem
			remoteFilePath := remote.filePath(fileEntry.Filename)
			remoteData, err = fetchChunksFilesystem(remoteFilePath, remoteChunks, missing)
			if err != nil {
				return fmt.Errorf("fetching remote chunks: %w", err)
			}
		}
	}

	// Now write all chunks in order
	for i, ci := range chunks {
		var data []byte

		if ci.localMapping != nil {
			// Read from local
			data, err = bupdate.ReadChunk(ci.localMapping.Filename, ci.localMapping.Offset, int64(ci.localMapping.Size))
			if err != nil {
				// Fall back to remote if available
				if rd, ok := remoteData[i]; ok {
					data = rd
				} else {
					return fmt.Errorf("reading local chunk: %w", err)
				}
			} else {
				// Verify SHA
				computedSHA := bupdate.BlobSHA(data)
				if !bytes.Equal(computedSHA[:], ci.ent.SHA[:]) {
					// Fall back to remote if available
					if rd, ok := remoteData[i]; ok {
						data = rd
					} else {
						return fmt.Errorf("local chunk checksum mismatch and no remote available")
					}
				}
			}
		} else {
			// Get from pre-fetched remote data
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

	if missing > 0 {
		fmt.Println() // newline after progress
	}

	return nil
}

// reconstructFile rebuilds the output file by combining local and remote chunks
func reconstructFile(outputPath string, fidx *bupdate.Fidx, remote *remoteSource, remoteFileName string, mappings *bupdate.FidxMappings) error {
	outf, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer outf.Close()

	// Build list of chunks we need, marking which are local vs remote
	var chunks []chunkInfo
	var remoteOffset int64
	var missing int64

	for i, ent := range fidx.Entries {
		mapping := mappings.FindMapping(ent.SHA)
		ci := chunkInfo{
			ent:          ent,
			localMapping: mapping,
			remoteOffset: remoteOffset,
			outputIdx:    i,
		}
		if mapping == nil {
			missing += int64(ent.Size)
		}
		chunks = append(chunks, ci)
		remoteOffset += int64(ent.Size)
	}

	// Collect remote chunks we need to fetch
	var remoteChunks []chunkInfo
	for i := range chunks {
		if chunks[i].localMapping == nil {
			remoteChunks = append(remoteChunks, chunks[i])
		}
	}

	// Fetch remote chunks (using pipelining for HTTP)
	var remoteData map[int][]byte
	if len(remoteChunks) > 0 {
		if remote.isHTTP {
			// Use HTTP pipelining
			httpReader, err := remote.getHTTPReaderForFile(remoteFileName)
			if err != nil {
				return fmt.Errorf("creating HTTP reader: %w", err)
			}
			defer httpReader.Close()

			remoteData, err = fetchChunksHTTP(httpReader, remoteChunks, missing)
			if err != nil {
				return fmt.Errorf("fetching remote chunks: %w", err)
			}
		} else {
			// Use filesystem
			remoteFilePath := remote.filePath(remoteFileName)
			remoteData, err = fetchChunksFilesystem(remoteFilePath, remoteChunks, missing)
			if err != nil {
				return fmt.Errorf("fetching remote chunks: %w", err)
			}
		}
	}

	// Now write all chunks in order
	for i, ci := range chunks {
		var data []byte

		if ci.localMapping != nil {
			// Read from local
			data, err = bupdate.ReadChunk(ci.localMapping.Filename, ci.localMapping.Offset, int64(ci.localMapping.Size))
			if err != nil {
				// Fall back to remote if available
				if rd, ok := remoteData[i]; ok {
					data = rd
				} else {
					return fmt.Errorf("reading local chunk: %w", err)
				}
			} else {
				// Verify SHA
				computedSHA := bupdate.BlobSHA(data)
				if !bytes.Equal(computedSHA[:], ci.ent.SHA[:]) {
					// Fall back to remote if available
					if rd, ok := remoteData[i]; ok {
						data = rd
					} else {
						return fmt.Errorf("local chunk checksum mismatch and no remote available")
					}
				}
			}
		} else {
			// Get from pre-fetched remote data
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

	if missing > 0 {
		fmt.Println() // newline after progress
	}

	return nil
}

// chunkInfo is used for tracking chunk fetch operations
type chunkInfo struct {
	ent           bupdate.FidxEntry
	localMapping  *bupdate.FidxMapping
	remoteOffset  int64
	outputIdx     int
}

// fetchChunksHTTP fetches chunks using HTTP pipelining.
// It batches requests in groups to balance pipelining benefits with memory usage.
func fetchChunksHTTP(reader *bupdate.HTTPReader, chunks []chunkInfo, totalMissing int64) (map[int][]byte, error) {
	result := make(map[int][]byte)
	got := int64(0)

	// Process chunks in batches for pipelining
	// Use batches of up to 16 requests to balance efficiency with memory
	const batchSize = 16

	for i := 0; i < len(chunks); i += batchSize {
		end := i + batchSize
		if end > len(chunks) {
			end = len(chunks)
		}
		batch := chunks[i:end]

		// Build range requests for this batch
		requests := make([]bupdate.RangeRequest, len(batch))
		for j, ci := range batch {
			requests[j] = bupdate.RangeRequest{
				Offset: ci.remoteOffset,
				Size:   int64(ci.ent.Size),
			}
		}

		// Send pipelined requests
		results, err := reader.ReadRanges(requests)
		if err != nil {
			return nil, fmt.Errorf("reading ranges: %w", err)
		}

		// Verify and store results
		for j, data := range results {
			ci := batch[j]

			// Verify SHA
			computedSHA := bupdate.BlobSHA(data)
			if !bytes.Equal(computedSHA[:], ci.ent.SHA[:]) {
				return nil, fmt.Errorf("remote chunk checksum mismatch at offset %d", ci.remoteOffset)
			}

			result[ci.outputIdx] = data
			got += int64(len(data))

			if totalMissing > 0 {
				pct := (got * 100) / totalMissing
				fmt.Printf("\r  Downloading... %d%% (%d/%d bytes)", pct, got, totalMissing)
			}
		}
	}

	return result, nil
}

// fetchChunksFilesystem fetches chunks from a local file.
func fetchChunksFilesystem(filePath string, chunks []chunkInfo, totalMissing int64) (map[int][]byte, error) {
	result := make(map[int][]byte)
	got := int64(0)

	for _, ci := range chunks {
		data, err := bupdate.ReadChunk(filePath, ci.remoteOffset, int64(ci.ent.Size))
		if err != nil {
			return nil, fmt.Errorf("reading chunk at offset %d: %w", ci.remoteOffset, err)
		}

		// Verify SHA
		computedSHA := bupdate.BlobSHA(data)
		if !bytes.Equal(computedSHA[:], ci.ent.SHA[:]) {
			return nil, fmt.Errorf("remote chunk checksum mismatch at offset %d", ci.remoteOffset)
		}

		result[ci.outputIdx] = data
		got += int64(len(data))

		if totalMissing > 0 {
			pct := (got * 100) / totalMissing
			fmt.Printf("\r  Downloading... %d%% (%d/%d bytes)", pct, got, totalMissing)
		}
	}

	return result, nil
}



