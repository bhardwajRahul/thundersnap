// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package metrics

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tailscale/thundersnap/frameid"
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

	// fs/<user>/<uuid>.jsonc -> counts (the sidecar's stem must be a UUID).
	uuid := frameid.MustNew().String()
	user := filepath.Join(fsDir, "user")
	if err := os.MkdirAll(filepath.Join(user, uuid), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(user, uuid+".jsonc"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	// A legacy-layout sidecar whose stem is NOT a UUID -> ignored.
	if err := os.WriteFile(filepath.Join(user, "frame1.jsonc"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	// A bare directory with no matching .jsonc -> not counted.
	if err := os.MkdirAll(filepath.Join(user, "bare"), 0755); err != nil {
		t.Fatal(err)
	}
	if n := CountFrames(fsDir); n != 1 {
		t.Errorf("CountFrames = %d, want 1", n)
	}

	// A second user with its own UUID sidecar -> counted across users.
	user2 := filepath.Join(fsDir, "user2")
	uuid2 := frameid.MustNew().String()
	if err := os.MkdirAll(user2, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(user2, uuid2+".jsonc"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if n := CountFrames(fsDir); n != 2 {
		t.Errorf("CountFrames across users = %d, want 2", n)
	}

	// A non-directory at the user level is skipped.
	if err := os.WriteFile(filepath.Join(fsDir, "loose.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if n := CountFrames(fsDir); n != 2 {
		t.Errorf("CountFrames after loose file = %d, want 2", n)
	}
}

// TestCollectNilClosures verifies the collector reports zero for the
// session/VM gauges when the Sources closures are nil, and counts disk state
// correctly.
func TestCollectNilClosures(t *testing.T) {
	fsDir := t.TempDir()
	snapsDir := t.TempDir()
	user := filepath.Join(fsDir, "user")
	uuid := frameid.MustNew().String()
	if err := os.MkdirAll(filepath.Join(user, uuid), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(user, uuid+".jsonc"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapsDir, "a.tsm"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	c := newCollector(Sources{
		FsDir:    fsDir,
		SnapsDir: snapsDir,
		Refs:     func() int { return 1 },
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
