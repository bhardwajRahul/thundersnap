package snapsubdir

import "testing"

func TestValidate(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		// Root / empty resolve to the subvolume root -> error.
		{in: "", wantErr: true},
		{in: "/", wantErr: true},
		{in: ".", wantErr: true},
		// Escaping inputs collapse against the anchored "/" to the root, which
		// is then rejected as the root rather than as a traversal. This is the
		// case that proves the old `clean == ".."`/`HasPrefix("../")` branch was
		// unreachable.
		{in: "..", wantErr: true},
		{in: "../x", want: "x"},
		{in: "a/../..", wantErr: true},
		{in: "/..", wantErr: true},
		// Normal subtrees.
		{in: "keep", want: "keep"},
		{in: "/keep", want: "keep"},
		{in: "keep/", want: "keep"},
		{in: "a/b/c", want: "a/b/c"},
		// Cleaning collapses interior "." and ".." and double slashes.
		{in: "a/../b", want: "b"},
		{in: "a//b", want: "a/b"},
		{in: "./keep", want: "keep"},
	}
	for _, tt := range tests {
		got, err := Validate(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("Validate(%q) = %q, nil; want error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Validate(%q) = _, %v; want %q", tt.in, err, tt.want)
			continue
		}
		if got != tt.want {
			t.Errorf("Validate(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
