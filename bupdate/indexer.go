package bupdate

import (
	"bufio"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

// IndexerOptions configures the fidx indexer
type IndexerOptions struct {
	// RefPath is the path to a reference mfidx file for incremental indexing.
	// If set, unchanged files will reuse entries from the reference.
	RefPath string

	// Progress enables progress reporting to stderr.
	// Only reports if stderr is a terminal.
	Progress bool
}

// RefFileInfo holds metadata about a file in the reference mfidx
type RefFileInfo struct {
	Entry   FileEntry
	RefPath string // path to the actual file in the reference filesystem
}

// progress tracks the current progress display state
type progress struct {
	w           io.Writer
	termWidth   int
	fileCount   int   // number of files processed so far
	totalBytes  int64 // total bytes in files so far (completed files + current file progress)
	lastUpdate  time.Time
	hasRef      bool // whether we're using a reference mfidx
}

// newProgress creates a new progress tracker
func newProgress(w io.Writer, hasRef bool) *progress {
	width := 80 // default
	if f, ok := w.(*os.File); ok {
		if w, _, err := term.GetSize(int(f.Fd())); err == nil && w > 0 {
			width = w
		}
	}
	return &progress{
		w:         w,
		termWidth: width,
		hasRef:    hasRef,
	}
}

// status prints a status line for the current file
// mode is "new/changed", "unchanged", or "" (no ref)
// currentFileBytes is progress within the current file (for large files)
func (p *progress) status(mode string, filename string, currentFileBytes int64) {
	if p.w == nil {
		return
	}

	// Rate limit updates to once per 100ms
	now := time.Now()
	if now.Sub(p.lastUpdate) < 100*time.Millisecond {
		return
	}
	p.lastUpdate = now

	totalM := (p.totalBytes + currentFileBytes) / (1024 * 1024)

	// Format: [%d files] [%dM] <mode> <filename>
	var prefix string
	if mode != "" {
		prefix = fmt.Sprintf("[%d files] [%dM] (%s)", p.fileCount, totalM, mode)
	} else {
		prefix = fmt.Sprintf("[%d files] [%dM]", p.fileCount, totalM)
	}

	// Calculate space for filename
	prefixLen := len(prefix) + 1 // +1 for space
	maxFilenameLen := p.termWidth - prefixLen
	if maxFilenameLen < 3 {
		maxFilenameLen = 3
	}

	displayName := filename
	if len(displayName) > maxFilenameLen {
		// Truncate with ellipsis at start
		displayName = "..." + displayName[len(displayName)-maxFilenameLen+3:]
	}

	line := fmt.Sprintf("%s %s", prefix, displayName)

	// Pad to terminal width to erase previous content
	if len(line) < p.termWidth {
		line = line + strings.Repeat(" ", p.termWidth-len(line))
	} else if len(line) > p.termWidth {
		line = line[:p.termWidth]
	}

	fmt.Fprintf(p.w, "\r%s", line)
}

// fileStarted increments the file count
func (p *progress) fileStarted() {
	p.fileCount++
}

// fileCompleted adds the file's size to the total
func (p *progress) fileCompleted(size int64) {
	p.totalBytes += size
}

// clear clears the current line
func (p *progress) clear() {
	if p.w == nil {
		return
	}
	fmt.Fprintf(p.w, "\r%s\r", strings.Repeat(" ", p.termWidth))
}

// done prints a final newline
func (p *progress) done() {
	if p.w == nil {
		return
	}
	p.clear()
	fmt.Fprintf(p.w, "Indexed %d files (%dM)\n", p.fileCount, p.totalBytes/(1024*1024))
}

// CreateFidx creates a fidx or mfidx file for the given path.
// If path is a directory, creates an mfidx for all files in it.
// If path is a file, creates a single-file fidx.
// Output is written to outPath.
func CreateFidx(path string, outPath string, opts IndexerOptions) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	if info.IsDir() {
		return CreateMFIDX([]string{path}, outPath, opts)
	}
	return CreateSingleFidx(path, outPath, opts)
}

