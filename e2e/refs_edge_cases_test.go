//go:build e2e

// Package e2e contains end-to-end tests for thundersnap ref edge cases.
//
// These tests exercise the refs package functionality with edge cases that
// could cause issues in production.
package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tailscale/thundersnap/frameid"
	"github.com/tailscale/thundersnap/refs"
)

// TestRefDeleteWithNonEmptyIDDirE2E tests that the ID directory check works correctly.
// This is a security-critical edge case: the /id directory contains private
// state like keys that should not be accidentally deleted.
func TestRefDeleteWithNonEmptyIDDirE2E(t *testing.T) {
	dir := t.TempDir()
	store := refs.NewStore(dir)

	uuid := frameid.MustNew()

	// Create a ref
	if err := store.Create("test-ref", uuid); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// ID dir doesn't exist initially
	exists, err := store.IDDirExists("test-ref")
	if err != nil {
		t.Fatalf("IDDirExists: %v", err)
	}
	if exists {
		t.Error("IDDirExists should be false for new ref")
	}

	// Create ID dir with a file (simulating secrets)
	if err := store.EnsureIDDir("test-ref"); err != nil {
		t.Fatalf("EnsureIDDir: %v", err)
	}
	secretPath := filepath.Join(dir, "id", "test-ref", "secret.key")
	if err := os.WriteFile(secretPath, []byte("secret-key-content"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Now IDDirExists should be true
	exists, err = store.IDDirExists("test-ref")
	if err != nil {
		t.Fatalf("IDDirExists after file: %v", err)
	}
	if !exists {
		t.Error("IDDirExists should be true when file exists")
	}

	// In a real scenario, thundersnapd would check IDDirExists before Delete
	// and refuse unless force=true. Here we verify the check works.
	t.Log("ID directory properly detected as non-empty")

	// RemoveIDDir should clean it up
	if err := store.RemoveIDDir("test-ref"); err != nil {
		t.Fatalf("RemoveIDDir: %v", err)
	}

	exists, _ = store.IDDirExists("test-ref")
	if exists {
		t.Error("IDDirExists should be false after RemoveIDDir")
	}

	// Now deletion of the ref should be safe
	if err := store.Delete("test-ref"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

// TestRefMoveUpdatesReflogE2E tests that moving a ref creates proper reflog entries.
func TestRefMoveUpdatesReflogE2E(t *testing.T) {
	dir := t.TempDir()
	store := refs.NewStore(dir)

	uuid1 := frameid.MustNew()
	uuid2 := frameid.MustNew()
	uuid3 := frameid.MustNew()

	// Create ref
	if err := store.Create("testref", uuid1); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Initial reflog should have 1 entry
	ref, _ := store.Get("testref")
	if len(ref.Reflog) != 1 {
		t.Errorf("initial reflog should have 1 entry, got %d", len(ref.Reflog))
	}
	if ref.Reflog[0].UUID != uuid1 {
		t.Errorf("initial reflog entry should be uuid1")
	}

	// Move to uuid2
	if err := store.Move("testref", uuid2); err != nil {
		t.Fatalf("Move to uuid2: %v", err)
	}

	ref, _ = store.Get("testref")
	if len(ref.Reflog) != 2 {
		t.Errorf("reflog after first move should have 2 entries, got %d", len(ref.Reflog))
	}
	// Newest first
	if ref.Reflog[0].UUID != uuid2 {
		t.Errorf("newest reflog entry should be uuid2")
	}
	if ref.Reflog[1].UUID != uuid1 {
		t.Errorf("second reflog entry should be uuid1")
	}

	// Move to uuid3
	if err := store.Move("testref", uuid3); err != nil {
		t.Fatalf("Move to uuid3: %v", err)
	}

	ref, _ = store.Get("testref")
	if len(ref.Reflog) != 3 {
		t.Errorf("reflog after second move should have 3 entries, got %d", len(ref.Reflog))
	}

	// Move back to uuid1 (should add 4th entry, not deduplicate)
	if err := store.Move("testref", uuid1); err != nil {
		t.Fatalf("Move back to uuid1: %v", err)
	}

	ref, _ = store.Get("testref")
	if len(ref.Reflog) != 4 {
		t.Errorf("reflog after move back should have 4 entries, got %d", len(ref.Reflog))
	}
	if ref.Reflog[0].UUID != uuid1 {
		t.Errorf("newest reflog entry should be uuid1")
	}
	// uuid1 should appear twice in the reflog
	count := 0
	for _, e := range ref.Reflog {
		if e.UUID == uuid1 {
			count++
		}
	}
	if count != 2 {
		t.Errorf("uuid1 should appear twice in reflog, found %d times", count)
	}
}

// TestRefMoveToSameUUIDAddsEntry tests that moving to the same UUID still
// adds a reflog entry (idempotent operation is still tracked).
func TestRefMoveToSameUUIDAddsEntry(t *testing.T) {
	dir := t.TempDir()
	store := refs.NewStore(dir)

	uuid := frameid.MustNew()

	if err := store.Create("sameref", uuid); err != nil {
		t.Fatalf("Create: %v", err)
	}

	ref, _ := store.Get("sameref")
	initialLen := len(ref.Reflog)
	t.Logf("Initial reflog length: %d", initialLen)

	// Move to the SAME UUID
	if err := store.Move("sameref", uuid); err != nil {
		t.Fatalf("Move to same UUID: %v", err)
	}

	ref, _ = store.Get("sameref")
	if len(ref.Reflog) != initialLen+1 {
		t.Errorf("reflog should gain entry even for same-UUID move: before=%d, after=%d",
			initialLen, len(ref.Reflog))
	}

	// Both entries should have the same UUID
	for i, e := range ref.Reflog {
		if e.UUID != uuid {
			t.Errorf("reflog[%d] should be uuid, got different", i)
		}
	}
}

// TestRefAutorunClearWithNilVsEmpty tests that both nil and empty slice clear autorun.
func TestRefAutorunClearWithNilVsEmpty(t *testing.T) {
	dir := t.TempDir()
	store := refs.NewStore(dir)

	uuid := frameid.MustNew()

	if err := store.Create("autoref", uuid); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Set autorun
	argv := []string{"/bin/sh", "-c", "echo hello"}
	if err := store.SetAutorun("autoref", argv); err != nil {
		t.Fatalf("SetAutorun: %v", err)
	}

	ref, _ := store.Get("autoref")
	if len(ref.Autorun) != 3 {
		t.Fatalf("autorun not set correctly")
	}

	// Clear with nil
	if err := store.SetAutorun("autoref", nil); err != nil {
		t.Fatalf("SetAutorun nil: %v", err)
	}

	ref, _ = store.Get("autoref")
	if len(ref.Autorun) != 0 {
		t.Errorf("autorun should be empty after nil, got %d elements", len(ref.Autorun))
	}

	// Set again
	if err := store.SetAutorun("autoref", argv); err != nil {
		t.Fatalf("SetAutorun again: %v", err)
	}

	ref, _ = store.Get("autoref")
	if len(ref.Autorun) != 3 {
		t.Fatalf("autorun not set correctly second time")
	}

	// Clear with empty slice
	if err := store.SetAutorun("autoref", []string{}); err != nil {
		t.Fatalf("SetAutorun empty: %v", err)
	}

	ref, _ = store.Get("autoref")
	if len(ref.Autorun) != 0 {
		t.Errorf("autorun should be empty after empty slice, got %d elements", len(ref.Autorun))
	}
}

// TestRefSequentialMoves tests that many sequential moves don't corrupt state.
func TestRefSequentialMoves(t *testing.T) {
	dir := t.TempDir()
	store := refs.NewStore(dir)

	uuid1 := frameid.MustNew()
	uuid2 := frameid.MustNew()

	if err := store.Create("seqref", uuid1); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Do many moves
	const numMoves = 50
	for i := 0; i < numMoves; i++ {
		target := uuid1
		if i%2 == 1 {
			target = uuid2
		}
		if err := store.Move("seqref", target); err != nil {
			t.Fatalf("Move %d: %v", i, err)
		}
	}

	ref, err := store.Get("seqref")
	if err != nil {
		t.Fatalf("Get after moves: %v", err)
	}

	expectedLen := numMoves + 1 // initial + moves
	if len(ref.Reflog) != expectedLen {
		t.Errorf("reflog should have %d entries, got %d", expectedLen, len(ref.Reflog))
	}

	// Final UUID depends on last move: index numMoves-1
	// If (numMoves-1) is odd, it's uuid2; if even, it's uuid1
	lastIndex := numMoves - 1
	if lastIndex%2 == 1 {
		if ref.UUID != uuid2 {
			t.Errorf("final UUID should be uuid2, got %v", ref.UUID)
		}
	} else {
		if ref.UUID != uuid1 {
			t.Errorf("final UUID should be uuid1, got %v", ref.UUID)
		}
	}

	// Verify reflog is in order (newest first)
	// Check timestamps are in descending order
	for i := 0; i < len(ref.Reflog)-1; i++ {
		if ref.Reflog[i].Time.Before(ref.Reflog[i+1].Time) {
			t.Errorf("reflog not in descending time order at index %d", i)
		}
	}
}

// TestRefPathTraversalAttempts tests that path traversal in ref names is rejected.
func TestRefPathTraversalAttempts(t *testing.T) {
	dir := t.TempDir()
	store := refs.NewStore(dir)

	uuid := frameid.MustNew()

	badNames := []string{
		"../escape",
		"foo/../bar",
		"foo/bar",
		"..",
		"foo..bar",
		"./current",
	}

	for _, name := range badNames {
		err := store.Create(name, uuid)
		if err == nil {
			t.Errorf("Create(%q) should fail for path traversal", name)
		}
	}
}

// TestRefOperationsOnNonexistent tests that operations on nonexistent refs
// return appropriate errors.
func TestRefOperationsOnNonexistent(t *testing.T) {
	dir := t.TempDir()
	store := refs.NewStore(dir)

	uuid := frameid.MustNew()

	// Get nonexistent
	_, err := store.Get("nonexistent")
	if err != refs.ErrRefNotFound {
		t.Errorf("Get nonexistent: got %v, want ErrRefNotFound", err)
	}

	// Move nonexistent
	err = store.Move("nonexistent", uuid)
	if err != refs.ErrRefNotFound {
		t.Errorf("Move nonexistent: got %v, want ErrRefNotFound", err)
	}

	// Delete nonexistent
	err = store.Delete("nonexistent")
	if err != refs.ErrRefNotFound {
		t.Errorf("Delete nonexistent: got %v, want ErrRefNotFound", err)
	}

	// SetAutorun nonexistent
	err = store.SetAutorun("nonexistent", []string{"/bin/sh"})
	if err != refs.ErrRefNotFound {
		t.Errorf("SetAutorun nonexistent: got %v, want ErrRefNotFound", err)
	}

	// Exists nonexistent
	if store.Exists("nonexistent") {
		t.Error("Exists should return false for nonexistent")
	}
}

// TestRefCreateDuplicateE2E tests that creating a ref with an existing name fails.
func TestRefCreateDuplicateE2E(t *testing.T) {
	dir := t.TempDir()
	store := refs.NewStore(dir)

	uuid1 := frameid.MustNew()
	uuid2 := frameid.MustNew()

	if err := store.Create("dupref", uuid1); err != nil {
		t.Fatalf("Create first: %v", err)
	}

	// Try to create with same name
	err := store.Create("dupref", uuid2)
	if err != refs.ErrRefExists {
		t.Errorf("Create duplicate: got %v, want ErrRefExists", err)
	}

	// Original should be unchanged
	ref, _ := store.Get("dupref")
	if ref.UUID != uuid1 {
		t.Errorf("original ref UUID changed")
	}
}

// TestRefMaxNameLength tests the 128 character limit for ref names.
func TestRefMaxNameLength(t *testing.T) {
	dir := t.TempDir()
	store := refs.NewStore(dir)

	uuid := frameid.MustNew()

	// Create a name with exactly 128 characters
	name128 := ""
	for len(name128) < 128 {
		name128 += "a"
	}
	if len(name128) != 128 {
		t.Fatalf("Setup error: name length = %d", len(name128))
	}

	// 128 chars should work
	if err := store.Create(name128, uuid); err != nil {
		t.Errorf("Create 128-char name: %v", err)
	}

	// 129 chars should fail
	name129 := name128 + "a"
	err := store.Create(name129, uuid)
	if err == nil {
		t.Error("Create 129-char name should fail")
	}
}

// TestRefEmptyName tests that empty ref names are rejected.
func TestRefEmptyName(t *testing.T) {
	dir := t.TempDir()
	store := refs.NewStore(dir)

	uuid := frameid.MustNew()

	err := store.Create("", uuid)
	if err == nil {
		t.Error("Create empty name should fail")
	}
}

// TestRefListEmpty tests listing refs when none exist.
func TestRefListEmpty(t *testing.T) {
	dir := t.TempDir()
	store := refs.NewStore(dir)

	names, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("List empty should return empty slice, got %d", len(names))
	}
}

// TestRefListAfterDeletes tests that List correctly reflects deletions.
func TestRefListAfterDeletes(t *testing.T) {
	dir := t.TempDir()
	store := refs.NewStore(dir)

	uuid := frameid.MustNew()

	// Create several refs
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := store.Create(name, uuid); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}

	names, _ := store.List()
	if len(names) != 3 {
		t.Errorf("List after creates: got %d, want 3", len(names))
	}

	// Delete one
	if err := store.Delete("beta"); err != nil {
		t.Fatalf("Delete beta: %v", err)
	}

	names, _ = store.List()
	if len(names) != 2 {
		t.Errorf("List after delete: got %d, want 2", len(names))
	}

	// beta should not be in list
	for _, n := range names {
		if n == "beta" {
			t.Error("beta should not be in list after delete")
		}
	}
}
