// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package tsm

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// IndexerOptions configures the TSM indexer
type IndexerOptions struct {
	// ProgressCallback, if non-nil, is called periodically with current stats.
	// The caller can use this to render progress information.
	ProgressCallback func(stats IndexerStats)

	// CrossDevice allows indexing across filesystem boundaries
	CrossDevice bool

	// ParentTSM and ParentTSC, if both non-nil, enable incremental indexing:
	// a regular file whose path, size and ctime match the parent snapshot is
	// assumed unchanged, so its chunk hashes are reused from the parent
	// instead of re-reading and re-hashing the file content. This makes a
	// second consecutive snap of an unchanged tree avoid all file I/O.
	//
	// ctime (not mtime) is used as the change-detection signal because
	// mtime is freely settable by any unprivileged process (utimensat/
	// os.Chtimes), so a file's content could be changed and its mtime reset
	// to an old value, defeating mtime-based detection. ctime is
	// kernel-controlled and bumped on any metadata change, including a
	// write, so it cannot be forged the same way.
	ParentTSM *TSMReader
	ParentTSC *TSCReader
}

// racyCtimeWindow is the window (the "racy git" technique) within which an
// observed ctime is too close to the start of this indexing run to be
// trusted as final. Some filesystems/clocks (e.g. Linux's jiffy-granularity
// coarse inode timestamps) can produce identical timestamps for two writes
// that are microseconds apart in wall-clock time, so a ctime observed within
// this window of "now" cannot yet be trusted to be stable: a subsequent
// write landing in the same coarse clock tick could produce an identical
// ctime and go undetected.
const racyCtimeWindow = 1 * time.Second

// racyAdjustedCtime returns the ctime value to *store* for change-detection
// purposes. If ctime is within racyCtimeWindow of referenceTime (the start
// of the indexing/extraction run that observed it), it is not yet safe to
// trust, so a deliberately-wrong value (ctime minus one nanosecond) is
// recorded instead. This guarantees a future comparison against this file's
// real ctime will always mismatch - forcing a re-hash rather than a
// possibly-incorrect reuse - until enough wall-clock time has passed that
// the ctime is no longer racy, at which point it is recorded as-is and safe
// reuse becomes possible again.
func racyAdjustedCtime(ctime int64, referenceTime time.Time) int64 {
	if referenceTime.Sub(time.Unix(0, ctime)) < racyCtimeWindow {
		return ctime - 1
	}
	return ctime
}

// Indexer creates TSM and TSC files from a filesystem
type Indexer struct {
	opts       IndexerOptions
	tsc        *TSCWriter
	tsm        *TSMWriter
	rootPath   string
	rootDev    uint64
	totalBytes int64
	lastUpdate time.Time

	// indexStart is when this indexing run began. It is the reference point
	// for the racy-ctime check (see racyAdjustedCtime): using the start of
	// the run, rather than the time each individual file happens to be
	// visited, is the conservative choice recommended by the "racy git"
	// technique this is modeled on.
	indexStart time.Time

	// unmodifiedEntries counts entries (of any type) that match the parent
	// snapshot's path and ctime, meaning they haven't changed.
	unmodifiedEntries int

	// modifiedEntries counts entries that are new or have changed since the
	// parent snapshot.
	modifiedEntries int

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
	idx.indexStart = time.Now()
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
			// Skip permission errors silently
			if os.IsPermission(walkErr) {
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

	// Emit final progress with the actual stats (not rate-limited). The
	// caller (createSnapshotSubdir) coordinates the three-snap progress line
	// and calls progress.Final() after all snaps complete, but each individual
	// snap needs to report its final stats so the combined line is accurate.
	if idx.opts.ProgressCallback != nil {
		idx.opts.ProgressCallback(idx.Stats())
	}

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
				// Hardlinks are counted based on whether they match parent
				if idx.isUnmodified(entry) {
					idx.unmodifiedEntries++
				} else {
					idx.modifiedEntries++
				}
				idx.updateProgress(relPath)
				return entry, nil
			}
			// First time seeing this inode, record it
			idx.hardlinks[key] = entryIndex
		}

		// Incremental fast path: if the parent snapshot has an identical
		// (path, size, ctime) regular file, reuse its chunks instead of
		// re-reading and re-hashing the content.
		if chunkRefs, ok := idx.reuseParentChunks(entry); ok {
			entry.ChunkRefs = chunkRefs
			entry.ChunkCount = uint32(len(chunkRefs))
			idx.unmodifiedEntries++
		} else {
			// Process file chunks
			chunkRefs, err := idx.processFileChunks(path, info.Size())
			if err != nil {
				return nil, err
			}
			entry.ChunkRefs = chunkRefs
			entry.ChunkCount = uint32(len(chunkRefs))
			idx.modifiedEntries++
		}

		// Apply the racy-ctime adjustment to the value that gets *stored*
		// for future change detection (see racyAdjustedCtime). This must
		// happen after reuseParentChunks, which needs the real, current
		// ctime to compare against the parent.
		entry.Ctime = racyAdjustedCtime(entry.Ctime, idx.indexStart)

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
		// Unix sockets are ephemeral and cannot be meaningfully restored.
		// Exclude them from snapshots to ensure idempotence (a recreated
		// socket with a new inode/ctime shouldn't change the snapshot hash).
		return nil, nil

	default:
		// Unknown type, skip
		return nil, nil
	}

	// Track modified/unmodified for non-file entries (files are tracked above)
	if entry.Type != EntryTypeFile {
		if idx.isUnmodified(entry) {
			idx.unmodifiedEntries++
		} else {
			idx.modifiedEntries++
		}
	}

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

