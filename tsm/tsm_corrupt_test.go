// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package tsm

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildValidTSM writes a small valid TSM (and the TSC it references) and
// returns the raw .tsm bytes for corruption tests.
func buildValidTSM(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	tscPath := filepath.Join(dir, "x.tsc")
	tsmPath := filepath.Join(dir, "x.tsm")

	tw := NewTSCWriter()
	tw.AddChunk(sha256.Sum256([]byte("c0")), 10, 0, 0)
	tw.AddChunk(sha256.Sum256([]byte("c1")), 20, 0, 0)
	tscSHA, _, err := tw.Write(tscPath)
	if err != nil {
		t.Fatalf("tsc write: %v", err)
	}

	mw := NewTSMWriter()
	mw.AddEntry(TSMEntry{Path: "dir", Type: EntryTypeDir, Mode: 0755})
	mw.AddEntry(TSMEntry{
		Path:       "dir/file",
		Type:       EntryTypeFile,
		Mode:       0644,
		Size:       30,
		ChunkRefs:  []uint32{0, 1},
		ChunkCount: 2,
	})
	if _, err := mw.Write(tsmPath, tscSHA, nil); err != nil {
		t.Fatalf("tsm write: %v", err)
	}
	data, err := os.ReadFile(tsmPath)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func buildValidTSC(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	tscPath := filepath.Join(dir, "x.tsc")
	tw := NewTSCWriter()
	tw.AddChunk(sha256.Sum256([]byte("c0")), 10, 0, 0)
	tw.AddChunk(sha256.Sum256([]byte("c1")), 20, 0, 0)
	if _, _, err := tw.Write(tscPath); err != nil {
		t.Fatalf("tsc write: %v", err)
	}
	data, err := os.ReadFile(tscPath)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseTSMValid(t *testing.T) {
	if _, err := ParseTSM(buildValidTSM(t)); err != nil {
		t.Fatalf("ParseTSM(valid) = %v, want nil", err)
	}
}

func TestParseTSMCorrupt(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func([]byte) []byte
		wantSub string
	}{
		{
			name:    "too short",
			mutate:  func(b []byte) []byte { return b[:TSMHeaderSize+TSMFooterSize-1] },
			wantSub: "too short",
		},
		{
			name:    "bad magic",
			mutate:  func(b []byte) []byte { b = clone(b); b[0] = 'X'; return b },
			wantSub: "magic",
		},
		{
			name: "checksum mismatch",
			mutate: func(b []byte) []byte {
				b = clone(b)
				// Flip a byte in the footer's stored SHA (first 32 footer bytes).
				b[len(b)-TSMFooterSize] ^= 0xff
				return b
			},
			wantSub: "checksum mismatch",
		},
		{
			name: "truncated entries",
			mutate: func(b []byte) []byte {
				// Drop the footer and a chunk of the body so FileCount entries
				// can't all be read.
				return b[:TSMHeaderSize+4]
			},
			wantSub: "", // any error is acceptable
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseTSM(tt.mutate(buildValidTSM(t)))
			if err == nil {
				t.Fatalf("ParseTSM(%s) = nil, want error", tt.name)
			}
			if tt.wantSub != "" && !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("ParseTSM(%s) error = %q, want substring %q", tt.name, err, tt.wantSub)
			}
		})
	}
}

func TestParseTSCValid(t *testing.T) {
	if _, err := ParseTSC(buildValidTSC(t)); err != nil {
		t.Fatalf("ParseTSC(valid) = %v, want nil", err)
	}
}

