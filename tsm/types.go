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
	// TSMVersion is the TSM file format version (encoded in TSMMagic).
	TSMVersion = 3

	// TSCVersion is the TSC file format version (encoded in TSCMagic).
	TSCVersion = 3

	// TSMMagic is the 4-byte magic prefix of a .tsm file; the final byte is
	// the format version (TSMVersion).
	TSMMagic = "TSM\x03"

	// TSCMagic is the 4-byte magic prefix of a .tsc file; the final byte is
	// the format version (TSCVersion).
	TSCMagic = "TSC\x03"

	// TSMHeaderSize is the fixed size in bytes of a .tsm header.
	TSMHeaderSize = 64
	// TSCHeaderSize is the fixed size in bytes of a .tsc header.
	TSCHeaderSize = 64
	// TSMFooterSize is the fixed size in bytes of a .tsm footer.
	TSMFooterSize = 64
	// TSCFooterSize is the fixed size in bytes of a .tsc footer.
	TSCFooterSize = 32
	// TSCEntrySize is a chunk entry's size without slab locations.
	TSCEntrySize = 40
	// TSCEntrySlab is a chunk entry's size with slab locations.
	TSCEntrySlab = 48
	// SHA256Size is the byte length of a SHA-256 digest.
	SHA256Size = 32

	// Content-defined chunking parameters (from bupdate). These define the
	// chunk-boundary algorithm and are part of the on-disk format: changing
	// any of them changes every chunk hash.

	// BUP_BLOBBITS is the target chunk-size exponent (average chunk ~2^bits).
	BUP_BLOBBITS = 13
	// BUP_BLOBSIZE is the target average chunk size, 2^BUP_BLOBBITS = 8192.
	BUP_BLOBSIZE = 1 << BUP_BLOBBITS
	// BUP_WINDOWBITS sizes the rolling-checksum window.
	BUP_WINDOWBITS = 7
	// BUP_WINDOWSIZE is the rolling-checksum window length. Note the deliberate
	// "-1": the window is 2^(WINDOWBITS-1) = 64 bytes, not 128.
	BUP_WINDOWSIZE = 1 << (BUP_WINDOWBITS - 1)
	// BLOB_MAX is the hard cap on a single chunk's size, 8192*4 = 32768 bytes.
	BLOB_MAX = 8192 * 4
	// BLOB_READ_SIZE is the streaming read buffer used by ChunkReader.
	BLOB_READ_SIZE = 1024 * 1024
	// ROLLSUM_CHAR_OFFSET is the per-byte bias added in the rolling checksum.
	ROLLSUM_CHAR_OFFSET = 31
	// FANOUT_BITS groups split levels for hierarchical chunking.
	FANOUT_BITS = 4
)

// EntryType represents the type of a filesystem entry. It is stored in the low
// nibble of a TSMEntry's packed type/flags field, so values must stay in 0..15.
type EntryType uint8

// EntryType values for the kinds of filesystem entries a manifest can hold.
const (
	EntryTypeFile     EntryType = 0
	EntryTypeDir      EntryType = 1
	EntryTypeSymlink  EntryType = 2
	EntryTypeHardlink EntryType = 3
	EntryTypeBlockDev EntryType = 4
	EntryTypeCharDev  EntryType = 5
	EntryTypeFifo     EntryType = 6
	EntryTypeSocket   EntryType = 7
)

// String returns a human-readable name for the entry type.
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
	Magic        [4]byte  // TSMMagic ("TSM\x03")
	Flags        uint32   // Bit flags
	FileCount    uint64   // Number of file entries
	TotalSize    uint64   // Total size of all files
	ChunkFileRef [8]byte  // First 8 bytes of .tsc SHA-256
	CreationTime int64    // Unix nanoseconds
	SourceFSUUID [8]byte  // Filesystem UUID (e.g., btrfs)
	Reserved     [16]byte // Reserved for future use
}

