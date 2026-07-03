// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package thundersnap

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestMonitorEventsPanic verifies that a guest "panic" event closes the
// panicked channel and that monitorEvents returns.
func TestMonitorEventsPanic(t *testing.T) {
	s := &VMSession{panicked: make(chan struct{})}

	// Pretty-printed JSON, as cloud-hypervisor emits it, with an unrelated
	// event first to ensure the loop continues past non-panic events.
	stream := `{
  "source": "vmm",
  "event": "starting"
}
{
  "source": "guest",
  "event": "panic"
}`

	done := make(chan struct{})
	go func() {
		s.monitorEvents(strings.NewReader(stream))
		close(done)
	}()

	select {
	case <-s.panicked:
	case <-time.After(2 * time.Second):
		t.Fatal("panicked channel not closed after guest panic event")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitorEvents did not return after panic")
	}
}

// TestMonitorEventsEOF verifies that a clean EOF (no panic) returns without
// closing the panicked channel.
func TestMonitorEventsEOF(t *testing.T) {
	s := &VMSession{panicked: make(chan struct{})}

	stream := `{"source":"vmm","event":"booting"}
{"source":"vmm","event":"running"}`

	done := make(chan struct{})
	go func() {
		s.monitorEvents(strings.NewReader(stream))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitorEvents did not return at EOF")
	}
	select {
	case <-s.panicked:
		t.Fatal("panicked channel closed without a panic event")
	default:
	}
}

// TestMonitorEventsMalformed verifies that malformed JSON terminates monitoring
// (logs+returns) without panicking and without closing panicked.
func TestMonitorEventsMalformed(t *testing.T) {
	s := &VMSession{panicked: make(chan struct{})}

	done := make(chan struct{})
	go func() {
		s.monitorEvents(strings.NewReader("{not valid json"))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitorEvents did not return on malformed input")
	}
	select {
	case <-s.panicked:
		t.Fatal("panicked channel closed on malformed input")
	default:
	}
}

// parsedResponse holds an already-drained HTTP response so callers don't have
// to read the body against a live (synchronous) net.Pipe.
type parsedResponse struct {
	StatusCode int
	Header     http.Header
	Body       string
}

// readResponse runs finish() against one end of a net.Pipe while reading and
// fully draining the serialized HTTP response from the other end. Because
// net.Pipe is unbuffered, finish()'s body write only completes once the reader
// has consumed the body, so we must read the body here (not after returning).
func readResponse(t *testing.T, setup func(w *vsockResponseWriter)) parsedResponse {
	t.Helper()
	client, server := net.Pipe()

	w := &vsockResponseWriter{conn: server, headers: make(http.Header)}
	setup(w)

	errCh := make(chan error, 1)
	go func() {
		err := w.finish()
		server.Close()
		errCh <- err
	}()

	resp, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: "GET"})
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if ferr := <-errCh; ferr != nil {
		t.Fatalf("finish: %v", ferr)
	}
	client.Close()
	return parsedResponse{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       string(body),
	}
}

// TestVsockResponseWriterDefaultStatus verifies that when WriteHeader is never
// called, finish defaults the status to 200.
func TestVsockResponseWriterDefaultStatus(t *testing.T) {
	resp := readResponse(t, func(w *vsockResponseWriter) {
		w.Write([]byte("hello"))
	})

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if resp.Body != "hello" {
		t.Errorf("body = %q, want %q", resp.Body, "hello")
	}
	if got := resp.Header.Get("Content-Length"); got != "5" {
		t.Errorf("Content-Length = %q, want 5", got)
	}
}

// TestVsockResponseWriterHeadersAndStatus verifies custom status, custom
// headers, and accumulation across multiple Write calls.
func TestVsockResponseWriterHeadersAndStatus(t *testing.T) {
	resp := readResponse(t, func(w *vsockResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTeapot)
		w.Write([]byte(`{"a":`))
		w.Write([]byte(`1}`))
	})

	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d, want 418", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if resp.Body != `{"a":1}` {
		t.Errorf("body = %q, want %q", resp.Body, `{"a":1}`)
	}
	if got := resp.Header.Get("Content-Length"); got != "7" {
		t.Errorf("Content-Length = %q, want 7", got)
	}
}

// TestVsockResponseWriterEmptyBody verifies a zero-length body still produces a
// valid response with Content-Length: 0.
func TestVsockResponseWriterEmptyBody(t *testing.T) {
	resp := readResponse(t, func(w *vsockResponseWriter) {
		w.WriteHeader(http.StatusNoContent)
	})

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
	if len(resp.Body) != 0 {
		t.Errorf("body = %q, want empty", resp.Body)
	}
	if got := resp.Header.Get("Content-Length"); got != "0" {
		t.Errorf("Content-Length = %q, want 0", got)
	}
}

// TestWaitReturnsOnDone verifies Wait unblocks (and returns nil) once done is
// closed.
func TestWaitReturnsOnDone(t *testing.T) {
	s := &VMSession{done: make(chan struct{})}
	go func() {
		time.Sleep(10 * time.Millisecond)
		close(s.done)
	}()

	got := make(chan error, 1)
	go func() { got <- s.Wait() }()

	select {
	case err := <-got:
		if err != nil {
			t.Errorf("Wait() = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after done closed")
	}
}
