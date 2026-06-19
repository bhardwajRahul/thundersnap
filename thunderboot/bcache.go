package thunderboot

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"hash/crc64"
	"os"
)

// bcache superblock constants from Linux kernel / bcache-tools
// See: https://github.com/koverstreet/bcache-tools/blob/master/bcache.h
const (
	SB_SECTOR          = 8 // superblock is at sector 8 (4096 bytes)
	SB_START           = SB_SECTOR * 512
	SB_LABEL_SIZE      = 32
	SB_JOURNAL_BUCKETS = 256
	BDEV_DATA_START    = 16 // sectors - where actual data starts for backing devices

	// Version constants
	BCACHE_SB_VERSION_CDEV             = 0
	BCACHE_SB_VERSION_BDEV             = 1
	BCACHE_SB_VERSION_CDEV_WITH_UUID   = 3
	BCACHE_SB_VERSION_BDEV_WITH_OFFSET = 4

	// Cache modes
	CACHE_MODE_WRITETHROUGH = 0
	CACHE_MODE_WRITEBACK    = 1
	CACHE_MODE_WRITEAROUND  = 2
	CACHE_MODE_NONE         = 3

	// Backing device states
	BDEV_STATE_NONE  = 0
	BDEV_STATE_CLEAN = 1
	BDEV_STATE_DIRTY = 2
	BDEV_STATE_STALE = 3

	// Cache replacement policies
	CACHE_REPLACEMENT_LRU    = 0
	CACHE_REPLACEMENT_FIFO   = 1
	CACHE_REPLACEMENT_RANDOM = 2
)

// bcache magic number
var bcacheMagic = [16]byte{
	0xc6, 0x85, 0x73, 0xf6, 0x4e, 0x1a, 0x45, 0xca,
	0x82, 0x65, 0xf5, 0x7f, 0x48, 0xba, 0x6d, 0x81,
}

// cacheSB represents the bcache superblock structure.
// This matches struct cache_sb from bcache.h.
type cacheSB struct {
	Csum    uint64
	Offset  uint64 // sector where this sb was written
	Version uint64
	Magic   [16]byte
	UUID    [16]byte
	SetUUID [16]byte // for cache devices: identifies the cache set
	Label   [32]byte
	Flags   uint64
	Seq     uint64
	Pad     [8]uint64

	// Union: cache device fields OR backing device fields
	// For cache devices:
	NBuckets   uint64
	BlockSize  uint16 // sectors
	BucketSize uint16 // sectors
	NrInSet    uint16
	NrThisDev  uint16
	// For backing devices, NBuckets becomes DataOffset

	LastMount       uint32
	FirstBucket     uint16
	NJournalBuckets uint16 // or Keys for backing devices

	// Journal buckets array (only used for cache devices)
	// We don't need to write this for simple formatting
}

// cacheSBSize is the size of the superblock we write (without journal buckets)
const cacheSBSize = 8 + 8 + 8 + 16 + 16 + 16 + 32 + 8 + 8 + 64 + 8 + 2 + 2 + 2 + 2 + 4 + 2 + 2

// crc64Table is the ECMA-182 polynomial table used by bcache
var crc64Table = crc64.MakeTable(crc64.ECMA)

// bchCRC64 computes the bcache checksum.
// It matches bch_crc64() from Linux kernel: init with ~0, crc64_be, xor with ~0.
func bchCRC64(data []byte) uint64 {
	// Note: Go's crc64.Checksum with ECMA table produces the same result as
	// Linux's crc64_be when we handle the init/finalize properly.
	// bch_crc64: crc = ~0; crc = crc64_be(crc, data); return crc ^ ~0;
	h := crc64.New(crc64Table)
	h.Write(data)
	return h.Sum64()
}

