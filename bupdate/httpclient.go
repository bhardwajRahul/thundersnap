// Package bupdate provides HTTP client with pipelining for fetching file ranges.
package bupdate

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// HTTPReader provides access to remote files over HTTP with range requests.
// It uses HTTP pipelining to efficiently fetch multiple chunks.
// A single HTTPReader can be reused for multiple files on the same host.
type HTTPReader struct {
	baseURL string
	conn    net.Conn
	br      *bufio.Reader
	bw      *bufio.Writer
	mu      sync.Mutex
	host    string
	path    string // default path (optional, for single-file use)
	closed  bool
}

// NewHTTPReader creates a new HTTPReader for the given URL.
// The URL should point to a file on an HTTP server that supports range requests.
func NewHTTPReader(rawURL string) (*HTTPReader, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parsing URL: %w", err)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme: %s", u.Scheme)
	}

	// For now, only support http (not https) for simplicity
	if u.Scheme == "https" {
		return nil, fmt.Errorf("https not supported, use http")
	}

	host := u.Host
	if !strings.Contains(host, ":") {
		host = host + ":80"
	}

	return &HTTPReader{
		baseURL: rawURL,
		host:    u.Host,
		path:    u.Path,
	}, nil
}

// NewHTTPReaderForHost creates an HTTPReader that can fetch any path on the given host.
// Use ReadRangesFromPath to fetch ranges from specific paths.
func NewHTTPReaderForHost(baseURL string) (*HTTPReader, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing URL: %w", err)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme: %s", u.Scheme)
	}

	if u.Scheme == "https" {
		return nil, fmt.Errorf("https not supported, use http")
	}

	return &HTTPReader{
		baseURL: baseURL,
		host:    u.Host,
	}, nil
}

// connect establishes a connection if not already connected.
func (r *HTTPReader) connect() error {
	if r.conn != nil {
		return nil
	}

	host := r.host
	if !strings.Contains(host, ":") {
		host = host + ":80"
	}

	conn, err := net.Dial("tcp", host)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", host, err)
	}

	r.conn = conn
	r.br = bufio.NewReader(conn)
	r.bw = bufio.NewWriter(conn)
	return nil
}

// RangeRequest represents a request for a byte range.
type RangeRequest struct {
	Offset int64
	Size   int64
}

// ReadRange reads a byte range from the remote file.
func (r *HTTPReader) ReadRange(offset, size int64) ([]byte, error) {
	results, err := r.ReadRanges([]RangeRequest{{Offset: offset, Size: size}})
	if err != nil {
		return nil, err
	}
	return results[0], nil
}

// ReadRanges reads multiple byte ranges using HTTP pipelining.
// Requests are sent in a pipeline and responses are read in order.
// Uses the path from the URL provided at construction time.
func (r *HTTPReader) ReadRanges(requests []RangeRequest) ([][]byte, error) {
	return r.ReadRangesFromPath(r.path, requests)
}

// ReadRangesFromPath reads multiple byte ranges from a specific path using HTTP pipelining.
// This allows reusing a single connection for multiple files on the same host.
func (r *HTTPReader) ReadRangesFromPath(path string, requests []RangeRequest) ([][]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil, fmt.Errorf("reader is closed")
	}

	if err := r.connect(); err != nil {
		return nil, err
	}

	// Send all requests in pipeline
	for _, req := range requests {
		rangeHeader := fmt.Sprintf("bytes=%d-%d", req.Offset, req.Offset+req.Size-1)
		reqStr := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nRange: %s\r\nConnection: keep-alive\r\n\r\n",
			path, r.host, rangeHeader)
		if _, err := r.bw.WriteString(reqStr); err != nil {
			r.closeConn()
			return nil, fmt.Errorf("writing request: %w", err)
		}
	}

	// Flush all requests
	if err := r.bw.Flush(); err != nil {
		r.closeConn()
		return nil, fmt.Errorf("flushing requests: %w", err)
	}

	// Read all responses in order
	results := make([][]byte, len(requests))
	for i, req := range requests {
		data, err := r.readResponse(req.Size)
		if err != nil {
			r.closeConn()
			return nil, fmt.Errorf("reading response %d: %w", i, err)
		}
		results[i] = data
	}

	return results, nil
}

