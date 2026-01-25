// slab reads a fidx (or mfidx) file and writes a sequential slab file
// containing all unique blocks from the original fidx in order, plus
// a .fidx file for the slab.
package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pborman/getopt/v2"
	"github.com/tailscale/thundersnap/bupdate"
)

var (
	outfile = getopt.StringLong("outfile", 'o', "", "output slab filename (required)")
	help    = getopt.BoolLong("help", 'h', "show help")
)

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: slab -o <slab> <fidx-or-mfidx>\n")
	fmt.Fprintf(os.Stderr, "\nCreates a slab file containing all unique blocks from the input fidx/mfidx,\n")
	fmt.Fprintf(os.Stderr, "plus a .fidx file describing the slab.\n\n")
	getopt.PrintUsage(os.Stderr)
	os.Exit(1)
}

func main() {
	getopt.SetUsage(usage)
	getopt.Parse()
	args := getopt.Args()

	if *help || len(args) != 1 || *outfile == "" {
		usage()
	}

	fidxPath := args[0]
	slabPath := *outfile
	slabFidxPath := slabPath + ".fidx"

	if err := createSlab(fidxPath, slabPath, slabFidxPath); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("wrote %s\n", slabPath)
	fmt.Printf("wrote %s\n", slabFidxPath)
}

// chunkLocation describes where to read a chunk from
type chunkLocation struct {
	filename string
	offset   int64
	size     uint16
}

// createSlab reads the fidx/mfidx, builds the slab and its fidx
func createSlab(fidxPath, slabPath, slabFidxPath string) error {
	// Load the fidx/mfidx
	fidx, err := bupdate.LoadFidx(fidxPath)
	if err != nil {
		return fmt.Errorf("loading fidx: %w", err)
	}

	// Build mapping from chunk entries to their source file locations
	locations, entries, err := buildChunkLocations(fidxPath, fidx)
	if err != nil {
		return fmt.Errorf("building chunk locations: %w", err)
	}

	// Create the slab file
	slabTmp := slabPath + ".tmp"
	slabFile, err := os.Create(slabTmp)
	if err != nil {
		return fmt.Errorf("creating slab: %w", err)
	}
	defer func() {
		slabFile.Close()
		os.Remove(slabTmp)
	}()

	slabBuf := bufio.NewWriter(slabFile)

	// Track seen SHAs to skip duplicates
	seen := make(map[[20]byte]bool)

	// Track the entries for the slab's fidx (in order, no duplicates)
	var slabEntries []bupdate.FidxEntry

	// Write unique blocks to slab in original order
	for i, entry := range entries {
		if seen[entry.SHA] {
			continue
		}
		seen[entry.SHA] = true

		loc := locations[i]
		data, err := bupdate.ReadChunk(loc.filename, loc.offset, int64(loc.size))
		if err != nil {
			return fmt.Errorf("reading chunk from %s offset %d: %w", loc.filename, loc.offset, err)
		}

		if _, err := slabBuf.Write(data); err != nil {
			return fmt.Errorf("writing to slab: %w", err)
		}

		slabEntries = append(slabEntries, entry)
	}

	if err := slabBuf.Flush(); err != nil {
		return fmt.Errorf("flushing slab: %w", err)
	}
	if err := slabFile.Close(); err != nil {
		return fmt.Errorf("closing slab: %w", err)
	}

	// Atomically rename slab to final name
	if err := os.Rename(slabTmp, slabPath); err != nil {
		return fmt.Errorf("renaming slab: %w", err)
	}

	// Create the slab's fidx file
	if err := writeFidx(slabFidxPath, slabEntries); err != nil {
		return fmt.Errorf("writing slab fidx: %w", err)
	}

	return nil
}

// buildChunkLocations returns parallel slices of locations and entries for all chunks
func buildChunkLocations(fidxPath string, fidx *bupdate.Fidx) ([]chunkLocation, []bupdate.FidxEntry, error) {
	var locations []chunkLocation
	var entries []bupdate.FidxEntry

	if fidx.IsMFIDX {
		// Multi-file index: source files are in a directory named after the mfidx
		baseDir := strings.TrimSuffix(fidxPath, ".mfidx")
		baseDir = strings.TrimSuffix(baseDir, ".fidx")

		for _, fileEntry := range fidx.Files {
			filePath := filepath.Join(baseDir, fileEntry.Filename)

			var offset int64
			for _, ent := range fileEntry.Entries {
				locations = append(locations, chunkLocation{
					filename: filePath,
					offset:   offset,
					size:     ent.Size,
				})
				entries = append(entries, ent)
				offset += int64(ent.Size)
			}
		}
	} else {
		// Single-file index: source file is the fidx path without .fidx extension
		sourceFile := strings.TrimSuffix(fidxPath, ".fidx")

		var offset int64
		for _, ent := range fidx.Entries {
			locations = append(locations, chunkLocation{
				filename: sourceFile,
				offset:   offset,
				size:     ent.Size,
			})
			entries = append(entries, ent)
			offset += int64(ent.Size)
		}
	}

	return locations, entries, nil
}

// writeFidx writes a single-file fidx for the slab
func writeFidx(path string, entries []bupdate.FidxEntry) error {
	tmpPath := path + ".tmp"
	outf, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	defer func() {
		outf.Close()
		os.Remove(tmpPath)
	}()

	bufw := bufio.NewWriter(outf)
	fidxHash := sha1.New()

	// Write header
	header := make([]byte, 8)
	copy(header[0:4], "FIDX")
	binary.BigEndian.PutUint32(header[4:8], bupdate.FIDX_VERSION)
	if _, err := bufw.Write(header); err != nil {
		return err
	}
	fidxHash.Write(header)

	// Write entries
	for _, entry := range entries {
		entryData := make([]byte, 24)
		copy(entryData[0:20], entry.SHA[:])
		binary.BigEndian.PutUint16(entryData[20:22], entry.Size)
		binary.BigEndian.PutUint16(entryData[22:24], entry.Level)
		if _, err := bufw.Write(entryData); err != nil {
			return err
		}
		fidxHash.Write(entryData)
	}

	// Write checksum
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

	return os.Rename(tmpPath, path)
}
