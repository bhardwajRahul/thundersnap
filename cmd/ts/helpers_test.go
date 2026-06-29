package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStderr swaps os.Stderr for a pipe, runs fn, and returns everything
// written to stderr during the call.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = old }()

	fn()
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestProgressRendererNonTTY(t *testing.T) {
	// A non-TTY renderer prints each message on its own line and finish() is a
	// no-op (no carriage-return clearing).
	got := captureStderr(t, func() {
		r := &progressRenderer{width: 80} // tty defaults to false
		r.progress("first")
		r.progress("second")
		r.finish()
	})
	if got != "first\nsecond\n" {
		t.Errorf("non-TTY output = %q, want %q", got, "first\nsecond\n")
	}
}

func TestProgressRendererTTYTruncatesAndClears(t *testing.T) {
	// A TTY renderer overwrites a single line with \r, truncates to width, and
	// finish() clears the line.
	got := captureStderr(t, func() {
		r := &progressRenderer{tty: true, width: 5}
		r.progress("abcdefgh") // truncated to "abcde"
		r.finish()
	})
	// progress: "\rabcde"  (no padding, lastLineLen starts at 0)
	// finish:   "\r     \r" (5 spaces)
	want := "\rabcde" + "\r     \r"
	if got != want {
		t.Errorf("TTY output = %q, want %q", got, want)
	}
}

func TestProgressRendererTTYPadsShorterLine(t *testing.T) {
	// When a shorter message follows a longer one, the renderer pads with
	// spaces to erase the leftover characters from the previous line.
	got := captureStderr(t, func() {
		r := &progressRenderer{tty: true, width: 80}
		r.progress("longmessage") // len 11
		r.progress("short")       // len 5, needs 6 spaces of padding
	})
	want := "\rlongmessage" + "\rshort" + strings.Repeat(" ", 6)
	if got != want {
		t.Errorf("TTY padded output = %q, want %q", got, want)
	}
}

func TestIsShellInvocation(t *testing.T) {
	tests := []struct {
		base string
		want bool
	}{
		{"sh", true},
		{"-sh", true},
		{"ts", false},
		{"bash", false},
		{"", false},
		{"ssh", false},
	}
	for _, tt := range tests {
		if got := isShellInvocation(tt.base); got != tt.want {
			t.Errorf("isShellInvocation(%q) = %v, want %v", tt.base, got, tt.want)
		}
	}
}

func TestResolveSnapSubdir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub", "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// resolveSnapSubdir treats relative args as cwd-relative, so run from root.
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(oldwd) })

	t.Run("absolute dir strips leading slash", func(t *testing.T) {
		abs := filepath.Join(root, "sub", "dir")
		got, err := resolveSnapSubdir(abs)
		if err != nil {
			t.Fatalf("resolveSnapSubdir(%q): %v", abs, err)
		}
		if want := strings.TrimPrefix(abs, "/"); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if strings.HasPrefix(got, "/") {
			t.Errorf("result %q should not start with /", got)
		}
	})

	t.Run("relative dir resolved against cwd", func(t *testing.T) {
		got, err := resolveSnapSubdir("sub/dir")
		if err != nil {
			t.Fatalf("resolveSnapSubdir(rel): %v", err)
		}
		want := strings.TrimPrefix(filepath.Join(root, "sub", "dir"), "/")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("file is not a directory", func(t *testing.T) {
		if _, err := resolveSnapSubdir(filepath.Join(root, "file.txt")); err == nil {
			t.Error("expected error for a regular file")
		}
	})

	t.Run("nonexistent path", func(t *testing.T) {
		if _, err := resolveSnapSubdir(filepath.Join(root, "nope")); err == nil {
			t.Error("expected error for nonexistent path")
		}
	})

	t.Run("root rejected", func(t *testing.T) {
		if _, err := resolveSnapSubdir("/"); err == nil {
			t.Error("expected error when resolving to container root")
		}
	})
}

func TestFindExecutable(t *testing.T) {
	dir := t.TempDir()

	// An executable file and a non-executable file in dir.
	execPath := filepath.Join(dir, "tool")
	if err := os.WriteFile(execPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	nonExecPath := filepath.Join(dir, "data")
	if err := os.WriteFile(nonExecPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("absolute path with slash", func(t *testing.T) {
		got, err := findExecutable(execPath)
		if err != nil || got != execPath {
			t.Errorf("findExecutable(%q) = (%q, %v), want (%q, nil)", execPath, got, err, execPath)
		}
	})

	t.Run("path with slash missing", func(t *testing.T) {
		if _, err := findExecutable(filepath.Join(dir, "missing")); err == nil {
			t.Error("expected error for missing slashed path")
		}
	})

	t.Run("found in PATH", func(t *testing.T) {
		t.Setenv("PATH", dir)
		got, err := findExecutable("tool")
		if err != nil || got != execPath {
			t.Errorf("findExecutable(tool) = (%q, %v), want (%q, nil)", got, err, execPath)
		}
	})

	t.Run("present but not executable is skipped", func(t *testing.T) {
		t.Setenv("PATH", dir)
		if _, err := findExecutable("data"); err == nil {
			t.Error("expected non-executable file to be skipped")
		}
	})

	t.Run("first match wins", func(t *testing.T) {
		dir2 := t.TempDir()
		exec2 := filepath.Join(dir2, "tool")
		if err := os.WriteFile(exec2, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir+":"+dir2)
		got, err := findExecutable("tool")
		if err != nil || got != execPath {
			t.Errorf("findExecutable(tool) = (%q, %v), want first match %q", got, err, execPath)
		}
	})

	t.Run("not found", func(t *testing.T) {
		t.Setenv("PATH", dir)
		if _, err := findExecutable("does-not-exist"); err == nil {
			t.Error("expected not-found error")
		}
	})
}
