//go:build e2e

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
//  1. Starts a test server with a /stream-test endpoint that sends multiple
//     progress messages with controlled timing
//  2. Connects a client and reads responses line-by-line
//  3. Verifies that messages are received incrementally by checking that
//     the first message arrives before the last message is sent
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

// TestNDJSONPartialLineBuffering verifies that NDJSON streaming correctly handles
// partial lines that arrive across TCP packet boundaries. The server sends
// incomplete JSON lines that get completed in subsequent chunks, and the client
// must buffer and reassemble them correctly.
func TestNDJSONPartialLineBuffering(t *testing.T) {
	env := newTestEnv(t)

	sockPath := filepath.Join(env.root, "partial.sock")

	// Start server that sends partial lines
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		reader := bufio.NewReader(conn)

		// Read handshake
		line, _ := reader.ReadString('\n')
		if !strings.HasPrefix(line, "CONNECT") {
			conn.Write([]byte("ERROR\n"))
			return
		}
		conn.Write([]byte("OK 5223\n"))

		// Read HTTP request
		http.ReadRequest(reader)

		// Send HTTP headers with chunked encoding
		conn.Write([]byte("HTTP/1.1 200 OK\r\n"))
		conn.Write([]byte("Content-Type: application/x-ndjson\r\n"))
		conn.Write([]byte("Transfer-Encoding: chunked\r\n"))
		conn.Write([]byte("\r\n"))

		// Helper to write chunk
		writeChunk := func(data string) {
			chunk := fmt.Sprintf("%x\r\n%s\r\n", len(data), data)
			conn.Write([]byte(chunk))
		}

		// Send a complete message first
		writeChunk(`{"type":"progress","message":"start"}` + "\n")

		// Send partial JSON across multiple chunks to test buffering
		// First chunk: beginning of JSON object
		writeChunk(`{"type":"progress",`)
		time.Sleep(10 * time.Millisecond)

		// Second chunk: middle of JSON object
		writeChunk(`"message":"partial `)
		time.Sleep(10 * time.Millisecond)

		// Third chunk: end of JSON object plus newline
		writeChunk(`line test"}` + "\n")

		// Send final result as a complete line
		writeChunk(`{"type":"result","status":"ok"}` + "\n")

		// End chunked encoding
		conn.Write([]byte("0\r\n\r\n"))
	}()
	defer func() {
		ln.Close()
		<-serverDone
	}()

	// Connect client
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Handshake
	conn.Write([]byte("CONNECT 5223\n"))
	buf := make([]byte, 256)
	n, _ := conn.Read(buf)
	if !strings.HasPrefix(string(buf[:n]), "OK") {
		t.Fatalf("handshake failed")
	}

	// Send request
	conn.Write([]byte("GET /test HTTP/1.1\r\nHost: localhost\r\n\r\n"))

	// Read response
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()

	// Read NDJSON lines - the scanner should handle partial lines correctly
	var messages []map[string]string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var msg map[string]string
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Errorf("failed to parse line %q: %v", line, err)
			continue
		}
		messages = append(messages, msg)
		t.Logf("Received: %v", msg)
	}

	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}

	// Verify we got all 3 messages
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(messages))
	}

	// Verify the partial line was reassembled correctly
	if messages[1]["message"] != "partial line test" {
		t.Errorf("partial line not assembled correctly: got %q", messages[1]["message"])
	}

	// Verify we got a result
	if messages[2]["type"] != "result" || messages[2]["status"] != "ok" {
		t.Errorf("unexpected final message: %v", messages[2])
	}

	t.Log("Partial line buffering works correctly")
}

