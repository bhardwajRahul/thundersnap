// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package metrics

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/tailscale/thundersnap/frameid"
	"github.com/tailscale/thundersnap/refs"
	"github.com/tailscale/thundersnap/tsm"
)

// TestMetricsExport is the regression test for "export prometheus metrics on
// :7575". It exercises the real production metrics handler (NewHandler, the
// same handler the daemon serves at /metrics) against a real on-disk fs/snaps
// layout and a real refs.Store, scrapes the handler over HTTP, and asserts:
//   - the standard OS-level collectors are present (go_goroutines, process_*
//     metrics), and
//   - the thundersnap gauges reflect the real counts of frames, snaps, refs,
//     and the supplied running-session / running-VM closures.
//
// This is a port of the former not_e2e/metrics_test.go TestMetricsExport,
// adapted to use t.TempDir + the tsm indexer instead of btrfs subvolumes so
// it runs as a plain package test (no root/btrfs required).
func TestMetricsExport(t *testing.T) {
	fsDir := t.TempDir()
	snapsDir := t.TempDir()

	// Two snaps: index two source trees into <snapsDir>/<id>. The metrics
	// collector counts snaps by their .tsm manifest files.
	for _, id := range []string{"aaa111", "bbb222"} {
		src := t.TempDir()
		if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("x"), 0644); err != nil {
			t.Fatalf("write source file: %v", err)
		}
		idx := tsm.NewIndexer(tsm.IndexerOptions{})
		if err := idx.Index(src, filepath.Join(snapsDir, id)); err != nil {
			t.Fatalf("index snap %s: %v", id, err)
		}
	}
	wantSnaps := 2

	// Three frames under fs/<user>/<uuid>/ each marked by a <uuid>.jsonc file,
	// the canonical layout the metrics collector counts. The sidecar stem must
	// be a valid frame UUID.
	wantFrames := 3
	for i := 0; i < wantFrames; i++ {
		name := frameid.MustNew().String()
		framePath := filepath.Join(fsDir, "testuser", name)
		if err := os.MkdirAll(framePath, 0755); err != nil {
			t.Fatalf("mkdir frame: %v", err)
		}
		if err := os.WriteFile(framePath+".jsonc", []byte("{}\n"), 0644); err != nil {
			t.Fatalf("write sidecar: %v", err)
		}
	}
	// A legacy-layout sidecar whose stem is NOT a UUID must NOT be counted.
	if err := os.MkdirAll(filepath.Join(fsDir, "testuser", "legacy"), 0755); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fsDir, "testuser", "legacy.jsonc"), []byte("{}\n"), 0644); err != nil {
		t.Fatalf("write legacy sidecar: %v", err)
	}
	// A bare directory with no .jsonc must NOT be counted as a frame.
	if err := os.MkdirAll(filepath.Join(fsDir, "testuser", "notaframe"), 0755); err != nil {
		t.Fatalf("mkdir notaframe: %v", err)
	}

	// Two refs in a real per-user ref store, counted via the Refs closure.
	dataDir := t.TempDir()
	store := refs.NewNamespaceStore(dataDir, "testuser")
	wantRefs := 2
	for i := 0; i < wantRefs; i++ {
		if err := store.Create(fmt.Sprintf("ref%d", i), frameid.MustNew()); err != nil {
			t.Fatalf("create ref%d: %v", i, err)
		}
	}

	wantSessions := 4
	wantVMs := 1

	handler, err := NewHandler(Sources{
		FsDir:           fsDir,
		SnapsDir:        snapsDir,
		Refs:            func() int { names, _ := store.List(); return len(names) },
		RunningSessions: func() int { return wantSessions },
		RunningVMs:      func() int { return wantVMs },
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	out := string(body)
	t.Logf("/metrics output:\n%s", out)

	for _, name := range []string{"go_goroutines", "process_open_fds", "process_start_time_seconds"} {
		if !regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\b`).MatchString(out) {
			t.Errorf("missing OS-level metric %q in /metrics output", name)
		}
	}

	checks := []struct {
		metric string
		want   int
	}{
		{"thundersnap_frames_total", wantFrames},
		{"thundersnap_snaps_total", wantSnaps},
		{"thundersnap_refs_total", wantRefs},
		{"thundersnap_running_sessions", wantSessions},
		{"thundersnap_running_vms", wantVMs},
	}
	for _, c := range checks {
		got, ok := scrapeGauge(out, c.metric)
		if !ok {
			t.Errorf("metric %q not found in /metrics output", c.metric)
			continue
		}
		if got != c.want {
			t.Errorf("%s = %d, want %d", c.metric, got, c.want)
		}
	}
}

// scrapeGauge extracts the integer value of a no-label Prometheus gauge line
// "<name> <value>" from the metrics exposition text.
func scrapeGauge(out, name string) (int, bool) {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\s+([0-9.eE+-]+)\s*$`)
	m := re.FindStringSubmatch(out)
	if m == nil {
		return 0, false
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	return int(f), true
}
