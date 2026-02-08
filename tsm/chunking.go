package tsm

import (
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

// ChunkCallback is called for each chunk during file chunking
type ChunkCallback func(sha [32]byte, size uint32, level uint16) error

// ChunkFile splits a file into content-defined chunks using SHA-256
func ChunkFile(filename string, callback ChunkCallback, progress func(int64, int64)) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return err
	}
	fileSize := stat.Size()

	return ChunkReader(f, fileSize, callback, progress)
}

// ChunkReader splits data from a reader into content-defined chunks using SHA-256
func ChunkReader(r io.Reader, fileSize int64, callback ChunkCallback, progress func(int64, int64)) error {
	buf := make([]byte, BLOB_READ_SIZE)
	var leftover []byte
	totalBytes := int64(0)

	for {
		// Prepend any leftover data from previous iteration
		if len(leftover) > 0 {
			copy(buf, leftover)
		}

		n, err := r.Read(buf[len(leftover):])
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
					sha := BlobSHA256(chunk)

					if err := callback(sha, uint32(chunkSize), uint16(level)); err != nil {
						return err
					}

					totalBytes += int64(chunkSize)
					offset += chunkSize

					if progress != nil {
						progress(totalBytes, fileSize)
					}
				}

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
		sha := BlobSHA256(leftover)
		if err := callback(sha, uint32(len(leftover)), 0); err != nil {
			return err
		}
	}

	return nil
}

// ChunkData splits a byte slice into content-defined chunks using SHA-256
func ChunkData(data []byte, callback ChunkCallback) error {
	offset := 0
	for offset < len(data) {
		remaining := len(data) - offset
		ofs, bits := FindSplitPoint(data[offset:])

		var chunkSize int
		var level int

		if ofs > 0 && ofs <= remaining {
			chunkSize = ofs
			if chunkSize > BLOB_MAX {
				chunkSize = BLOB_MAX
				level = 0
			} else {
				level = (bits - BUP_BLOBBITS) / FANOUT_BITS
			}
		} else {
			// No split point found - take remaining or BLOB_MAX
			if remaining > BLOB_MAX {
				chunkSize = BLOB_MAX
				level = 0
			} else {
				chunkSize = remaining
				level = 0
			}
		}

		if chunkSize > 0 {
			chunk := data[offset : offset+chunkSize]
			sha := BlobSHA256(chunk)

			if err := callback(sha, uint32(chunkSize), uint16(level)); err != nil {
				return err
			}

			offset += chunkSize
		}
	}
	return nil
}
