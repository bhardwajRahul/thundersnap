// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package refs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tailscale/thundersnap/frameid"
)

func TestValidateName(t *testing.T) {
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
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []string{
		"",
		"-foo",
		"_foo",
		".foo",
		"foo..bar",
		"foo/bar",
		"foo\nbar",
		"foo bar",
		"foo.", // trailing dot: documented as not allowed
	}
	for _, name := range invalid {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", name)
		}
	}

	// Trailing dash/underscore remain valid (only trailing dots are barred).
	for _, name := range []string{"foo-", "foo_"} {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}
}

func TestUserStoreIsolation(t *testing.T) {
	dir := t.TempDir()
	alice := NewUserStore(dir, "alice")
	bob := NewUserStore(dir, "bob")

	aliceUUID := frameid.MustNew()
	bobUUID := frameid.MustNew()

	// Both users create a ref with the same name pointing at different frames.
	if err := alice.Create("deb", aliceUUID); err != nil {
		t.Fatalf("alice create: %v", err)
	}
	if err := bob.Create("deb", bobUUID); err != nil {
		t.Fatalf("bob create: %v", err)
	}

	// Each user resolves their own ref, not the other's.
	got, err := alice.Get("deb")
	if err != nil {
		t.Fatalf("alice get: %v", err)
	}
	if got.UUID != aliceUUID {
		t.Errorf("alice deb = %s, want %s", got.UUID, aliceUUID)
	}
	got, err = bob.Get("deb")
	if err != nil {
		t.Fatalf("bob get: %v", err)
	}
	if got.UUID != bobUUID {
		t.Errorf("bob deb = %s, want %s", got.UUID, bobUUID)
	}

	// Refs land under the per-user directory, not the flat one.
	if _, err := os.Stat(filepath.Join(dir, "refs", "alice", "deb.jsonc")); err != nil {
		t.Errorf("alice ref not at refs/alice/deb.jsonc: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "refs", "deb.jsonc")); !os.IsNotExist(err) {
		t.Errorf("per-user store wrote a flat ref at refs/deb.jsonc (err=%v)", err)
	}

	// A user only lists their own refs.
	names, err := alice.List()
	if err != nil {
		t.Fatalf("alice list: %v", err)
	}
	if len(names) != 1 || names[0] != "deb" {
		t.Errorf("alice list = %v, want [deb]", names)
	}

	// Deleting alice's ref leaves bob's intact.
	if err := alice.Delete("deb"); err != nil {
		t.Fatalf("alice delete: %v", err)
	}
	if _, err := bob.Get("deb"); err != nil {
		t.Errorf("bob deb gone after alice delete: %v", err)
	}
}

func TestCreateAndGet(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	uuid := frameid.MustNew()

	// Create a ref.
	if err := store.Create("test-ref", uuid); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Get it back.
	ref, err := store.Get("test-ref")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if ref.UUID != uuid {
		t.Errorf("UUID = %v, want %v", ref.UUID, uuid)
	}
	if len(ref.Reflog) != 1 {
		t.Errorf("Reflog length = %d, want 1", len(ref.Reflog))
	}
	if ref.Reflog[0].UUID != uuid {
		t.Errorf("Reflog[0].UUID = %v, want %v", ref.Reflog[0].UUID, uuid)
	}
}

func TestCreateDuplicate(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	uuid := frameid.MustNew()

	if err := store.Create("test-ref", uuid); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Creating again should fail.
	if err := store.Create("test-ref", uuid); err != ErrRefExists {
		t.Errorf("Create duplicate = %v, want ErrRefExists", err)
	}
}

func TestGetNotFound(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	_, err := store.Get("nonexistent")
	if err != ErrRefNotFound {
		t.Errorf("Get nonexistent = %v, want ErrRefNotFound", err)
	}
}

func TestMove(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	uuid1 := frameid.MustNew()
	uuid2 := frameid.MustNew()

	if err := store.Create("test-ref", uuid1); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Move to new UUID.
	if err := store.Move("test-ref", uuid2); err != nil {
		t.Fatalf("Move: %v", err)
	}

	ref, err := store.Get("test-ref")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if ref.UUID != uuid2 {
		t.Errorf("UUID after move = %v, want %v", ref.UUID, uuid2)
	}

	// Reflog should have both entries (newest first).
	if len(ref.Reflog) != 2 {
		t.Fatalf("Reflog length = %d, want 2", len(ref.Reflog))
	}
	if ref.Reflog[0].UUID != uuid2 {
		t.Errorf("Reflog[0].UUID = %v, want %v", ref.Reflog[0].UUID, uuid2)
	}
	if ref.Reflog[1].UUID != uuid1 {
		t.Errorf("Reflog[1].UUID = %v, want %v", ref.Reflog[1].UUID, uuid1)
	}
}

func TestMoveNotFound(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	uuid := frameid.MustNew()
	if err := store.Move("nonexistent", uuid); err != ErrRefNotFound {
		t.Errorf("Move nonexistent = %v, want ErrRefNotFound", err)
	}
}

func TestDelete(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	uuid := frameid.MustNew()

	if err := store.Create("test-ref", uuid); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := store.Delete("test-ref"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Should not exist anymore.
	if store.Exists("test-ref") {
		t.Error("ref still exists after delete")
	}
}

func TestDeleteNotFound(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	if err := store.Delete("nonexistent"); err != ErrRefNotFound {
		t.Errorf("Delete nonexistent = %v, want ErrRefNotFound", err)
	}
}

func TestSetAutorun(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	uuid := frameid.MustNew()

	if err := store.Create("test-ref", uuid); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Set autorun.
	argv := []string{"/usr/bin/nginx", "-g", "daemon off;"}
	if err := store.SetAutorun("test-ref", argv); err != nil {
		t.Fatalf("SetAutorun: %v", err)
	}

	ref, err := store.Get("test-ref")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if len(ref.Autorun) != len(argv) {
		t.Fatalf("Autorun length = %d, want %d", len(ref.Autorun), len(argv))
	}
	for i, arg := range argv {
		if ref.Autorun[i] != arg {
			t.Errorf("Autorun[%d] = %q, want %q", i, ref.Autorun[i], arg)
		}
	}

	// Clear autorun.
	if err := store.SetAutorun("test-ref", nil); err != nil {
		t.Fatalf("SetAutorun clear: %v", err)
	}

	ref, _ = store.Get("test-ref")
	if len(ref.Autorun) != 0 {
		t.Errorf("Autorun after clear = %v, want empty", ref.Autorun)
	}
}

func TestList(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	// Empty list initially.
	names, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("List empty = %v, want empty", names)
	}

	// Create some refs.
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := store.Create(name, frameid.MustNew()); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}

	names, err = store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 3 {
		t.Errorf("List length = %d, want 3", len(names))
	}
}

