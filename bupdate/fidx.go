package bupdate

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// LoadFidx reads and parses a fidx or mfidx file
func LoadFidx(path string) (*Fidx, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	if len(data) < 8+20 {
		return nil, fmt.Errorf("file too short")
	}

	// Check header
	if string(data[0:4]) != "FIDX" {
		return nil, fmt.Errorf("invalid FIDX magic")
	}

	version := binary.BigEndian.Uint32(data[4:8])
	if version != FIDX_VERSION {
		return nil, fmt.Errorf("unsupported version: %d", version)
	}

	// Extract footer (last 20 bytes)
	footerSHA := data[len(data)-20:]
	data = data[:len(data)-20]

	// Verify checksum
	h := sha1.New()
	h.Write(data)
	computedSHA := h.Sum(nil)
	if !bytes.Equal(computedSHA, footerSHA) {
		return nil, fmt.Errorf("fidx checksum mismatch")
	}

	// Parse entries (skip 8-byte header)
	entryData := data[8:]

	// Detect if this is an mfidx
	isMFIDX := false
	if len(entryData) >= 24 {
		allZero := true
		for i := 0; i < 20; i++ {
			if entryData[i] != 0 {
				allZero = false
				break
			}
		}
		if allZero {
			isMFIDX = true
		}
	}

	if isMFIDX {
		return parseMFIDX(path, entryData, computedSHA)
	}

	// Regular single-file fidx
	if len(entryData)%24 != 0 {
		return nil, fmt.Errorf("invalid entry data length")
	}

	numEntries := len(entryData) / 24
	entries := make([]FidxEntry, numEntries)
	var fileSize int64

	for i := 0; i < numEntries; i++ {
		offset := i * 24
		var ent FidxEntry
		copy(ent.SHA[:], entryData[offset:offset+20])
		ent.Size = binary.BigEndian.Uint16(entryData[offset+20 : offset+22])
		ent.Level = binary.BigEndian.Uint16(entryData[offset+22 : offset+24])
		entries[i] = ent
		fileSize += int64(ent.Size)
	}

	fidx := &Fidx{
		Filename: path,
		Entries:  entries,
		FileSize: fileSize,
		IsMFIDX:  false,
	}
	copy(fidx.FileSHA[:], computedSHA)

	return fidx, nil
}

func parseMFIDX(path string, entryData []byte, computedSHA []byte) (*Fidx, error) {
	var files []FileEntry
	offset := 0

	for offset < len(entryData) {
		if offset+24 > len(entryData) {
			return nil, fmt.Errorf("unexpected end of mfidx data")
		}

		// Check for file separator
		isFileSeparator := true
		for i := 0; i < 20; i++ {
			if entryData[offset+i] != 0 {
				isFileSeparator = false
				break
			}
		}

		if !isFileSeparator {
			return nil, fmt.Errorf("expected file separator at offset %d", offset)
		}

		// Read metadata length
		metadataLen := binary.BigEndian.Uint16(entryData[offset+22 : offset+24])
		offset += 24

		if offset+int(metadataLen) > len(entryData) {
			return nil, fmt.Errorf("metadata extends beyond file")
		}

		// Parse metadata
		metadata := entryData[offset : offset+int(metadataLen)]

		// Read null-terminated filename
		filenameEnd := bytes.IndexByte(metadata, 0)
		if filenameEnd == -1 {
			return nil, fmt.Errorf("filename not null-terminated")
		}
		filename := string(metadata[:filenameEnd])
		metadata = metadata[filenameEnd+1:]

		if len(metadata) < 16 {
			return nil, fmt.Errorf("insufficient metadata")
		}

		fileSize := binary.BigEndian.Uint64(metadata[0:8])
		mtime := binary.BigEndian.Uint64(metadata[8:16])

		offset += int(metadataLen)

		// Read chunk entries for this file
		var entries []FidxEntry

		for offset < len(entryData) {
			if offset+24 > len(entryData) {
				break
			}

			// Check if next entry is a file separator
			isNextSeparator := true
			for i := 0; i < 20; i++ {
				if entryData[offset+i] != 0 {
					isNextSeparator = false
					break
				}
			}

			if isNextSeparator {
				break
			}

			var ent FidxEntry
			copy(ent.SHA[:], entryData[offset:offset+20])
			ent.Size = binary.BigEndian.Uint16(entryData[offset+20 : offset+22])
			ent.Level = binary.BigEndian.Uint16(entryData[offset+22 : offset+24])
			entries = append(entries, ent)
			offset += 24
		}

		files = append(files, FileEntry{
			Filename: filename,
			FileSize: fileSize,
			Mtime:    mtime,
			Entries:  entries,
		})
	}

	fidx := &Fidx{
		Filename: path,
		IsMFIDX:  true,
		Files:    files,
	}
	copy(fidx.FileSHA[:], computedSHA)

	return fidx, nil
}

// WriteFileSeparator writes a file separator entry followed by file metadata
func WriteFileSeparator(w io.Writer, sep FileSeparator) (int, error) {
	separatorEntry := make([]byte, 24)
	metadataSize := len(sep.Filename) + 1 + 8 + 8
	paddingSize := (8 - (metadataSize % 8)) % 8
	totalMetadataSize := metadataSize + paddingSize

	binary.BigEndian.PutUint16(separatorEntry[22:24], uint16(totalMetadataSize))

	if _, err := w.Write(separatorEntry); err != nil {
		return 24, err
	}

	if _, err := w.Write([]byte(sep.Filename)); err != nil {
		return 24, err
	}
	if _, err := w.Write([]byte{0}); err != nil {
		return 24 + len(sep.Filename), err
	}

	sizeBuf := make([]byte, 8)
	binary.BigEndian.PutUint64(sizeBuf, sep.FileSize)
	if _, err := w.Write(sizeBuf); err != nil {
		return 24 + len(sep.Filename) + 1, err
	}

	mtimeBuf := make([]byte, 8)
	binary.BigEndian.PutUint64(mtimeBuf, sep.Mtime)
	if _, err := w.Write(mtimeBuf); err != nil {
		return 24 + len(sep.Filename) + 1 + 8, err
	}

	if paddingSize > 0 {
		padding := make([]byte, paddingSize)
		if _, err := w.Write(padding); err != nil {
			return 24 + len(sep.Filename) + 1 + 8 + 8, err
		}
	}

	return 24 + totalMetadataSize, nil
}
