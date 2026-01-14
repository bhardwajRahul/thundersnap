# bupdate Package

Shared library for fidx file format and content-defined chunking functionality.

## Overview

This package provides the core functionality used by both `cmd/fidx` and `cmd/bupdate`:
- Content-defined chunking with rolling checksums
- FIDX/MFIDX file format reading and writing
- File reconstruction from chunks

## Key Types

### FidxEntry
Represents a single chunk with SHA-1 hash, size, and hierarchical level.

### FileEntry
Represents a file within a multi-file index (mfidx), including metadata and chunks.

### Fidx
Parsed representation of a fidx or mfidx file.

### FidxMappings
Collection of chunk mappings for deduplication, with fast SHA lookup.

## Key Functions

### Chunking
- `FindSplitPoint(buf []byte) (int, int)` - Find content-defined split points
- `BlobSHA(data []byte) [20]byte` - Compute git-style blob SHA-1
- `ChunkFile(filename, writeEntry, progressCallback)` - Split file into chunks

### File I/O
- `LoadFidx(path string) (*Fidx, error)` - Read and parse fidx/mfidx files
- `WriteFileSeparator(w, sep)` - Write mfidx file separator with metadata

### Reconstruction
- `FindMapping(sha)` - Binary search for chunk in mappings
- `ReadChunk(filename, offset, size)` - Read chunk from file
- `CopyFile(dst, src)` - Copy file atomically

## Constants

- `FIDX_VERSION = 1` - Current file format version
- `BLOB_MAX = 32768` - Maximum chunk size (32KB)
- `BLOB_READ_SIZE = 1048576` - Read buffer size (1MB)
- Content-defined chunking parameters from bup

## Usage Example

```go
import "github.com/tailscale/thundersnap/bupdate"

// Load a fidx file
fidx, err := bupdate.LoadFidx("file.fidx")
if err != nil {
    log.Fatal(err)
}

// Chunk a file
err = bupdate.ChunkFile("input.bin", func(entry bupdate.FidxEntry) error {
    // Process each chunk
    fmt.Printf("Chunk: %x, size: %d\n", entry.SHA, entry.Size)
    return nil
}, nil)
```

## Integration with cmd programs

The `cmd/fidx` and `cmd/bupdate` programs can be refactored to use this library by:
1. Importing `github.com/tailscale/thundersnap/bupdate`
2. Replacing local type definitions with package types
3. Using package functions instead of duplicated code
4. Keeping only CLI parsing and output formatting in cmd files
