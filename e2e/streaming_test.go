// Package e2e contains end-to-end tests for thundersnap streaming protocol.
package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestStreamingProgressBasic verifies that NDJSON progress messages arrive
// incrementally during a long operation, not all buffered until the end.
//
// This test:
// 1. Starts a test server with a /stream-test endpoint that sends multiple
//    progress messages with controlled timing
// 2. Connects a client and reads responses line-by-line
// 3. Verifies that messages are received incrementally by checking that
//    the first message arrives before the last message is sent
func TestStreamingProgressBasic(t *testing.T) {
	env := newTestEnv(t)

	// Start the test control server with streaming endpoint
	sockPath := filepath.Join(env.root, "stream.sock")
	ctrl := startStreamingTestServer(t, env, sockPath)
	defer ctrl.Close()

	// Connect and read streaming response
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send vsock handshake
	if _, err := conn.Write([]byte("CONNECT 5223\n")); err != nil {
		t.Fatalf("handshake write: %v", err)
	}

	// Read OK response
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("handshake read: %v", err)
	}
	if !strings.HasPrefix(string(buf[:n]), "OK") {
		t.Fatalf("handshake failed: %s", string(buf[:n]))
	}

	// Send HTTP request for streaming endpoint
	req := "GET /stream-test HTTP/1.1\r\n" +
		"Host: localhost\r\n" +
		"\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// Read HTTP response headers
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()

	// Verify content type indicates streaming
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "ndjson") && !strings.Contains(contentType, "json") {
		t.Logf("Note: Content-Type is %q (expected application/x-ndjson)", contentType)
	}

	// Read NDJSON lines with timestamps to verify incremental delivery
	type receivedMessage struct {
		event    streamTestEvent
		received time.Time
	}
	var messages []receivedMessage

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var event streamTestEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Logf("Warning: failed to parse line %q: %v", line, err)
			continue
		}

		messages = append(messages, receivedMessage{
			event:    event,
			received: time.Now(),
		})
		t.Logf("Received message %d: type=%s message=%q", len(messages), event.Type, event.Message)
	}

	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}

	// Verify we received multiple progress messages plus a result
	if len(messages) < 3 {
		t.Fatalf("expected at least 3 messages (progress + result), got %d", len(messages))
	}

	// Count progress vs result messages
	var progressCount, resultCount int
	for _, m := range messages {
		switch m.event.Type {
		case "progress":
			progressCount++
		case "result":
			resultCount++
		}
	}

	if progressCount < 2 {
		t.Errorf("expected at least 2 progress messages, got %d", progressCount)
	}
	if resultCount != 1 {
		t.Errorf("expected exactly 1 result message, got %d", resultCount)
	}

	// Verify the last message is a result with status ok
	lastMsg := messages[len(messages)-1]
	if lastMsg.event.Type != "result" {
		t.Errorf("last message should be result, got %q", lastMsg.event.Type)
	}
	if lastMsg.event.Status != "ok" {
		t.Errorf("result status should be ok, got %q", lastMsg.event.Status)
	}

	// Key test: verify messages arrived incrementally
	// The server sends messages with small delays between them.
	// If streaming works, the time between receiving the first and last message
	// should be at least as long as the server's total delay.
	// If buffered, all messages would arrive at nearly the same time.
	if len(messages) >= 2 {
		firstReceived := messages[0].received
		lastReceived := messages[len(messages)-1].received
		totalDuration := lastReceived.Sub(firstReceived)

		// Server sends 3 progress messages with 50ms delays = 150ms minimum
		// Allow some slack for scheduling, but if all messages arrive within 20ms,
		// they were definitely buffered.
		minExpectedDuration := 50 * time.Millisecond
		if totalDuration < minExpectedDuration {
			t.Errorf("Messages arrived too quickly (duration=%v), suggesting buffering. "+
				"Expected incremental delivery with duration >= %v", totalDuration, minExpectedDuration)
		} else {
			t.Logf("Messages arrived incrementally over %v (expected >= %v)", totalDuration, minExpectedDuration)
		}
	}

	t.Logf("Streaming test passed: received %d progress messages and %d result message",
		progressCount, resultCount)
}

