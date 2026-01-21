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
	"time"

	"github.com/pborman/getopt/v2"
	"github.com/tailscale/thundersnap/bupdate"
	"golang.org/x/term"
)

// progress tracks the current progress display state
type progress struct {
	termWidth    int
	fileNum      int
	totalFiles   int
	sizeStrWidth int       // fixed width for the size portion like "[DL 999M/999M]"
	lastUpdate   time.Time // last time we printed an update
}

// newProgress creates a new progress tracker
func newProgress(totalFiles int, maxFileSize int64) *progress {
	width := 80 // default
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		width = w
	}

	// Calculate the width needed for the size string based on max file size
	// Format: "[DL %dM/%dM]" - DL is the longest prefix (2 chars vs 0)
	maxM := maxFileSize / (1024 * 1024)
	maxMStr := fmt.Sprintf("%d", maxM)
	// "[DL " + maxM + "M/" + maxM + "M]" = 4 + len + 2 + len + 2 = 8 + 2*len
	sizeStrWidth := 8 + 2*len(maxMStr)

	return &progress{
		termWidth:    width,
		totalFiles:   totalFiles,
		sizeStrWidth: sizeStrWidth,
	}
}

// setFile sets the current file number (1-indexed)
func (p *progress) setFile(n int) {
	p.fileNum = n
}

// status prints a status line for the current file
// mode is "DL" for downloading, "" for writing
func (p *progress) status(mode string, current, total int64, filename string) {
	// Rate limit updates to once per 100ms
	now := time.Now()
	if now.Sub(p.lastUpdate) < 100*time.Millisecond {
		return
	}
	p.lastUpdate = now

	// Format: [File %d/%d] [%dM/%dM] <filename>
	// or:     [File %d/%d] [DL %dM/%dM] <filename>

	currentM := current / (1024 * 1024)
	totalM := total / (1024 * 1024)

	var sizeStr string
	if mode == "DL" {
		sizeStr = fmt.Sprintf("[DL %dM/%dM]", currentM, totalM)
	} else {
		sizeStr = fmt.Sprintf("[%dM/%dM]", currentM, totalM)
	}
	// Left-pad to fixed width
	if len(sizeStr) < p.sizeStrWidth {
		sizeStr = strings.Repeat(" ", p.sizeStrWidth-len(sizeStr)) + sizeStr
	}

	fileStr := fmt.Sprintf("[File %d/%d]", p.fileNum, p.totalFiles)

	// Calculate space for filename
	prefixLen := len(fileStr) + 1 + p.sizeStrWidth + 1 // +1 for spaces
	maxFilenameLen := p.termWidth - prefixLen
	if maxFilenameLen < 3 {
		maxFilenameLen = 3
	}

	displayName := filename
	if len(displayName) > maxFilenameLen {
		// Truncate with ellipsis at start
		displayName = "..." + displayName[len(displayName)-maxFilenameLen+3:]
	}

	line := fmt.Sprintf("%s %s %s", fileStr, sizeStr, displayName)

	// Pad to terminal width to erase previous content
	if len(line) < p.termWidth {
		line = line + strings.Repeat(" ", p.termWidth-len(line))
	} else if len(line) > p.termWidth {
		line = line[:p.termWidth]
	}

	fmt.Printf("\r%s", line)
}

// clear clears the current line
func (p *progress) clear() {
	fmt.Printf("\r%s\r", strings.Repeat(" ", p.termWidth))
}

// done prints a final newline
func (p *progress) done() {
	fmt.Println()
}

// remoteSource abstracts access to remote files, either via filesystem or HTTP.
type remoteSource struct {
	isHTTP     bool
	baseURL    string              // for HTTP: base URL without trailing slash
	basePath   string              // for filesystem: base directory path
	httpReader *bupdate.HTTPReader // reused HTTP reader for chunk fetches (shared across all files)
}

