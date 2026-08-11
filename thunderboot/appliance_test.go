package thunderboot

import "testing"

func TestParseDiskSize(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want int64
	}{
		{"512M", 512 << 20},
		{"2G", 2 << 30},
		{"2T", 2 << 40},
	} {
		got, err := parseDiskSize(tt.in)
		if err != nil {
			t.Fatalf("parseDiskSize(%q): %v", tt.in, err)
		}
		if got != tt.want {
			t.Errorf("parseDiskSize(%q)=%d, want %d", tt.in, got, tt.want)
		}
	}
	for _, in := range []string{"", "1", "0G", "-1G", "1K", "999999999999999999999T"} {
		if _, err := parseDiskSize(in); err == nil {
			t.Errorf("parseDiskSize(%q) unexpectedly succeeded", in)
		}
	}
}
