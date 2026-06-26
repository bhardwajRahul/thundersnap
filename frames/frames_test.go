package frames

import (
	"testing"

	"github.com/tailscale/thundersnap/frameid"
	"github.com/tailscale/thundersnap/snaphash"
)

func TestCreateAndGet(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	uuid := frameid.MustNew()
	rootfs := snaphash.Sum([]byte("rootfs"))
	home := snaphash.Sum([]byte("home"))
	work := snaphash.Sum([]byte("work"))

	frame := &Frame{
		Rootfs:    rootfs,
		Home:      home,
		Work:      work,
		Isolation: "container",
	}

	if err := store.Create(uuid, frame); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := store.Get(uuid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.Rootfs != rootfs {
		t.Errorf("Rootfs = %v, want %v", got.Rootfs, rootfs)
	}
	if got.Home != home {
		t.Errorf("Home = %v, want %v", got.Home, home)
	}
	if got.Work != work {
		t.Errorf("Work = %v, want %v", got.Work, work)
	}
	if got.Isolation != "container" {
		t.Errorf("Isolation = %q, want %q", got.Isolation, "container")
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt not set")
	}
}

func TestCreateDuplicate(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	uuid := frameid.MustNew()
	frame := &Frame{Rootfs: snaphash.Sum([]byte("rootfs"))}

	if err := store.Create(uuid, frame); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := store.Create(uuid, frame); err != ErrFrameExists {
		t.Errorf("Create duplicate = %v, want ErrFrameExists", err)
	}
}

func TestGetNotFound(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	uuid := frameid.MustNew()
	_, err := store.Get(uuid)
	if err != ErrFrameNotFound {
		t.Errorf("Get nonexistent = %v, want ErrFrameNotFound", err)
	}
}

func TestUpdate(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	uuid := frameid.MustNew()
	frame := &Frame{Rootfs: snaphash.Sum([]byte("rootfs"))}

	if err := store.Create(uuid, frame); err != nil {
		t.Fatalf("Create: %v", err)
	}

	frame.Isolation = "vm"
	if err := store.Update(uuid, frame); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, _ := store.Get(uuid)
	if got.Isolation != "vm" {
		t.Errorf("Isolation after update = %q, want %q", got.Isolation, "vm")
	}
}

func TestUpdateNotFound(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	uuid := frameid.MustNew()
	frame := &Frame{}

	if err := store.Update(uuid, frame); err != ErrFrameNotFound {
		t.Errorf("Update nonexistent = %v, want ErrFrameNotFound", err)
	}
}

func TestDelete(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	uuid := frameid.MustNew()
	frame := &Frame{Rootfs: snaphash.Sum([]byte("rootfs"))}

	if err := store.Create(uuid, frame); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := store.Delete(uuid); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if store.Exists(uuid) {
		t.Error("frame still exists after delete")
	}
}

func TestDeleteNotFound(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	uuid := frameid.MustNew()
	if err := store.Delete(uuid); err != ErrFrameNotFound {
		t.Errorf("Delete nonexistent = %v, want ErrFrameNotFound", err)
	}
}

func TestAddHistoryEntry(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	uuid := frameid.MustNew()
	frame := &Frame{Rootfs: snaphash.Sum([]byte("rootfs"))}

	if err := store.Create(uuid, frame); err != nil {
		t.Fatalf("Create: %v", err)
	}

	snap1 := snaphash.Sum([]byte("snap1"))
	snap2 := snaphash.Sum([]byte("snap2"))

	if err := store.AddHistoryEntry(uuid, snap1, "first snapshot"); err != nil {
		t.Fatalf("AddHistoryEntry 1: %v", err)
	}

	if err := store.AddHistoryEntry(uuid, snap2, "second snapshot"); err != nil {
		t.Fatalf("AddHistoryEntry 2: %v", err)
	}

	got, _ := store.Get(uuid)
	if len(got.History) != 2 {
		t.Fatalf("History length = %d, want 2", len(got.History))
	}

	// Most recent first.
	if got.History[0].Snap != snap2 {
		t.Errorf("History[0].Snap = %v, want %v", got.History[0].Snap, snap2)
	}
	if got.History[0].Message != "second snapshot" {
		t.Errorf("History[0].Message = %q, want %q", got.History[0].Message, "second snapshot")
	}
	if got.History[1].Snap != snap1 {
		t.Errorf("History[1].Snap = %v, want %v", got.History[1].Snap, snap1)
	}
}

func TestAddTaint(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	uuid := frameid.MustNew()
	frame := &Frame{Rootfs: snaphash.Sum([]byte("rootfs"))}

	if err := store.Create(uuid, frame); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := store.AddTaint(uuid, "network"); err != nil {
		t.Fatalf("AddTaint: %v", err)
	}

	got, _ := store.Get(uuid)
	if len(got.Taints) != 1 || got.Taints[0] != "network" {
		t.Errorf("Taints = %v, want [network]", got.Taints)
	}

	// Adding same taint again should be idempotent.
	if err := store.AddTaint(uuid, "network"); err != nil {
		t.Fatalf("AddTaint duplicate: %v", err)
	}

	got, _ = store.Get(uuid)
	if len(got.Taints) != 1 {
		t.Errorf("Taints after duplicate = %v, want [network]", got.Taints)
	}

	// Add another taint.
	if err := store.AddTaint(uuid, "gpu"); err != nil {
		t.Fatalf("AddTaint gpu: %v", err)
	}

	got, _ = store.Get(uuid)
	// Should be sorted.
	if len(got.Taints) != 2 || got.Taints[0] != "gpu" || got.Taints[1] != "network" {
		t.Errorf("Taints = %v, want [gpu network]", got.Taints)
	}
}

func TestList(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	// Empty list initially.
	uuids, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(uuids) != 0 {
		t.Errorf("List empty = %v, want empty", uuids)
	}

	// Create some frames.
	for i := 0; i < 3; i++ {
		uuid := frameid.MustNew()
		frame := &Frame{Rootfs: snaphash.Sum([]byte{byte(i)})}
		if err := store.Create(uuid, frame); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}

	uuids, err = store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(uuids) != 3 {
		t.Errorf("List length = %d, want 3", len(uuids))
	}
}

func TestExists(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	uuid := frameid.MustNew()

	if store.Exists(uuid) {
		t.Error("Exists before create = true")
	}

	frame := &Frame{Rootfs: snaphash.Sum([]byte("rootfs"))}
	store.Create(uuid, frame)

	if !store.Exists(uuid) {
		t.Error("Exists after create = false")
	}
}

func TestUnionTaints(t *testing.T) {
	result := UnionTaints(
		[]string{"a", "b"},
		[]string{"b", "c"},
		[]string{"a", "c", "d"},
	)

	expected := []string{"a", "b", "c", "d"}
	if len(result) != len(expected) {
		t.Fatalf("UnionTaints length = %d, want %d", len(result), len(expected))
	}
	for i, v := range expected {
		if result[i] != v {
			t.Errorf("UnionTaints[%d] = %q, want %q", i, result[i], v)
		}
	}
}

func TestUnionTaintsEmpty(t *testing.T) {
	result := UnionTaints(nil, nil)
	if result != nil {
		t.Errorf("UnionTaints empty = %v, want nil", result)
	}
}