// newRemoteSource creates a remoteSource from a remote path or URL.
func newRemoteSource(remote string) (*remoteSource, error) {
	if bupdate.IsHTTPURL(remote) {
		baseURL := strings.TrimSuffix(remote, "/")
		// Create a shared HTTP reader for all files on this host
		reader, err := bupdate.NewHTTPReaderForHost(baseURL)
		if err != nil {
			return nil, fmt.Errorf("creating HTTP reader: %w", err)
		}
		return &remoteSource{
			isHTTP:     true,
			baseURL:    baseURL,
			httpReader: reader,
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

// httpPath returns the URL path for a file (for use with the shared HTTP reader).
func (r *remoteSource) httpPath(relativePath string) string {
	u, _ := url.Parse(r.baseURL + "/" + relativePath)
	return u.Path
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
	// Load all local fidx files to build chunk mappings
	mappings, filesByChecksums, err := loadLocalMappings(localDir)
	if err != nil {
		return fmt.Errorf("loading local mappings: %w", err)
	}

	// Create remote source (filesystem or HTTP)
	remote, err := newRemoteSource(remoteDir)
	if err != nil {
		return fmt.Errorf("creating remote source: %w", err)
	}
	defer remote.close()

	// Load the remote fidx file
	remoteFidx, err := remote.loadFidx(targetFidx)
	if err != nil {
		return fmt.Errorf("loading remote fidx: %w", err)
	}

	// Check if we already have this index
	localFidxPath := filepath.Join(localDir, targetFidx)
	if localFidx, err := bupdate.LoadFidx(localFidxPath); err == nil {
		if bytes.Equal(localFidx.FileSHA[:], remoteFidx.FileSHA[:]) {
			return nil // already up to date
		}
	}

	if remoteFidx.IsMFIDX {
		// Multi-file index - extract all files
		return bupdateMFIDX(localDir, remote, remoteFidx, targetFidx, localFidxPath, mappings, filesByChecksums)
	}

	// Single file reconstruction
	prog := newProgress(1, remoteFidx.FileSize)
	prog.setFile(1)

	// Determine output filename (strip .fidx extension)
	outputName := strings.TrimSuffix(targetFidx, ".fidx")
	outputPath := filepath.Join(localDir, outputName)
	tmpOutputPath := outputPath + ".tmp"

	// Remote file path is <fidx-hash>/bin
	remoteFilePath := outputName + "/bin"

	// Reconstruct the file
	if err := reconstructFile(tmpOutputPath, remoteFidx, remote, remoteFilePath, mappings, prog); err != nil {
		os.Remove(tmpOutputPath)
		prog.clear()
		return fmt.Errorf("reconstructing file: %w", err)
	}

	// Atomically rename to final location
	if err := os.Rename(tmpOutputPath, outputPath); err != nil {
		prog.clear()
		return fmt.Errorf("rename: %w", err)
	}

	// Copy the fidx file to local
	if err := remote.copyFidx(localFidxPath, targetFidx); err != nil {
		prog.clear()
		return fmt.Errorf("copying fidx: %w", err)
	}

	prog.clear()
	return nil
}

// bupdateMFIDX handles reconstruction of all files from a multi-file index
func bupdateMFIDX(localDir string, remote *remoteSource, remoteFidx *bupdate.Fidx, targetFidx, localFidxPath string, mappings *bupdate.FidxMappings, filesByChecksums *bupdate.FileByChecksums) error {
	// Find the maximum file size for consistent progress display width
	var maxFileSize int64
	for _, fileEntry := range remoteFidx.Files {
		if int64(fileEntry.FileSize) > maxFileSize {
			maxFileSize = int64(fileEntry.FileSize)
		}
	}
	prog := newProgress(len(remoteFidx.Files), maxFileSize)

	// Remote base path is the fidx name without extension (e.g., "e2682e8932c7c1cd0ca8ce01330f0265")
	remoteBasePath := strings.TrimSuffix(targetFidx, ".fidx")
	remoteBasePath = strings.TrimSuffix(remoteBasePath, ".mfidx")

	// Reconstruct each file
	for i, fileEntry := range remoteFidx.Files {
		prog.setFile(i + 1)

		// Output path includes the fidx basename as a directory prefix
		// e.g., xyz/e2682e8932c7c1cd0ca8ce01330f0265/bin
		outputPath := filepath.Join(localDir, remoteBasePath, fileEntry.Filename)
		tmpOutputPath := outputPath + ".tmp"

		// Remote file path includes the fidx base path
		remoteFilePath := remoteBasePath + "/" + fileEntry.Filename

		// Ensure parent directory exists
		if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
			prog.clear()
			return fmt.Errorf("creating directory for %s: %w", fileEntry.Filename, err)
		}

		// Check if remote is a symlink (only possible for filesystem sources)
		isSymlink := false
		if !remote.isHTTP {
			remoteFilePathLocal := remote.filePath(remoteFilePath)
			remoteInfo, err := os.Lstat(remoteFilePathLocal)
			isSymlink = err == nil && remoteInfo.Mode()&os.ModeSymlink != 0
		}

		if isSymlink {
			// Create a temporary Fidx struct for this file
			fileFidx := &bupdate.Fidx{
				Entries:  fileEntry.Entries,
				FileSize: int64(fileEntry.FileSize),
			}
			// For symlinks, reconstruct the target string then create symlink
			remoteFilePathLocal := remote.filePath(remoteFilePath)
			if err := reconstructSymlinkFromMFIDX(outputPath, fileFidx, remoteFilePathLocal, fileEntry, mappings, prog); err != nil {
				prog.clear()
				return fmt.Errorf("reconstructing symlink %s: %w", fileEntry.Filename, err)
			}
		} else {
			// Regular file - check if we can use COW clone
			if cloneSource, ok := filesByChecksums.Find(fileEntry.Entries); ok {
				// Found an identical file - try COW clone
				os.Remove(outputPath) // Remove any existing file first
				if err := bupdate.CloneFile(outputPath, cloneSource); err == nil {
					// Clone succeeded, skip reconstruction
					continue
				}
				// Clone failed (maybe not on btrfs), fall through to normal reconstruction
			}

			// Create a temporary Fidx struct for this file
			fileFidx := &bupdate.Fidx{
				Entries:  fileEntry.Entries,
				FileSize: int64(fileEntry.FileSize),
			}

			// Predict download for this specific file
			missing, _ := predictDownloadForEntries(fileEntry.Entries, mappings)

			if err := reconstructFileFromMFIDX(tmpOutputPath, fileFidx, remote, remoteFilePath, fileEntry, mappings, prog, missing); err != nil {
				os.Remove(tmpOutputPath)
				prog.clear()
				return fmt.Errorf("reconstructing %s: %w", fileEntry.Filename, err)
			}

			// Atomically rename to final location
			if err := os.Rename(tmpOutputPath, outputPath); err != nil {
				prog.clear()
				return fmt.Errorf("rename %s: %w", fileEntry.Filename, err)
			}
		}

		// Register this newly created file for future COW clones within this run
		if !isSymlink {
			filesByChecksums.Add(fileEntry.Entries, outputPath)
		}
	}

	prog.clear()

	// Copy the mfidx file to local
	if err := remote.copyFidx(localFidxPath, targetFidx); err != nil {
		return fmt.Errorf("copying mfidx: %w", err)
	}

	return nil
}

// loadLocalMappings scans the local directory for .fidx and .mfidx files and builds chunk mappings.
// It also returns a FileByChecksums map for COW clone optimization.
func loadLocalMappings(dir string) (*bupdate.FidxMappings, *bupdate.FileByChecksums, error) {
	var allMappings []bupdate.FidxMapping
	filesByChecksums := bupdate.NewFileByChecksums()

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
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
			continue // skip invalid/corrupted fidx files silently
		}

		if fidx.IsMFIDX {
			// Multi-file index - process each file within it
			// The fidx basename (without extension) is used as a directory prefix
			fidxBaseName := strings.TrimSuffix(entry.Name(), ".fidx")
			fidxBaseName = strings.TrimSuffix(fidxBaseName, ".mfidx")

			for _, fileEntry := range fidx.Files {
				// Files are stored under <dir>/<fidx-basename>/<filename>
				filePath := filepath.Join(dir, fidxBaseName, fileEntry.Filename)

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
					// Don't add symlinks to filesByChecksums - can't FICLONE symlinks
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

				// Register this file for potential COW cloning
				filesByChecksums.Add(fileEntry.Entries, filePath)
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

			// Register this file for potential COW cloning
			filesByChecksums.Add(fidx.Entries, filePath)
		}
	}

	// Sort mappings by SHA for binary search
	sort.Slice(allMappings, func(i, j int) bool {
		return bytes.Compare(allMappings[i].SHA[:], allMappings[j].SHA[:]) < 0
	})

	return &bupdate.FidxMappings{Mappings: allMappings}, filesByChecksums, nil
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
func reconstructSymlinkFromMFIDX(outputPath string, fidx *bupdate.Fidx, remoteFilePath string, fileEntry bupdate.FileEntry, mappings *bupdate.FidxMappings, prog *progress) error {
	// Remove existing file/symlink if it exists
	os.Remove(outputPath)

	// Try to read the symlink target from remote (may not exist)
	symlinkTarget, remoteErr := os.Readlink(remoteFilePath)
	hasRemote := remoteErr == nil

	// Reconstruct the target string from chunks (should be a single chunk)
	var reconstructedTarget []byte
	downloaded := int64(0)
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

			downloaded += chunkSize
			prog.status("DL", downloaded, missing, fileEntry.Filename)
		}

		reconstructedTarget = append(reconstructedTarget, chunkData...)
	}

	// Create the symlink with the reconstructed target
	targetStr := string(reconstructedTarget)
	if err := os.Symlink(targetStr, outputPath); err != nil {
		return fmt.Errorf("creating symlink: %w", err)
	}

	return nil
}

