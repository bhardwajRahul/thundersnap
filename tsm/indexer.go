package tsm

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// IndexerOptions configures the TSM indexer
type IndexerOptions struct {
	// Progress enables progress reporting
	Progress bool

	// ProgressWriter receives progress updates
	ProgressWriter io.Writer

	// IsTTY indicates whether progress writer is a terminal
	IsTTY bool

	// CrossDevice allows indexing across filesystem boundaries
	CrossDevice bool
}

// Indexer creates TSM and TSC files from a filesystem
type Indexer struct {
	opts       IndexerOptions
	tsc        *TSCWriter
	tsm        *TSMWriter
	rootPath   string
	rootDev    uint64
	fileCount  int
	totalBytes int64
	lastUpdate time.Time

	// Track hardlinks: device+inode -> entry index
	hardlinks map[uint64]uint32
}

// NewIndexer creates a new filesystem indexer
func NewIndexer(opts IndexerOptions) *Indexer {
	tsm := NewTSMWriter()
	// Zero the creation time for reproducible output. The TSM SHA should
	// depend only on content, not when the indexing happened.
	tsm.SetCreationTime(time.Time{})
	return &Indexer{
		opts:      opts,
		tsc:       NewTSCWriter(),
		tsm:       tsm,
		hardlinks: make(map[uint64]uint32),
	}
}

// Index indexes a filesystem path and writes TSM/TSC files
func (idx *Indexer) Index(rootPath, outBase string) error {
	idx.rootPath = filepath.Clean(rootPath)

	// Get root device ID for filesystem boundary detection
	rootInfo, err := os.Lstat(idx.rootPath)
	if err != nil {
		return fmt.Errorf("stat root: %w", err)
	}

	if stat, ok := rootInfo.Sys().(*syscall.Stat_t); ok {
		idx.rootDev = stat.Dev
	}

	// Collect all paths first (for sorting)
	var allPaths []string
	err = filepath.Walk(idx.rootPath, func(walkPath string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			// Log permission errors but continue
			if os.IsPermission(walkErr) {
				idx.logProgress("permission denied: %s\n", walkPath)
				return nil
			}
			return walkErr
		}

		// Check filesystem boundary
		if !idx.opts.CrossDevice {
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				if stat.Dev != idx.rootDev {
					if info.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
			}
		}

		allPaths = append(allPaths, walkPath)
		return nil
	})
	if err != nil {
		return fmt.Errorf("walking filesystem: %w", err)
	}

	// Sort paths for deterministic output
	sort.Strings(allPaths)

	// First pass: collect all entries and chunks
	// We need to process files to know their chunk indices
	entryIndex := uint32(0)
	for _, path := range allPaths {
		relPath, err := filepath.Rel(idx.rootPath, path)
		if err != nil {
			relPath = path
		}
		if relPath == "." {
			relPath = ""
		}

		info, err := os.Lstat(path)
		if err != nil {
			if os.IsPermission(err) {
				continue
			}
			return fmt.Errorf("stat %s: %w", path, err)
		}

		entry, err := idx.processEntry(path, relPath, info, entryIndex)
		if err != nil {
			if os.IsPermission(err) {
				continue
			}
			return fmt.Errorf("processing %s: %w", path, err)
		}

		if entry != nil {
			idx.tsm.AddEntry(*entry)
			entryIndex++
		}
	}

	// Write TSC file first (we need its hash for TSM)
	tscPath := outBase + ".tsc"
	tscSHA, indexMap, err := idx.tsc.Write(tscPath)
	if err != nil {
		return fmt.Errorf("writing tsc: %w", err)
	}

	// Write TSM file (the writer will remap chunk refs using indexMap)
	tsmPath := outBase + ".tsm"
	_, err = idx.tsm.Write(tsmPath, tscSHA, indexMap)
	if err != nil {
		return fmt.Errorf("writing tsm: %w", err)
	}

	idx.logProgress("Indexed %d files, %d unique chunks, %d MB\n",
		idx.tsm.EntryCount(), idx.tsc.ChunkCount(), idx.totalBytes/(1024*1024))

	return nil
}

