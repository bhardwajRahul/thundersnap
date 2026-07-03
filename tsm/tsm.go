// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package tsm

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"time"
)

// TSM file layout:
//   [Header 64B]
//   [File Entry 1..N]
//   [Chunk Ref Table: array of uint32 TSC indices]
//   [Footer 64B]
//
// Each file entry's ChunkStart is an offset into the chunk ref table,
// and ChunkCount is the number of uint32 entries to read from that offset.
// This allows files to reference non-contiguous chunks in the SHA-sorted TSC.

// TSMWriter writes a .tsm (ThunderSnap Manifest) file
type TSMWriter struct {
	entries      []TSMEntry
	fileCount    uint64
	totalSize    uint64
	flags        uint32
	creationTime int64
	fsUUID       [8]byte
}

// NewTSMWriter creates a new TSM writer
func NewTSMWriter() *TSMWriter {
	return &TSMWriter{
		creationTime: time.Now().UnixNano(),
	}
}

// SetCreationTime sets the creation timestamp
func (w *TSMWriter) SetCreationTime(t time.Time) {
	w.creationTime = t.UnixNano()
}

// SetFSUUID sets the filesystem UUID
func (w *TSMWriter) SetFSUUID(uuid [8]byte) {
	w.fsUUID = uuid
}

// AddEntry adds a file entry to the manifest
func (w *TSMWriter) AddEntry(entry TSMEntry) {
	w.entries = append(w.entries, entry)
	w.fileCount++
	if entry.Type == EntryTypeFile {
		w.totalSize += entry.Size
	}
}

// EntryCount returns the number of entries
func (w *TSMWriter) EntryCount() uint64 {
	return w.fileCount
}

// TotalSize returns the total size of all regular files
func (w *TSMWriter) TotalSize() uint64 {
	return w.totalSize
}

// sortEntries sorts entries by path
func (w *TSMWriter) sortEntries() {
	sort.Slice(w.entries, func(i, j int) bool {
		return w.entries[i].Path < w.entries[j].Path
	})
}

// encodeEntry encodes a single TSMEntry to bytes
func encodeEntry(entry *TSMEntry) []byte {
	pathBytes := []byte(entry.Path)
	pathLen := len(pathBytes)

	// Calculate base entry size
	// 2 (entry len) + 2 (path len) + pathLen + 2 (type+flags) + 4 (mode) +
	// 4 (uid) + 4 (gid) + 8 (size) + 8 (mtime) + 8 (ctime) + 8 (atime) +
	// 4 (chunk start) + 4 (chunk count) = 58 + pathLen + type-specific
	baseSize := 58 + pathLen

	// Add type-specific data size
	typeDataSize := 0
	switch entry.Type {
	case EntryTypeSymlink:
		typeDataSize = 2 + len(entry.LinkTarget) // 2-byte length + target
	case EntryTypeHardlink:
		typeDataSize = 4 // 4-byte link index
	case EntryTypeBlockDev, EntryTypeCharDev:
		typeDataSize = 8 // 4-byte major + 4-byte minor
	}

	totalSize := baseSize + typeDataSize
	buf := make([]byte, totalSize)
	offset := 0

	// Entry length
	binary.BigEndian.PutUint16(buf[offset:offset+2], uint16(totalSize))
	offset += 2

	// Path length
	binary.BigEndian.PutUint16(buf[offset:offset+2], uint16(pathLen))
	offset += 2

	// Path
	copy(buf[offset:offset+pathLen], pathBytes)
	offset += pathLen

	// Type + flags (combined uint16)
	typeFlags := uint16(entry.Type) | entry.Flags
	binary.BigEndian.PutUint16(buf[offset:offset+2], typeFlags)
	offset += 2

	// Mode
	binary.BigEndian.PutUint32(buf[offset:offset+4], entry.Mode)
	offset += 4

	// UID
	binary.BigEndian.PutUint32(buf[offset:offset+4], entry.UID)
	offset += 4

	// GID
	binary.BigEndian.PutUint32(buf[offset:offset+4], entry.GID)
	offset += 4

	// Size
	binary.BigEndian.PutUint64(buf[offset:offset+8], entry.Size)
	offset += 8

	// Mtime
	binary.BigEndian.PutUint64(buf[offset:offset+8], uint64(entry.Mtime))
	offset += 8

	// Ctime
	binary.BigEndian.PutUint64(buf[offset:offset+8], uint64(entry.Ctime))
	offset += 8

	// Atime
	binary.BigEndian.PutUint64(buf[offset:offset+8], uint64(entry.Atime))
	offset += 8

	// Chunk start index (into chunk ref table)
	binary.BigEndian.PutUint32(buf[offset:offset+4], entry.ChunkStart)
	offset += 4

	// Chunk count
	binary.BigEndian.PutUint32(buf[offset:offset+4], entry.ChunkCount)
	offset += 4

	// Type-specific data
	switch entry.Type {
	case EntryTypeSymlink:
		targetBytes := []byte(entry.LinkTarget)
		binary.BigEndian.PutUint16(buf[offset:offset+2], uint16(len(targetBytes)))
		offset += 2
		copy(buf[offset:], targetBytes)
	case EntryTypeHardlink:
		binary.BigEndian.PutUint32(buf[offset:offset+4], entry.LinkIndex)
	case EntryTypeBlockDev, EntryTypeCharDev:
		binary.BigEndian.PutUint32(buf[offset:offset+4], entry.DevMajor)
		binary.BigEndian.PutUint32(buf[offset+4:offset+8], entry.DevMinor)
	}

	return buf
}

