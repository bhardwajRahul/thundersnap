// fidx generates file index (.fidx) files using content-defined chunking.
// This is a Go port of bup's fidx-cmd.py
package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

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

		fmt.Printf("Creating multi-file index: %s\n", *outfile)

		opts := bupdate.IndexerOptions{
			RefPath:  *refFile,
			Progress: true,
		}

		if err := bupdate.CreateMFIDX(args, *outfile, opts); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("Wrote %s\n", *outfile)
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
		return bupdate.ChunkFile(filename, func(entry bupdate.FidxEntry) error {
			fmt.Printf("%x %d %d\n", entry.SHA[:], entry.Level, entry.Size)
			return nil
		}, func(totalBytes, fSize int64) {})
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

	opts := bupdate.IndexerOptions{
		Progress: false, // single file mode doesn't need progress
	}

	if err := bupdate.CreateSingleFidx(filename, outpath, opts); err != nil {
		return err
	}

	fmt.Printf("  wrote %s\n", outpath)
	return nil
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
