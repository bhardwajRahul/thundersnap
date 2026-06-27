package tsm

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sort"
)

// TSCWriter writes a .tsc (ThunderSnap Chunks) file
type TSCWriter struct {
	chunks    []TSCEntry
	chunkMap  map[[32]byte]uint32 // SHA -> index
	totalSize uint64
}

// NewTSCWriter creates a new TSC writer
func NewTSCWriter() *TSCWriter {
	return &TSCWriter{
		chunkMap: make(map[[32]byte]uint32),
	}
}

// AddChunk adds a chunk to the index, returning its index.
// Duplicate chunks are deduplicated and return their existing index.
func (w *TSCWriter) AddChunk(sha [32]byte, size uint32, level uint16, flags uint16) uint32 {
	if idx, ok := w.chunkMap[sha]; ok {
		return idx
	}

	idx := uint32(len(w.chunks))
	w.chunks = append(w.chunks, TSCEntry{
		SHA256: sha,
		Size:   size,
		Level:  level,
		Flags:  flags,
	})
	w.chunkMap[sha] = idx
	w.totalSize += uint64(size)
	return idx
}

// ChunkCount returns the number of unique chunks
func (w *TSCWriter) ChunkCount() uint64 {
	return uint64(len(w.chunks))
}

// TotalChunkSize returns the total size of all unique chunks
func (w *TSCWriter) TotalChunkSize() uint64 {
	return w.totalSize
}

// GetChunkIndex returns the index of a chunk by SHA, or -1 if not found
func (w *TSCWriter) GetChunkIndex(sha [32]byte) int {
	if idx, ok := w.chunkMap[sha]; ok {
		return int(idx)
	}
	return -1
}

// sortedChunks holds the sorted chunks and original-to-sorted index mapping
type sortedChunks struct {
	entries  []TSCEntry
	indexMap []uint32 // original index -> sorted index
}

// sortChunks sorts chunks by SHA and returns the sorted list with index mapping
func (w *TSCWriter) sortChunks() *sortedChunks {
	// Create index mapping for sorting
	indices := make([]int, len(w.chunks))
	for i := range indices {
		indices[i] = i
	}

	// Sort indices by SHA
	sort.Slice(indices, func(i, j int) bool {
		return bytes.Compare(w.chunks[indices[i]].SHA256[:], w.chunks[indices[j]].SHA256[:]) < 0
	})

	// Build sorted entries and reverse mapping
	sorted := &sortedChunks{
		entries:  make([]TSCEntry, len(w.chunks)),
		indexMap: make([]uint32, len(w.chunks)),
	}

	for sortedIdx, origIdx := range indices {
		sorted.entries[sortedIdx] = w.chunks[origIdx]
		sorted.indexMap[origIdx] = uint32(sortedIdx)
	}

	return sorted
}

// Write writes the TSC file and returns the SHA-256 of the file and the index mapping
func (w *TSCWriter) Write(path string) ([32]byte, []uint32, error) {
	sorted := w.sortChunks()

	tmpPath := path + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return [32]byte{}, nil, err
	}
	defer func() {
		f.Close()
		os.Remove(tmpPath)
	}()

	bufw := bufio.NewWriter(f)
	hash := sha256.New()
	mw := io.MultiWriter(bufw, hash)

	// Write header
	header := make([]byte, TSCHeaderSize)
	copy(header[0:4], TSCMagic)
	binary.BigEndian.PutUint32(header[4:8], 0) // Flags (no slab locations)
	binary.BigEndian.PutUint64(header[8:16], uint64(len(sorted.entries)))
	binary.BigEndian.PutUint64(header[16:24], w.totalSize)
	binary.BigEndian.PutUint32(header[24:28], 0) // Slab count
	binary.BigEndian.PutUint32(header[28:32], 0) // Slab table offset
	// Remaining 32 bytes are reserved (zero)

	if _, err := mw.Write(header); err != nil {
		return [32]byte{}, nil, err
	}

	// Write chunk entries (sorted by SHA)
	entryBuf := make([]byte, TSCEntrySize)
	for _, entry := range sorted.entries {
		copy(entryBuf[0:32], entry.SHA256[:])
		binary.BigEndian.PutUint32(entryBuf[32:36], entry.Size)
		binary.BigEndian.PutUint16(entryBuf[36:38], entry.Level)
		binary.BigEndian.PutUint16(entryBuf[38:40], entry.Flags)

		if _, err := mw.Write(entryBuf); err != nil {
			return [32]byte{}, nil, err
		}
	}

	// Write footer
	var fileSHA [32]byte
	copy(fileSHA[:], hash.Sum(nil))
	if _, err := bufw.Write(fileSHA[:]); err != nil {
		return [32]byte{}, nil, err
	}

	if err := bufw.Flush(); err != nil {
		return [32]byte{}, nil, err
	}
	if err := f.Close(); err != nil {
		return [32]byte{}, nil, err
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return [32]byte{}, nil, err
	}

	return fileSHA, sorted.indexMap, nil
}