// encodeEntryForHash encodes an entry for hash computation, zeroing ctime and atime.
// These fields are stored in the TSM for change detection, but excluded from the
// hash because they cannot be preserved during replication (ctime is kernel-controlled,
// atime changes on read). This allows identical content to produce identical snap IDs.
func encodeEntryForHash(entry *TSMEntry) []byte {
	// Make a copy with zeroed ctime/atime
	hashEntry := *entry
	hashEntry.Ctime = 0
	hashEntry.Atime = 0
	return encodeEntry(&hashEntry)
}

// Write writes the TSM file with a reference to the TSC file.
// The indexMap maps original TSC indices to sorted TSC indices.
func (w *TSMWriter) Write(path string, tscSHA [32]byte, indexMap []uint32) ([32]byte, error) {
	w.sortEntries()

	// Build the chunk reference table and assign ChunkStart offsets.
	// The chunk ref table is an array of uint32 TSC indices, one per chunk
	// reference across all files. Each file's ChunkStart points into this table.
	var chunkRefTable []uint32
	for i := range w.entries {
		e := &w.entries[i]
		if e.ChunkCount > 0 && len(e.ChunkRefs) > 0 {
			e.ChunkStart = uint32(len(chunkRefTable))
			e.ChunkCount = uint32(len(e.ChunkRefs))
			for _, origIdx := range e.ChunkRefs {
				// Map original TSC index to sorted TSC index
				if indexMap != nil && int(origIdx) < len(indexMap) {
					chunkRefTable = append(chunkRefTable, indexMap[origIdx])
				} else {
					chunkRefTable = append(chunkRefTable, origIdx)
				}
			}
		}
	}

	tmpPath := path + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return [32]byte{}, err
	}
	defer func() {
		f.Close()
		os.Remove(tmpPath)
	}()

	bufw := bufio.NewWriter(f)
	hash := sha256.New()

	// Write header to both file and hash
	header := make([]byte, TSMHeaderSize)
	copy(header[0:4], TSMMagic)
	binary.BigEndian.PutUint32(header[4:8], w.flags)
	binary.BigEndian.PutUint64(header[8:16], w.fileCount)
	binary.BigEndian.PutUint64(header[16:24], w.totalSize)
	copy(header[24:32], tscSHA[:8]) // First 8 bytes of TSC SHA
	binary.BigEndian.PutUint64(header[32:40], uint64(w.creationTime))
	copy(header[40:48], w.fsUUID[:])
	// Remaining 16 bytes are reserved (zero)

	if _, err := bufw.Write(header); err != nil {
		return [32]byte{}, err
	}
	hash.Write(header)

	// Write entries (sorted by path)
	// File gets full entry data (with ctime/atime for change detection).
	// Hash gets entry data with ctime/atime zeroed (for reproducible snap IDs).
	for i := range w.entries {
		entryData := encodeEntry(&w.entries[i])
		if _, err := bufw.Write(entryData); err != nil {
			return [32]byte{}, err
		}
		hashData := encodeEntryForHash(&w.entries[i])
		hash.Write(hashData)
	}

	// Write chunk reference table to both file and hash
	// Each entry is a uint32 index into the TSC file
	refBuf := make([]byte, 4)
	for _, ref := range chunkRefTable {
		binary.BigEndian.PutUint32(refBuf, ref)
		if _, err := bufw.Write(refBuf); err != nil {
			return [32]byte{}, err
		}
		hash.Write(refBuf)
	}

	// Compute file SHA and write footer
	var fileSHA [32]byte
	copy(fileSHA[:], hash.Sum(nil))

	// Footer: our SHA + TSC SHA
	footer := make([]byte, TSMFooterSize)
	copy(footer[0:32], fileSHA[:])
	copy(footer[32:64], tscSHA[:])

	if _, err := bufw.Write(footer); err != nil {
		return [32]byte{}, err
	}

	if err := bufw.Flush(); err != nil {
		return [32]byte{}, err
	}
	if err := f.Close(); err != nil {
		return [32]byte{}, err
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return [32]byte{}, err
	}

	return fileSHA, nil
}