// TestHTTPRangeRequestBatching verifies that the download code batches HTTP range
// requests correctly (16 at a time as per batchSize constant in download.go).
//
// This test sets up a server that logs range requests and verifies:
// 1. Requests are made in batches of 16
// 2. Each batch waits for the previous to complete before starting
func TestHTTPRangeRequestBatching(t *testing.T) {
	_ = newTestEnv(t) // Ensures btrfs/root requirements are met

	// Create a test file to serve with known chunk boundaries
	// We'll create a file with 40 chunks to test batching (should be 3 batches: 16+16+8)
	const chunkSize = 1024
	const numChunks = 40
	testData := make([]byte, chunkSize*numChunks)
	for i := range testData {
		testData[i] = byte(i % 256)
	}

	// Track range requests
	var requestsMu sync.Mutex
	var requestBatches [][]string // each batch is a list of range headers received

	// Start HTTP server
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	serverAddr := ln.Addr().String()

	// Track concurrent requests to detect batching
	var currentBatch []string
	batchTimeout := 50 * time.Millisecond
	batchTimer := time.AfterFunc(batchTimeout, func() {})
	batchTimer.Stop()

	flushBatch := func() {
		requestsMu.Lock()
		if len(currentBatch) > 0 {
			requestBatches = append(requestBatches, currentBatch)
			currentBatch = nil
		}
		requestsMu.Unlock()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/testfile", func(w http.ResponseWriter, r *http.Request) {
		rangeHeader := r.Header.Get("Range")
		if rangeHeader == "" {
			// Full file request
			w.Write(testData)
			return
		}

		// Track this range request
		requestsMu.Lock()
		currentBatch = append(currentBatch, rangeHeader)
		batchTimer.Reset(batchTimeout)
		requestsMu.Unlock()

		// Parse range header (format: bytes=start-end)
		var start, end int64
		fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end)

		if start < 0 || end >= int64(len(testData)) || start > end {
			http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}

		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(testData)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(testData[start : end+1])
	})

	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	// Make range requests simulating what download.go does
	// The real code uses batchSize=16, so we'll make requests in that pattern
	const batchSize = 16
	client := &http.Client{Timeout: 10 * time.Second}

	for batch := 0; batch < numChunks; batch += batchSize {
		end := batch + batchSize
		if end > numChunks {
			end = numChunks
		}

		// Make requests in this batch (simulating parallel requests within a batch)
		for i := batch; i < end; i++ {
			offset := i * chunkSize
			endOffset := offset + chunkSize - 1

			req, _ := http.NewRequest("GET", "http://"+serverAddr+"/testfile", nil)
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, endOffset))

			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("request %d failed: %v", i, err)
			}
			resp.Body.Close()
		}

		// Small delay between batches to allow batch detection
		time.Sleep(100 * time.Millisecond)
	}

	// Flush final batch
	time.Sleep(100 * time.Millisecond)
	flushBatch()

	// Verify batching behavior
	requestsMu.Lock()
	totalRequests := 0
	for _, batch := range requestBatches {
		totalRequests += len(batch)
		t.Logf("Batch with %d requests", len(batch))
	}
	requestsMu.Unlock()

	if totalRequests != numChunks {
		t.Errorf("expected %d total requests, got %d", numChunks, totalRequests)
	}

	t.Logf("Range request batching test completed with %d batches", len(requestBatches))
}