func TestParseTSCCorrupt(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func([]byte) []byte
		wantSub string
	}{
		{
			name:    "too short",
			mutate:  func(b []byte) []byte { return b[:TSCHeaderSize+TSCFooterSize-1] },
			wantSub: "too short",
		},
		{
			name:    "bad magic",
			mutate:  func(b []byte) []byte { b = clone(b); b[1] = 'Z'; return b },
			wantSub: "magic",
		},
		{
			name: "checksum mismatch",
			mutate: func(b []byte) []byte {
				b = clone(b)
				b[len(b)-1] ^= 0xff // flip a byte in the footer SHA
				return b
			},
			wantSub: "checksum mismatch",
		},
		{
			name: "chunk count mismatch",
			mutate: func(b []byte) []byte {
				b = clone(b)
				// Bump the header ChunkCount (bytes 8..16, big-endian) so it no
				// longer matches the data length, then re-checksum so we reach
				// the count check rather than failing the checksum first.
				b[15] = 99
				rechecksumTSC(b)
				return b
			},
			wantSub: "chunk count mismatch",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseTSC(tt.mutate(buildValidTSC(t)))
			if err == nil {
				t.Fatalf("ParseTSC(%s) = nil, want error", tt.name)
			}
			if tt.wantSub != "" && !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("ParseTSC(%s) error = %q, want substring %q", tt.name, err, tt.wantSub)
			}
		})
	}
}

func clone(b []byte) []byte {
	c := make([]byte, len(b))
	copy(c, b)
	return c
}

// rechecksumTSC recomputes the trailing 32-byte SHA over everything before the
// footer, so a header mutation reaches the post-checksum validation logic.
func rechecksumTSC(b []byte) {
	content := b[:len(b)-TSCFooterSize]
	sum := sha256.Sum256(content)
	copy(b[len(b)-TSCFooterSize:], sum[:])
}

// TestChunkDataReaderParity asserts that ChunkData (whole-buffer) and
// ChunkReader (streaming) produce identical chunk boundaries, hashes, AND
// hierarchical levels for the same input, across sizes that span buffer
// boundaries. Both now delegate to the canonical chunkStream, so this is a
// guard against any future divergence reintroducing two real implementations.
// Comparing level (not just sha/size) closes the gap where the old whole-buffer
// path could have computed a different level than the streaming path.
func TestChunkDataReaderParity(t *testing.T) {
	sizes := []int{0, 1, 100, BLOB_MAX - 1, BLOB_MAX, BLOB_MAX + 1, BLOB_MAX*3 + 777, BLOB_READ_SIZE + 4096}
	for _, n := range sizes {
		data := make([]byte, n)
		// Pseudo-random but deterministic content so split points actually occur.
		for i := range data {
			data[i] = byte((i*1103515245 + 12345) >> 7)
		}

		type chunk struct {
			sha   [32]byte
			size  uint32
			level uint16
		}
		var fromData, fromReader []chunk
		if err := ChunkData(data, func(sha [32]byte, size uint32, level uint16) error {
			fromData = append(fromData, chunk{sha, size, level})
			return nil
		}); err != nil {
			t.Fatalf("ChunkData(n=%d): %v", n, err)
		}
		if err := ChunkReader(bytes.NewReader(data), int64(n), func(sha [32]byte, size uint32, level uint16) error {
			fromReader = append(fromReader, chunk{sha, size, level})
			return nil
		}, nil); err != nil {
			t.Fatalf("ChunkReader(n=%d): %v", n, err)
		}

		if len(fromData) != len(fromReader) {
			t.Errorf("n=%d: chunk count ChunkData=%d ChunkReader=%d", n, len(fromData), len(fromReader))
			continue
		}
		var totalData, totalReader uint32
		for i := range fromData {
			if fromData[i] != fromReader[i] {
				t.Errorf("n=%d chunk %d: ChunkData=%x/%d/L%d ChunkReader=%x/%d/L%d", n, i,
					fromData[i].sha[:4], fromData[i].size, fromData[i].level,
					fromReader[i].sha[:4], fromReader[i].size, fromReader[i].level)
			}
			totalData += fromData[i].size
			totalReader += fromReader[i].size
		}
		if int(totalData) != n || int(totalReader) != n {
			t.Errorf("n=%d: total bytes ChunkData=%d ChunkReader=%d", n, totalData, totalReader)
		}
	}
}
