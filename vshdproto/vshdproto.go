// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// Package vshdproto defines the TLV (type-length-value) framing used on a vshd
// session connection after the null-delimited request header.
//
// The request header is written/parsed by the daemon and vshd directly (it is a
// simple null-delimited sequence). Everything after the header is a stream of
// TLV frames in BOTH directions:
//
//	frame = type:uint8 | length:uint32 (big-endian) | payload[length]
//
// Host -> guest frames carry terminal input (FrameStdin) and window-size changes
// (FrameWinsize). Guest -> host frames carry program output (FrameStdout,
// FrameStderr) and the final exit status (FrameExit). A PTY session is signalled
// by the host sending a leading FrameWinsize before any FrameStdin; the guest
// then allocates a pty sized to that winsize and applies later FrameWinsize
// frames to it. A non-PTY session simply never sends a FrameWinsize.
package vshdproto

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Frame types. Values are stable wire constants; do not renumber.
const (
	FrameStdin   uint8 = 1 // host->guest: raw terminal/stdin bytes
	FrameStdout  uint8 = 2 // guest->host: program stdout
	FrameStderr  uint8 = 3 // guest->host: program stderr
	FrameWinsize uint8 = 4 // host->guest: Winsize (8 bytes)
	FrameExit    uint8 = 5 // guest->host: int32 exit code (4 bytes)
)

// maxFrameLen bounds an individual frame payload to guard against a corrupt or
// hostile length prefix causing an unbounded allocation. 16 MiB is far above any
// legitimate terminal write or stdin chunk.
const maxFrameLen = 16 << 20

// headerLen is the size of the fixed frame header: 1 type byte + 4 length bytes.
const headerLen = 5

// Winsize is a terminal window size, mirroring the fields of pty.Winsize.
type Winsize struct {
	Rows uint16
	Cols uint16
	X    uint16 // x pixels (usually 0)
	Y    uint16 // y pixels (usually 0)
}

// WriteFrame writes a single TLV frame. A nil/empty payload writes a zero-length
// frame (valid; e.g. an EOF marker if a caller wants one).
func WriteFrame(w io.Writer, typ uint8, payload []byte) error {
	if len(payload) > maxFrameLen {
		return fmt.Errorf("vshdproto: payload too large: %d > %d", len(payload), maxFrameLen)
	}
	var hdr [headerLen]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// ReadFrame reads a single TLV frame. It returns io.EOF only when the reader is
// at a clean frame boundary (no bytes of a new frame had arrived); a truncated
// frame returns io.ErrUnexpectedEOF.
func ReadFrame(r io.Reader) (typ uint8, payload []byte, err error) {
	var hdr [headerLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err // io.EOF here means clean close at a boundary
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrameLen {
		return 0, nil, fmt.Errorf("vshdproto: frame length %d exceeds max %d", n, maxFrameLen)
	}
	if n == 0 {
		return hdr[0], nil, nil
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

// EncodeWinsize serialises a Winsize into the 8-byte FrameWinsize payload.
func EncodeWinsize(ws Winsize) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint16(b[0:], ws.Rows)
	binary.BigEndian.PutUint16(b[2:], ws.Cols)
	binary.BigEndian.PutUint16(b[4:], ws.X)
	binary.BigEndian.PutUint16(b[6:], ws.Y)
	return b
}

// DecodeWinsize parses an 8-byte FrameWinsize payload.
func DecodeWinsize(payload []byte) (Winsize, error) {
	if len(payload) != 8 {
		return Winsize{}, fmt.Errorf("vshdproto: winsize payload is %d bytes, want 8", len(payload))
	}
	return Winsize{
		Rows: binary.BigEndian.Uint16(payload[0:]),
		Cols: binary.BigEndian.Uint16(payload[2:]),
		X:    binary.BigEndian.Uint16(payload[4:]),
		Y:    binary.BigEndian.Uint16(payload[6:]),
	}, nil
}

// EncodeExit serialises an exit code into the 4-byte FrameExit payload.
func EncodeExit(code int32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(code))
	return b
}

// DecodeExit parses a 4-byte FrameExit payload.
func DecodeExit(payload []byte) (int32, error) {
	if len(payload) != 4 {
		return 0, fmt.Errorf("vshdproto: exit payload is %d bytes, want 4", len(payload))
	}
	return int32(binary.BigEndian.Uint32(payload)), nil
}
