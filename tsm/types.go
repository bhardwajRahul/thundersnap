// Package tsm provides types and functions for the TSM (ThunderSnap Manifest)
// and TSC (ThunderSnap Chunks) file formats.
//
// TSM files contain filesystem metadata (paths, permissions, ownership, timestamps)
// and per-file chunk references. TSC files contain a sorted, deduplicated list of
// all unique chunks with their SHA-256 hashes and sizes.
package tsm

import (
	"crypto/sha256"
	"fmt"
)

const (
	// TSM file format version
	TSMVersion = 3

	// TSC file format version
	TSCVersion = 3

	// TSM magic bytes
	TSMMagic = "TSM\x03"

	// TSC magic bytes
	TSCMagic = "TSC\x03"

	// Header sizes
	TSMHeaderSize  = 64
	TSCHeaderSize  = 64
	TSMFooterSize  = 64
	TSCFooterSize  = 32
	TSCEntrySize   = 40 // Without slab locations
	TSCEntrySlab   = 48 // With slab locations
	SHA256Size     = 32

	// Content-defined chunking parameters (from bupdate)
	BUP_BLOBBITS   = 13
	BUP_BLOBSIZE   = 1 << BUP_BLOBBITS // 8192
	BUP_WINDOWBITS = 7
	BUP_WINDOWSIZE = 1 << (BUP_WINDOWBITS - 1) // 64
	BLOB_MAX       = 8192 * 4                   // 32768 bytes
	BLOB_READ_SIZE = 1024 * 1024
	ROLLSUM_CHAR_OFFSET = 31
	FANOUT_BITS    = 4
)

// EntryType represents the type of a filesystem entry
type EntryType uint8

const (
	EntryTypeFile    EntryType = 0
	EntryTypeDir     EntryType = 1
	EntryTypeSymlink EntryType = 2
	EntryTypeHardlink EntryType = 3
	EntryTypeBlockDev EntryType = 4
	EntryTypeCharDev  EntryType = 5
	EntryTypeFifo    EntryType = 6
	EntryTypeSocket  EntryType = 7
)

func (t EntryType) String() string {
	switch t {
	case EntryTypeFile:
		return "file"
	case EntryTypeDir:
		return "dir"
	case EntryTypeSymlink:
		return "symlink"
	case EntryTypeHardlink:
		return "hardlink"
	case EntryTypeBlockDev:
		return "blockdev"
	case EntryTypeCharDev:
		return "chardev"
	case EntryTypeFifo:
		return "fifo"
	case EntryTypeSocket:
		return "socket"
	default:
		return fmt.Sprintf("unknown(%d)", t)
	}
}

// TSMHeader is the 64-byte header of a .tsm file
type TSMHeader struct {
	Magic           [4]byte  // "TSM\x02"
	Flags           uint32   // Bit flags
	FileCount       uint64   // Number of file entries
	TotalSize       uint64   // Total size of all files
	ChunkFileRef    [8]byte  // First 8 bytes of .tsc SHA-256
	CreationTime    int64    // Unix nanoseconds
	SourceFSUUID    [8]byte  // Filesystem UUID (e.g., btrfs)
	Reserved        [16]byte // Reserved for future use
}

// TSMFlags bit definitions
const (
	TSMFlagHasExtendedAttrs = 1 << 0
	TSMFlagHasACLs          = 1 << 1
	TSMFlagHasXattrs        = 1 << 2
)

// TSMEntry represents a file entry in the manifest
type TSMEntry struct {
	Path        string    // File path (UTF-8)
	Type        EntryType // Entry type (file, dir, symlink, etc.)
	Flags       uint16    // Entry flags
	Mode        uint32    // Full st_mode including type bits
	UID         uint32    // Owner user ID
	GID         uint32    // Owner group ID
	Size        uint64    // File size in bytes
	Mtime       int64     // Modification time (Unix nanos)
	Ctime       int64     // Change time (Unix nanos)
	Atime       int64     // Access time (Unix nanos)
	ChunkStart  uint32    // Offset into chunk reference table
	ChunkCount  uint32    // Number of chunks

	// ChunkRefs contains the TSC indices for this file's chunks, in order.
	// This is populated during indexing and on read from the chunk ref table.
	// Not serialized directly in the entry — stored in a separate section.
	ChunkRefs   []uint32

	// Type-specific data
	LinkTarget  string    // For symlinks: target path
	LinkIndex   uint32    // For hardlinks: target entry index
	DevMajor    uint32    // For devices: major number
	DevMinor    uint32    // For devices: minor number
}

// TSMEntryFlags bit definitions
const (
	TSMEntryFlagHasXattr  = 1 << 4
	TSMEntryFlagIsSparse  = 1 << 5
)

// TSMFooter is the 64-byte footer of a .tsm file
type TSMFooter struct {
	SHA256      [32]byte // SHA-256 of all preceding bytes
	TSCHash     [32]byte // SHA-256 of the corresponding .tsc file
}

// TSCHeader is the 64-byte header of a .tsc file
type TSCHeader struct {
	Magic            [4]byte  // "TSC\x02"
	Flags            uint32   // Bit flags
	ChunkCount       uint64   // Number of chunk entries
	TotalChunkSize   uint64   // Total size of all chunk data
	SlabCount        uint32   // Number of slabs (0 if no slab locations)
	SlabTableOffset  uint32   // Offset to slab name table
	Reserved         [32]byte // Reserved for future use
}

// TSCFlags bit definitions
const (
	TSCFlagHasSlabLocations    = 1 << 0
	TSCFlagHasCompressionHints = 1 << 1
)

// TSCEntry represents a chunk entry in the chunk index (40 bytes without slab)
type TSCEntry struct {
	SHA256 [32]byte // SHA-256 hash
	Size   uint32   // Chunk size
	Level  uint16   // Bupsplit level for hierarchical grouping
	Flags  uint16   // Chunk flags
}

// TSCEntryFlags bit definitions
const (
	TSCEntryFlagZeroBlock = 1 << 0 // Don't store/fetch, reconstruct as zeros
	TSCEntryFlagLiteral   = 1 << 1 // Stored uncompressed in slab
)

// TSCFooter is the 32-byte footer of a .tsc file
type TSCFooter struct {
	SHA256 [32]byte // SHA-256 of all preceding bytes
}

// ZeroBlockSHA is the SHA-256 of a BLOB_MAX-sized block of all zeros
var ZeroBlockSHA [32]byte

func init() {
	// Compute SHA of all-zeros block using git blob format
	h := sha256.New()
	fmt.Fprintf(h, "blob %d\x00", BLOB_MAX)
	h.Write(make([]byte, BLOB_MAX))
	copy(ZeroBlockSHA[:], h.Sum(nil))
}

// BlobSHA256 computes the git blob SHA-256 of data
func BlobSHA256(data []byte) [32]byte {
	h := sha256.New()
	// Git blob format: "blob <size>\0<data>"
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}
