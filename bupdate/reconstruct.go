package bupdate

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sort"
)

// FindMapping performs binary search to find a chunk by SHA
func (m *FidxMappings) FindMapping(sha [20]byte) *FidxMapping {
	i := sort.Search(len(m.Mappings), func(i int) bool {
		return bytes.Compare(m.Mappings[i].SHA[:], sha[:]) >= 0
	})

	if i < len(m.Mappings) && bytes.Equal(m.Mappings[i].SHA[:], sha[:]) {
		return &m.Mappings[i]
	}
	return nil
}

// ReadChunk reads a chunk from a file at the specified offset.
// For symlinks, it reads from the link target content (via Readlink), not the target file.
func ReadChunk(filename string, offset, size int64) ([]byte, error) {
	// Check if this is a symlink - if so, read the link target as the content
	info, err := os.Lstat(filename)
	if err != nil {
		return nil, err
	}

	if info.Mode()&os.ModeSymlink != 0 {
		// For symlinks, the "content" is the link target string
		target, err := os.Readlink(filename)
		if err != nil {
			return nil, err
		}
		data := []byte(target)
		if offset+size > int64(len(data)) {
			return nil, fmt.Errorf("symlink read out of bounds: offset=%d size=%d len=%d", offset, size, len(data))
		}
		return data[offset : offset+size], nil
	}

	// Regular file
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if _, err := f.Seek(offset, 0); err != nil {
		return nil, err
	}

	data := make([]byte, size)
	n, err := io.ReadFull(f, data)
	if err != nil {
		return nil, err
	}
	if int64(n) != size {
		return nil, fmt.Errorf("short read: expected %d, got %d", size, n)
	}

	return data, nil
}

// CopyFile copies a file from src to dst
func CopyFile(dst, src string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0644)
}