// TSMReader reads a .tsm file
type TSMReader struct {
	Header        TSMHeader
	Entries       []TSMEntry
	ChunkRefTable []uint32 // Chunk reference table: TSC indices
	SHA256        [32]byte // SHA-256 of the file (excluding footer)
	TSCSHA        [32]byte // Expected SHA-256 of the corresponding .tsc file
}

// ReadTSM reads and parses a .tsm file
func ReadTSM(path string) (*TSMReader, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	return ParseTSM(data)
}

// ParseTSM parses TSM data from a byte slice
func ParseTSM(data []byte) (*TSMReader, error) {
	if len(data) < TSMHeaderSize+TSMFooterSize {
		return nil, fmt.Errorf("tsm file too short")
	}

	// Check magic
	if string(data[0:4]) != TSMMagic {
		return nil, fmt.Errorf("invalid TSM magic: %q", data[0:4])
	}

	// Extract footer
	footer := data[len(data)-TSMFooterSize:]
	contentData := data[:len(data)-TSMFooterSize]

	reader := &TSMReader{}
	copy(reader.TSCSHA[:], footer[32:64])

	// Parse header
	copy(reader.Header.Magic[:], data[0:4])
	reader.Header.Flags = binary.BigEndian.Uint32(data[4:8])
	reader.Header.FileCount = binary.BigEndian.Uint64(data[8:16])
	reader.Header.TotalSize = binary.BigEndian.Uint64(data[16:24])
	copy(reader.Header.ChunkFileRef[:], data[24:32])
	reader.Header.CreationTime = int64(binary.BigEndian.Uint64(data[32:40]))
	copy(reader.Header.SourceFSUUID[:], data[40:48])

	// Parse file entries
	offset := TSMHeaderSize
	for uint64(len(reader.Entries)) < reader.Header.FileCount {
		if offset+2 > len(contentData) {
			return nil, fmt.Errorf("unexpected end of tsm data at offset %d", offset)
		}

		entryLen := int(binary.BigEndian.Uint16(data[offset : offset+2]))
		if entryLen < 58 {
			return nil, fmt.Errorf("invalid entry length %d at offset %d", entryLen, offset)
		}
		if offset+entryLen > len(contentData) {
			return nil, fmt.Errorf("entry extends beyond file at offset %d", offset)
		}

		entry, err := decodeEntry(data[offset : offset+entryLen])
		if err != nil {
			return nil, fmt.Errorf("decoding entry at offset %d: %w", offset, err)
		}

		reader.Entries = append(reader.Entries, *entry)
		offset += entryLen
	}

	// Parse chunk reference table (remaining data after entries, before footer)
	chunkRefDataLen := len(contentData) - offset
	if chunkRefDataLen%4 != 0 {
		return nil, fmt.Errorf("chunk ref table size %d not multiple of 4", chunkRefDataLen)
	}

	numRefs := chunkRefDataLen / 4
	reader.ChunkRefTable = make([]uint32, numRefs)
	for i := 0; i < numRefs; i++ {
		reader.ChunkRefTable[i] = binary.BigEndian.Uint32(data[offset : offset+4])
		offset += 4
	}

	// Verify checksum by recomputing with ctime/atime zeroed (same as writer).
	// The file stores real ctime/atime for change detection, but the checksum
	// excludes them for reproducibility.
	h := sha256.New()
	h.Write(data[:TSMHeaderSize]) // Header is the same
	for i := range reader.Entries {
		h.Write(encodeEntryForHash(&reader.Entries[i]))
	}
	// Chunk ref table
	refBuf := make([]byte, 4)
	for _, ref := range reader.ChunkRefTable {
		binary.BigEndian.PutUint32(refBuf, ref)
		h.Write(refBuf)
	}
	computedSHA := h.Sum(nil)

	if !bytes.Equal(computedSHA, footer[0:32]) {
		return nil, fmt.Errorf("tsm checksum mismatch")
	}
	copy(reader.SHA256[:], computedSHA)

	// Populate ChunkRefs on each entry from the chunk ref table
	for i := range reader.Entries {
		e := &reader.Entries[i]
		if e.ChunkCount > 0 {
			start := int(e.ChunkStart)
			end := start + int(e.ChunkCount)
			if start < 0 || end > len(reader.ChunkRefTable) {
				return nil, fmt.Errorf("entry %q: chunk refs [%d:%d] out of range (table has %d entries)",
					e.Path, start, end, len(reader.ChunkRefTable))
			}
			e.ChunkRefs = reader.ChunkRefTable[start:end]
		}
	}

	return reader, nil
}

