// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tailscale/thundersnap/snaphash"
)

// injectMeshPeer POSTs a /ts/ping to dstHTTPBaseURL so that dst's meshState
// records peerURL/peerHostname as a mesh peer. This is the same discovery
// endpoint real tsnet nodes use; in --test-http-listen mode the daemon mounts
// it (buildTestHTTPMux) but never starts the ping loop, so the test must seed
// peers itself. No production code is changed — this exercises the real
// handleTsPing/recordPeer path.
func injectMeshPeer(t *testing.T, dstHTTPBaseURL, peerURL, peerHostname string) {
	t.Helper()
	body, err := json.Marshal(map[string]string{
		"url":      peerURL,
		"hostname": peerHostname,
	})
	if err != nil {
		t.Fatalf("injectMeshPeer marshal: %v", err)
	}
	resp, err := http.Post(dstHTTPBaseURL+"/ts/ping", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("injectMeshPeer: POST %s/ts/ping: %v", dstHTTPBaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("injectMeshPeer: %s/ts/ping returned %d: %s", dstHTTPBaseURL, resp.StatusCode, b)
	}
}

// testMeshWhoHasAndDownload reproduces the cross-machine workflow the user
// reported as broken: `ts snap` on machine A, then `ts who-has <rootID>` and
// `ts download-snap <triplet>` on machine B. It runs the REAL production
// handlers end to end — handleWhoHas, handleDownloadSnap, doDownloadSnap,
// tsm.CheckPeersForSnapshot, the bupdateFileServer, and the ts CLI — against
// two independent --test-http-listen daemons wired as mesh peers via /ts/ping.
//
// The suite's shared daemon (d) is intentionally not used: this scenario needs
// two HTTP-enabled daemons. The shared daemon has no --test-http-listen, so
// buildTestHTTPMux never runs, globalMeshState stays nil, and /who-has would
// return "mesh not enabled" — which is exactly the gap TODO.md (W6) tracks.
//
// This test closes that gap: it is the first e2e test to drive the real
// handleWhoHas/handleDownloadSnap/CheckPeersForSnapshot/bupdateFileServer
// across two daemons, so it would surface the naming/URL mismatch the user
// suspected (snap ID printed by `ts snap` vs. the .tsm filename served by
// /bupdate/ vs. the URL CheckPeersForSnapshot HEADs).
func testMeshWhoHasAndDownload(t *testing.T, d *daemonInstance) {
	_ = d // suite daemon unused; see comment above

	// Two independent daemons, each with a real HTTP mux (so globalMeshState
	// is populated and /ts/ping + /bupdate/ are served).
	envA := newTestEnv(t)
	dA, httpA := startDaemonWithHTTP(t, envA)
	envB := newTestEnv(t)
	dB, httpB := startDaemonWithHTTP(t, envB)
	t.Logf("mesh daemons: A ssh=%s http=%s | B ssh=%s http=%s", dA.addr, httpA, dB.addr, httpB)

	// Wire the mesh symmetrically, mirroring what the tsnet ping loop does in
	// production: each daemon learns the other as a peer.
	injectMeshPeer(t, httpB, httpA, "daemonA")
	injectMeshPeer(t, httpA, httpB, "daemonB")

	// --- Machine A: create a frame, write markers, take a full snap. ---
	createFrameViaDaemon(t, dA, "meshsrc")

	rootMarker := "ROOT_MESH_MARKER_7"
	homeMarker := "HOME_MESH_MARKER_9"
	if out, exit, err := sshExec(t, dA, "root@meshsrc", "echo "+rootMarker+" > /r.txt"); err != nil || exit != 0 {
		t.Fatalf("A: write root marker: err=%v exit=%d out=%q", err, exit, out)
	}
	if out, exit, err := sshExec(t, dA, "root@meshsrc", "echo "+homeMarker+" > /home/h.txt"); err != nil || exit != 0 {
		t.Fatalf("A: write home marker: err=%v exit=%d out=%q", err, exit, out)
	}

	triplet := tsSnapWait(t, dA, "meshsrc")
	rootSnap, homeSnap, workSnap := snapTriplet(t, triplet)
	t.Logf("A snap triplet: root=%s home=%s work=%s", rootSnap, homeSnap, workSnap)
	if homeSnap == "nil" {
		t.Fatalf("home snap is nil; expected /home/h.txt to produce a non-nil home snap")
	}
	// work is empty, so workSnap should be "nil"; that's fine, download-snap
	// skips nil components.

	// --- Negative control: who-has for a valid but nonexistent snap. ---
	// Uses a canonical snaphash-encoded ID that no daemon has, so the real
	// CheckPeersForSnapshot HEADs A's /bupdate/<bogus>.tsm, gets 404, and
	// reports no peers. Guards against who-has false positives.
	bogusID := snaphash.Encode(snaphash.Sum([]byte("mesh-test-bogus-nonexistent-snap")))
	bout, berr, bexit, _ := sshExecSplit(t, dB, "root@", "ts who-has "+bogusID)
	if bexit == 0 {
		t.Errorf("who-has bogus snap: expected non-zero exit, got 0 (stdout=%q stderr=%q)", bout, berr)
	}
	if !strings.Contains(berr, "No peers") {
		t.Errorf("who-has bogus snap: stderr=%q, want it to mention \"No peers\"", berr)
	}

	// --- Machine B: who-has for the root snap must find A. ---
	whoOut, whoErr, whoExit, _ := sshExecSplit(t, dB, "root@", "ts who-has "+rootSnap)
	if whoExit != 0 {
		t.Fatalf("B: ts who-has %s: exit=%d stdout=%q stderr=%q", rootSnap, whoExit, whoOut, whoErr)
	}
	// cmdWhoHas prints "<peerURL>/bupdate/" per peer on stdout. A was injected
	// as a peer of B, and A serves rootSnap.tsm, so stdout must contain A's
	// bupdate URL. This is the assertion that would fail if the snap ID
	// printed by `ts snap` didn't match the .tsm filename served by /bupdate/
	// or the URL CheckPeersForSnapshot HEADs.
	wantBupdate := strings.TrimSuffix(httpA, "/") + "/bupdate/"
	if !strings.Contains(whoOut, wantBupdate) {
		t.Fatalf("B: ts who-has %s stdout=%q does not contain A's bupdate URL %q (stderr=%q)", rootSnap, whoOut, wantBupdate, whoErr)
	}
	t.Logf("B: who-has found snap %s at %q", rootSnap, strings.TrimSpace(whoOut))

	// --- Machine B: download the full triplet from A. ---
	// This drives handleDownloadSnap -> doDownloadSnap -> CheckPeersForSnapshot
	// -> tsm.Download (stamp/tsm/tsc + chunk range fetches from /bupdate/).
	dlOut, dlErr, dlExit, _ := sshExecSplit(t, dB, "root@", "ts download-snap "+triplet)
	if dlExit != 0 {
		t.Fatalf("B: ts download-snap %s: exit=%d stdout=%q stderr=%q", triplet, dlExit, dlOut, dlErr)
	}
	t.Logf("B: downloaded snap %s from A (stderr=%q)", triplet, dlErr)

	// --- Machine B: the downloaded snaps must now be local. ---
	snapsOut, snapsErr, snapsExit, _ := sshExecSplit(t, dB, "root@", "ts snaps")
	if snapsExit != 0 {
		t.Fatalf("B: ts snaps: exit=%d stdout=%q stderr=%q", snapsExit, snapsOut, snapsErr)
	}
	if !strings.Contains(snapsOut, rootSnap) {
		t.Errorf("B: ts snaps output does not list downloaded root snap %s:\n%s", rootSnap, snapsOut)
	}
	if !strings.Contains(snapsOut, homeSnap) {
		t.Errorf("B: ts snaps output does not list downloaded home snap %s:\n%s", homeSnap, snapsOut)
	}

	// --- Machine B: build a frame from the downloaded triplet and verify. ---
	if out, exit, err := sshExec(t, dB, "root@", "ts frame --ref=meshdst "+triplet); err != nil || exit != 0 {
		t.Fatalf("B: ts frame --ref=meshdst %s: err=%v exit=%d out=%q", triplet, err, exit, out)
	}

	// Root marker (from downloaded rootSnap) must survive into the new frame.
	rOut, rExit, rErr := sshExec(t, dB, "root@meshdst", "read line < /r.txt && echo $line")
	if rErr != nil || rExit != 0 {
		t.Fatalf("B: read /r.txt in meshdst: err=%v exit=%d out=%q", rErr, rExit, rOut)
	}
	if got := strings.TrimSpace(rOut); got != rootMarker {
		t.Errorf("B: meshdst /r.txt = %q, want %q", got, rootMarker)
	} else {
		t.Logf("B: meshdst preserved root marker %q (downloaded root snap %s)", got, rootSnap)
	}

	// Home marker (from downloaded homeSnap) must survive into the new frame.
	hOut, hExit, hErr := sshExec(t, dB, "root@meshdst", "read line < /home/h.txt && echo $line")
	if hErr != nil || hExit != 0 {
		t.Fatalf("B: read /home/h.txt in meshdst: err=%v exit=%d out=%q", hErr, hExit, hOut)
	}
	if got := strings.TrimSpace(hOut); got != homeMarker {
		t.Errorf("B: meshdst /home/h.txt = %q, want %q", got, homeMarker)
	} else {
		t.Logf("B: meshdst preserved home marker %q (downloaded home snap %s)", got, homeSnap)
	}

	// Sanity: the work component was nil in the triplet, so the new frame's
	// /work is a fresh empty subvolume and must NOT carry the root marker.
	if out, exit, _ := sshExec(t, dB, "root@meshdst", "read line < /work/r.txt && echo $line"); exit == 0 {
		t.Errorf("B: meshdst /work/r.txt unexpectedly readable (should be empty): out=%q", out)
	}
}
