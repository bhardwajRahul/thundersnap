package tsm

import (
	"bytes"
	"io"
	"os"
)

// rollsum implements the bup rolling checksum used for content-defined
// chunking. It is an internal primitive; callers use ChunkFile/ChunkReader.
type rollsum struct {
	s1     uint32
	s2     uint32
	window [BUP_WINDOWSIZE]byte
	wofs   int
}

// reset initializes the rolling checksum to its zero-window starting state.
func (r *rollsum) reset() {
	r.s1 = BUP_WINDOWSIZE * ROLLSUM_CHAR_OFFSET
	r.s2 = BUP_WINDOWSIZE * (BUP_WINDOWSIZE - 1) * ROLLSUM_CHAR_OFFSET
	r.wofs = 0
	for i := range r.window {
		r.window[i] = 0
	}
}

func (r *rollsum) add(drop, add byte) {
	r.s1 += uint32(add) - uint32(drop)
	r.s2 += r.s1 - (BUP_WINDOWSIZE * (uint32(drop) + ROLLSUM_CHAR_OFFSET))
}

// roll advances the window by one byte, dropping the oldest byte.
func (r *rollsum) roll(ch byte) {
	r.add(r.window[r.wofs], ch)
	r.window[r.wofs] = ch
	r.wofs = (r.wofs + 1) % BUP_WINDOWSIZE
}

// digest returns the current rolling-checksum value.
func (r *rollsum) digest() uint32 {
	return (r.s1 << 16) | (r.s2 & 0xffff)
}

// findSplitPoint finds a content-defined split point in buf. It returns
// (offset, bits): offset is the split position (0 if no split found) and bits
// is the number of trailing one-bits in the rollsum at the split, which sets
// the hierarchical chunk level.
//
// A split is declared when the low BUP_BLOBBITS bits of s2 are all ones — that
// is, (s2 & (BUP_BLOBSIZE-1)) equals (^0 & (BUP_BLOBSIZE-1)). Requiring an
// all-ones low pattern makes the boundary depend only on content, so identical
// data always splits at the same places (the determinism the format relies on).
func findSplitPoint(buf []byte) (int, int) {
	var r rollsum
	r.reset()

	for count := 0; count < len(buf); count++ {
		r.roll(buf[count])
		if (r.s2 & (BUP_BLOBSIZE - 1)) == ((^uint32(0)) & (BUP_BLOBSIZE - 1)) {
			// Found a split point
			rsum := r.digest()
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

// ChunkReader splits data from a reader into content-defined chunks using
// SHA-256. It is a thin wrapper over the canonical chunkStream implementation.
func ChunkReader(r io.Reader, fileSize int64, callback ChunkCallback, progress func(int64, int64)) error {
	return chunkStream(r, fileSize, callback, progress)
}

// ChunkData splits a byte slice into content-defined chunks using SHA-256. It
// is a thin wrapper that streams the in-memory buffer through the canonical
// chunkStream implementation, so byte-slice and reader inputs always produce
// identical chunk boundaries and hashes.
func ChunkData(data []byte, callback ChunkCallback) error {
	return chunkStream(bytes.NewReader(data), int64(len(data)), callback, nil)
}

// chunkStream is the single canonical content-defined chunker. Both ChunkData
// (whole-buffer) and ChunkReader/ChunkFile (streaming) delegate here so the
// chunk-boundary logic lives in exactly one place and the on-disk chunk hashes
// can never drift between the two entry points.
//
// It reads through a fixed BLOB_READ_SIZE buffer, scanning for content-defined
// split points with findSplitPoint. When no split is found it carries the
// unconsumed tail ("leftover") into the next read; a run that reaches BLOB_MAX
// without a natural boundary is force-split at BLOB_MAX (level 0) so memory use
// stays bounded. progress, when non-nil, is called after each chunk with the
// running byte total and the caller-supplied fileSize.
func chunkStream(r io.Reader, fileSize int64, callback ChunkCallback, progress func(int64, int64)) error {
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
				ofs, bits := findSplitPoint(data[offset:])

				var chunkSize int
				var level int

				if ofs > 0 {
					chunkSize = ofs
					if chunkSize > BLOB_MAX {
						chunkSize = BLOB_MAX
						level = 0
					} else {
						// Hierarchical level from the number of extra trailing
						// one-bits; bounded by the rollsum width, so the later
						// uint16(level) cast at the callback can never truncate.
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
