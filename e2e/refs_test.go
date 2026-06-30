// Package e2e contains end-to-end tests for thundersnap.
package e2e

import (
	"path/filepath"
	"testing"

	"github.com/tailscale/thundersnap/frameid"
	"github.com/tailscale/thundersnap/frames"
	"github.com/tailscale/thundersnap/refs"
	"github.com/tailscale/thundersnap/snaphash"
)

// TestRefsPackage tests the refs package functionality without requiring btrfs.
// This is a unit-level test that runs as part of e2e because it tests the
// packages that will be used by the full e2e tests.
func TestRefsPackage(t *testing.T) {
	dir := t.TempDir()
	store := refs.NewStore(dir)

	// Create a UUID for testing
	uuid1 := frameid.MustNew()
	uuid2 := frameid.MustNew()

	// Test create
	t.Run("create", func(t *testing.T) {
		if err := store.Create("test-ref", uuid1); err != nil {
			t.Fatalf("Create: %v", err)
		}

		if !store.Exists("test-ref") {
			t.Error("ref doesn't exist after create")
		}
	})

	// Test get
	t.Run("get", func(t *testing.T) {
		ref, err := store.Get("test-ref")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}

		if ref.UUID != uuid1 {
			t.Errorf("UUID = %v, want %v", ref.UUID, uuid1)
		}

		if len(ref.Reflog) != 1 {
			t.Errorf("Reflog length = %d, want 1", len(ref.Reflog))
		}
	})

	// Test move
	t.Run("move", func(t *testing.T) {
		if err := store.Move("test-ref", uuid2); err != nil {
			t.Fatalf("Move: %v", err)
		}

		ref, _ := store.Get("test-ref")
		if ref.UUID != uuid2 {
			t.Errorf("UUID after move = %v, want %v", ref.UUID, uuid2)
		}

		if len(ref.Reflog) != 2 {
			t.Errorf("Reflog length after move = %d, want 2", len(ref.Reflog))
		}
	})

	// Test autorun
	t.Run("autorun", func(t *testing.T) {
		argv := []string{"/bin/sh", "-c", "echo hello"}
		if err := store.SetAutorun("test-ref", argv); err != nil {
			t.Fatalf("SetAutorun: %v", err)
		}

		ref, _ := store.Get("test-ref")
		if len(ref.Autorun) != 3 {
			t.Errorf("Autorun length = %d, want 3", len(ref.Autorun))
		}

		// Clear autorun
		if err := store.SetAutorun("test-ref", nil); err != nil {
			t.Fatalf("SetAutorun clear: %v", err)
		}

		ref, _ = store.Get("test-ref")
		if len(ref.Autorun) != 0 {
			t.Errorf("Autorun length after clear = %d, want 0", len(ref.Autorun))
		}
	})

	// Test list
	t.Run("list", func(t *testing.T) {
		// Create another ref
		store.Create("another-ref", uuid1)

		names, err := store.List()
		if err != nil {
			t.Fatalf("List: %v", err)
		}

		if len(names) != 2 {
			t.Errorf("List length = %d, want 2", len(names))
		}
	})

	// Test delete
	t.Run("delete", func(t *testing.T) {
		if err := store.Delete("test-ref"); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		if store.Exists("test-ref") {
			t.Error("ref still exists after delete")
		}
	})

	// Test ID directory
	t.Run("id_dir", func(t *testing.T) {
		// ID dir doesn't exist initially
		exists, err := store.IDDirExists("another-ref")
		if err != nil {
			t.Fatalf("IDDirExists: %v", err)
		}
		if exists {
			t.Error("IDDirExists = true for empty dir")
		}

		// Create ID dir
		if err := store.EnsureIDDir("another-ref"); err != nil {
			t.Fatalf("EnsureIDDir: %v", err)
		}
	})
}

