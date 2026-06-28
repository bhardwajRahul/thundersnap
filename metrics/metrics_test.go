package metrics

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tailscale/thundersnap/frameid"
	"github.com/tailscale/thundersnap/refs"
)

func TestCountSnaps(t *testing.T) {
	// Non-existent directory counts as zero.
	if n := CountSnaps(filepath.Join(t.TempDir(), "nope")); n != 0 {
		t.Errorf("CountSnaps(missing) = %d, want 0", n)
	}

	dir := t.TempDir()
	// Empty directory counts as zero.
	if n := CountSnaps(dir); n != 0 {
		t.Errorf("CountSnaps(empty) = %d, want 0", n)
	}

	// Two .tsm manifests, plus noise that must not be counted.
	for _, name := range []string{"a.tsm", "b.tsm"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "c.tsc"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if n := CountSnaps(dir); n != 2 {
		t.Errorf("CountSnaps = %d, want 2", n)
	}
}

// TestCountSnapsCountsTsmDir documents that CountSnaps matches on the .tsm name
// suffix and does not require a regular file: a directory named foo.tsm is
// counted. In practice snaps are .tsm files, so this is benign, but the test
// pins the current behavior.
func TestCountSnapsCountsTsmDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "snap.tsm"), 0755); err != nil {
		t.Fatal(err)
	}
	if n := CountSnaps(dir); n != 1 {
		t.Errorf("CountSnaps(dir with foo.tsm/) = %d, want 1", n)
	}
}

func TestCountFrames(t *testing.T) {
	// Non-existent fsDir counts as zero.
	if n := CountFrames(filepath.Join(t.TempDir(), "nope")); n != 0 {
		t.Errorf("CountFrames(missing) = %d, want 0", n)
	}

	fsDir := t.TempDir()
	// Empty fsDir counts as zero.
	if n := CountFrames(fsDir); n != 0 {
		t.Errorf("CountFrames(empty) = %d, want 0", n)
	}

	// user/frame1/ with frame1.jsonc -> counts.
	user := filepath.Join(fsDir, "user")
	if err := os.MkdirAll(filepath.Join(user, "frame1"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(user, "frame1.jsonc"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	// A bare directory with no matching .jsonc -> not counted.
	if err := os.MkdirAll(filepath.Join(user, "bare"), 0755); err != nil {
		t.Fatal(err)
	}
	// A .jsonc whose <name>/ is a file, not a dir -> not counted (the entry
	// must be a directory).
	if err := os.WriteFile(filepath.Join(user, "asfile"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(user, "asfile.jsonc"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if n := CountFrames(fsDir); n != 1 {
		t.Errorf("CountFrames = %d, want 1", n)
	}

	// A non-directory at the user level is skipped.
	if err := os.WriteFile(filepath.Join(fsDir, "loose.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if n := CountFrames(fsDir); n != 1 {
		t.Errorf("CountFrames after loose file = %d, want 1", n)
	}
}

func TestCountRefs(t *testing.T) {
	// Nil store counts as zero.
	if n := CountRefs(nil); n != 0 {
		t.Errorf("CountRefs(nil) = %d, want 0", n)
	}

	store := refs.NewStore(t.TempDir())
	// Empty store counts as zero.
	if n := CountRefs(store); n != 0 {
		t.Errorf("CountRefs(empty) = %d, want 0", n)
	}

	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := store.Create(name, frameid.MustNew()); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}
	if n := CountRefs(store); n != 3 {
		t.Errorf("CountRefs = %d, want 3", n)
	}
}

// TestCollectNilClosures verifies the collector reports zero for the
// session/VM gauges when the Sources closures are nil, and counts disk state
// correctly.
func TestCollectNilClosures(t *testing.T) {
	fsDir := t.TempDir()
	snapsDir := t.TempDir()
	user := filepath.Join(fsDir, "user")
	if err := os.MkdirAll(filepath.Join(user, "f1"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(user, "f1.jsonc"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapsDir, "a.tsm"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	store := refs.NewStore(t.TempDir())
	if err := store.Create("r1", frameid.MustNew()); err != nil {
		t.Fatal(err)
	}

	c := newCollector(Sources{
		FsDir:    fsDir,
		SnapsDir: snapsDir,
		Refs:     store,
		// RunningSessions and RunningVMs deliberately nil.
	})

	// Verify the full collector via a registry: nil closures must yield 0.
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	expectGauge(t, reg, "thundersnap_frames_total", 1)
	expectGauge(t, reg, "thundersnap_snaps_total", 1)
	expectGauge(t, reg, "thundersnap_refs_total", 1)
	expectGauge(t, reg, "thundersnap_running_sessions", 0)
	expectGauge(t, reg, "thundersnap_running_vms", 0)
}

// TestCollectClosures verifies the session/VM closures are read on each scrape.
func TestCollectClosures(t *testing.T) {
	c := newCollector(Sources{
		FsDir:           t.TempDir(),
		SnapsDir:        t.TempDir(),
		RunningSessions: func() int { return 4 },
		RunningVMs:      func() int { return 2 },
	})
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	expectGauge(t, reg, "thundersnap_running_sessions", 4)
	expectGauge(t, reg, "thundersnap_running_vms", 2)
}

func TestNewRegistryDoubleRegister(t *testing.T) {
	src := Sources{FsDir: t.TempDir(), SnapsDir: t.TempDir()}
	reg, err := NewRegistry(src)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	// Registering thundersnap's collector again must fail (duplicate descs).
	if err := reg.Register(newCollector(src)); err == nil {
		t.Error("re-registering collector should fail")
	}
}

func TestNewHandler(t *testing.T) {
	h, err := NewHandler(Sources{FsDir: t.TempDir(), SnapsDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	if h == nil {
		t.Error("NewHandler returned nil handler")
	}
}

// expectGauge gathers reg and asserts the named gauge has value want.
func expectGauge(t *testing.T, reg *prometheus.Registry, name string, want float64) {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if g := m.GetGauge(); g != nil {
				if got := g.GetValue(); got != want {
					t.Errorf("%s = %v, want %v", name, got, want)
				}
				return
			}
		}
	}
	t.Errorf("gauge %s not found", name)
}