// CreateMFIDX creates a multi-file fidx from multiple paths.
// Paths can be files or directories; directories are recursively expanded.
func CreateMFIDX(paths []string, outPath string, opts IndexerOptions) error {
	return createMFIDX(paths, outPath, opts)
}

// CreateSingleFidx creates a single-file fidx
func CreateSingleFidx(filename string, outPath string, opts IndexerOptions) error {
	tmppath := outPath + ".tmp"
	outf, err := os.Create(tmppath)
	if err != nil {
		return err
	}
	defer func() {
		outf.Close()
		os.Remove(tmppath) // Clean up on error
	}()

	// Use buffered writer for performance
	bufw := bufio.NewWriter(outf)

	// Hash accumulator for the entire fidx file
	fidxHash := sha1.New()

	// Write header
	header := make([]byte, 8)
	copy(header[0:4], "FIDX")
	binary.BigEndian.PutUint32(header[4:8], FIDX_VERSION)
	if _, err := bufw.Write(header); err != nil {
		return err
	}
	fidxHash.Write(header)

	// Write entries
	if err := ChunkFile(filename, func(entry FidxEntry) error {
		entryData := make([]byte, 24)
		copy(entryData[0:20], entry.SHA[:])
		binary.BigEndian.PutUint16(entryData[20:22], entry.Size)
		binary.BigEndian.PutUint16(entryData[22:24], entry.Level)
		if _, err := bufw.Write(entryData); err != nil {
			return err
		}
		fidxHash.Write(entryData)
		return nil
	}, func(totalBytes, fSize int64) {}); err != nil {
		return err
	}

	// Write fidx file checksum
	checksum := fidxHash.Sum(nil)
	if _, err := bufw.Write(checksum); err != nil {
		return err
	}

	if err := bufw.Flush(); err != nil {
		return err
	}
	if err := outf.Close(); err != nil {
		return err
	}

	// Atomically rename to final name
	if err := os.Rename(tmppath, outPath); err != nil {
		return err
	}

	return nil
}

