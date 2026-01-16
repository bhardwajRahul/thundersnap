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
	"github.com/tailscale/thundersnap/bupdate"
)

var (
	outdir  = getopt.StringLong("outdir", 'd', "", "directory to write output (.fidx) files")
	outfile = getopt.StringLong("outfile", 'o', "", "filename to write fidx/mfidx data")
	refFile = getopt.StringLong("ref", 'r', "", "reference mfidx file to copy entries from for unchanged files")
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

// chunkFile wraps bupdate.ChunkFile with progress reporting
func chunkFile(filename string, writeEntry func(bupdate.FidxEntry) error) error {
	stat, _ := os.Stat(filename)
	fileSize := stat.Size()
	var lastProgress int64 = -1
	
	return bupdate.ChunkFile(filename, writeEntry, func(totalBytes, fSize int64) {
		if fileSize > 10*1024*1024 {
			progress := (totalBytes * 100) / fSize
			if progress/10 != lastProgress/10 {
				fmt.Printf("    %d%% (%d MB / %d MB)\n", progress, totalBytes/(1024*1024), fSize/(1024*1024))
				lastProgress = progress
			}
		}
	})
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
	sep := bupdate.FileSeparator{
		Filename: filename,
		FileSize: uint64(len(target)),
		Mtime:    uint64(stat.ModTime().Unix()),
	}

	var sepBuf []byte
	sepBuf = make([]byte, 0, 1024)
	sepWriter := &bytesWriter{buf: &sepBuf}

	if _, err := bupdate.WriteFileSeparator(sepWriter, sep); err != nil {
		return fmt.Errorf("write separator for %s: %w", filename, err)
	}

	if _, err := outf.Write(sepBuf); err != nil {
		return err
	}
	fidxHash.Write(sepBuf)

	// Write the link target as a single chunk
	targetBytes := []byte(target)
	sha := bupdate.BlobSHA(targetBytes)

	entry := bupdate.FidxEntry{
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

// processMFIDX creates a multi-file FIDX
func processMFIDX(filenames []string, outpath string, refMap map[string]*refFileInfo) error {
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
	binary.BigEndian.PutUint32(header[4:8], bupdate.FIDX_VERSION)
	if _, err := outf.Write(header); err != nil {
		return err
	}
	fidxHash.Write(header)

	// Track stats for reference hits
	var reusedFiles, indexedFiles int

	// Process each file
	for _, filename := range allFiles {
		// Get file info (use Lstat to not follow symlinks)
		stat, err := os.Lstat(filename)
		if err != nil {
			return fmt.Errorf("lstat %s: %w", filename, err)
		}

		// Check if we can reuse entries from reference mfidx
		if refMap != nil {
			if ref, ok := refMap[filename]; ok {
				if fileMatches(filename, ref) {
					// File is unchanged - copy entries from reference
					fmt.Printf("  %s (unchanged)\n", filename)
					if err := writeRefEntries(outf, fidxHash, filename, stat, ref); err != nil {
						return err
					}
					reusedFiles++
					continue
				}
			}
		}

		fmt.Printf("  %s\n", filename)
		indexedFiles++

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
		sep := bupdate.FileSeparator{
			Filename: filename,
			FileSize: uint64(stat.Size()),
			Mtime:    uint64(stat.ModTime().Unix()),
		}

		// Create a buffer for separator data to add to hash
		var sepBuf []byte
		sepBuf = make([]byte, 0, 1024)
		sepWriter := &bytesWriter{buf: &sepBuf}

		if _, err := bupdate.WriteFileSeparator(sepWriter, sep); err != nil {
			return fmt.Errorf("write separator for %s: %w", filename, err)
		}

		// Write to file and hash
		if _, err := outf.Write(sepBuf); err != nil {
			return err
		}
		fidxHash.Write(sepBuf)

		// Write chunk entries for this file
		if err := chunkFile(filename, func(entry bupdate.FidxEntry) error {
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

	fmt.Printf("Wrote %s (%d indexed, %d reused from ref)\n", outpath, indexedFiles, reusedFiles)
	return nil
}

// writeRefEntries writes file separator and chunk entries from a reference mfidx
func writeRefEntries(outf *os.File, fidxHash io.Writer, filename string, stat os.FileInfo, ref *refFileInfo) error {
	// Write file separator entry (using current file's metadata)
	sep := bupdate.FileSeparator{
		Filename: filename,
		FileSize: ref.entry.FileSize,
		Mtime:    ref.entry.Mtime,
	}

	var sepBuf []byte
	sepBuf = make([]byte, 0, 1024)
	sepWriter := &bytesWriter{buf: &sepBuf}

	if _, err := bupdate.WriteFileSeparator(sepWriter, sep); err != nil {
		return fmt.Errorf("write separator for %s: %w", filename, err)
	}

	if _, err := outf.Write(sepBuf); err != nil {
		return err
	}
	fidxHash.Write(sepBuf)

	// Write chunk entries copied from reference
	for _, entry := range ref.entry.Entries {
		entryData := make([]byte, 24)
		copy(entryData[0:20], entry.SHA[:])
		binary.BigEndian.PutUint16(entryData[20:22], entry.Size)
		binary.BigEndian.PutUint16(entryData[22:24], entry.Level)
		if _, err := outf.Write(entryData); err != nil {
			return err
		}
		fidxHash.Write(entryData)
	}

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

// refFileInfo holds metadata about a file in the reference mfidx
type refFileInfo struct {
	entry   bupdate.FileEntry
	refPath string // path to the actual file in the reference filesystem
}

// loadRefMFIDX loads a reference mfidx and builds a map of filename -> refFileInfo
// The refBase is the directory containing the files that the mfidx indexes
func loadRefMFIDX(mfidxPath string) (map[string]*refFileInfo, string, error) {
	fidx, err := bupdate.LoadFidx(mfidxPath)
	if err != nil {
		return nil, "", fmt.Errorf("loading reference mfidx: %w", err)
	}
	if !fidx.IsMFIDX {
		return nil, "", fmt.Errorf("reference file is not an mfidx")
	}

	// The mfidx contains paths relative to some base directory
	// We need to figure out the base directory from the mfidx path
	// Assume the mfidx is in the same directory as the files it indexes,
	// or that the paths in the mfidx are the actual paths
	refBase := filepath.Dir(mfidxPath)

	result := make(map[string]*refFileInfo)
	for _, fe := range fidx.Files {
		// Store by the filename as recorded in the mfidx
		result[fe.Filename] = &refFileInfo{
			entry:   fe,
			refPath: filepath.Join(refBase, fe.Filename),
		}
	}

	return result, refBase, nil
}

// fileMatches checks if the current file matches the reference file metadata
// by comparing mtime, ctime, size, and inode number
func fileMatches(currentPath string, ref *refFileInfo) bool {
	currentStat, err := os.Lstat(currentPath)
	if err != nil {
		return false
	}

	// Check size
	if uint64(currentStat.Size()) != ref.entry.FileSize {
		return false
	}

	// Check mtime
	if uint64(currentStat.ModTime().Unix()) != ref.entry.Mtime {
		return false
	}

	// Get the reference file stat
	refStat, err := os.Lstat(ref.refPath)
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

// printFidx reads and prints fidx/mfidx contents
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
	if version != bupdate.FIDX_VERSION {
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

	if *refFile != "" && !*mfidx {
		fmt.Fprintln(os.Stderr, "error: --ref requires --mfidx")
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

		// Load reference mfidx if specified
		var refMap map[string]*refFileInfo
		if *refFile != "" {
			var err error
			refMap, _, err = loadRefMFIDX(*refFile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error loading reference mfidx: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Loaded reference mfidx with %d files\n", len(refMap))
		}

		if err := processMFIDX(args, *outfile, refMap); err != nil {
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
		return chunkFile(filename, func(entry bupdate.FidxEntry) error {
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
	binary.BigEndian.PutUint32(header[4:8], bupdate.FIDX_VERSION)
	if _, err := outf.Write(header); err != nil {
		return err
	}
	fidxHash.Write(header)

	// Write entries
	if err := chunkFile(filename, func(entry bupdate.FidxEntry) error {
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
