// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package vshdproto

import (
	"bytes"
	"io"
	"testing"
)

// TestFrameRoundTrip writes a sequence of frames and reads them back, asserting
// type and payload survive intact, including a zero-length payload.
func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	cases := []struct {
		typ     uint8
		payload []byte
	}{
		{FrameStdin, []byte("hello")},
		{FrameStdout, []byte("world")},
		{FrameStderr, []byte("err")},
		{FrameWinsize, EncodeWinsize(Winsize{Rows: 40, Cols: 100})},
		{FrameExit, EncodeExit(7)},
		{FrameStdout, nil}, // zero-length frame
	}
	for _, c := range cases {
		if err := WriteFrame(&buf, c.typ, c.payload); err != nil {
			t.Fatalf("WriteFrame(%d): %v", c.typ, err)
		}
	}
	for i, c := range cases {
		typ, payload, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame #%d: %v", i, err)
		}
		if typ != c.typ {
			t.Errorf("frame #%d type = %d, want %d", i, typ, c.typ)
		}
		if !bytes.Equal(payload, c.payload) {
			t.Errorf("frame #%d payload = %q, want %q", i, payload, c.payload)
		}
	}
	if _, _, err := ReadFrame(&buf); err != io.EOF {
		t.Errorf("ReadFrame at boundary = %v, want io.EOF", err)
	}
}

// TestWinsizeCodec round-trips a winsize through encode/decode and rejects a
// wrong-length payload.
func TestWinsizeCodec(t *testing.T) {
	ws := Winsize{Rows: 24, Cols: 80, X: 640, Y: 480}
	got, err := DecodeWinsize(EncodeWinsize(ws))
	if err != nil {
		t.Fatalf("DecodeWinsize: %v", err)
	}
	if got != ws {
		t.Errorf("winsize round-trip = %+v, want %+v", got, ws)
	}
	if _, err := DecodeWinsize([]byte{1, 2, 3}); err == nil {
		t.Error("DecodeWinsize(3 bytes) = nil error, want error")
	}
}

// TestExitCodec round-trips an exit code (including negative) and rejects a
// wrong-length payload.
func TestExitCodec(t *testing.T) {
	for _, code := range []int32{0, 1, 130, -1} {
		got, err := DecodeExit(EncodeExit(code))
		if err != nil {
			t.Fatalf("DecodeExit(%d): %v", code, err)
		}
		if got != code {
			t.Errorf("exit round-trip = %d, want %d", got, code)
		}
	}
	if _, err := DecodeExit([]byte{1, 2}); err == nil {
		t.Error("DecodeExit(2 bytes) = nil error, want error")
	}
}

// TestReadFrameTruncated confirms a frame whose payload is cut short returns
// ErrUnexpectedEOF rather than a clean EOF.
func TestReadFrameTruncated(t *testing.T) {
	var buf bytes.Buffer
	WriteFrame(&buf, FrameStdout, []byte("0123456789"))
	truncated := buf.Bytes()[:headerLen+4] // header + 4 of 10 payload bytes
	_, _, err := ReadFrame(bytes.NewReader(truncated))
	if err != io.ErrUnexpectedEOF {
		t.Errorf("ReadFrame(truncated) = %v, want io.ErrUnexpectedEOF", err)
	}
}

// TestReadFrameReassembly verifies a frame split across multiple short reads is
// reassembled correctly (io.ReadFull semantics).
func TestReadFrameReassembly(t *testing.T) {
	var buf bytes.Buffer
	payload := bytes.Repeat([]byte("x"), 1000)
	WriteFrame(&buf, FrameStdin, payload)
	typ, got, err := ReadFrame(iotest1byte{bytes.NewReader(buf.Bytes())})
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if typ != FrameStdin || !bytes.Equal(got, payload) {
		t.Errorf("reassembled frame mismatch: typ=%d len=%d", typ, len(got))
	}
}

// iotest1byte returns at most one byte per Read to exercise reassembly.
type iotest1byte struct{ r io.Reader }

func (o iotest1byte) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}
