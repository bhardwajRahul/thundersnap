// fidx generates file index (.fidx) files using content-defined chunking.
// This is a Go port of bup's fidx-cmd.py
package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/pborman/getopt/v2"
)

const (
	// FIDX_VERSION is the fidx file format version
	FIDX_VERSION = 1

	// Content-defined chunking parameters (from bupsplit.h)
	BUP_BLOBBITS   = 13
	BUP_BLOBSIZE   = 1 << BUP_BLOBBITS // 8192
	BUP_WINDOWBITS = 7
	BUP_WINDOWSIZE = 1 << (BUP_WINDOWBITS - 1) // 64

	// BLOB_MAX is the maximum chunk size
	BLOB_MAX = 8192 * 4 // 32768 bytes

	// BLOB_READ_SIZE is the buffer size for reading
	BLOB_READ_SIZE = 1024 * 1024

	// ROLLSUM_CHAR_OFFSET is the character offset for rollsum
	ROLLSUM_CHAR_OFFSET = 31

	// FANOUT_BITS for hierarchical level calculation
	FANOUT_BITS = 4
)

var (
	outdir  = getopt.StringLong("outdir", 'd', "", "directory to write output (.fidx) files")
	outfile = getopt.StringLong("outfile", 'o', "", "filename to write fidx/mfidx data")
	mfidx   = getopt.BoolLong("mfidx", 'm', "multi-file fidx: write all files into a single .mfidx file (use with -o)")
	ascii   = getopt.BoolLong("ascii", 'A', "write the index in human-readable ascii format to stdout")
	print   = getopt.BoolLong("print", 'p', "print contents of existing fidx/mfidx file, record by record")
	help    = getopt.BoolLong("help", 'h', "show help")
)

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: fidx [options] <filenames...>\n")
	getopt.PrintUsage(os.Stderr)
	os.Exit(1)
}

// Rollsum implements the rolling checksum used for content-defined chunking
type Rollsum struct {
	s1     uint32
	s2     uint32
	window [BUP_WINDOWSIZE]byte
	wofs   int
}

func (r *Rollsum) init() {
	r.s1 = BUP_WINDOWSIZE * ROLLSUM_CHAR_OFFSET
	r.s2 = BUP_WINDOWSIZE * (BUP_WINDOWSIZE - 1) * ROLLSUM_CHAR_OFFSET
	r.wofs = 0
	for i := range r.window {
		r.window[i] = 0
	}
}

func (r *Rollsum) add(drop, add byte) {
	r.s1 += uint32(add) - uint32(drop)
	r.s2 += r.s1 - (BUP_WINDOWSIZE * (uint32(drop) + ROLLSUM_CHAR_OFFSET))
}

func (r *Rollsum) roll(ch byte) {
	r.add(r.window[r.wofs], ch)
	r.window[r.wofs] = ch
	r.wofs = (r.wofs + 1) % BUP_WINDOWSIZE
}

func (r *Rollsum) digest() uint32 {
	return (r.s1 << 16) | (r.s2 & 0xffff)
}

// findSplitPoint finds a content-defined split point in the buffer
// Returns (offset, bits) where offset is the split position (0 if no split found)
// and bits is the number of matching bits in the rollsum
func findSplitPoint(buf []byte) (int, int) {
	var r Rollsum
	r.init()

	for count := 0; count < len(buf); count++ {
		r.roll(buf[count])
		if (r.s2 & (BUP_BLOBSIZE - 1)) == ((^uint32(0)) & (BUP_BLOBSIZE - 1)) {
			// Found a split point
			rsum := r.digest()
			bits := BUP_BLOBBITS
			rsum >>= BUP_BLOBBITS
			for (rsum>>1)&1 != 0 {
				bits++
				rsum >>= 1
			}
			return count + 1, bits
		}
	}
	return 0, 0
}

// FidxEntry represents a single chunk entry
type FidxEntry struct {
	SHA   [20]byte
	Size  uint16
	Level uint16
}

