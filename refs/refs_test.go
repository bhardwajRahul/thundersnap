package refs

import (
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
	}
	for _, name := range invalid {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", name)
		}
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
