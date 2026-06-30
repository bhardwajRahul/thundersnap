package main

import (
	"testing"

	"github.com/tailscale/thundersnap/frameid"
)

func TestSelectTargetUserExplicit(t *testing.T) {
	// A non-empty targetUser is returned verbatim without touching the
	// filesystem, so this branch is safe to unit test.
	if got := selectTargetUser("/nonexistent/rootfs", "alice"); got != "alice" {
		t.Errorf("selectTargetUser(_, \"alice\") = %q, want \"alice\"", got)
	}
}

func TestTailscaleUserFromRootFS(t *testing.T) {
	fsDir := "/var/lib/thundersnap/fs"
	old := flagFsDir
	flagFsDir = &fsDir
	defer func() { flagFsDir = old }()

	tests := []struct {
		rootFS  string
		want    string
		wantErr bool
	}{
		{"/var/lib/thundersnap/fs/alice/work", "alice", false},
		{"/var/lib/thundersnap/fs/bob@example.com/main", "bob@example.com", false},
		{"/var/lib/thundersnap/fs/alice", "", true}, // only one component
	}
	for _, tt := range tests {
		got, err := tailscaleUserFromRootFS(tt.rootFS)
		if tt.wantErr {
			if err == nil {
				t.Errorf("tailscaleUserFromRootFS(%q) = (%q,nil), want error", tt.rootFS, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("tailscaleUserFromRootFS(%q): %v", tt.rootFS, err)
			continue
		}
		if got != tt.want {
			t.Errorf("tailscaleUserFromRootFS(%q) = %q, want %q", tt.rootFS, got, tt.want)
		}
	}
}

func TestFrameUUIDFromRootFS(t *testing.T) {
	fsDir := "/var/lib/thundersnap/fs"
	old := flagFsDir
	flagFsDir = &fsDir
	defer func() { flagFsDir = old }()

	// Valid UUIDs (UUIDv4 format)
	validUUID := "01234567-89ab-cdef-0123-456789abcdef"

	tests := []struct {
		rootFS  string
		want    string
		wantErr bool
	}{
		{"/var/lib/thundersnap/fs/alice/" + validUUID, validUUID, false},
		{"/var/lib/thundersnap/fs/bob@example.com/" + validUUID, validUUID, false},
		{"/var/lib/thundersnap/fs/alice", "", true},                         // only one component
		{"/var/lib/thundersnap/fs/alice/not-a-uuid", "", true},              // invalid UUID
		{"/var/lib/thundersnap/fs/alice/01234567-89ab-cdef-0123", "", true}, // truncated UUID
	}
	for _, tt := range tests {
		got, err := frameUUIDFromRootFS(tt.rootFS)
		if tt.wantErr {
			if err == nil {
				t.Errorf("frameUUIDFromRootFS(%q) = (%s,nil), want error", tt.rootFS, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("frameUUIDFromRootFS(%q): %v", tt.rootFS, err)
			continue
		}
		if got.String() != tt.want {
			t.Errorf("frameUUIDFromRootFS(%q) = %s, want %s", tt.rootFS, got, tt.want)
		}
	}
}

func TestFrameUUIDFromRootFSValidUUID(t *testing.T) {
	fsDir := "/var/lib/thundersnap/fs"
	old := flagFsDir
	flagFsDir = &fsDir
	defer func() { flagFsDir = old }()

	// Generate a real UUID and verify round-trip
	uuid := frameid.MustNew()
	rootFS := "/var/lib/thundersnap/fs/testuser/" + uuid.String()

	got, err := frameUUIDFromRootFS(rootFS)
	if err != nil {
		t.Fatalf("frameUUIDFromRootFS(%q): %v", rootFS, err)
	}
	if got != uuid {
		t.Errorf("frameUUIDFromRootFS(%q) = %s, want %s", rootFS, got, uuid)
	}
}

func TestSanitizeForPath(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"foo", "foo"},
		{"a/b", "a_b"},
		{"a\x00b", "a_b"},        // null byte replaced
		{"a..b", "a_b"},          // ".." replaced
		{"..", "_"},              // ".." -> "_" (not empty)
		{"", "_"},                // empty -> "_"
		{".hidden", "hidden"},    // leading dot stripped
		{"...", "_."},            // ".." consumed first, trailing "." kept, no leading dot to trim
		{"../../etc", "____etc"}, // each ".." and "/" becomes "_"
	}
	for _, tt := range tests {
		if got := sanitizeForPath(tt.in); got != tt.want {
			t.Errorf("sanitizeForPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestStripDomain(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"user@example.com", "user"},
		{"user", "user"},
		{"user@a@b", "user"}, // splits at first @
		{"@host", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := stripDomain(tt.in); got != tt.want {
			t.Errorf("stripDomain(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// parseFrameSpec and isFrameSpec are covered in snap_test.go; the extra cases
// worth pinning here are the >3-part drop and the empty spec.
func TestParseFrameSpecExtraParts(t *testing.T) {
	if r, h, w := parseFrameSpec("root:home:work:extra"); r != "root" || h != "home" || w != "work" {
		t.Errorf("parseFrameSpec(4 parts) = (%q,%q,%q), want (root,home,work) with 4th dropped", r, h, w)
	}
	if r, h, w := parseFrameSpec(""); r != "" || h != "" || w != "" {
		t.Errorf("parseFrameSpec(\"\") = (%q,%q,%q), want empty", r, h, w)
	}
}

func TestHasBlankRootfs(t *testing.T) {
	tests := []struct {
		spec                   string
		isBlank, isExplicitNil bool
	}{
		{"root:home", false, false}, // non-blank rootfs
		{"nil:home", true, true},    // explicit "nil"
		{":home", true, false},      // empty rootfs, not explicit
		{"::", true, false},         // empty rootfs
		{"root", false, false},      // not a frame spec (no colon)
		{"", false, false},          // not a frame spec
	}
	for _, tt := range tests {
		isBlank, isExplicitNil := hasBlankRootfs(tt.spec)
		if isBlank != tt.isBlank || isExplicitNil != tt.isExplicitNil {
			t.Errorf("hasBlankRootfs(%q) = (%v,%v), want (%v,%v)",
				tt.spec, isBlank, isExplicitNil, tt.isBlank, tt.isExplicitNil)
		}
	}
}

func TestDeriveDataDirs(t *testing.T) {
	fsDir, snapsDir := deriveDataDirs("/var/lib/thundersnap")
	if fsDir != "/var/lib/thundersnap/fs" {
		t.Errorf("fsDir = %q, want /var/lib/thundersnap/fs", fsDir)
	}
	if snapsDir != "/var/lib/thundersnap/snaps" {
		t.Errorf("snapsDir = %q, want /var/lib/thundersnap/snaps", snapsDir)
	}
}

func TestParseRangeHeader(t *testing.T) {
	const size = 1000
	tests := []struct {
		name       string
		header     string
		fileSize   int64
		start, end int64
		wantErr    bool
	}{
		{name: "full range", header: "bytes=0-99", fileSize: size, start: 0, end: 99},
		{name: "open-ended", header: "bytes=100-", fileSize: size, start: 100, end: 999},
		{name: "suffix", header: "bytes=-500", fileSize: size, start: 500, end: 999},
		{name: "suffix larger than file", header: "bytes=-5000", fileSize: size, start: 0, end: 999},
		{name: "end clamped to file size", header: "bytes=0-5000", fileSize: size, start: 0, end: 999},
		{name: "start at last byte", header: "bytes=999-999", fileSize: size, start: 999, end: 999},
		{name: "missing prefix", header: "0-99", fileSize: size, wantErr: true},
		{name: "multi-range rejected", header: "bytes=0-10,20-30", fileSize: size, wantErr: true},
		{name: "start beyond file", header: "bytes=1000-1001", fileSize: size, wantErr: true},
		{name: "start greater than end", header: "bytes=50-10", fileSize: size, wantErr: true},
		{name: "empty range", header: "bytes=-", fileSize: size, wantErr: true},
		{name: "non-numeric start", header: "bytes=abc-10", fileSize: size, wantErr: true},
		{name: "non-numeric end", header: "bytes=10-abc", fileSize: size, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, err := parseRangeHeader(tt.header, tt.fileSize)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseRangeHeader(%q, %d) = (%d,%d,nil), want error",
						tt.header, tt.fileSize, start, end)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRangeHeader(%q, %d): %v", tt.header, tt.fileSize, err)
			}
			if start != tt.start || end != tt.end {
				t.Errorf("parseRangeHeader(%q, %d) = (%d,%d), want (%d,%d)",
					tt.header, tt.fileSize, start, end, tt.start, tt.end)
			}
		})
	}
}