// FileSeparator represents file metadata in MFIDX format
type FileSeparator struct {
	Filename string
	FileSize uint64
	Mtime    uint64
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

// writeFileSeparator writes a file separator entry followed by file metadata
// Returns the number of bytes written
func writeFileSeparator(w io.Writer, sep FileSeparator) (int, error) {
	// File separator entry: 24 bytes
	// - 20 bytes of zeros (special marker)
	// - 2 bytes reserved (0x0000)
	// - 2 bytes for metadata length following
	separatorEntry := make([]byte, 24)
	// All zeros for SHA (already zero-initialized)
	// Reserved field at offset 20 is already zero

	// Calculate metadata size
	metadataSize := len(sep.Filename) + 1 + 8 + 8 // null-terminated filename + uint64 size + uint64 mtime
	// Align to 8-byte boundary
	paddingSize := (8 - (metadataSize % 8)) % 8
	totalMetadataSize := metadataSize + paddingSize

	binary.BigEndian.PutUint16(separatorEntry[22:24], uint16(totalMetadataSize))

	if _, err := w.Write(separatorEntry); err != nil {
		return 24, err
	}

	// Write file metadata
	// 1. Null-terminated filename
	if _, err := w.Write([]byte(sep.Filename)); err != nil {
		return 24, err
	}
	if _, err := w.Write([]byte{0}); err != nil {
		return 24 + len(sep.Filename), err
	}

	// 2. File size (uint64, big-endian)
	sizeBuf := make([]byte, 8)
	binary.BigEndian.PutUint64(sizeBuf, sep.FileSize)
	if _, err := w.Write(sizeBuf); err != nil {
		return 24 + len(sep.Filename) + 1, err
	}

	// 3. Modification time (uint64, big-endian, Unix timestamp)
	mtimeBuf := make([]byte, 8)
	binary.BigEndian.PutUint64(mtimeBuf, sep.Mtime)
	if _, err := w.Write(mtimeBuf); err != nil {
		return 24 + len(sep.Filename) + 1 + 8, err
	}

	// 4. Padding to 8-byte boundary
	if paddingSize > 0 {
		padding := make([]byte, paddingSize)
		if _, err := w.Write(padding); err != nil {
			return 24 + len(sep.Filename) + 1 + 8 + 8, err
		}
	}

	return 24 + totalMetadataSize, nil
}

// chunkFile splits a file into content-defined chunks and returns the entries
func chunkFile(filename string, writeEntry func(FidxEntry) error) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	// Get file size for progress reporting
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	fileSize := stat.Size()

	buf := make([]byte, BLOB_READ_SIZE)
	var leftover []byte
	totalBytes := int64(0)
	lastProgress := int64(-1)

	for {
		// Prepend any leftover data from previous iteration
		if len(leftover) > 0 {
			copy(buf, leftover)
		}

		n, err := f.Read(buf[len(leftover):])
		if n > 0 {
			data := buf[:len(leftover)+n]

			// Find chunks in this buffer
			offset := 0
			for {
				remaining := len(data) - offset
				ofs, bits := findSplitPoint(data[offset:])

				var chunkSize int
				var level int

				if ofs > 0 {
					chunkSize = ofs
					if chunkSize > BLOB_MAX {
						chunkSize = BLOB_MAX
						level = 0
					} else {
						// Calculate hierarchical level
						level = (bits - BUP_BLOBBITS) / FANOUT_BITS
					}
				} else {
					// No split point found
					if err == io.EOF {
						// Last chunk - take everything remaining
						chunkSize = len(data) - offset
						level = 0
					} else if remaining >= BLOB_MAX {
						// Force a split at BLOB_MAX to avoid accumulating too much data
						chunkSize = BLOB_MAX
						level = 0
					} else {
						// Need more data, save for next iteration
						break
					}
				}

				if chunkSize > 0 {
					chunk := data[offset : offset+chunkSize]
					sha := blobSHA(chunk)

					entry := FidxEntry{
						SHA:   sha,
						Size:  uint16(chunkSize),
						Level: uint16(level),
					}

					if err := writeEntry(entry); err != nil {
						return err
					}

					totalBytes += int64(chunkSize)
					offset += chunkSize

					// Print progress every 10MB for large files
					if fileSize > 10*1024*1024 {
						progress := (totalBytes * 100) / fileSize
						if progress/10 != lastProgress/10 {
							fmt.Printf("    %d%% (%d MB / %d MB)\n", progress, totalBytes/(1024*1024), fileSize/(1024*1024))
							lastProgress = progress
						}
					}
				}

				if ofs == 0 {
					break
				}
			}

			// Save any unprocessed data for next iteration
			leftover = make([]byte, len(data)-offset)
			copy(leftover, data[offset:])
		}

		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}

	// Write any final leftover data as the last chunk
	if len(leftover) > 0 {
		sha := blobSHA(leftover)
		entry := FidxEntry{
			SHA:   sha,
			Size:  uint16(len(leftover)),
			Level: 0,
		}
		if err := writeEntry(entry); err != nil {
			return err
		}
	}

	return nil
}

