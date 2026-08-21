// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package tsm

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestCheckPeersForSnapshotUsesHTTPClient proves the httpClient parameter is
// honored: a peer URL is reached only through the provided client's dialer.
// This is the regression guard for the tsnet-vs-host-network split (see
// cmd/thundersnapd main.go meshState.httpClient): in production the ping loop
// discovers peers via srv.Dial (tsnet), but who-has/download-snap must use the
// same dialer for their fetches — a plain http.DefaultClient cannot route
// *.ts.net and fails with connection refused, which checkURLExists silently
// treats as "peer doesn't have the snap".
func TestCheckPeersForSnapshotUsesHTTPClient(t *testing.T) {
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/bupdate/snapA.tsm", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	// A custom dialer that rewrites the peer's hostname:port to the test
	// server's real loopback address, simulating a tsnet dialer that can
	// resolve a .ts.net URL to a reachable endpoint. Any request through a
	// client using this dialer lands on the test server; a request through a
	// plain client does not (the bogus hostname does not resolve).
	customClient := &http.Client{
		Transport: &http.Transport{
			DialContext: customDialer(server.Listener.Addr().String()),
		},
	}

	// Peer URL uses a bogus hostname that only customDialer can reach. This
	// mirrors a *.ts.net address: resolvable via tsnet, unreachable via the
	// host stack. Use an http:// URL with a non-resolvable host + the test
	// server's port (port is ignored by the custom dialer).
	peerURL := "http://nonexistent-mesh-host.ts.net:7575"

	peers := []PeerInfo{{URL: peerURL, Hostname: "peerA"}}

	// With the custom (tsnet-like) client, the HEAD reaches the server and
	// reports HasSnap=true.
	got := CheckPeersForSnapshot(peers, "snapA", customClient)
	if len(got) != 1 || !got[0].HasSnap {
		t.Fatalf("custom client: expected HasSnap=true for snapA, got %+v", got)
	}
	if hits.Load() != 1 {
		t.Errorf("custom client: expected 1 hit on bupdate server, got %d", hits.Load())
	}

	// With http.DefaultClient (the host stack), the bogus .ts.net hostname
	// is unresolvable and the HEAD fails. Whether the failure surfaces as a
	// connection-refused (swallowed by checkURLExists) or a DNS error (returned
	// in Err), HasSnap is false either way — and handleWhoHas/handleDownloadSnap
	// only consult HasSnap, so who-has returns empty with status "ok". This
	// is the silent "who-has returns no peers" symptom from the host-stack
	// split. This branch documents that unreachable peers are never reported
	// as having the snap.
	hits.Store(0)
	got = CheckPeersForSnapshot(peers, "snapA", http.DefaultClient)
	if len(got) != 1 {
		t.Fatalf("default client: expected 1 result, got %d", len(got))
	}
	if got[0].HasSnap {
		t.Errorf("default client: expected HasSnap=false for unreachable .ts.net URL, but got true (hits=%d)", hits.Load())
	}
	if hits.Load() != 0 {
		t.Errorf("default client: expected 0 hits on bupdate server (URL unreachable), got %d", hits.Load())
	}
}

// customDialer returns a DialContext that ignores the requested address and
// dials the given target, simulating a tsnet dialer that resolves a .ts.net
// name to a reachable tailnet endpoint.
func customDialer(target string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(_ context.Context, _, _ string) (net.Conn, error) {
		return net.Dial("tcp", target)
	}
}