// createMFIDX creates a multi-file fidx
func createMFIDX(paths []string, outPath string, opts IndexerOptions) error {
	// Expand directories into file lists
	allFiles, err := collectFiles(paths)
	if err != nil {
		return err
	}

	// Load reference mfidx if specified
	var refMap map[string]*RefFileInfo
	if opts.RefPath != "" {
		var err error
		refMap, _, err = LoadRefMFIDX(opts.RefPath)
		if err != nil {
			return fmt.Errorf("loading reference mfidx: %w", err)
		}
	}

	tmppath := outPath + ".tmp"
	outf, err := os.Create(tmppath)
	if err != nil {
		return err
	}
	defer func() {
		outf.Close()
		os.Remove(tmppath) // Clean up on error
	}()

	// Use buffered writer for performance
	bufw := bufio.NewWriter(outf)

	// Hash accumulator for the entire mfidx file
	fidxHash := sha1.New()

	// Write header
	header := make([]byte, 8)
	copy(header[0:4], "FIDX")
	binary.BigEndian.PutUint32(header[4:8], FIDX_VERSION)
	if _, err := bufw.Write(header); err != nil {
		return err
	}
	fidxHash.Write(header)

	// Progress tracker
	var prog *progress
	if opts.Progress && term.IsTerminal(int(os.Stderr.Fd())) {
		prog = newProgress(os.Stderr, refMap != nil)
	}

	// Process each file
	for _, filename := range allFiles {
		// Compute the stripped filename for storage in the mfidx
		storedName := stripPathPrefix(outPath, filename)

		// Get file info (use Lstat to not follow symlinks)
		stat, err := os.Lstat(filename)
		if err != nil {
			return fmt.Errorf("lstat %s: %w", filename, err)
		}

		fileSize := stat.Size()

		// Check if we can reuse entries from reference mfidx
		if refMap != nil {
			if ref, ok := refMap[storedName]; ok {
				if fileMatches(filename, ref) {
					// File is unchanged - copy entries from reference
					if prog != nil {
						prog.fileStarted()
						prog.status("unchanged", storedName, 0)
					}
					if err := writeRefEntries(bufw, fidxHash, storedName, stat, ref); err != nil {
						return err
					}
					if prog != nil {
						prog.fileCompleted(fileSize)
					}
					continue
				}
			}
		}

		if prog != nil {
			prog.fileStarted()
			var mode string
			if refMap != nil {
				mode = "new/changed"
			}
			prog.status(mode, storedName, 0)
		}

		// Handle symlinks specially - index the link target
		if stat.Mode()&os.ModeSymlink != 0 {
			if err := processSymlink(filename, storedName, bufw, fidxHash); err != nil {
				return err
			}
			if prog != nil {
				prog.fileCompleted(fileSize)
			}
			continue
		}

		// Skip non-regular files (devices, sockets, pipes, etc.)
		if !stat.Mode().IsRegular() {
			continue
		}

		// Write file separator entry
		sep := FileSeparator{
			Filename: storedName,
			FileSize: uint64(stat.Size()),
			Mtime:    uint64(stat.ModTime().Unix()),
		}

		// Create a buffer for separator data to add to hash
		var sepBuf []byte
		sepBuf = make([]byte, 0, 1024)
		sepWriter := &bytesWriter{buf: &sepBuf}

		if _, err := WriteFileSeparator(sepWriter, sep); err != nil {
			return fmt.Errorf("write separator for %s: %w", filename, err)
		}

		// Write to file and hash
		if _, err := bufw.Write(sepBuf); err != nil {
			return err
		}
		fidxHash.Write(sepBuf)

		// Write chunk entries for this file
		var mode string
		if refMap != nil {
			mode = "new/changed"
		}
		if err := chunkFileWithProgress(filename, storedName, mode, prog, func(entry FidxEntry) error {
			entryData := make([]byte, 24)
			copy(entryData[0:20], entry.SHA[:])
			binary.BigEndian.PutUint16(entryData[20:22], entry.Size)
			binary.BigEndian.PutUint16(entryData[22:24], entry.Level)
			if _, err := bufw.Write(entryData); err != nil {
				return err
			}
			fidxHash.Write(entryData)
			return nil
		}); err != nil {
			return fmt.Errorf("chunk %s: %w", filename, err)
		}
		if prog != nil {
			prog.fileCompleted(fileSize)
		}
	}

	if prog != nil {
		prog.done()
	}

	// Write mfidx file checksum
	checksum := fidxHash.Sum(nil)
	if _, err := bufw.Write(checksum); err != nil {
		return err
	}

	if err := bufw.Flush(); err != nil {
		return err
	}
	if err := outf.Close(); err != nil {
		return err
	}

	// Atomically rename to final name
	if err := os.Rename(tmppath, outPath); err != nil {
		return err
	}

	return nil
}

// chunkFileWithProgress wraps ChunkFile with progress reporting
func chunkFileWithProgress(filename, storedName, mode string, prog *progress, writeEntry func(FidxEntry) error) error {
	return ChunkFile(filename, writeEntry, func(totalBytes, fSize int64) {
		if prog != nil {
			prog.status(mode, storedName, totalBytes)
		}
	})
}

