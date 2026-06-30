//go:build e2e

package e2e

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/tailscale/thundersnap/frameid"
	"github.com/tailscale/thundersnap/metrics"
	"github.com/tailscale/thundersnap/refs"
	"github.com/tailscale/thundersnap/tsm"
)

// TestMetricsExport is the regression test for "export prometheus metrics on
// :7575". It exercises the real production metrics package (metrics.NewHandler,
// the same handler the daemon serves at /metrics) against a real on-disk fs/snaps
// layout and a real refs.Store, then scrapes the handler over HTTP and asserts:
//
//   - the standard OS-level collectors are present (go_goroutines, a process_*
//     metric), matching aperture's Go+process collector export, and
//   - the thundersnap gauges reflect the real counts of frames, snaps, refs, and
//     the supplied running-session / running-VM closures.
func TestMetricsExport(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()
	t.Logf("base snapshot: %s", baseSnap)

	// Two snaps: index two read-only subvolumes into <snapsDir>/<id>.tsm. The
	// metrics collector counts snaps by their .tsm manifest files.
	for _, id := range []string{"aaa111", "bbb222"} {
		src := filepath.Join(env.snapshotsDir, "src-"+id)
		if out, err := exec.Command("btrfs", "subvolume", "snapshot", "-r",
			filepath.Join(env.snapshotsDir, baseSnap), src).CombinedOutput(); err != nil {
			t.Fatalf("btrfs snapshot %s: %v\n%s", id, err, out)
		}
		defer exec.Command("btrfs", "subvolume", "delete", src).Run()
		idx := tsm.NewIndexer(tsm.IndexerOptions{})
		if err := idx.Index(src, filepath.Join(env.snapshotsDir, id)); err != nil {
			t.Fatalf("index snap %s: %v", id, err)
		}
	}
	wantSnaps := 2

	// Three frames under fs/<user>/<uuid>/ each marked by a <uuid>.jsonc file,
	// the canonical layout the daemon's frame listing (and the metrics
	// collector) counts. The sidecar stem must be a valid frame UUID.
	wantFrames := 3
	for i := 0; i < wantFrames; i++ {
		user := "testuser"
		name := frameid.MustNew().String()
		framePath := filepath.Join(env.fsDir, user, name)
		if err := os.MkdirAll(framePath, 0755); err != nil {
			t.Fatalf("mkdir frame: %v", err)
		}
		mustWriteFile(t, framePath+".jsonc", "{}\n")
	}
	// A legacy-layout sidecar whose stem is NOT a UUID must NOT be counted.
	if err := os.MkdirAll(filepath.Join(env.fsDir, "testuser", "legacy"), 0755); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}
	mustWriteFile(t, filepath.Join(env.fsDir, "testuser", "legacy.jsonc"), "{}\n")
	// A bare directory with no .jsonc must NOT be counted as a frame.
	if err := os.MkdirAll(filepath.Join(env.fsDir, "testuser", "notaframe"), 0755); err != nil {
		t.Fatalf("mkdir notaframe: %v", err)
	}

	// Two refs in a real per-user ref store, counted via the Refs closure the
	// daemon supplies (summing each user's List()).
	dataDir := filepath.Join(env.root, "state")
	store := refs.NewUserStore(dataDir, "testuser")
	wantRefs := 2
	for i := 0; i < wantRefs; i++ {
		if err := store.Create(fmt.Sprintf("ref%d", i), frameid.MustNew()); err != nil {
			t.Fatalf("create ref%d: %v", i, err)
		}
	}

	// Supply running session / VM counts via closures, as the daemon does from
	// its in-memory state.
	wantSessions := 4
	wantVMs := 1

	handler, err := metrics.NewHandler(metrics.Sources{
		FsDir:           env.fsDir,
		SnapsDir:        env.snapshotsDir,
		Refs:            func() int { names, _ := store.List(); return len(names) },
		RunningSessions: func() int { return wantSessions },
		RunningVMs:      func() int { return wantVMs },
	})
	if err != nil {
		t.Fatalf("metrics.NewHandler: %v", err)
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

	// OS-level metrics from the standard Go + process collectors.
	for _, name := range []string{"go_goroutines", "process_open_fds", "process_start_time_seconds"} {
		if !regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\b`).MatchString(out) {
			t.Errorf("missing OS-level metric %q in /metrics output", name)
		}
	}

	// thundersnap-specific gauges with their expected values.
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