// streamTestEvent matches the SnapStreamEvent format from thundersnapd.
type streamTestEvent struct {
	Type       string `json:"type"`                  // "progress" or "result"
	Message    string `json:"message,omitempty"`     // progress message
	Status     string `json:"status,omitempty"`      // "ok" or "error" (for result)
	SnapshotID string `json:"snapshot_id,omitempty"` // snapshot ID (for result)
}

// streamingTestServer is a test server that supports streaming responses.
type streamingTestServer struct {
	sockPath string
	listener net.Listener
	env      *testEnv
	done     chan struct{}
}

func (s *streamingTestServer) Close() {
	s.listener.Close()
	<-s.done
}

// startStreamingTestServer starts a test server with streaming support.
func startStreamingTestServer(t *testing.T, env *testEnv, sockPath string) *streamingTestServer {
	t.Helper()

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen %s: %v", sockPath, err)
	}

	srv := &streamingTestServer{
		sockPath: sockPath,
		listener: ln,
		env:      env,
		done:     make(chan struct{}),
	}

	go func() {
		defer close(srv.done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.handleConn(conn)
		}
	}()

	return srv
}

func (s *streamingTestServer) handleConn(conn net.Conn) {
	defer conn.Close()

	reader := bufio.NewReader(conn)

	// Read vsock handshake
	line, err := reader.ReadString('\n')
	if err != nil {
		return
	}

	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "CONNECT ") {
		conn.Write([]byte("ERROR invalid handshake\n"))
		return
	}

	// Send OK response
	conn.Write([]byte("OK 5223\n"))

	// Handle HTTP request
	req, err := http.ReadRequest(reader)
	if err != nil {
		return
	}

	if req.URL.Path == "/stream-test" {
		s.handleStreamTest(conn, req)
	} else {
		// Return 404 for other paths
		conn.Write([]byte("HTTP/1.0 404 Not Found\r\n\r\n"))
	}
}

// handleStreamTest writes streaming NDJSON progress messages directly to the connection.
// This simulates how the real /snap?stream=1 endpoint works.
func (s *streamingTestServer) handleStreamTest(conn net.Conn, req *http.Request) {
	// Write HTTP response headers for streaming
	// Use chunked transfer encoding to allow incremental delivery
	headers := "HTTP/1.1 200 OK\r\n" +
		"Content-Type: application/x-ndjson\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"\r\n"
	conn.Write([]byte(headers))

	// Helper to write a chunked message
	writeChunk := func(data []byte) error {
		// Chunked format: size in hex, CRLF, data, CRLF
		chunk := fmt.Sprintf("%x\r\n%s\r\n", len(data), data)
		_, err := conn.Write([]byte(chunk))
		return err
	}

	encoder := json.NewEncoder(&chunkWriter{conn: conn, writeChunk: writeChunk})

	// Send progress messages with delays to test incremental delivery
	progressMessages := []string{
		"Starting operation...",
		"Processing files...",
		"Finalizing...",
	}

	for i, msg := range progressMessages {
		event := streamTestEvent{
			Type:    "progress",
			Message: msg,
		}
		if err := encoder.Encode(event); err != nil {
			return
		}

		// Small delay between messages to verify incremental delivery
		// This is short enough to not slow down tests significantly,
		// but long enough to detect buffering
		if i < len(progressMessages)-1 {
			time.Sleep(50 * time.Millisecond)
		}
	}

	// Final delay before result
	time.Sleep(50 * time.Millisecond)

	// Send final result
	result := streamTestEvent{
		Type:       "result",
		Status:     "ok",
		SnapshotID: "test-snapshot-123",
	}
	encoder.Encode(result)

	// End chunked encoding
	conn.Write([]byte("0\r\n\r\n"))
}

// chunkWriter wraps writes to produce chunked transfer encoding.
type chunkWriter struct {
	conn       net.Conn
	writeChunk func([]byte) error
	mu         sync.Mutex
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.writeChunk(p); err != nil {
		return 0, err
	}
	return len(p), nil
}