// processSymlink handles a symlink by indexing its target path as content
func processSymlink(filename, storedName string, outf io.Writer, fidxHash io.Writer) error {
	// Read the symlink target
	target, err := os.Readlink(filename)
	if err != nil {
		return fmt.Errorf("readlink %s: %w", filename, err)
	}

	// Get symlink metadata
	stat, err := os.Lstat(filename)
	if err != nil {
		return fmt.Errorf("lstat %s: %w", filename, err)
	}

	// Write file separator entry
	// For symlinks, size is the length of the target string
	sep := FileSeparator{
		Filename: storedName,
		FileSize: uint64(len(target)),
		Mtime:    uint64(stat.ModTime().Unix()),
	}

	var sepBuf []byte
	sepBuf = make([]byte, 0, 1024)
	sepWriter := &bytesWriter{buf: &sepBuf}

	if _, err := WriteFileSeparator(sepWriter, sep); err != nil {
		return fmt.Errorf("write separator for %s: %w", filename, err)
	}

	if _, err := outf.Write(sepBuf); err != nil {
		return err
	}
	fidxHash.Write(sepBuf)

	// Write the link target as a single chunk
	targetBytes := []byte(target)
	sha := BlobSHA(targetBytes)

	entry := FidxEntry{
		SHA:   sha,
		Size:  uint16(len(target)),
		Level: 0,
	}

	entryData := make([]byte, 24)
	copy(entryData[0:20], entry.SHA[:])
	binary.BigEndian.PutUint16(entryData[20:22], entry.Size)
	binary.BigEndian.PutUint16(entryData[22:24], entry.Level)

	if _, err := outf.Write(entryData); err != nil {
		return err
	}
	fidxHash.Write(entryData)

	return nil
}

// collectFiles expands directories into file lists
func collectFiles(paths []string) ([]string, error) {
	var result []string
	seen := make(map[string]bool) // Avoid duplicates

	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("lstat %s: %w", path, err)
		}

		if !info.IsDir() {
			// Regular file or symlink - add it
			if !seen[path] {
				result = append(result, path)
				seen[path] = true
			}
			continue
		}

		// It's a directory - get its device ID to detect filesystem boundaries
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil, fmt.Errorf("cannot get device ID for %s", path)
		}
		rootDev := stat.Dev

		// Walk the directory tree
		err = filepath.Walk(path, func(walkPath string, walkInfo os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}

			// Check if we've crossed a filesystem boundary
			walkStat, ok := walkInfo.Sys().(*syscall.Stat_t)
			if !ok {
				return fmt.Errorf("cannot get device ID for %s", walkPath)
			}

			if walkStat.Dev != rootDev {
				// Different filesystem - skip this subtree
				if walkInfo.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}

			// Skip directories themselves (we only want files and symlinks)
			if walkInfo.IsDir() {
				return nil
			}

			// Add file or symlink
			if !seen[walkPath] {
				result = append(result, walkPath)
				seen[walkPath] = true
			}

			return nil
		})

		if err != nil {
			return nil, fmt.Errorf("walking %s: %w", path, err)
		}
	}

	return result, nil
}

// stripPathPrefix returns filename with the mfidx-relative prefix stripped.
// If outpath is "a/b/c/xyz.fidx" and filename is "a/b/c/xyz/foo/bar.txt",
// returns "foo/bar.txt". If filename doesn't start with the prefix, returns it unchanged.
func stripPathPrefix(outpath, filename string) string {
	// Get directory containing the fidx
	outDir := filepath.Dir(outpath)
	// Get base name without .fidx extension
	base := filepath.Base(outpath)
	base = strings.TrimSuffix(base, ".mfidx")
	base = strings.TrimSuffix(base, ".fidx")

	// Build the prefix to strip: dir/base/
	var prefix string
	if outDir == "." {
		prefix = base + "/"
	} else {
		prefix = filepath.Join(outDir, base) + "/"
	}

	// Strip prefix if present
	if strings.HasPrefix(filename, prefix) {
		return filename[len(prefix):]
	}
	return filename
}