// processEntry processes a single filesystem entry
func (idx *Indexer) processEntry(path, relPath string, info os.FileInfo, entryIndex uint32) (*TSMEntry, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("cannot get syscall.Stat_t for %s", path)
	}

	entry := &TSMEntry{
		Path:  relPath,
		Mode:  uint32(info.Mode()),
		UID:   stat.Uid,
		GID:   stat.Gid,
		Size:  uint64(info.Size()),
		Mtime: stat.Mtim.Nano(),
		Ctime: stat.Ctim.Nano(),
		Atime: stat.Atim.Nano(),
	}

	// Zero out timestamps on the root entry so that two trees with identical
	// contents produce identical hashes regardless of when they were created.
	if relPath == "" {
		entry.Mtime = 0
		entry.Ctime = 0
		entry.Atime = 0
	}

	mode := info.Mode()

	switch {
	case mode.IsDir():
		entry.Type = EntryTypeDir

	case mode.IsRegular():
		entry.Type = EntryTypeFile

		// Check for hardlinks
		if stat.Nlink > 1 {
			key := uint64(stat.Dev)<<32 | uint64(stat.Ino)
			if firstIdx, ok := idx.hardlinks[key]; ok {
				// This is a hardlink to an already-seen file
				entry.Type = EntryTypeHardlink
				entry.LinkIndex = firstIdx
				return entry, nil
			}
			// First time seeing this inode, record it
			idx.hardlinks[key] = entryIndex
		}

		// Process file chunks
		chunkRefs, err := idx.processFileChunks(path, info.Size())
		if err != nil {
			return nil, err
		}
		entry.ChunkRefs = chunkRefs
		entry.ChunkCount = uint32(len(chunkRefs))

	case mode&os.ModeSymlink != 0:
		entry.Type = EntryTypeSymlink
		target, err := os.Readlink(path)
		if err != nil {
			return nil, fmt.Errorf("readlink: %w", err)
		}
		entry.LinkTarget = target
		entry.Size = uint64(len(target))

	case mode&os.ModeDevice != 0:
		if mode&os.ModeCharDevice != 0 {
			entry.Type = EntryTypeCharDev
		} else {
			entry.Type = EntryTypeBlockDev
		}
		entry.DevMajor = unix.Major(stat.Rdev)
		entry.DevMinor = unix.Minor(stat.Rdev)

	case mode&os.ModeNamedPipe != 0:
		entry.Type = EntryTypeFifo

	case mode&os.ModeSocket != 0:
		entry.Type = EntryTypeSocket

	default:
		// Unknown type, skip
		return nil, nil
	}

	idx.fileCount++
	idx.updateProgress(relPath)

	return entry, nil
}

// processFileChunks chunks a regular file and adds chunks to TSC.
// Returns a slice of TSC indices (original/unsorted) for the file's chunks in order.
func (idx *Indexer) processFileChunks(path string, size int64) ([]uint32, error) {
	if size == 0 {
		return nil, nil
	}

	var chunkRefs []uint32

	err := ChunkFile(path, func(sha [32]byte, chunkSize uint32, level uint16) error {
		flags := uint16(0)

		// Check for zero block
		if sha == ZeroBlockSHA {
			flags |= TSCEntryFlagZeroBlock
		}

		chunkIdx := idx.tsc.AddChunk(sha, chunkSize, level, flags)
		chunkRefs = append(chunkRefs, chunkIdx)
		idx.totalBytes += int64(chunkSize)

		return nil
	}, nil)

	if err != nil {
		return nil, err
	}

	return chunkRefs, nil
}

// logProgress writes a progress message if progress is enabled
func (idx *Indexer) logProgress(format string, args ...interface{}) {
	if idx.opts.Progress && idx.opts.ProgressWriter != nil {
		fmt.Fprintf(idx.opts.ProgressWriter, format, args...)
	}
}

// updateProgress updates the progress display
func (idx *Indexer) updateProgress(path string) {
	if !idx.opts.Progress || idx.opts.ProgressWriter == nil {
		return
	}

	// Rate limit to 10 updates per second
	now := time.Now()
	if now.Sub(idx.lastUpdate) < 100*time.Millisecond {
		return
	}
	idx.lastUpdate = now

	if idx.opts.IsTTY {
		// Truncate path for display
		displayPath := path
		if len(displayPath) > 60 {
			displayPath = "..." + displayPath[len(displayPath)-57:]
		}
		fmt.Fprintf(idx.opts.ProgressWriter, "\r[%d files] [%dM] %s",
			idx.fileCount, idx.totalBytes/(1024*1024), displayPath)
	}
}

// Create is a convenience function to create TSM/TSC files from a path
func Create(rootPath, outBase string, opts IndexerOptions) error {
	indexer := NewIndexer(opts)
	return indexer.Index(rootPath, outBase)
}

// IndexerStats returns statistics about the indexing
type IndexerStats struct {
	FileCount  int
	ChunkCount uint64
	TotalBytes int64
}

// Stats returns current indexing statistics
func (idx *Indexer) Stats() IndexerStats {
	return IndexerStats{
		FileCount:  idx.fileCount,
		ChunkCount: idx.tsc.ChunkCount(),
		TotalBytes: idx.totalBytes,
	}
}

// stripPathPrefix returns path with the prefix stripped
func stripPathPrefix(basePath, fullPath string) string {
	rel, err := filepath.Rel(basePath, fullPath)
	if err != nil {
		return fullPath
	}
	// Ensure we don't return ".."
	if strings.HasPrefix(rel, "..") {
		return fullPath
	}
	return rel
}
