package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultOutBase(t *testing.T) {
	tests := []struct {
		name string
		in   string // already filepath.Clean'd, as in main()
		want string
	}{
		{name: "simple relative", in: "snaps/1", want: "snaps/1"},
		{name: "absolute", in: "/var/lib/x", want: "/var/lib/x"},
		{name: "dot", in: ".", want: "."},
		{name: "root maps to root", in: "/", want: "root"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Guard the precondition that the input is Clean'd, matching main().
			if c := filepath.Clean(tt.in); c != tt.in {
				t.Fatalf("test input %q is not Clean'd (= %q)", tt.in, c)
			}
			if got := defaultOutBase(tt.in); got != tt.want {
				t.Errorf("defaultOutBase(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestPrintFileUnknownExtension verifies the extension dispatch rejects
// anything other than .tsm/.tsc (and is case-sensitive: .TSM is unknown).
func TestPrintFileUnknownExtension(t *testing.T) {
	for _, path := range []string{"foo.txt", "foo", "foo.TSM", "foo.TSC"} {
		err := printFile(path)
		if err == nil {
			t.Errorf("printFile(%q) = nil, want unknown-extension error", path)
			continue
		}
		if !strings.Contains(err.Error(), "unknown file extension") {
			t.Errorf("printFile(%q) error = %v, want unknown-extension error", path, err)
		}
	}
}
