// Package bupdate provides core functionality for fidx file format and content-defined chunking.
package bupdate

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

// FidxEntry represents a single chunk entry
type FidxEntry struct {
	SHA   [20]byte
	Size  uint16
	Level uint16
}

// FileEntry represents a single file within an mfidx
type FileEntry struct {
	Filename string
	FileSize uint64
	Mtime    uint64
	Entries  []FidxEntry
}

// Fidx represents a parsed fidx file
type Fidx struct {
	Filename string
	Entries  []FidxEntry
	FileSHA  [20]byte // SHA-1 of the entire fidx file (excluding footer)
	FileSize int64    // total size of reconstructed file
	IsMFIDX  bool     // true if this is a multi-file index
	Files    []FileEntry // for mfidx files
}

// FileSeparator represents file metadata in MFIDX format
type FileSeparator struct {
	Filename string
	FileSize uint64
	Mtime    uint64
}

// FidxMapping maps a chunk SHA to its location in a local file
type FidxMapping struct {
	SHA      [20]byte
	Filename string
	Offset   int64
	Size     uint16
}

// FidxMappings is a sorted collection of chunk mappings for fast lookup
type FidxMappings struct {
	Mappings []FidxMapping
}