// processSymlink handles a symlink by indexing its target path as content
func processSymlink(filename string, outf *os.File, fidxHash io.Writer) error {
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
		Filename: filename,
		FileSize: uint64(len(target)),
		Mtime:    uint64(stat.ModTime().Unix()),
	}

	var sepBuf []byte
	sepBuf = make([]byte, 0, 1024)
	sepWriter := &bytesWriter{buf: &sepBuf}

	if _, err := writeFileSeparator(sepWriter, sep); err != nil {
		return fmt.Errorf("write separator for %s: %w", filename, err)
	}

	if _, err := outf.Write(sepBuf); err != nil {
		return err
	}
	fidxHash.Write(sepBuf)

	// Write the link target as a single chunk
	targetBytes := []byte(target)
	sha := blobSHA(targetBytes)

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

// collectFiles expands directories into file lists, respecting filesystem boundaries
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

// processMFIDX creates a multi-file FIDX containing all input files
func processMFIDX(filenames []string, outpath string) error {
	// Expand directories into file lists
	allFiles, err := collectFiles(filenames)
	if err != nil {
		return err
	}

	fmt.Printf("Creating multi-file index: %s (%d files)\n", outpath, len(allFiles))

	tmppath := outpath + ".tmp"
	outf, err := os.Create(tmppath)
	if err != nil {
		return err
	}
	defer func() {
		outf.Close()
		os.Remove(tmppath) // Clean up on error
	}()

	// Hash accumulator for the entire mfidx file
	fidxHash := sha1.New()

	// Write header
	header := make([]byte, 8)
	copy(header[0:4], "FIDX")
	binary.BigEndian.PutUint32(header[4:8], FIDX_VERSION)
	if _, err := outf.Write(header); err != nil {
		return err
	}
	fidxHash.Write(header)

	// Process each file
	for _, filename := range allFiles {
		fmt.Printf("  %s\n", filename)

		// Get file info (use Lstat to not follow symlinks)
		stat, err := os.Lstat(filename)
		if err != nil {
			return fmt.Errorf("lstat %s: %w", filename, err)
		}

		// Handle symlinks specially - index the link target
		if stat.Mode()&os.ModeSymlink != 0 {
			if err := processSymlink(filename, outf, fidxHash); err != nil {
				return err
			}
			continue
		}

		// Skip non-regular files (devices, sockets, pipes, etc.)
		if !stat.Mode().IsRegular() {
			fmt.Printf("    skipping non-regular file\n")
			continue
		}

		// Write file separator entry
		sep := FileSeparator{
			Filename: filename,
			FileSize: uint64(stat.Size()),
			Mtime:    uint64(stat.ModTime().Unix()),
		}

		// Create a buffer for separator data to add to hash
		var sepBuf []byte
		sepBuf = make([]byte, 0, 1024)
		sepWriter := &bytesWriter{buf: &sepBuf}

		if _, err := writeFileSeparator(sepWriter, sep); err != nil {
			return fmt.Errorf("write separator for %s: %w", filename, err)
		}

		// Write to file and hash
		if _, err := outf.Write(sepBuf); err != nil {
			return err
		}
		fidxHash.Write(sepBuf)

		// Write chunk entries for this file
		if err := chunkFile(filename, func(entry FidxEntry) error {
			entryData := make([]byte, 24)
			copy(entryData[0:20], entry.SHA[:])
			binary.BigEndian.PutUint16(entryData[20:22], entry.Size)
			binary.BigEndian.PutUint16(entryData[22:24], entry.Level)
			if _, err := outf.Write(entryData); err != nil {
				return err
			}
			fidxHash.Write(entryData)
			return nil
		}); err != nil {
			return fmt.Errorf("chunk %s: %w", filename, err)
		}
	}

	// Write mfidx file checksum
	checksum := fidxHash.Sum(nil)
	if _, err := outf.Write(checksum); err != nil {
		return err
	}

	if err := outf.Close(); err != nil {
		return err
	}

	// Atomically rename to final name
	if err := os.Rename(tmppath, outpath); err != nil {
		return err
	}

	fmt.Printf("Wrote %s\n", outpath)
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

// printFidx reads and prints the contents of a fidx or mfidx file
func printFidx(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	if len(data) < 8+20 {
		return fmt.Errorf("file too short")
	}

	// Check header
	if string(data[0:4]) != "FIDX" {
		return fmt.Errorf("invalid FIDX magic")
	}

	version := binary.BigEndian.Uint32(data[4:8])
	if version != FIDX_VERSION {
		return fmt.Errorf("unsupported version: %d", version)
	}

	fmt.Printf("File: %s\n", path)
	fmt.Printf("FIDX version: %d\n", version)

	// Extract footer (last 20 bytes)
	footerSHA := data[len(data)-20:]
	data = data[:len(data)-20]

	// Verify checksum
	h := sha1.New()
	h.Write(data)
	computedSHA := h.Sum(nil)
	if !bytes.Equal(computedSHA, footerSHA) {
		fmt.Printf("WARNING: Checksum mismatch!\n")
		fmt.Printf("  Expected: %x\n", footerSHA)
		fmt.Printf("  Computed: %x\n", computedSHA)
	} else {
		fmt.Printf("Checksum: %x (valid)\n", computedSHA)
	}

	// Parse entries (skip 8-byte header)
	entryData := data[8:]

	// Detect if this is an mfidx
	isMFIDX := false
	if len(entryData) >= 24 {
		allZero := true
		for i := 0; i < 20; i++ {
			if entryData[i] != 0 {
				allZero = false
				break
			}
		}
		if allZero {
			isMFIDX = true
		}
	}

	if isMFIDX {
		fmt.Printf("Type: Multi-file index (.mfidx)\n\n")
		return printMFIDX(entryData)
	}

	fmt.Printf("Type: Single-file index (.fidx)\n")
	fmt.Printf("Total entries: %d\n\n", len(entryData)/24)

	// Print entries
	var totalSize uint64
	for i := 0; i*24 < len(entryData); i++ {
		offset := i * 24
		if offset+24 > len(entryData) {
			break
		}

		var sha [20]byte
		copy(sha[:], entryData[offset:offset+20])
		size := binary.BigEndian.Uint16(entryData[offset+20 : offset+22])
		level := binary.BigEndian.Uint16(entryData[offset+22 : offset+24])

		fmt.Printf("Entry %d:\n", i)
		fmt.Printf("  SHA:   %x\n", sha)
		fmt.Printf("  Size:  %d bytes\n", size)
		fmt.Printf("  Level: %d\n", level)
		totalSize += uint64(size)
	}

	fmt.Printf("\nTotal data size: %d bytes\n", totalSize)
	return nil
}

// printMFIDX prints the contents of a multi-file index
func printMFIDX(entryData []byte) error {
	offset := 0
	fileNum := 0

	for offset < len(entryData) {
		if offset+24 > len(entryData) {
			break
		}

		// Check for file separator
		isFileSeparator := true
		for i := 0; i < 20; i++ {
			if entryData[offset+i] != 0 {
				isFileSeparator = false
				break
			}
		}

		if !isFileSeparator {
			return fmt.Errorf("expected file separator at offset %d", offset)
		}

		// Read metadata length
		metadataLen := binary.BigEndian.Uint16(entryData[offset+22 : offset+24])
		offset += 24

		if offset+int(metadataLen) > len(entryData) {
			return fmt.Errorf("metadata extends beyond file")
		}

		// Parse metadata
		metadata := entryData[offset : offset+int(metadataLen)]

		// Read null-terminated filename
		filenameEnd := bytes.IndexByte(metadata, 0)
		if filenameEnd == -1 {
			return fmt.Errorf("filename not null-terminated")
		}
		filename := string(metadata[:filenameEnd])
		metadata = metadata[filenameEnd+1:]

		if len(metadata) < 16 {
			return fmt.Errorf("insufficient metadata")
		}

		fileSize := binary.BigEndian.Uint64(metadata[0:8])
		mtime := binary.BigEndian.Uint64(metadata[8:16])

		fmt.Printf("File %d: %s\n", fileNum, filename)
		fmt.Printf("  Size:  %d bytes\n", fileSize)
		fmt.Printf("  Mtime: %d (Unix timestamp)\n", mtime)

		offset += int(metadataLen)

		// Read chunk entries
		entryNum := 0
		var computedSize uint64

		for offset < len(entryData) {
			if offset+24 > len(entryData) {
				break
			}

			// Check if next entry is a file separator
			isNextSeparator := true
			for i := 0; i < 20; i++ {
				if entryData[offset+i] != 0 {
					isNextSeparator = false
					break
				}
			}

			if isNextSeparator {
				break
			}

			var sha [20]byte
			copy(sha[:], entryData[offset:offset+20])
			size := binary.BigEndian.Uint16(entryData[offset+20 : offset+22])
			level := binary.BigEndian.Uint16(entryData[offset+22 : offset+24])

			fmt.Printf("  Entry %d: SHA=%x Size=%d Level=%d\n", entryNum, sha, size, level)
			computedSize += uint64(size)
			entryNum++
			offset += 24
		}

		fmt.Printf("  Total chunks: %d\n", entryNum)
		fmt.Printf("  Computed size: %d bytes\n", computedSize)
		if computedSize != fileSize {
			fmt.Printf("  WARNING: Computed size doesn't match file size!\n")
		}
		fmt.Printf("\n")

		fileNum++
	}

	fmt.Printf("Total files: %d\n", fileNum)
	return nil
}

func main() {
	getopt.SetUsage(usage)
	getopt.Parse()
	args := getopt.Args()

	if *help || len(args) == 0 {
		usage()
	}

	if *outfile != "" && *outdir != "" {
		fmt.Fprintln(os.Stderr, "error: --outfile is incompatible with --outdir")
		os.Exit(1)
	}

	if *mfidx && *outdir != "" {
		fmt.Fprintln(os.Stderr, "error: --mfidx is incompatible with --outdir")
		os.Exit(1)
	}

	if *mfidx && *ascii {
		fmt.Fprintln(os.Stderr, "error: --mfidx is incompatible with --ascii")
		os.Exit(1)
	}

	if *print && *mfidx {
		fmt.Fprintln(os.Stderr, "error: --print is incompatible with --mfidx (which creates files)")
		os.Exit(1)
	}

	if *print && *ascii {
		fmt.Fprintln(os.Stderr, "error: --print is incompatible with --ascii")
		os.Exit(1)
	}

	// Print mode - dump existing fidx/mfidx files
	if *print {
		for _, filename := range args {
			if err := printFidx(filename); err != nil {
				fmt.Fprintf(os.Stderr, "error printing %s: %v\n", filename, err)
				os.Exit(1)
			}
		}
		return
	}

	// Multi-file FIDX mode
	if *mfidx {
		if *outfile == "" {
			fmt.Fprintln(os.Stderr, "error: --mfidx requires --outfile")
			os.Exit(1)
		}
		if err := processMFIDX(args, *outfile); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Regular single-file mode validation
	if *outfile != "" && len(args) > 1 {
		fmt.Fprintln(os.Stderr, "error: --outfile only works with a single input file (or use --mfidx for multi-file)")
		os.Exit(1)
	}

	// Regular single-file mode
	for _, filename := range args {
		if err := processFile(filename); err != nil {
			fmt.Fprintf(os.Stderr, "error processing %s: %v\n", filename, err)
			os.Exit(1)
		}
	}
}

func processFile(filename string) error {
	fmt.Printf("%s\n", filename)

	if *ascii {
		// ASCII mode - print to stdout
		return chunkFile(filename, func(entry FidxEntry) error {
			fmt.Printf("%x %d %d\n", entry.SHA[:], entry.Level, entry.Size)
			return nil
		})
	}

	// Binary mode - write .fidx file
	var outpath string
	if *outfile != "" {
		outpath = *outfile
	} else if *outdir != "" {
		outpath = filepath.Join(*outdir, filepath.Base(filename)+".fidx")
	} else {
		outpath = filename + ".fidx"
	}

	tmppath := outpath + ".tmp"
	outf, err := os.Create(tmppath)
	if err != nil {
		return err
	}
	defer func() {
		outf.Close()
		os.Remove(tmppath) // Clean up on error
	}()

	// Hash accumulator for the entire fidx file
	fidxHash := sha1.New()

	// Write header
	header := make([]byte, 8)
	copy(header[0:4], "FIDX")
	binary.BigEndian.PutUint32(header[4:8], FIDX_VERSION)
	if _, err := outf.Write(header); err != nil {
		return err
	}
	fidxHash.Write(header)

	// Write entries
	if err := chunkFile(filename, func(entry FidxEntry) error {
		entryData := make([]byte, 24)
		copy(entryData[0:20], entry.SHA[:])
		binary.BigEndian.PutUint16(entryData[20:22], entry.Size)
		binary.BigEndian.PutUint16(entryData[22:24], entry.Level)
		if _, err := outf.Write(entryData); err != nil {
			return err
		}
		fidxHash.Write(entryData)
		return nil
	}); err != nil {
		return err
	}

	// Write fidx file checksum
	checksum := fidxHash.Sum(nil)
	if _, err := outf.Write(checksum); err != nil {
		return err
	}

	if err := outf.Close(); err != nil {
		return err
	}

	// Atomically rename to final name
	if err := os.Rename(tmppath, outpath); err != nil {
		return err
	}

	fmt.Printf("  wrote %s\n", outpath)
	return nil
}