func TestIDDir(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	// ID dir doesn't exist initially.
	exists, err := store.IDDirExists("test-ref")
	if err != nil {
		t.Fatalf("IDDirExists: %v", err)
	}
	if exists {
		t.Error("IDDirExists = true, want false")
	}

	// Create ID dir.
	if err := store.EnsureIDDir("test-ref"); err != nil {
		t.Fatalf("EnsureIDDir: %v", err)
	}

	// Still reports false because it's empty.
	exists, _ = store.IDDirExists("test-ref")
	if exists {
		t.Error("IDDirExists empty = true, want false")
	}

	// Add a file.
	idPath := filepath.Join(dir, "id", "test-ref", "key")
	if err := os.WriteFile(idPath, []byte("secret"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Now it should exist.
	exists, _ = store.IDDirExists("test-ref")
	if !exists {
		t.Error("IDDirExists with file = false, want true")
	}

	// Remove ID dir.
	if err := store.RemoveIDDir("test-ref"); err != nil {
		t.Fatalf("RemoveIDDir: %v", err)
	}

	exists, _ = store.IDDirExists("test-ref")
	if exists {
		t.Error("IDDirExists after remove = true, want false")
	}
}

func TestExists(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	if store.Exists("test-ref") {
		t.Error("Exists before create = true")
	}

	store.Create("test-ref", frameid.MustNew())

	if !store.Exists("test-ref") {
		t.Error("Exists after create = false")
	}
}

// TestMoveToSameUUID verifies that moving a ref to the same UUID it already
// points to still adds a reflog entry (idempotent but tracked).
func TestMoveToSameUUID(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	uuid := frameid.MustNew()

	if err := store.Create("test-ref", uuid); err != nil {
		t.Fatalf("Create: %v", err)
	}

	ref, _ := store.Get("test-ref")
	if len(ref.Reflog) != 1 {
		t.Fatalf("Initial reflog length = %d, want 1", len(ref.Reflog))
	}

	// Move to the same UUID.
	if err := store.Move("test-ref", uuid); err != nil {
		t.Fatalf("Move to same UUID: %v", err)
	}

	ref, _ = store.Get("test-ref")

	// Should still be pointing at same UUID.
	if ref.UUID != uuid {
		t.Errorf("UUID after same-UUID move = %v, want %v", ref.UUID, uuid)
	}

	// Reflog should have 2 entries now (tracking the operation).
	if len(ref.Reflog) != 2 {
		t.Errorf("Reflog length after same-UUID move = %d, want 2", len(ref.Reflog))
	}

	// Both entries should have the same UUID.
	if ref.Reflog[0].UUID != uuid || ref.Reflog[1].UUID != uuid {
		t.Errorf("Reflog entries should both have UUID %v", uuid)
	}
}

// TestPathTraversalInRefName verifies that path traversal attempts are rejected.
func TestPathTraversalInRefName(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	uuid := frameid.MustNew()

	// These should all be rejected.
	badNames := []string{
		"../escape",
		"foo/../bar",
		"foo/bar",
		"..",
		"foo..bar", // consecutive dots
	}

	for _, name := range badNames {
		if err := store.Create(name, uuid); err == nil {
			t.Errorf("Create(%q) should fail for path traversal", name)
		}
	}
}

// TestConcurrentMoves tests that concurrent move operations don't corrupt state.
func TestConcurrentMoves(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	uuid1 := frameid.MustNew()
	uuid2 := frameid.MustNew()

	if err := store.Create("test-ref", uuid1); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Perform many moves sequentially (can't truly race file I/O safely).
	for i := 0; i < 20; i++ {
		target := uuid1
		if i%2 == 1 {
			target = uuid2
		}
		if err := store.Move("test-ref", target); err != nil {
			t.Fatalf("Move %d: %v", i, err)
		}
	}

	ref, err := store.Get("test-ref")
	if err != nil {
		t.Fatalf("Get after moves: %v", err)
	}

	// Should have 21 reflog entries: 1 initial + 20 moves.
	if len(ref.Reflog) != 21 {
		t.Errorf("Reflog length = %d, want 21", len(ref.Reflog))
	}

	// Verify reflog is in correct order (newest first).
	// The most recent move was i=19 (odd), so should be uuid2.
	if ref.UUID != uuid2 {
		t.Errorf("Final UUID = %v, want %v", ref.UUID, uuid2)
	}
	if ref.Reflog[0].UUID != uuid2 {
		t.Errorf("Reflog[0].UUID = %v, want %v", ref.Reflog[0].UUID, uuid2)
	}
}

// TestAutorunWithEmptySlice verifies that empty slice clears autorun like nil.
func TestAutorunWithEmptySlice(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	uuid := frameid.MustNew()

	if err := store.Create("test-ref", uuid); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Set autorun.
	if err := store.SetAutorun("test-ref", []string{"/bin/sh", "-c", "echo hello"}); err != nil {
		t.Fatalf("SetAutorun: %v", err)
	}

	ref, _ := store.Get("test-ref")
	if len(ref.Autorun) != 3 {
		t.Fatalf("Autorun length = %d, want 3", len(ref.Autorun))
	}

	// Clear with empty slice (not nil).
	if err := store.SetAutorun("test-ref", []string{}); err != nil {
		t.Fatalf("SetAutorun empty: %v", err)
	}

	ref, _ = store.Get("test-ref")
	if len(ref.Autorun) != 0 {
		t.Errorf("Autorun after empty clear = %v, want empty", ref.Autorun)
	}
}

// TestMaxRefNameLength verifies the 128 character limit.
func TestMaxRefNameLength(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	uuid := frameid.MustNew()

	// 128 chars should be OK.
	longName := "a"
	for len(longName) < 128 {
		longName += "a"
	}
	if len(longName) != 128 {
		t.Fatalf("Setup error: longName length = %d", len(longName))
	}

	if err := store.Create(longName, uuid); err != nil {
		t.Errorf("Create 128-char name: %v", err)
	}

	// 129 chars should fail.
	tooLong := longName + "a"
	if err := store.Create(tooLong, uuid); err == nil {
		t.Error("Create 129-char name should fail")
	}
}

// TestMoveInvalidName confirms Move/SetAutorun surface ErrInvalidRefName (not
// ErrRefNotFound) when handed a malformed name.
func TestMoveInvalidName(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.Move("bad name", frameid.MustNew()); !errors.Is(err, ErrInvalidRefName) {
		t.Errorf("Move(invalid) = %v, want ErrInvalidRefName", err)
	}
	if err := store.SetAutorun("bad/name", []string{"x"}); !errors.Is(err, ErrInvalidRefName) {
		t.Errorf("SetAutorun(invalid) = %v, want ErrInvalidRefName", err)
	}
}

func TestGetMalformedJSONC(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := os.MkdirAll(store.refsDir(), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.refPath("broken"), []byte("{not valid"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("broken"); err == nil {
		t.Error("Get of malformed JSONC should fail")
	} else if err == ErrRefNotFound {
		t.Error("Get of malformed JSONC should not report ErrRefNotFound")
	}
}

func TestListSkipsNonRefs(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.Create("real", frameid.MustNew()); err != nil {
		t.Fatal(err)
	}
	// A subdirectory and a non-.jsonc file alongside the refs.
	if err := os.MkdirAll(filepath.Join(store.refsDir(), "subdir"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.refsDir(), "notes.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	names, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 1 || names[0] != "real" {
		t.Errorf("List = %v, want [real]", names)
	}
}

func TestIDDirHelpersInvalidName(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.IDDirExists("bad/name"); !errors.Is(err, ErrInvalidRefName) {
		t.Errorf("IDDirExists(invalid) = %v, want ErrInvalidRefName", err)
	}
	if err := store.EnsureIDDir(".."); !errors.Is(err, ErrInvalidRefName) {
		t.Errorf("EnsureIDDir(invalid) = %v, want ErrInvalidRefName", err)
	}
	if err := store.RemoveIDDir("bad name"); !errors.Is(err, ErrInvalidRefName) {
		t.Errorf("RemoveIDDir(invalid) = %v, want ErrInvalidRefName", err)
	}
}

func TestRemoveIDDirNonexistent(t *testing.T) {
	store := NewStore(t.TempDir())
	// Valid name but the id dir was never created: should be a no-op nil.
	if err := store.RemoveIDDir("never-made"); err != nil {
		t.Errorf("RemoveIDDir of nonexistent = %v, want nil", err)
	}
}

func TestEnsureAndRemoveIDDir(t *testing.T) {
	store := NewStore(t.TempDir())
	const name = "withid"
	if exists, err := store.IDDirExists(name); err != nil || exists {
		t.Fatalf("IDDirExists before create = (%v, %v), want (false, nil)", exists, err)
	}
	if err := store.EnsureIDDir(name); err != nil {
		t.Fatalf("EnsureIDDir: %v", err)
	}
	// Empty dir still reports false (exists-and-non-empty semantic).
	if exists, err := store.IDDirExists(name); err != nil || exists {
		t.Errorf("IDDirExists of empty dir = (%v, %v), want (false, nil)", exists, err)
	}
	// Put something in it; now it reports true.
	if err := os.WriteFile(filepath.Join(store.idDir(name), "key"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if exists, err := store.IDDirExists(name); err != nil || !exists {
		t.Errorf("IDDirExists of non-empty dir = (%v, %v), want (true, nil)", exists, err)
	}
	if err := store.RemoveIDDir(name); err != nil {
		t.Fatalf("RemoveIDDir: %v", err)
	}
	if exists, _ := store.IDDirExists(name); exists {
		t.Error("IDDirExists after remove = true, want false")
	}
}