// readResponse reads a single HTTP response.
func (r *HTTPReader) readResponse(expectedSize int64) ([]byte, error) {
	// Read status line
	statusLine, err := r.br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("reading status line: %w", err)
	}

	// Parse status code
	parts := strings.SplitN(strings.TrimSpace(statusLine), " ", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid status line: %s", statusLine)
	}

	statusCode, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("parsing status code: %w", err)
	}

	// 206 Partial Content is expected for range requests
	if statusCode != http.StatusPartialContent && statusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code %d for %s", statusCode, r.baseURL)
	}

	// Read headers
	var contentLength int64 = -1
	for {
		line, err := r.br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("reading header: %w", err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break // End of headers
		}

		// Parse Content-Length
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			valStr := strings.TrimSpace(line[len("content-length:"):])
			contentLength, err = strconv.ParseInt(valStr, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parsing content-length: %w", err)
			}
		}
	}

	// Determine how much to read
	toRead := expectedSize
	if contentLength >= 0 {
		toRead = contentLength
	}

	// Read body
	data := make([]byte, toRead)
	_, err = io.ReadFull(r.br, data)
	if err != nil {
		return nil, fmt.Errorf("reading body: %w", err)
	}

	return data, nil
}

// closeConn closes the underlying connection.
func (r *HTTPReader) closeConn() {
	if r.conn != nil {
		r.conn.Close()
		r.conn = nil
		r.br = nil
		r.bw = nil
	}
}

// Close closes the HTTPReader and releases resources.
func (r *HTTPReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.closeConn()
	return nil
}

// ReadChunkHTTP reads a chunk from a remote file over HTTP.
func ReadChunkHTTP(reader *HTTPReader, offset, size int64) ([]byte, error) {
	return reader.ReadRange(offset, size)
}

// IsHTTPURL returns true if the given string looks like an HTTP URL.
func IsHTTPURL(s string) bool {
	return strings.Contains(s, "://")
}

// FetchFullFile downloads an entire file from an HTTP URL.
// Unlike ReadRange, this doesn't use range requests and fetches the complete file.
func FetchFullFile(rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parsing URL: %w", err)
	}

	if u.Scheme == "https" {
		return nil, fmt.Errorf("https not supported, use http")
	}

	host := u.Host
	if !strings.Contains(host, ":") {
		host = host + ":80"
	}

	conn, err := net.Dial("tcp", host)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", host, err)
	}
	defer conn.Close()

	// Send request without Range header to get full file
	reqStr := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n",
		u.Path, u.Host)
	if _, err := conn.Write([]byte(reqStr)); err != nil {
		return nil, fmt.Errorf("writing request: %w", err)
	}

	br := bufio.NewReader(conn)

	// Read status line
	statusLine, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("reading status line: %w", err)
	}

	parts := strings.SplitN(strings.TrimSpace(statusLine), " ", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid status line: %s", statusLine)
	}

	statusCode, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("parsing status code: %w", err)
	}

	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code %d for %s", statusCode, rawURL)
	}

	// Read headers
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("reading header: %w", err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
	}

	// Read entire body (Connection: close means server will close when done)
	data, err := io.ReadAll(br)
	if err != nil {
		return nil, fmt.Errorf("reading body: %w", err)
	}

	return data, nil
}

// LoadFidxHTTP loads a fidx file from an HTTP URL.
func LoadFidxHTTP(rawURL string) (*Fidx, error) {
	data, err := FetchFullFile(rawURL)
	if err != nil {
		return nil, err
	}
	return ParseFidxData(rawURL, data)
}