// isUnmodified checks whether the given entry matches the parent snapshot's
// entry at the same path (same type and ctime). This is used for all entry
// types to determine if they should be counted as "unmodified" vs "modified"
// in progress reporting.
func (idx *Indexer) isUnmodified(entry *TSMEntry) bool {
	if idx.opts.ParentTSM == nil {
		return false
	}
	parent, ok := idx.opts.ParentTSM.LookupPath(entry.Path)
	if !ok {
		return false
	}
	return parent.Type == entry.Type && parent.Ctime == entry.Ctime
}

// reuseParentChunks checks whether the given regular-file entry is unchanged
// since the parent snapshot (same path, size and ctime). If so, it copies the
// parent's chunks into this indexer's TSC and returns the new chunk refs,
// avoiding any read/hash of the file content. The second return value reports
// whether the fast path was taken.
//
// ctime, rather than mtime, is the comparison signal: mtime can be forged by
// any unprivileged process (e.g. via os.Chtimes) to make changed content look
// unchanged, whereas ctime is kernel-controlled and always bumped when a
// file's content or metadata changes. To guard against ctime's coarse
// resolution on some filesystems/clocks, entries have already had
// racyAdjustedCtime applied when they were stored as a parent, so a ctime
// that was too close to "now" at that time will safely fail to match here
// and force a re-hash.
//
// Reusing chunks is safe for snapshot-ID reproducibility: the reused chunks
// have identical SHA/size/level, and the entry's mtime is taken from the
// filesystem, so the resulting TSM hash (which is computed over mtime, not
// ctime - see encodeEntryForHash) is identical to a full re-index whenever
// the file is genuinely unchanged.
func (idx *Indexer) reuseParentChunks(entry *TSMEntry) ([]uint32, bool) {
	// TSM_DEBUG_REUSE=1 enables verbose logging of the reuse decision, for
	// debugging TestSnapshotHomeWorkIncrementalPreserveParent flakiness.
	debug := os.Getenv("TSM_DEBUG_REUSE") != ""

	if idx.opts.ParentTSM == nil || idx.opts.ParentTSC == nil {
		return nil, false
	}
	parent, ok := idx.opts.ParentTSM.LookupPath(entry.Path)
	if !ok {
		if debug {
			fmt.Fprintf(os.Stderr, "[tsm debug] reuseParentChunks: path=%q no parent entry (LookupPath miss)\n", entry.Path)
		}
		return nil, false
	}
	// Only reuse when nothing observable about the file content has changed.
	if parent.Type != EntryTypeFile ||
		parent.Size != entry.Size ||
		parent.Ctime != entry.Ctime {
		if debug {
			fmt.Fprintf(os.Stderr, "[tsm debug] reuseParentChunks: path=%q MISMATCH typeOK=%v sizeMatch=%v (parent=%d entry=%d) ctimeMatch=%v (parent=%d entry=%d delta=%dns)\n",
				entry.Path, parent.Type == EntryTypeFile,
				parent.Size == entry.Size, parent.Size, entry.Size,
				parent.Ctime == entry.Ctime, parent.Ctime, entry.Ctime, entry.Ctime-parent.Ctime)
		}
		return nil, false
	}
	if debug {
		fmt.Fprintf(os.Stderr, "[tsm debug] reuseParentChunks: path=%q MATCH size=%d ctime=%d -> reusing %d chunk(s)\n",
			entry.Path, entry.Size, entry.Ctime, len(parent.ChunkRefs))
	}

	chunkRefs := make([]uint32, 0, len(parent.ChunkRefs))
	for _, tscIdx := range parent.ChunkRefs {
		if int(tscIdx) >= len(idx.opts.ParentTSC.Entries) {
			// Parent index out of range: bail out and re-hash to be safe.
			return nil, false
		}
		c := idx.opts.ParentTSC.Entries[tscIdx]
		newIdx := idx.tsc.AddChunk(c.SHA256, c.Size, c.Level, c.Flags)
		chunkRefs = append(chunkRefs, newIdx)
		idx.totalBytes += int64(c.Size)
	}
	return chunkRefs, true
}

// updateProgress calls the progress callback with current stats, rate-limited
// to about 4 updates per second (~250ms).
func (idx *Indexer) updateProgress(path string) {
	if idx.opts.ProgressCallback == nil {
		return
	}

	// Rate limit to about 4 updates per second (~250ms).
	now := time.Now()
	if now.Sub(idx.lastUpdate) < 250*time.Millisecond {
		return
	}
	idx.lastUpdate = now

	idx.opts.ProgressCallback(idx.Stats())
}

// Create is a convenience function to create TSM/TSC files from a path
func Create(rootPath, outBase string, opts IndexerOptions) error {
	indexer := NewIndexer(opts)
	return indexer.Index(rootPath, outBase)
}

// IndexerStats returns statistics about the indexing
type IndexerStats struct {
	UnmodifiedEntries int
	ModifiedEntries   int
	ChunkCount        uint64
	TotalBytes        int64
}

// Stats returns current indexing statistics
func (idx *Indexer) Stats() IndexerStats {
	return IndexerStats{
		UnmodifiedEntries: idx.unmodifiedEntries,
		ModifiedEntries:   idx.modifiedEntries,
		ChunkCount:        idx.tsc.ChunkCount(),
		TotalBytes:        idx.totalBytes,
	}
}