// reconstructFileFromMFIDX rebuilds a file from an mfidx by combining local and remote chunks
func reconstructFileFromMFIDX(outputPath string, fidx *bupdate.Fidx, remote *remoteSource, remoteFilePath string, fileEntry bupdate.FileEntry, mappings *bupdate.FidxMappings, prog *progress, missing int64) error {
	outf, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer outf.Close()

	// Pre-allocate the file to its final size (helps filesystem allocate extents)
	if err := outf.Truncate(fidx.FileSize); err != nil {
		return fmt.Errorf("truncating file: %w", err)
	}

	// Build list of chunks we need, marking which are local vs remote
	var chunks []chunkInfo
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
	}

	// Collect remote chunks we need to fetch (excluding zero blocks)
	var remoteChunks []chunkInfo
	for i := range chunks {
		if chunks[i].localMapping == nil {
			// Skip zero blocks - we'll leave holes for them
			if chunks[i].ent.Size == bupdate.BLOB_MAX && chunks[i].ent.SHA == bupdate.ZeroBlockSHA {
				continue
			}
			remoteChunks = append(remoteChunks, chunks[i])
		}
	}

	// Fetch remote chunks (using pipelining for HTTP)
	var remoteData map[int][]byte
	if len(remoteChunks) > 0 {
		if remote.isHTTP {
			// Use HTTP pipelining with shared connection
			httpPath := remote.httpPath(remoteFilePath)
			remoteData, err = fetchChunksHTTP(remote.httpReader, httpPath, remoteChunks, missing, prog, fileEntry.Filename)
			if err != nil {
				return fmt.Errorf("fetching remote chunks: %w", err)
			}
		} else {
			// Use filesystem
			remoteFilePathFull := remote.filePath(remoteFilePath)
			remoteData, err = fetchChunksFilesystem(remoteFilePathFull, remoteChunks, missing, prog, fileEntry.Filename)
			if err != nil {
				return fmt.Errorf("fetching remote chunks: %w", err)
			}
		}
	}

	// Now write all chunks in order
	var written int64
	for i, ci := range chunks {
		chunkSize := int64(ci.ent.Size)

		// Check for zero block - leave a hole instead of writing
		if ci.ent.Size == bupdate.BLOB_MAX && ci.ent.SHA == bupdate.ZeroBlockSHA {
			// Seek forward to leave a sparse hole (file was pre-truncated with zeros)
			if _, err := outf.Seek(chunkSize, 1); err != nil {
				return fmt.Errorf("seeking past zero block: %w", err)
			}
			written += chunkSize
			prog.status("", written, fidx.FileSize, fileEntry.Filename)
			continue
		}

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
		written += int64(len(data))
		prog.status("", written, fidx.FileSize, fileEntry.Filename)
	}

	return nil
}