// LoadRefMFIDX loads a reference mfidx and builds a map of filename -> RefFileInfo
func LoadRefMFIDX(mfidxPath string) (map[string]*RefFileInfo, string, error) {
	fidx, err := LoadFidx(mfidxPath)
	if err != nil {
		return nil, "", fmt.Errorf("loading reference mfidx: %w", err)
	}
	if !fidx.IsMFIDX {
		return nil, "", fmt.Errorf("reference file is not an mfidx")
	}

	// The mfidx contains paths relative to some base directory
	// The base directory is the mfidx path with .fidx/.mfidx extension removed
	// e.g., snaps/1.fidx -> snaps/1/
	refBase := strings.TrimSuffix(mfidxPath, ".mfidx")
	refBase = strings.TrimSuffix(refBase, ".fidx")

	// Verify the reference base directory exists
	if info, err := os.Stat(refBase); err != nil {
		return nil, "", fmt.Errorf("reference base directory %q does not exist (expected directory matching %s)", refBase, mfidxPath)
	} else if !info.IsDir() {
		return nil, "", fmt.Errorf("reference base %q is not a directory", refBase)
	}

	result := make(map[string]*RefFileInfo)
	for _, fe := range fidx.Files {
		// Store by the filename as recorded in the mfidx
		result[fe.Filename] = &RefFileInfo{
			Entry:   fe,
			RefPath: filepath.Join(refBase, fe.Filename),
		}
	}

	return result, refBase, nil
}

// fileMatches checks if the current file matches the reference file metadata
// by comparing mtime, ctime, size, and inode number
func fileMatches(currentPath string, ref *RefFileInfo) bool {
	currentStat, err := os.Lstat(currentPath)
	if err != nil {
		return false
	}

	// Check size
	if uint64(currentStat.Size()) != ref.Entry.FileSize {
		return false
	}

	// Check mtime
	if uint64(currentStat.ModTime().Unix()) != ref.Entry.Mtime {
		return false
	}

	// Get the reference file stat
	refStat, err := os.Lstat(ref.RefPath)
	if err != nil {
		return false
	}

	// Get syscall.Stat_t for both files to compare ctime and inode
	currentSys, ok := currentStat.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	refSys, ok := refStat.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}

	// Compare inode number
	if currentSys.Ino != refSys.Ino {
		return false
	}

	// Compare ctime
	if currentSys.Ctim != refSys.Ctim {
		return false
	}

	return true
}

// writeRefEntries writes file separator and chunk entries from a reference mfidx
func writeRefEntries(outf io.Writer, fidxHash io.Writer, filename string, stat os.FileInfo, ref *RefFileInfo) error {
	// Write file separator entry (using current file's metadata)
	sep := FileSeparator{
		Filename: filename,
		FileSize: ref.Entry.FileSize,
		Mtime:    ref.Entry.Mtime,
	}

	var sepBuf []byte
	sepBuf = make([]byte, 0, 1024)
	sepWriter := &bytesWriter{buf: &sepBuf}

	if _, err := WriteFileSeparator(sepWriter, sep); err != nil {
		return fmt.Errorf("write separator for %s: %w", filename, err)
	}

	if _, err := outf.Write(sepBuf); err != nil {
		return err
	}
	fidxHash.Write(sepBuf)

	// Write chunk entries copied from reference (batched for performance)
	entryData := make([]byte, 24*len(ref.Entry.Entries))
	for i, entry := range ref.Entry.Entries {
		off := i * 24
		copy(entryData[off:off+20], entry.SHA[:])
		binary.BigEndian.PutUint16(entryData[off+20:off+22], entry.Size)
		binary.BigEndian.PutUint16(entryData[off+22:off+24], entry.Level)
	}
	if _, err := outf.Write(entryData); err != nil {
		return err
	}
	fidxHash.Write(entryData)

	return nil
}

// bytesWriter wraps a byte slice for io.Writer compatibility
type bytesWriter struct {
	buf *[]byte
}

func (bw *bytesWriter) Write(p []byte) (n int, err error) {
	*bw.buf = append(*bw.buf, p...)
	return len(p), nil
}
