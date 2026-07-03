// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package tsm

import (
	"net/http"
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
