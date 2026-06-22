package tsm

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PeerInfo contains information about a mesh peer.
type PeerInfo struct {
	URL      string // Base URL of the peer (e.g., "http://host:7575")
	Hostname string // Hostname of the peer
}

// PeerResult represents the result of checking a peer for a snapshot.
type PeerResult struct {
	PeerURL  string // Base URL of the peer (e.g., "http://host:7575")
	Hostname string // Hostname of the peer
	HasSnap  bool   // Whether this peer has the snapshot
	Err      error  // Any error that occurred
}

// CheckPeersForSnapshot queries multiple peers in parallel to find which ones
// have a given snapshot (by checking if the .tsm file exists).
func CheckPeersForSnapshot(peers []PeerInfo, snapshotID string) []PeerResult {
	results := make([]PeerResult, len(peers))
	var wg sync.WaitGroup

	for i, peer := range peers {
		wg.Add(1)
		go func(idx int, p PeerInfo) {
			defer wg.Done()

			baseURL := strings.TrimSuffix(p.URL, "/")
			tsmURL := baseURL + "/bupdate/" + snapshotID + ".tsm"

			exists, err := checkURLExists(tsmURL)

			results[idx] = PeerResult{
				PeerURL:  baseURL,
				Hostname: p.Hostname,
				HasSnap:  exists,
				Err:      err,
			}
		}(i, peer)
	}

	wg.Wait()
	return results
}

// checkURLExists does a HEAD request to check if a URL exists.
func checkURLExists(url string) (bool, error) {
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Head(url)
	if err != nil {
		// Try to determine if this is a connection refused (peer not running)
		// vs other errors
		if isConnRefused(err) {
			return false, nil // Peer not running, not an error
		}
		return false, err
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK, nil
}

// isConnRefused checks if an error is a connection refused error.
func isConnRefused(err error) bool {
	if err == nil {
		return false
	}
	// Check for "connection refused" in the error string
	errStr := err.Error()
	return strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "connect: connection refused")
}

// checkURLExistsRaw uses raw TCP to check if a URL exists (for better control).
// This is an alternative to checkURLExists that doesn't follow redirects.
func checkURLExistsRaw(rawURL string) (bool, error) {
	// Parse URL manually
	if !strings.HasPrefix(rawURL, "http://") {
		return false, fmt.Errorf("only http:// URLs supported")
	}

	urlPart := strings.TrimPrefix(rawURL, "http://")
	slashIdx := strings.Index(urlPart, "/")
	if slashIdx == -1 {
		return false, fmt.Errorf("invalid URL: no path")
	}

	hostPort := urlPart[:slashIdx]
	path := urlPart[slashIdx:]

	// Add default port if not specified
	if !strings.Contains(hostPort, ":") {
		hostPort += ":80"
	}

	// Connect
	conn, err := net.DialTimeout("tcp", hostPort, 5*time.Second)
	if err != nil {
		if isConnRefused(err) {
			return false, nil
		}
		return false, err
	}
	defer conn.Close()

	// Set deadline
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Send HEAD request
	req := fmt.Sprintf("HEAD %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", path, hostPort)
	if _, err := conn.Write([]byte(req)); err != nil {
		return false, err
	}

	// Read response
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		return false, err
	}

	// Parse status line
	resp := string(buf[:n])
	lines := strings.SplitN(resp, "\r\n", 2)
	if len(lines) == 0 {
		return false, fmt.Errorf("empty response")
	}

	parts := strings.SplitN(lines[0], " ", 3)
	if len(parts) < 2 {
		return false, fmt.Errorf("invalid status line: %s", lines[0])
	}

	statusCode, err := strconv.Atoi(parts[1])
	if err != nil {
		return false, fmt.Errorf("invalid status code: %s", parts[1])
	}

	return statusCode == http.StatusOK, nil
}
