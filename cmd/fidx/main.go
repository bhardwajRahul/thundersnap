// fidx generates file index (.fidx) files using content-defined chunking.
// This is a Go port of bup's fidx-cmd.py
package main

import (
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"

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

	buf := make([]byte, BLOB_READ_SIZE)
	var leftover []byte
	totalBytes := int64(0)

	for {
		n, err := f.Read(buf[len(leftover):])
		if n > 0 {
			// Prepend any leftover data from previous iteration
			data := buf[:len(leftover)+n]
			if len(leftover) > 0 {
				copy(buf, leftover)
			}

			// Find chunks in this buffer
			offset := 0
			for {
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

// processMFIDX creates a multi-file FIDX containing all input files
func processMFIDX(filenames []string, outpath string) error {
	fmt.Printf("Creating multi-file index: %s\n", outpath)

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
	for _, filename := range filenames {
		fmt.Printf("  %s\n", filename)

		// Get file info
		stat, err := os.Stat(filename)
		if err != nil {
			return fmt.Errorf("stat %s: %w", filename, err)
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