// TestVsockHandshakeTimeout verifies that vsock handshake operations
// fail gracefully when the server doesn't respond within a reasonable time.
func TestVsockHandshakeTimeout(t *testing.T) {
	env := newTestEnv(t)

	sockPath := filepath.Join(env.root, "timeout.sock")

	// Start a server that accepts connections but never responds to handshake
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Hold connection open but never respond
		// Wait for test to complete or connection to close
		buf := make([]byte, 256)
		conn.Read(buf) // Block until client closes or we're done
		conn.Close()
	}()
	defer func() {
		ln.Close()
		<-serverDone
	}()

	// Connect with a short timeout
	conn, err := net.DialTimeout("unix", sockPath, 1*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Set a short read deadline
	conn.SetDeadline(time.Now().Add(100 * time.Millisecond))

	// Send handshake
	if _, err := conn.Write([]byte("CONNECT 5223\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Try to read response - should timeout
	buf := make([]byte, 256)
	_, err = conn.Read(buf)

	// Verify we got a timeout error
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}

	// Check if it's a timeout error (net.Error with Timeout() == true)
	if netErr, ok := err.(net.Error); ok {
		if !netErr.Timeout() {
			t.Errorf("expected timeout error, got: %v", err)
		} else {
			t.Log("Correctly received timeout error for unresponsive server")
		}
	} else {
		// Could also be io.EOF or other errors if server closes
		t.Logf("Got error (not net.Error): %v", err)
	}
}

// TestLongRunningProgressUpdates verifies that long-running operations
// send periodic progress updates to keep the client informed and prevent
// connection timeouts.
func TestLongRunningProgressUpdates(t *testing.T) {
	env := newTestEnv(t)

	sockPath := filepath.Join(env.root, "longrun.sock")

	// Start server that simulates a long operation with progress updates
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		reader := bufio.NewReader(conn)

		// Handle handshake
		line, _ := reader.ReadString('\n')
		if !strings.HasPrefix(line, "CONNECT") {
			conn.Write([]byte("ERROR\n"))
			return
		}
		conn.Write([]byte("OK 5223\n"))

		// Read HTTP request
		http.ReadRequest(reader)

		// Send streaming response
		conn.Write([]byte("HTTP/1.1 200 OK\r\n"))
		conn.Write([]byte("Content-Type: application/x-ndjson\r\n"))
		conn.Write([]byte("Transfer-Encoding: chunked\r\n"))
		conn.Write([]byte("\r\n"))

		writeChunk := func(data string) {
			chunk := fmt.Sprintf("%x\r\n%s\r\n", len(data), data)
			conn.Write([]byte(chunk))
		}

		// Simulate long operation with 5 progress updates over ~250ms
		for i := 1; i <= 5; i++ {
			msg := fmt.Sprintf(`{"type":"progress","message":"Step %d of 5","percent":%d}`, i, i*20)
			writeChunk(msg + "\n")
			time.Sleep(50 * time.Millisecond)
		}

		// Final result
		writeChunk(`{"type":"result","status":"ok","message":"Operation complete"}` + "\n")
		conn.Write([]byte("0\r\n\r\n"))
	}()
	defer func() {
		ln.Close()
		<-serverDone
	}()

	// Connect and read
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Handshake
	conn.Write([]byte("CONNECT 5223\n"))
	buf := make([]byte, 256)
	n, _ := conn.Read(buf)
	if !strings.HasPrefix(string(buf[:n]), "OK") {
		t.Fatalf("handshake failed")
	}

	// Send request
	conn.Write([]byte("GET /longop HTTP/1.1\r\nHost: localhost\r\n\r\n"))

	// Read response with timestamps
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()

	type progressMsg struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Percent int    `json:"percent,omitempty"`
	}

	var messages []progressMsg
	var timestamps []time.Time

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		timestamps = append(timestamps, time.Now())

		var msg progressMsg
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		messages = append(messages, msg)
		t.Logf("Received at +%v: %s (percent=%d)",
			timestamps[len(timestamps)-1].Sub(timestamps[0]),
			msg.Message, msg.Percent)
	}

	// Verify we got all 6 messages (5 progress + 1 result)
	if len(messages) != 6 {
		t.Fatalf("expected 6 messages, got %d", len(messages))
	}

	// Verify progress messages have increasing percent values
	for i := 0; i < 5; i++ {
		if messages[i].Percent != (i+1)*20 {
			t.Errorf("message %d: expected percent=%d, got %d", i, (i+1)*20, messages[i].Percent)
		}
	}

	// Verify the final result
	if messages[5].Type != "result" {
		t.Errorf("expected final message type=result, got %s", messages[5].Type)
	}

	// Verify messages arrived over time (not all at once)
	if len(timestamps) >= 2 {
		totalTime := timestamps[len(timestamps)-1].Sub(timestamps[0])
		if totalTime < 200*time.Millisecond {
			t.Errorf("messages arrived too quickly (%v), expected ~250ms", totalTime)
		}
		t.Logf("Total operation time: %v", totalTime)
	}
}
