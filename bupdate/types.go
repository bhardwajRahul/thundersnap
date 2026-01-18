// Package bupdate provides core functionality for fidx file format and content-defined chunking.
package bupdate

import (
	"crypto/sha1"
	"fmt"
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

// FileByChecksums maps a checksum list key to the file path that has those exact checksums.
// The key is the concatenated SHA bytes of all chunks in order.
type FileByChecksums struct {
	Files map[string]string // checksum key -> file path
}

// NewFileByChecksums creates a new FileByChecksums
func NewFileByChecksums() *FileByChecksums {
	return &FileByChecksums{
		Files: make(map[string]string),
	}
}

// MakeChecksumKey creates a key from a list of FidxEntry checksums
func MakeChecksumKey(entries []FidxEntry) string {
	key := make([]byte, len(entries)*20)
	for i, ent := range entries {
		copy(key[i*20:(i+1)*20], ent.SHA[:])
	}
	return string(key)
}

// Add registers a file with its checksum list
func (f *FileByChecksums) Add(entries []FidxEntry, filePath string) {
	key := MakeChecksumKey(entries)
	// Only store the first file we encounter with this checksum list
	if _, exists := f.Files[key]; !exists {
		f.Files[key] = filePath
	}
}

// Find looks up a file that has the exact same checksum list
func (f *FileByChecksums) Find(entries []FidxEntry) (string, bool) {
	key := MakeChecksumKey(entries)
	path, ok := f.Files[key]
	return path, ok
}

// ZeroBlockSHA is the SHA of a BLOB_MAX-sized block of all zeros.
// This is used to detect sparse file holes during reconstruction.
var ZeroBlockSHA [20]byte

func init() {
	// Compute SHA of all-zeros block using git blob format
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", BLOB_MAX)
	h.Write(make([]byte, BLOB_MAX))
	copy(ZeroBlockSHA[:], h.Sum(nil))
}