// TestFramesPackage tests the frames package functionality.
func TestFramesPackage(t *testing.T) {
	dir := t.TempDir()
	store := frames.NewStore(dir)

	uuid := frameid.MustNew()
	rootfs := snaphash.Sum([]byte("test-rootfs"))
	home := snaphash.Sum([]byte("test-home"))

	// Test create
	t.Run("create", func(t *testing.T) {
		frame := &frames.Frame{
			Rootfs:    rootfs,
			Home:      home,
			Isolation: "container",
		}

		if err := store.Create(uuid, frame); err != nil {
			t.Fatalf("Create: %v", err)
		}

		if !store.Exists(uuid) {
			t.Error("frame doesn't exist after create")
		}
	})

	// Test get
	t.Run("get", func(t *testing.T) {
		frame, err := store.Get(uuid)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}

		if frame.Rootfs != rootfs {
			t.Errorf("Rootfs mismatch")
		}
		if frame.Home != home {
			t.Errorf("Home mismatch")
		}
		if frame.Isolation != "container" {
			t.Errorf("Isolation = %q, want %q", frame.Isolation, "container")
		}
	})

	// Test history
	t.Run("history", func(t *testing.T) {
		snap1 := snaphash.Sum([]byte("snap1"))
		snap2 := snaphash.Sum([]byte("snap2"))

		if err := store.AddHistoryEntry(uuid, snap1, "first snapshot"); err != nil {
			t.Fatalf("AddHistoryEntry 1: %v", err)
		}

		if err := store.AddHistoryEntry(uuid, snap2, "second snapshot"); err != nil {
			t.Fatalf("AddHistoryEntry 2: %v", err)
		}

		frame, _ := store.Get(uuid)
		if len(frame.History) != 2 {
			t.Fatalf("History length = %d, want 2", len(frame.History))
		}

		// Most recent first
		if frame.History[0].Snap != snap2 {
			t.Errorf("History[0] is not snap2")
		}
		if frame.History[1].Snap != snap1 {
			t.Errorf("History[1] is not snap1")
		}
	})

	// Test taints
	t.Run("taints", func(t *testing.T) {
		if err := store.AddTaint(uuid, "network"); err != nil {
			t.Fatalf("AddTaint: %v", err)
		}

		frame, _ := store.Get(uuid)
		if len(frame.Taints) != 1 || frame.Taints[0] != "network" {
			t.Errorf("Taints = %v, want [network]", frame.Taints)
		}
	})

	// Test list
	t.Run("list", func(t *testing.T) {
		uuids, err := store.List()
		if err != nil {
			t.Fatalf("List: %v", err)
		}

		if len(uuids) != 1 {
			t.Errorf("List length = %d, want 1", len(uuids))
		}
	})

	// Test delete
	t.Run("delete", func(t *testing.T) {
		if err := store.Delete(uuid); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		if store.Exists(uuid) {
			t.Error("frame still exists after delete")
		}
	})
}

// TestSnaphashEncoding tests the snap hash encoding.
func TestSnaphashEncoding(t *testing.T) {
	// Test round-trip
	hash := snaphash.Sum([]byte("test data"))
	encoded := snaphash.Encode(hash)

	if len(encoded) != snaphash.EncodedSize {
		t.Errorf("Encoded length = %d, want %d", len(encoded), snaphash.EncodedSize)
	}

	// First char should not be - or _
	if encoded[0] == '-' || encoded[0] == '_' {
		t.Errorf("First char is %c, should not be - or _", encoded[0])
	}

	// Decode back
	decoded, err := snaphash.Decode(encoded)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded != hash {
		t.Errorf("Round-trip failed")
	}
}

// TestFrameidGeneration tests UUID generation.
func TestFrameidGeneration(t *testing.T) {
	uuid1 := frameid.MustNew()
	uuid2 := frameid.MustNew()

	if uuid1 == uuid2 {
		t.Error("Two UUIDs should not be equal")
	}

	if frameid.IsZero(uuid1) {
		t.Error("Generated UUID should not be zero")
	}

	// Test parsing
	s := uuid1.String()
	parsed, err := frameid.Parse(s)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if parsed != uuid1 {
		t.Error("Parsed UUID doesn't match original")
	}
}

// TestRefNameValidation tests ref name validation.
func TestRefNameValidation(t *testing.T) {
	valid := []string{
		"foo",
		"foo-bar",
		"foo_bar",
		"foo.bar",
		"Foo123",
		"a",
		"a1",
	}

	for _, name := range valid {
		if err := refs.ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []string{
		"",
		"-foo",    // starts with dash
		"_foo",    // starts with underscore
		".foo",    // starts with dot
		"foo..bar", // consecutive dots
		"foo/bar", // contains slash
	}

	for _, name := range invalid {
		if err := refs.ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", name)
		}
	}
}

// TestFramePath tests frame path generation.
func TestFramePath(t *testing.T) {
	dir := t.TempDir()
	store := frames.NewStore(dir)

	uuid := frameid.MustNew()
	path := store.Path(uuid)

	expected := filepath.Join(dir, "fs", uuid.String())
	if path != expected {
		t.Errorf("Path = %q, want %q", path, expected)
	}
}
