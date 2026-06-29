package thunderclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/tailscale/thundersnap/thunderproto"
)

type echoReq struct {
	Name string `json:"name"`
}

type echoResp struct {
	Greeting string `json:"greeting"`
}

// fakeControlServer listens on a unix socket and speaks the same emulated vsock
// handshake the real daemon does, then serves one HTTP request per connection.
// It returns the socket path so tests can dial it via dialUnix.
func fakeControlServer(t *testing.T, handler http.Handler) string {
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
				rec := &connResponseWriter{conn: conn, header: make(http.Header)}
				handler.ServeHTTP(rec, req)
				rec.flush()
			}()
		}
	}()
	return sockPath
}

// connResponseWriter is a minimal ResponseWriter that writes a complete HTTP/1.0
// response (with Content-Length) to the raw connection.
type connResponseWriter struct {
	conn   net.Conn
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
func (w *connResponseWriter) flush() {
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
	resp.Write(w.conn)
}

// clientFor builds an *http.Client whose transport dials the fake server through
// dialUnix. This exercises the same unix-socket handshake path as Dial without
// depending on the /dev/vsock-based inVM() branch (which the test host may have).
func clientFor(sockPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return dialUnix(sockPath)
			},
		},
	}
}

// TestDialUnixHandshake confirms dialUnix completes the CONNECT/OK handshake and
// returns a connection that can carry a subsequent HTTP request.
func TestDialUnixHandshake(t *testing.T) {
	sockPath := fakeControlServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("pong"))
	}))

	conn, err := dialUnix(sockPath)
	if err != nil {
		t.Fatalf("dialUnix: %v", err)
	}
	defer conn.Close()

	// Write a raw HTTP request and read the status line back.
	if _, err := conn.Write([]byte("GET /ping HTTP/1.0\r\nHost: localhost\r\n\r\n")); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestPostJSONRoundTrip drives a JSON round trip over the dialUnix transport,
// confirming the request is marshaled and the response decoded. It mirrors what
// PostJSON does but with a transport pinned to the unix path.
func TestPostJSONRoundTrip(t *testing.T) {
	sockPath := fakeControlServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req echoReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(echoResp{Greeting: "hello " + req.Name})
	}))

	body, _ := json.Marshal(echoReq{Name: "world"})
	resp, err := clientFor(sockPath).Post("http://localhost/echo", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	var out echoResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Greeting != "hello world" {
		t.Errorf("Greeting = %q, want %q", out.Greeting, "hello world")
	}
}

// TestNewHTTPClientNonNil confirms NewHTTPClient returns a client with a
// transport wired (the dial policy itself is environment-dependent).
func TestNewHTTPClientNonNil(t *testing.T) {
	c := NewHTTPClient("/thunder.sock")
	if c == nil || c.Transport == nil {
		t.Fatal("NewHTTPClient returned a client without a transport")
	}
}
