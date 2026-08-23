// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/tailscale/thundersnap/thunderproto"
	"github.com/tailscale/thundersnap/version"
)

// fakeControlServer starts a unix-socket server that speaks the emulated vsock
// CONNECT/OK handshake (as thundersnapd's control socket does) and serves one
// HTTP request per accepted connection through h. It returns the socket path so
// a test can point runVersion/getServerVersion (which use thunderclient.Dial,
// i.e. the same unix-socket + handshake path) straight at it.
//
// This mirrors thunderclient_test.go's fakeControlServer; it lives here because
// that one is in package thunderclient and its dialUnix is unexported.
func fakeControlServer(t *testing.T, h http.Handler) string {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "thunder.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				if err := thunderproto.ReadServerHandshake(conn, r); err != nil {
					return
				}
				req, err := http.ReadRequest(r)
				if err != nil {
					return
				}
				rec := &connResponseWriter{header: make(http.Header)}
				h.ServeHTTP(rec, req)
				rec.flush(conn)
			}()
		}
	}()
	return sockPath
}

// connResponseWriter is a minimal http.ResponseWriter that buffers the response
// and writes a complete HTTP/1.0 reply (with Content-Length) to the raw conn.
type connResponseWriter struct {
	header http.Header
	body   []byte
	code   int
}

func (w *connResponseWriter) Header() http.Header { return w.header }
func (w *connResponseWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}
func (w *connResponseWriter) Write(p []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	w.body = append(w.body, p...)
	return len(p), nil
}
func (w *connResponseWriter) flush(conn net.Conn) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	resp := &http.Response{
		StatusCode:    w.code,
		ProtoMajor:    1,
		ProtoMinor:    0,
		Header:        w.header,
		ContentLength: int64(len(w.body)),
		Body:          io.NopCloser(bytes.NewReader(w.body)),
	}
	resp.Write(conn)
}

// versionHandler serves POST /version replying with the given version string.
func versionHandler(serverVersion string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ControlResponse{Status: "ok", Version: serverVersion})
	})
}

// TestRunVersionMatch: when the server reports the same version the client has,
// runVersion returns matched=true and no error, and the server version equals
// the client's.
func TestRunVersionMatch(t *testing.T) {
	sock := fakeControlServer(t, versionHandler(version.String()))
	res := runVersion(sock)
	if res.err != nil {
		t.Fatalf("runVersion err = %v, want nil", res.err)
	}
	if !res.matched {
		t.Fatalf("matched = false; client=%q server=%q", res.clientVer, res.serverVer)
	}
	if res.serverVer != res.clientVer {
		t.Errorf("serverVer = %q, want %q", res.serverVer, res.clientVer)
	}
}

// TestRunVersionMismatch: when the server reports a different version,
// runVersion returns matched=false with the two distinct versions filled in.
func TestRunVersionMismatch(t *testing.T) {
	sock := fakeControlServer(t, versionHandler("v9.9.9-mismatched"))
	res := runVersion(sock)
	if res.err != nil {
		t.Fatalf("runVersion err = %v, want nil", res.err)
	}
	if res.matched {
		t.Fatalf("matched = true, want false (client=%q server=%q)", res.clientVer, res.serverVer)
	}
	if res.serverVer != "v9.9.9-mismatched" {
		t.Errorf("serverVer = %q, want v9.9.9-mismatched", res.serverVer)
	}
}

// TestRunVersionUnreachable: when the socket does not exist, runVersion surfaces a
// transport error (and does not report a match).
func TestRunVersionUnreachable(t *testing.T) {
	res := runVersion(filepath.Join(t.TempDir(), "no-such.sock"))
	if res.err == nil {
		t.Fatal("err = nil, want a connection error for an unreachable daemon")
	}
	if res.matched {
		t.Fatal("matched = true, want false when the server is unreachable")
	}
}