// TSMFlags are file-level (header.Flags) bit definitions. NOTE: these are
// reserved format placeholders that the current writer never sets.
// HasExtendedAttrs and HasXattrs name the same concept with two different bits;
// they predate a settled design and are kept only so the bit positions stay
// stable in the on-disk format.
const (
	TSMFlagHasExtendedAttrs = 1 << 0
	TSMFlagHasACLs          = 1 << 1
	TSMFlagHasXattrs        = 1 << 2
)

// TSMEntry represents a file entry in the manifest
type TSMEntry struct {
	Path       string    // File path (UTF-8)
	Type       EntryType // Entry type (file, dir, symlink, etc.)
	Flags      uint16    // Entry flags
	Mode       uint32    // Full st_mode including type bits
	UID        uint32    // Owner user ID
	GID        uint32    // Owner group ID
	Size       uint64    // File size in bytes
	Mtime      int64     // Modification time (Unix nanos)
	Ctime      int64     // Change time (Unix nanos)
	Atime      int64     // Access time (Unix nanos)
	ChunkStart uint32    // Offset into chunk reference table
	ChunkCount uint32    // Number of chunks

	// ChunkRefs contains the TSC indices for this file's chunks, in order.
	// This is populated during indexing and on read from the chunk ref table.
	// Not serialized directly in the entry — stored in a separate section.
	ChunkRefs []uint32

	// Type-specific data
	LinkTarget string // For symlinks: target path
	LinkIndex  uint32 // For hardlinks: target entry index
	DevMajor   uint32 // For devices: major number
	DevMinor   uint32 // For devices: minor number
}

// TSMEntryFlags are per-entry (TSMEntry.Flags) bit definitions. The low nibble
// (bits 0-3) of the packed type/flags field holds the EntryType, so entry flags
// start at bit 4. These are reserved placeholders the current writer never sets.
const (
	TSMEntryFlagHasXattr = 1 << 4
	TSMEntryFlagIsSparse = 1 << 5
)

// TSMFooter is the 64-byte footer of a .tsm file
type TSMFooter struct {
	SHA256  [32]byte // SHA-256 of all preceding bytes
	TSCHash [32]byte // SHA-256 of the corresponding .tsc file
}

// TSCHeader is the 64-byte header of a .tsc file
type TSCHeader struct {
	Magic           [4]byte  // TSCMagic ("TSC\x03")
	Flags           uint32   // Bit flags
	ChunkCount      uint64   // Number of chunk entries
	TotalChunkSize  uint64   // Total size of all chunk data
	SlabCount       uint32   // Number of slabs (0 if no slab locations)
	SlabTableOffset uint32   // Offset to slab name table
	Reserved        [32]byte // Reserved for future use
}

// TSCFlags are chunk-file-level (TSCHeader.Flags) bit definitions.
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

// TSCEntryFlags are per-chunk (TSCEntry.Flags) bit definitions.
const (
	TSCEntryFlagZeroBlock = 1 << 0 // Don't store/fetch, reconstruct as zeros
	TSCEntryFlagLiteral   = 1 << 1 // Stored uncompressed in slab
)

// TSCFooter is the 32-byte footer of a .tsc file
type TSCFooter struct {
	SHA256 [32]byte // SHA-256 of all preceding bytes
}

// ZeroBlockSHA is the SHA-256 of a BLOB_MAX-sized block of all zeros, used to
// recognize and elide all-zero chunks (TSCEntryFlagZeroBlock).
var ZeroBlockSHA [32]byte

func init() {
	// Compute SHA of all-zeros block using the same git-blob framing as
	// BlobSHA256 so it matches chunk hashes produced during indexing.
	h := sha256.New()
	fmt.Fprintf(h, "blob %d\x00", BLOB_MAX)
	h.Write(make([]byte, BLOB_MAX))
	copy(ZeroBlockSHA[:], h.Sum(nil))
}

// BlobSHA256 computes the git/bup blob SHA-256 of data: the digest of the
// framing "blob <size>\0" followed by data. The git-object framing is what
// makes these hashes interoperable with bup/git content-addressed stores.
func BlobSHA256(data []byte) [32]byte {
	h := sha256.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}
