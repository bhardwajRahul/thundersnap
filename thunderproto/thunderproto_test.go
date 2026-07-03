// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package thunderproto

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// TestHandshakeHalves exercises each handshake half independently: the server
// accepts a valid CONNECT and replies OK, and the client emits CONNECT and
// accepts an OK reply. Both use the package Port so the two sides cannot drift.
func TestHandshakeHalves(t *testing.T) {
	// Server side: feed a valid CONNECT and capture the reply.
	var serverReply bytes.Buffer
	serverIn := bufio.NewReader(strings.NewReader(fmt.Sprintf("CONNECT %d\n", Port)))
	if err := ReadServerHandshake(&serverReply, serverIn); err != nil {
		t.Fatalf("ReadServerHandshake valid: %v", err)
	}
	wantOK := fmt.Sprintf("OK %d", Port)
	if got := serverReply.String(); !strings.HasPrefix(got, wantOK) {
		t.Errorf("server reply = %q, want %q prefix", got, wantOK)
	}

	// Client side: drive against a stub that replies OK.
	var clientOut bytes.Buffer
	clientIn := bufio.NewReader(strings.NewReader(fmt.Sprintf("OK %d\n", Port)))
	if err := WriteClientHandshake(&clientOut, clientIn); err != nil {
		t.Fatalf("WriteClientHandshake valid: %v", err)
	}
	wantConnect := fmt.Sprintf("CONNECT %d\n", Port)
	if got := clientOut.String(); got != wantConnect {
		t.Errorf("client wrote %q, want %q", got, wantConnect)
	}
}

// TestReadServerHandshakeBadPort confirms the server rejects a CONNECT for the
// wrong port and emits an ERROR reply.
func TestReadServerHandshakeBadPort(t *testing.T) {
	var reply bytes.Buffer
	r := bufio.NewReader(strings.NewReader("CONNECT 9999\n"))
	if err := ReadServerHandshake(&reply, r); err == nil {
		t.Fatal("ReadServerHandshake(wrong port) = nil, want error")
	}
	if !strings.HasPrefix(reply.String(), "ERROR") {
		t.Errorf("reply = %q, want ERROR prefix", reply.String())
	}
}

// TestReadServerHandshakeMalformed confirms a non-CONNECT line is rejected.
func TestReadServerHandshakeMalformed(t *testing.T) {
	var reply bytes.Buffer
	r := bufio.NewReader(strings.NewReader("GARBAGE\n"))
	if err := ReadServerHandshake(&reply, r); err == nil {
		t.Fatal("ReadServerHandshake(malformed) = nil, want error")
	}
	if !strings.HasPrefix(reply.String(), "ERROR") {
		t.Errorf("reply = %q, want ERROR prefix", reply.String())
	}
}

// TestWriteClientHandshakeServerError confirms the client surfaces an error when
// the server replies with anything other than OK.
func TestWriteClientHandshakeServerError(t *testing.T) {
	var out bytes.Buffer
	r := bufio.NewReader(strings.NewReader("ERROR invalid port\n"))
	if err := WriteClientHandshake(&out, r); err == nil {
		t.Fatal("WriteClientHandshake(server error) = nil, want error")
	}
}
