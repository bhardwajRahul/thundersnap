package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestUnionTaints(t *testing.T) {
	tests := []struct {
		name   string
		sets   [][]string
		want   []string
	}{
		{
			name: "empty",
			sets: nil,
			want: nil,
		},
		{
			name: "single set",
			sets: [][]string{{"a", "b", "c"}},
			want: []string{"a", "b", "c"},
		},
		{
			name: "two sets no overlap",
			sets: [][]string{{"a", "b"}, {"c", "d"}},
			want: []string{"a", "b", "c", "d"},
		},
		{
			name: "two sets with overlap",
			sets: [][]string{{"a", "b", "c"}, {"b", "c", "d"}},
			want: []string{"a", "b", "c", "d"},
		},
		{
			name: "three sets",
			sets: [][]string{{"a"}, {"b"}, {"c"}},
			want: []string{"a", "b", "c"},
		},
		{
			name: "empty sets",
			sets: [][]string{{}, {}, {}},
			want: nil,
		},
		{
			name: "mixed empty and non-empty",
			sets: [][]string{{}, {"a"}, {}},
			want: []string{"a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := UnionTaints(tt.sets...)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("UnionTaints() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIntersectTaints(t *testing.T) {
	tests := []struct {
		name string
		a    []string
		b    []string
		want []string
	}{
		{
			name: "both empty",
			a:    nil,
			b:    nil,
			want: nil,
		},
		{
			name: "first empty",
			a:    nil,
			b:    []string{"a", "b"},
			want: nil,
		},
		{
			name: "second empty",
			a:    []string{"a", "b"},
			b:    nil,
			want: nil,
		},
		{
			name: "no overlap",
			a:    []string{"a", "b"},
			b:    []string{"c", "d"},
			want: nil,
		},
		{
			name: "full overlap",
			a:    []string{"a", "b"},
			b:    []string{"a", "b"},
			want: []string{"a", "b"},
		},
		{
			name: "partial overlap",
			a:    []string{"a", "b", "c"},
			b:    []string{"b", "c", "d"},
			want: []string{"b", "c"},
		},
		{
			name: "single element overlap",
			a:    []string{"a", "b", "c"},
			b:    []string{"b"},
			want: []string{"b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IntersectTaints(tt.a, tt.b)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("IntersectTaints() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTaintsEqual(t *testing.T) {
	tests := []struct {
		name string
		a    []string
		b    []string
		want bool
	}{
		{
			name: "both nil",
			a:    nil,
			b:    nil,
			want: true,
		},
		{
			name: "both empty",
			a:    []string{},
			b:    []string{},
			want: true,
		},
		{
			name: "equal",
			a:    []string{"a", "b"},
			b:    []string{"a", "b"},
			want: true,
		},
		{
			name: "different length",
			a:    []string{"a", "b"},
			b:    []string{"a"},
			want: false,
		},
		{
			name: "same length different content",
			a:    []string{"a", "b"},
			b:    []string{"a", "c"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := taintsEqual(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("taintsEqual() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSnapMetaReadWrite(t *testing.T) {
	tmpDir := t.TempDir()

	meta := &SnapMeta{
		Parent: "abc123",
		Taints: []string{"pii:customers", "unsafe-permissions"},
		Source: &SnapSource{
			Type: "docker",
			Ref:  "docker.io/library/ubuntu:24.04@sha256:abcd1234",
		},
	}

	// Write
	if err := writeSnapMeta(tmpDir, "test-snap", meta); err != nil {
		t.Fatalf("writeSnapMeta: %v", err)
	}

	// Verify file exists
	path := filepath.Join(tmpDir, "test-snap.jsonc")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snap meta file not created: %v", err)
	}

	// Read back
	got, err := readSnapMeta(tmpDir, "test-snap")
	if err != nil {
		t.Fatalf("readSnapMeta: %v", err)
	}

	if got.Parent != meta.Parent {
		t.Errorf("Parent = %q, want %q", got.Parent, meta.Parent)
	}
	if !reflect.DeepEqual(got.Taints, meta.Taints) {
		t.Errorf("Taints = %v, want %v", got.Taints, meta.Taints)
	}
	if got.Source == nil {
		t.Fatal("Source is nil")
	}
	if got.Source.Type != meta.Source.Type {
		t.Errorf("Source.Type = %q, want %q", got.Source.Type, meta.Source.Type)
	}
	if got.Source.Ref != meta.Source.Ref {
		t.Errorf("Source.Ref = %q, want %q", got.Source.Ref, meta.Source.Ref)
	}
}

func TestSnapMetaReadNonExistent(t *testing.T) {
	tmpDir := t.TempDir()

	got, err := readSnapMeta(tmpDir, "nonexistent")
	if err != nil {
		t.Fatalf("readSnapMeta: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for nonexistent file, got %+v", got)
	}
}

func TestFrameMetaReadWrite(t *testing.T) {
	tmpDir := t.TempDir()
	framePath := filepath.Join(tmpDir, "test-frame")

	// Create the frame directory (normally btrfs would do this)
	if err := os.MkdirAll(framePath, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	meta := &FrameMeta{
		Rootfs:    "abc123",
		Home:      "def456",
		Work:      "789xyz",
		Taints:    []string{"pii:customers"},
		Isolation: "vm",
	}

	// Write
	if err := writeFrameMeta(framePath, meta); err != nil {
		t.Fatalf("writeFrameMeta: %v", err)
	}

	// Verify file exists
	path := framePath + ".jsonc"
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("frame meta file not created: %v", err)
	}

	// Read back
	got, err := readFrameMeta(framePath)
	if err != nil {
		t.Fatalf("readFrameMeta: %v", err)
	}

	if got.Rootfs != meta.Rootfs {
		t.Errorf("Rootfs = %q, want %q", got.Rootfs, meta.Rootfs)
	}
	if got.Home != meta.Home {
		t.Errorf("Home = %q, want %q", got.Home, meta.Home)
	}
	if got.Work != meta.Work {
		t.Errorf("Work = %q, want %q", got.Work, meta.Work)
	}
	if !reflect.DeepEqual(got.Taints, meta.Taints) {
		t.Errorf("Taints = %v, want %v", got.Taints, meta.Taints)
	}
	if got.Isolation != meta.Isolation {
		t.Errorf("Isolation = %q, want %q", got.Isolation, meta.Isolation)
	}
}

func TestFrameMetaReadNonExistent(t *testing.T) {
	tmpDir := t.TempDir()
	framePath := filepath.Join(tmpDir, "nonexistent")

	got, err := readFrameMeta(framePath)
	if err != nil {
		t.Fatalf("readFrameMeta: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for nonexistent file, got %+v", got)
	}
}

func TestHujsonParsing(t *testing.T) {
	tmpDir := t.TempDir()

	// Write hujson with comments and trailing commas
	hujsonContent := `{
  // This is a comment
  "parent": "abc123",
  "taints": [
    "pii:customers",
    "unsafe-permissions", // trailing comma
  ],
}
`
	path := filepath.Join(tmpDir, "test-snap.jsonc")
	if err := os.WriteFile(path, []byte(hujsonContent), 0644); err != nil {
		t.Fatalf("write hujson: %v", err)
	}

	// Read should succeed
	got, err := readSnapMeta(tmpDir, "test-snap")
	if err != nil {
		t.Fatalf("readSnapMeta: %v", err)
	}

	if got.Parent != "abc123" {
		t.Errorf("Parent = %q, want %q", got.Parent, "abc123")
	}
	if len(got.Taints) != 2 {
		t.Errorf("len(Taints) = %d, want 2", len(got.Taints))
	}
}

func TestRelinkSnapChildren(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a parent chain: grandparent -> parent -> child1, child2
	// When we delete "parent", child1 and child2 should point to grandparent
	grandparentID := "grandparent-snap"
	parentID := "parent-snap"
	child1ID := "child1-snap"
	child2ID := "child2-snap"
	unrelatedID := "unrelated-snap"

	// Write snap metadata
	grandparentMeta := &SnapMeta{Parent: ""}
	parentMeta := &SnapMeta{Parent: grandparentID}
	child1Meta := &SnapMeta{Parent: parentID}
	child2Meta := &SnapMeta{Parent: parentID}
	unrelatedMeta := &SnapMeta{Parent: "some-other-parent"}

	if err := writeSnapMeta(tmpDir, grandparentID, grandparentMeta); err != nil {
		t.Fatalf("write grandparent meta: %v", err)
	}
	if err := writeSnapMeta(tmpDir, parentID, parentMeta); err != nil {
		t.Fatalf("write parent meta: %v", err)
	}
	if err := writeSnapMeta(tmpDir, child1ID, child1Meta); err != nil {
		t.Fatalf("write child1 meta: %v", err)
	}
	if err := writeSnapMeta(tmpDir, child2ID, child2Meta); err != nil {
		t.Fatalf("write child2 meta: %v", err)
	}
	if err := writeSnapMeta(tmpDir, unrelatedID, unrelatedMeta); err != nil {
		t.Fatalf("write unrelated meta: %v", err)
	}

	// Relink children when deleting parent
	if err := relinkSnapChildren(tmpDir, parentID, grandparentID); err != nil {
		t.Fatalf("relinkSnapChildren: %v", err)
	}

	// Verify child1 now points to grandparent
	child1After, err := readSnapMeta(tmpDir, child1ID)
	if err != nil {
		t.Fatalf("read child1 after: %v", err)
	}
	if child1After.Parent != grandparentID {
		t.Errorf("child1.Parent = %q, want %q", child1After.Parent, grandparentID)
	}

	// Verify child2 now points to grandparent
	child2After, err := readSnapMeta(tmpDir, child2ID)
	if err != nil {
		t.Fatalf("read child2 after: %v", err)
	}
	if child2After.Parent != grandparentID {
		t.Errorf("child2.Parent = %q, want %q", child2After.Parent, grandparentID)
	}

	// Verify unrelated snap is unchanged
	unrelatedAfter, err := readSnapMeta(tmpDir, unrelatedID)
	if err != nil {
		t.Fatalf("read unrelated after: %v", err)
	}
	if unrelatedAfter.Parent != "some-other-parent" {
		t.Errorf("unrelated.Parent = %q, want %q", unrelatedAfter.Parent, "some-other-parent")
	}

	// Verify grandparent is unchanged
	grandparentAfter, err := readSnapMeta(tmpDir, grandparentID)
	if err != nil {
		t.Fatalf("read grandparent after: %v", err)
	}
	if grandparentAfter.Parent != "" {
		t.Errorf("grandparent.Parent = %q, want empty", grandparentAfter.Parent)
	}
}

func TestRelinkSnapChildrenToEmptyParent(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a chain: root (no parent) -> child
	// When we delete root, child should have empty parent
	rootID := "root-snap"
	childID := "child-snap"

	rootMeta := &SnapMeta{Parent: ""}
	childMeta := &SnapMeta{Parent: rootID}

	if err := writeSnapMeta(tmpDir, rootID, rootMeta); err != nil {
		t.Fatalf("write root meta: %v", err)
	}
	if err := writeSnapMeta(tmpDir, childID, childMeta); err != nil {
		t.Fatalf("write child meta: %v", err)
	}

	// Relink children when deleting root (no parent to inherit)
	if err := relinkSnapChildren(tmpDir, rootID, ""); err != nil {
		t.Fatalf("relinkSnapChildren: %v", err)
	}

	// Verify child now has empty parent
	childAfter, err := readSnapMeta(tmpDir, childID)
	if err != nil {
		t.Fatalf("read child after: %v", err)
	}
	if childAfter.Parent != "" {
		t.Errorf("child.Parent = %q, want empty", childAfter.Parent)
	}
}

func TestRelinkSnapChildrenPreservesTaints(t *testing.T) {
	tmpDir := t.TempDir()

	// When relinking, child's taints should be preserved
	parentID := "parent-snap"
	childID := "child-snap"

	parentMeta := &SnapMeta{Parent: "grandparent", Taints: []string{"taint-a"}}
	childMeta := &SnapMeta{Parent: parentID, Taints: []string{"taint-b", "taint-c"}}

	if err := writeSnapMeta(tmpDir, parentID, parentMeta); err != nil {
		t.Fatalf("write parent meta: %v", err)
	}
	if err := writeSnapMeta(tmpDir, childID, childMeta); err != nil {
		t.Fatalf("write child meta: %v", err)
	}

	// Relink
	if err := relinkSnapChildren(tmpDir, parentID, "grandparent"); err != nil {
		t.Fatalf("relinkSnapChildren: %v", err)
	}

	// Verify child's taints are preserved
	childAfter, err := readSnapMeta(tmpDir, childID)
	if err != nil {
		t.Fatalf("read child after: %v", err)
	}
	if !reflect.DeepEqual(childAfter.Taints, []string{"taint-b", "taint-c"}) {
		t.Errorf("child.Taints = %v, want [taint-b, taint-c]", childAfter.Taints)
	}
}