// decodeEntry decodes a TSMEntry from bytes
func decodeEntry(data []byte) (*TSMEntry, error) {
	if len(data) < 58 {
		return nil, fmt.Errorf("entry too short: %d bytes", len(data))
	}

	offset := 0

	// Entry length (skip, we already know it)
	offset += 2

	// Path length
	pathLen := int(binary.BigEndian.Uint16(data[offset : offset+2]))
	offset += 2

	if offset+pathLen > len(data) {
		return nil, fmt.Errorf("path extends beyond entry")
	}

	entry := &TSMEntry{}
	entry.Path = string(data[offset : offset+pathLen])
	offset += pathLen

	// Type + flags
	typeFlags := binary.BigEndian.Uint16(data[offset : offset+2])
	entry.Type = EntryType(typeFlags & 0x0F)
	entry.Flags = typeFlags & 0xFFF0
	offset += 2

	// Mode
	entry.Mode = binary.BigEndian.Uint32(data[offset : offset+4])
	offset += 4

	// UID
	entry.UID = binary.BigEndian.Uint32(data[offset : offset+4])
	offset += 4

	// GID
	entry.GID = binary.BigEndian.Uint32(data[offset : offset+4])
	offset += 4

	// Size
	entry.Size = binary.BigEndian.Uint64(data[offset : offset+8])
	offset += 8

	// Mtime
	entry.Mtime = int64(binary.BigEndian.Uint64(data[offset : offset+8]))
	offset += 8

	// Ctime
	entry.Ctime = int64(binary.BigEndian.Uint64(data[offset : offset+8]))
	offset += 8

	// Atime
	entry.Atime = int64(binary.BigEndian.Uint64(data[offset : offset+8]))
	offset += 8

	// Chunk start index (into chunk ref table)
	entry.ChunkStart = binary.BigEndian.Uint32(data[offset : offset+4])
	offset += 4

	// Chunk count
	entry.ChunkCount = binary.BigEndian.Uint32(data[offset : offset+4])
	offset += 4

	// Type-specific data
	switch entry.Type {
	case EntryTypeSymlink:
		if offset+2 > len(data) {
			return nil, fmt.Errorf("symlink entry too short")
		}
		targetLen := int(binary.BigEndian.Uint16(data[offset : offset+2]))
		offset += 2
		if offset+targetLen > len(data) {
			return nil, fmt.Errorf("symlink target extends beyond entry")
		}
		entry.LinkTarget = string(data[offset : offset+targetLen])
	case EntryTypeHardlink:
		if offset+4 > len(data) {
			return nil, fmt.Errorf("hardlink entry too short")
		}
		entry.LinkIndex = binary.BigEndian.Uint32(data[offset : offset+4])
	case EntryTypeBlockDev, EntryTypeCharDev:
		if offset+8 > len(data) {
			return nil, fmt.Errorf("device entry too short")
		}
		entry.DevMajor = binary.BigEndian.Uint32(data[offset : offset+4])
		entry.DevMinor = binary.BigEndian.Uint32(data[offset+4 : offset+8])
	}

	return entry, nil
}