// TSCReader reads a .tsc file
type TSCReader struct {
	Header  TSCHeader
	Entries []TSCEntry
	SHA256  [32]byte // SHA-256 of the file (excluding footer)
}

// ReadTSC reads and parses a .tsc file
func ReadTSC(path string) (*TSCReader, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	return ParseTSC(data)
}

// ParseTSC parses TSC data from a byte slice
func ParseTSC(data []byte) (*TSCReader, error) {
	if len(data) < TSCHeaderSize+TSCFooterSize {
		return nil, fmt.Errorf("tsc file too short")
	}

	// Check magic
	if string(data[0:4]) != TSCMagic {
		return nil, fmt.Errorf("invalid TSC magic: %q", data[0:4])
	}

	// Extract and verify footer
	footerSHA := data[len(data)-TSCFooterSize:]
	contentData := data[:len(data)-TSCFooterSize]

	h := sha256.New()
	h.Write(contentData)
	computedSHA := h.Sum(nil)

	if !bytes.Equal(computedSHA, footerSHA) {
		return nil, fmt.Errorf("tsc checksum mismatch")
	}

	reader := &TSCReader{}
	copy(reader.SHA256[:], computedSHA)

	// Parse header
	copy(reader.Header.Magic[:], data[0:4])
	reader.Header.Flags = binary.BigEndian.Uint32(data[4:8])
	reader.Header.ChunkCount = binary.BigEndian.Uint64(data[8:16])
	reader.Header.TotalChunkSize = binary.BigEndian.Uint64(data[16:24])
	reader.Header.SlabCount = binary.BigEndian.Uint32(data[24:28])
	reader.Header.SlabTableOffset = binary.BigEndian.Uint32(data[28:32])

	// Validate entry count
	entryDataLen := len(contentData) - TSCHeaderSize
	expectedEntries := entryDataLen / TSCEntrySize
	if uint64(expectedEntries) != reader.Header.ChunkCount {
		return nil, fmt.Errorf("chunk count mismatch: header says %d, data has room for %d",
			reader.Header.ChunkCount, expectedEntries)
	}

	// Parse entries
	reader.Entries = make([]TSCEntry, reader.Header.ChunkCount)
	offset := TSCHeaderSize
	for i := range reader.Entries {
		copy(reader.Entries[i].SHA256[:], data[offset:offset+32])
		reader.Entries[i].Size = binary.BigEndian.Uint32(data[offset+32 : offset+36])
		reader.Entries[i].Level = binary.BigEndian.Uint16(data[offset+36 : offset+38])
		reader.Entries[i].Flags = binary.BigEndian.Uint16(data[offset+38 : offset+40])
		offset += TSCEntrySize
	}

	return reader, nil
}

// LookupChunk finds a chunk by SHA using binary search.
// Returns the entry and true if found, or zero entry and false if not found.
func (r *TSCReader) LookupChunk(sha [32]byte) (TSCEntry, bool) {
	idx := sort.Search(len(r.Entries), func(i int) bool {
		return bytes.Compare(r.Entries[i].SHA256[:], sha[:]) >= 0
	})

	if idx < len(r.Entries) && bytes.Equal(r.Entries[idx].SHA256[:], sha[:]) {
		return r.Entries[idx], true
	}
	return TSCEntry{}, false
}
