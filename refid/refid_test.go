package refid

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestIDDir(t *testing.T) {
	if got, want := IDDir("/frames/abc"), filepath.Join("/frames/abc", "id"); got != want {
		t.Errorf("IDDir = %q, want %q", got, want)
	}
}

func TestPath(t *testing.T) {
	if got, want := Path("/frames/abc", "main"), filepath.Join("/frames/abc", "id", "main"); got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

func TestValidateRefName(t *testing.T) {
	valid := []string{"main", "feature-1", "a.b", "x_y", "v2"}
	for _, name := range valid {
		if err := validateRefName(name); err != nil {
			t.Errorf("validateRefName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []string{"", ".", "..", "../escape", "a/b", "/abs", "id/main", "nested/../x"}
	for _, name := range invalid {
		if err := validateRefName(name); !errors.Is(err, ErrInvalidRefName) {
			t.Errorf("validateRefName(%q) = %v, want ErrInvalidRefName", name, err)
		}
	}
}

// TestEnsureRejectsBadName confirms the mutating entry points reject an unsafe
// ref name before touching the filesystem (defense in depth: callers already
// validate, but refid guards itself).
func TestEnsureRejectsBadName(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"", "..", "../escape", "a/b"} {
		if err := Ensure(dir, name); !errors.Is(err, ErrInvalidRefName) {
			t.Errorf("Ensure(%q) = %v, want ErrInvalidRefName", name, err)
		}
		if err := Move(dir, dir, name); !errors.Is(err, ErrInvalidRefName) {
			t.Errorf("Move(%q) = %v, want ErrInvalidRefName", name, err)
		}
		if err := Remove(dir, name); !errors.Is(err, ErrInvalidRefName) {
			t.Errorf("Remove(%q) = %v, want ErrInvalidRefName", name, err)
		}
	}
}

// TestRemoveNonexistentIsNil confirms removing a ref that has no identity
// subvolume (and no plain dir) is a no-op. This path does not require btrfs:
// isSubvolume returns false and the RemoveAll of a missing path is ignored.
func TestRemoveNonexistentIsNil(t *testing.T) {
	dir := t.TempDir()
	if err := Remove(dir, "doesnotexist"); err != nil {
		t.Errorf("Remove of nonexistent ref = %v, want nil", err)
	}
}