// PatchEntryCtimes returns a copy of tsmData with the Ctime field of each
// file entry overwritten with the value from ctimes, keyed by path. Entries
// whose path is not present in ctimes are left unchanged.
//
// This is safe post-hoc surgery: ctime is excluded from the TSM's identity
// hash (see encodeEntryForHash), so patching it does not change the
// manifest's SHA256/snapshot ID. It exists so a downloaded snapshot's local
// .tsm can be updated to record the receiving host's own ctimes (observed
// after extraction) instead of the sending peer's, which are meaningless for
// local change detection on this host.
func PatchEntryCtimes(tsmData []byte, ctimes map[string]int64) ([]byte, error) {
	if len(tsmData) < TSMHeaderSize+TSMFooterSize {
		return nil, fmt.Errorf("tsm file too short")
	}

	fileCount := binary.BigEndian.Uint64(tsmData[8:16])
	contentLen := len(tsmData) - TSMFooterSize

	out := make([]byte, len(tsmData))
	copy(out, tsmData)

	offset := TSMHeaderSize
	for seen := uint64(0); seen < fileCount; seen++ {
		if offset+4 > contentLen {
			return nil, fmt.Errorf("unexpected end of tsm data at offset %d", offset)
		}

		entryLen := int(binary.BigEndian.Uint16(tsmData[offset : offset+2]))
		if entryLen < 58 {
			return nil, fmt.Errorf("invalid entry length %d at offset %d", entryLen, offset)
		}
		if offset+entryLen > contentLen {
			return nil, fmt.Errorf("entry extends beyond file at offset %d", offset)
		}

		pathLen := int(binary.BigEndian.Uint16(tsmData[offset+2 : offset+4]))
		if offset+4+pathLen > contentLen {
			return nil, fmt.Errorf("path extends beyond entry at offset %d", offset)
		}
		path := string(tsmData[offset+4 : offset+4+pathLen])

		if ctime, ok := ctimes[path]; ok {
			// Ctime field offset within the entry: 2 (entry len) + 2 (path
			// len) + pathLen + 2 (type+flags) + 4 (mode) + 4 (uid) +
			// 4 (gid) + 8 (size) + 8 (mtime) = 34 + pathLen.
			ctimeOffset := offset + 34 + pathLen
			if ctimeOffset+8 > contentLen {
				return nil, fmt.Errorf("ctime field extends beyond entry at offset %d", offset)
			}
			binary.BigEndian.PutUint64(out[ctimeOffset:ctimeOffset+8], uint64(ctime))
		}

		offset += entryLen
	}

	return out, nil
}

// LookupPath finds an entry by path using binary search.
// Returns the entry and true if found, or nil and false if not found.
func (r *TSMReader) LookupPath(path string) (*TSMEntry, bool) {
	idx := sort.Search(len(r.Entries), func(i int) bool {
		return r.Entries[i].Path >= path
	})

	if idx < len(r.Entries) && r.Entries[idx].Path == path {
		return &r.Entries[idx], true
	}
	return nil, false
}

// GetFileChunkSHAs returns the SHA-256 hashes for a file entry's chunks,
// looking them up in the provided TSC reader.
func GetFileChunkSHAs(entry *TSMEntry, tsc *TSCReader) [][32]byte {
	if entry.ChunkCount == 0 || len(entry.ChunkRefs) == 0 {
		return nil
	}

	shas := make([][32]byte, len(entry.ChunkRefs))
	for i, tscIdx := range entry.ChunkRefs {
		if int(tscIdx) < len(tsc.Entries) {
			shas[i] = tsc.Entries[tscIdx].SHA256
		}
	}
	return shas
}
