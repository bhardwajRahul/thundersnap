package sftpfs

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestToHostPathConfinement is the security-critical test: client paths must map
// inside rootFS, and traversal attempts via .. must either be cleaned back into
// rootFS or rejected, never escaping to the host filesystem.
func TestToHostPathConfinement(t *testing.T) {
	root := "/var/lib/thundersnap/fs/abc"
	h := NewHandler(root, "/home/user", -1, -1)

	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"/", root, false},
		{"/home/user/file.txt", root + "/home/user/file.txt", false},
		{"home/user/file.txt", root + "/home/user/file.txt", false},
		// filepath.Clean collapses leading .. against the rooted "/" so these
		// resolve back inside rootFS rather than escaping.
		{"/../../../etc/passwd", root + "/etc/passwd", false},
		{"..", root, false},
		{"/a/../b", root + "/b", false},
	}
	for _, c := range cases {
		got, err := h.toHostPath(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("toHostPath(%q) = %q, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("toHostPath(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("toHostPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestListerat confirms the ListerAt slice adapter copies entries and reports
// io.EOF at and past the end.
func TestListerat(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "a"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "b"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	ai, _ := os.Stat(filepath.Join(tmp, "a"))
	bi, _ := os.Stat(filepath.Join(tmp, "b"))
	l := listerat([]os.FileInfo{ai, bi})

	// A buffer that exactly holds the remaining entries is filled with no EOF
	// (matching sftp's convention: EOF is only signaled once a short read occurs).
	buf := make([]os.FileInfo, 2)
	n, err := l.ListAt(buf, 0)
	if n != 2 || err != nil {
		t.Errorf("ListAt(0, len 2) = (%d, %v), want (2, nil)", n, err)
	}

	// A larger buffer gets a short read terminated by EOF.
	big := make([]os.FileInfo, 5)
	n, err = l.ListAt(big, 0)
	if n != 2 || err != io.EOF {
		t.Errorf("ListAt(0, len 5) = (%d, %v), want (2, EOF)", n, err)
	}

	// Offset past the end returns EOF immediately.
	n, err = l.ListAt(buf, 2)
	if n != 0 || err != io.EOF {
		t.Errorf("ListAt(past end) = (%d, %v), want (0, EOF)", n, err)
	}
}

// TestLinkInfo confirms the synthetic readlink FileInfo reports symlink mode and
// the target name.
func TestLinkInfo(t *testing.T) {
	li := linkInfo{name: "/target"}
	if li.Name() != "/target" {
		t.Errorf("Name() = %q, want /target", li.Name())
	}
	if li.Mode()&os.ModeSymlink == 0 {
		t.Errorf("Mode() = %v, want symlink bit set", li.Mode())
	}
	if li.IsDir() {
		t.Error("IsDir() = true, want false")
	}
}

// TestHomeDir confirms the configured home directory round-trips for use as the
// SFTP start directory.
func TestHomeDir(t *testing.T) {
	h := NewHandler("/root", "/home/alice", 1000, 1000)
	if h.HomeDir() != "/home/alice" {
		t.Errorf("HomeDir() = %q, want /home/alice", h.HomeDir())
	}
}
