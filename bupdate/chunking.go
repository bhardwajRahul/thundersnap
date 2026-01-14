package bupdate

import (
	"crypto/sha1"
	"fmt"
	"io"
	"os"
)

// Rollsum implements the rolling checksum used for content-defined chunking
type Rollsum struct {
	s1     uint32
	s2     uint32
	window [BUP_WINDOWSIZE]byte
	wofs   int
}

func (r *Rollsum) Init() {
	r.s1 = BUP_WINDOWSIZE * ROLLSUM_CHAR_OFFSET
	r.s2 = BUP_WINDOWSIZE * (BUP_WINDOWSIZE - 1) * ROLLSUM_CHAR_OFFSET
	r.wofs = 0
	for i := range r.window {
		r.window[i] = 0
	}
}

func (r *Rollsum) add(drop, add byte) {
	r.s1 += uint32(add) - uint32(drop)
	r.s2 += r.s1 - (BUP_WINDOWSIZE * (uint32(drop) + ROLLSUM_CHAR_OFFSET))
}

func (r *Rollsum) Roll(ch byte) {
	r.add(r.window[r.wofs], ch)
	r.window[r.wofs] = ch
	r.wofs = (r.wofs + 1) % BUP_WINDOWSIZE
}

func (r *Rollsum) Digest() uint32 {
	return (r.s1 << 16) | (r.s2 & 0xffff)
}

// FindSplitPoint finds a content-defined split point in the buffer
// Returns (offset, bits) where offset is the split position (0 if no split found)
// and bits is the number of matching bits in the rollsum
func FindSplitPoint(buf []byte) (int, int) {
	var r Rollsum
	r.Init()

	for count := 0; count < len(buf); count++ {
		r.Roll(buf[count])
		if (r.s2 & (BUP_BLOBSIZE - 1)) == ((^uint32(0)) & (BUP_BLOBSIZE - 1)) {
			// Found a split point
			rsum := r.Digest()
			bits := BUP_BLOBBITS
			rsum >>= BUP_BLOBBITS
			for (rsum>>1)&1 != 0 {
				bits++
				rsum >>= 1
			}
			return count + 1, bits
		}
	}
	return 0, 0
}

// BlobSHA computes the git blob SHA-1 of data
func BlobSHA(data []byte) [20]byte {
	h := sha1.New()
	// Git blob format: "blob <size>\0<data>"
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	var result [20]byte
	copy(result[:], h.Sum(nil))
	return result
}

// ChunkFile splits a file into content-defined chunks and calls writeEntry for each chunk
func ChunkFile(filename string, writeEntry func(FidxEntry) error, progressCallback func(int64, int64)) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	// Get file size for progress reporting
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	fileSize := stat.Size()

	buf := make([]byte, BLOB_READ_SIZE)
	var leftover []byte
	totalBytes := int64(0)

	for {
		// Prepend any leftover data from previous iteration
		if len(leftover) > 0 {
			copy(buf, leftover)
		}

		n, err := f.Read(buf[len(leftover):])
		if n > 0 {
			data := buf[:len(leftover)+n]

			// Find chunks in this buffer
			offset := 0
			for {
				remaining := len(data) - offset
				ofs, bits := FindSplitPoint(data[offset:])

				var chunkSize int
				var level int

				if ofs > 0 {
					chunkSize = ofs
					if chunkSize > BLOB_MAX {
						chunkSize = BLOB_MAX
						level = 0
					} else {
						// Calculate hierarchical level
						level = (bits - BUP_BLOBBITS) / FANOUT_BITS
					}
				} else {
					// No split point found
					if err == io.EOF {
						// Last chunk - take everything remaining
						chunkSize = len(data) - offset
						level = 0
					} else if remaining >= BLOB_MAX {
						// Force a split at BLOB_MAX to avoid accumulating too much data
						chunkSize = BLOB_MAX
						level = 0
					} else {
						// Need more data, save for next iteration
						break
					}
				}

				if chunkSize > 0 {
					chunk := data[offset : offset+chunkSize]
					sha := BlobSHA(chunk)

					entry := FidxEntry{
						SHA:   sha,
						Size:  uint16(chunkSize),
						Level: uint16(level),
					}

					if err := writeEntry(entry); err != nil {
						return err
					}

					totalBytes += int64(chunkSize)
					offset += chunkSize

					// Call progress callback if provided
					if progressCallback != nil {
						progressCallback(totalBytes, fileSize)
					}
				}

				// If we found a natural split point, continue looking for more
				// If we didn't find one and didn't force a split, we need more data
				if ofs == 0 && chunkSize == 0 {
					break
				}
			}

			// Save any unprocessed data for next iteration
			leftover = make([]byte, len(data)-offset)
			copy(leftover, data[offset:])
		}

		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}

	// Write any final leftover data as the last chunk
	if len(leftover) > 0 {
		sha := BlobSHA(leftover)
		entry := FidxEntry{
			SHA:   sha,
			Size:  uint16(len(leftover)),
			Level: 0,
		}
		if err := writeEntry(entry); err != nil {
			return err
		}
	}

	return nil
}