// FormatBcacheCache formats a file as a bcache cache device.
// This replaces the make-bcache -C command.
func FormatBcacheCache(path string, bucketSizeKB int) error {
	if bucketSizeKB == 0 {
		bucketSizeKB = 512 // default 512KB buckets
	}

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	// Get file size
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	size := fi.Size()

	bucketSizeSectors := uint16(bucketSizeKB * 1024 / 512)
	blockSizeSectors := uint16(1) // 512 bytes, minimum

	// Calculate number of buckets
	// First bucket starts after superblock area
	firstBucket := uint16((SB_START + 512) / (int64(bucketSizeSectors) * 512))
	if firstBucket < 1 {
		firstBucket = 1
	}
	nbuckets := uint64(size / (int64(bucketSizeSectors) * 512))

	// Generate UUIDs
	var uuid, setUUID [16]byte
	if _, err := rand.Read(uuid[:]); err != nil {
		return fmt.Errorf("generate uuid: %w", err)
	}
	if _, err := rand.Read(setUUID[:]); err != nil {
		return fmt.Errorf("generate set_uuid: %w", err)
	}

	sb := &cacheSB{
		Offset:      SB_SECTOR,
		Version:     BCACHE_SB_VERSION_CDEV_WITH_UUID,
		Magic:       bcacheMagic,
		UUID:        uuid,
		SetUUID:     setUUID,
		Flags:       0,
		Seq:         0,
		NBuckets:    nbuckets,
		BlockSize:   blockSizeSectors,
		BucketSize:  bucketSizeSectors,
		NrInSet:     1,
		NrThisDev:   0,
		FirstBucket: firstBucket,
	}

	// Serialize and compute checksum
	data := serializeCacheSB(sb)
	sb.Csum = bchCRC64(data[8:]) // checksum excludes first 8 bytes (csum field)

	// Re-serialize with checksum
	data = serializeCacheSB(sb)

	// Zero the first 8KB (SB_START) and write superblock
	zeros := make([]byte, SB_START)
	if _, err := f.WriteAt(zeros, 0); err != nil {
		return fmt.Errorf("zero header: %w", err)
	}
	if _, err := f.WriteAt(data, SB_START); err != nil {
		return fmt.Errorf("write superblock: %w", err)
	}

	return f.Sync()
}

// FormatBcacheBacking formats a file as a bcache backing device.
// This replaces the make-bcache -B command.
func FormatBcacheBacking(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	// Generate UUID (backing devices don't have set_uuid initially)
	var uuid [16]byte
	if _, err := rand.Read(uuid[:]); err != nil {
		return fmt.Errorf("generate uuid: %w", err)
	}

	// For backing devices, we use the simple BDEV version
	// DataOffset field (in place of NBuckets) specifies where data starts
	sb := &cacheSB{
		Offset:    SB_SECTOR,
		Version:   BCACHE_SB_VERSION_BDEV,
		Magic:     bcacheMagic,
		UUID:      uuid,
		Flags:     0,
		Seq:       0,
		NBuckets:  BDEV_DATA_START, // This is actually DataOffset for backing devices
		BlockSize: 1,               // 512 bytes
	}

	// Serialize and compute checksum
	data := serializeCacheSB(sb)
	sb.Csum = bchCRC64(data[8:])

	// Re-serialize with checksum
	data = serializeCacheSB(sb)

	// Zero the first 8KB (SB_START) and write superblock
	zeros := make([]byte, SB_START)
	if _, err := f.WriteAt(zeros, 0); err != nil {
		return fmt.Errorf("zero header: %w", err)
	}
	if _, err := f.WriteAt(data, SB_START); err != nil {
		return fmt.Errorf("write superblock: %w", err)
	}

	return f.Sync()
}

// serializeCacheSB serializes the superblock to bytes.
func serializeCacheSB(sb *cacheSB) []byte {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, sb.Csum)
	binary.Write(buf, binary.LittleEndian, sb.Offset)
	binary.Write(buf, binary.LittleEndian, sb.Version)
	buf.Write(sb.Magic[:])
	buf.Write(sb.UUID[:])
	buf.Write(sb.SetUUID[:])
	buf.Write(sb.Label[:])
	binary.Write(buf, binary.LittleEndian, sb.Flags)
	binary.Write(buf, binary.LittleEndian, sb.Seq)
	for i := 0; i < 8; i++ {
		binary.Write(buf, binary.LittleEndian, sb.Pad[i])
	}
	binary.Write(buf, binary.LittleEndian, sb.NBuckets)
	binary.Write(buf, binary.LittleEndian, sb.BlockSize)
	binary.Write(buf, binary.LittleEndian, sb.BucketSize)
	binary.Write(buf, binary.LittleEndian, sb.NrInSet)
	binary.Write(buf, binary.LittleEndian, sb.NrThisDev)
	binary.Write(buf, binary.LittleEndian, sb.LastMount)
	binary.Write(buf, binary.LittleEndian, sb.FirstBucket)
	binary.Write(buf, binary.LittleEndian, sb.NJournalBuckets)
	return buf.Bytes()
}