// reconstructFile rebuilds the output file by combining local and remote chunks
func reconstructFile(outputPath string, fidx *bupdate.Fidx, remote *remoteSource, remoteFileName string, mappings *bupdate.FidxMappings, prog *progress) error {
	outf, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer outf.Close()

	// Pre-allocate the file to its final size (helps filesystem allocate extents)
	if err := outf.Truncate(fidx.FileSize); err != nil {
		return fmt.Errorf("truncating file: %w", err)
	}

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

	// Collect remote chunks we need to fetch (excluding zero blocks)
	var remoteChunks []chunkInfo
	for i := range chunks {
		if chunks[i].localMapping == nil {
			// Skip zero blocks - we'll leave holes for them
			if chunks[i].ent.Size == bupdate.BLOB_MAX && chunks[i].ent.SHA == bupdate.ZeroBlockSHA {
				continue
			}
			remoteChunks = append(remoteChunks, chunks[i])
		}
	}

	// Fetch remote chunks (using pipelining for HTTP)
	var remoteData map[int][]byte
	if len(remoteChunks) > 0 {
		if remote.isHTTP {
			// Use HTTP pipelining with shared connection
			httpPath := remote.httpPath(remoteFileName)
			remoteData, err = fetchChunksHTTP(remote.httpReader, httpPath, remoteChunks, missing, prog, remoteFileName)
			if err != nil {
				return fmt.Errorf("fetching remote chunks: %w", err)
			}
		} else {
			// Use filesystem
			remoteFilePath := remote.filePath(remoteFileName)
			remoteData, err = fetchChunksFilesystem(remoteFilePath, remoteChunks, missing, prog, remoteFileName)
			if err != nil {
				return fmt.Errorf("fetching remote chunks: %w", err)
			}
		}
	}

	// Now write all chunks in order
	var written int64
	for i, ci := range chunks {
		chunkSize := int64(ci.ent.Size)

		// Check for zero block - leave a hole instead of writing
		if ci.ent.Size == bupdate.BLOB_MAX && ci.ent.SHA == bupdate.ZeroBlockSHA {
			// Seek forward to leave a sparse hole (file was pre-truncated with zeros)
			if _, err := outf.Seek(chunkSize, 1); err != nil {
				return fmt.Errorf("seeking past zero block: %w", err)
			}
			written += chunkSize
			prog.status("", written, fidx.FileSize, remoteFileName)
			continue
		}

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
		written += int64(len(data))
		prog.status("", written, fidx.FileSize, remoteFileName)
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
// The path parameter specifies which file to fetch from on the server.
func fetchChunksHTTP(reader *bupdate.HTTPReader, path string, chunks []chunkInfo, totalMissing int64, prog *progress, filename string) (map[int][]byte, error) {
	result := make(map[int][]byte)
	downloaded := int64(0)

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

		// Send pipelined requests using the specified path
		results, err := reader.ReadRangesFromPath(path, requests)
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
			downloaded += int64(len(data))
			prog.status("DL", downloaded, totalMissing, filename)
		}
	}

	return result, nil
}

// fetchChunksFilesystem fetches chunks from a local file.
func fetchChunksFilesystem(filePath string, chunks []chunkInfo, totalMissing int64, prog *progress, filename string) (map[int][]byte, error) {
	result := make(map[int][]byte)
	downloaded := int64(0)

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
		downloaded += int64(len(data))
		prog.status("DL", downloaded, totalMissing, filename)
	}

	return result, nil
}



