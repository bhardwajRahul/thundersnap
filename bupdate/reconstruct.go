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

// ReadChunk reads a chunk from a file at the specified offset
func ReadChunk(filename string, offset, size int64) ([]byte, error) {
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
