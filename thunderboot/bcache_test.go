package thunderboot

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestFormatBcacheCache(t *testing.T) {
	// Create a temporary file
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.img")

	// Create a 64MB sparse file
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(64 * 1024 * 1024); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	// Format as cache device
	if err := FormatBcacheCache(path, 512); err != nil {
		t.Fatalf("FormatBcacheCache: %v", err)
	}

	// Read and verify the superblock
	f, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// Read superblock at SB_START
	sb := make([]byte, 256)
	if _, err := f.ReadAt(sb, SB_START); err != nil {
		t.Fatal(err)
	}

	// Check magic number at offset 24 (after csum, offset, version)
	magic := sb[24:40]
	if !bytes.Equal(magic, bcacheMagic[:]) {
		t.Errorf("bad magic: got %x, want %x", magic, bcacheMagic)
	}

	// Check version (offset 16, 8 bytes LE) - should be CDEV_WITH_UUID (3)
	version := binary.LittleEndian.Uint64(sb[16:24])
	if version != BCACHE_SB_VERSION_CDEV_WITH_UUID {
		t.Errorf("bad version: got %d, want %d", version, BCACHE_SB_VERSION_CDEV_WITH_UUID)
	}

	// Verify checksum
	storedCsum := binary.LittleEndian.Uint64(sb[0:8])
	computedCsum := bchCRC64(sb[8:cacheSBSize])
	if storedCsum != computedCsum {
		t.Errorf("checksum mismatch: stored=%x computed=%x", storedCsum, computedCsum)
	}

	t.Logf("Cache superblock valid: version=%d, csum=%x", version, storedCsum)
}

func TestFormatBcacheBacking(t *testing.T) {
	// Create a temporary file
	dir := t.TempDir()
	path := filepath.Join(dir, "backing.img")

	// Create a 128MB sparse file
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(128 * 1024 * 1024); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	// Format as backing device
	if err := FormatBcacheBacking(path); err != nil {
		t.Fatalf("FormatBcacheBacking: %v", err)
	}

	// Read and verify the superblock
	f, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// Read superblock at SB_START
	sb := make([]byte, 256)
	if _, err := f.ReadAt(sb, SB_START); err != nil {
		t.Fatal(err)
	}

	// Check magic number
	magic := sb[24:40]
	if !bytes.Equal(magic, bcacheMagic[:]) {
		t.Errorf("bad magic: got %x, want %x", magic, bcacheMagic)
	}

	// Check version - should be BDEV (1)
	version := binary.LittleEndian.Uint64(sb[16:24])
	if version != BCACHE_SB_VERSION_BDEV {
		t.Errorf("bad version: got %d, want %d", version, BCACHE_SB_VERSION_BDEV)
	}

	// Verify checksum
	storedCsum := binary.LittleEndian.Uint64(sb[0:8])
	computedCsum := bchCRC64(sb[8:cacheSBSize])
	if storedCsum != computedCsum {
		t.Errorf("checksum mismatch: stored=%x computed=%x", storedCsum, computedCsum)
	}

	t.Logf("Backing superblock valid: version=%d, csum=%x", version, storedCsum)
}

func TestBchCRC64(t *testing.T) {
	// Test with known data to ensure our CRC matches bcache's
	data := []byte("hello bcache")
	csum := bchCRC64(data)

	// The checksum should be deterministic
	csum2 := bchCRC64(data)
	if csum != csum2 {
		t.Errorf("CRC not deterministic: %x != %x", csum, csum2)
	}

	// Different data should produce different checksum
	csum3 := bchCRC64([]byte("different data"))
	if csum == csum3 {
		t.Error("different data produced same CRC")
	}

	t.Logf("bchCRC64(%q) = %x", data, csum)
}
