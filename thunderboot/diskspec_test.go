package thunderboot

import "testing"

func TestParseDiskSpec(t *testing.T) {
	for _, tt := range []struct {
		in, want string
		allowNBD bool
	}{
		{"/dev/vda", "/dev/vda", true},
		{"raid0;/dev/vda,/dev/vdb", "raid0;/dev/vda,/dev/vdb", false},
		{"raid1;/dev/sda,/dev/sdb", "raid1;/dev/sda,/dev/sdb", true},
		{"nbd://10.0.2.2:10809/export", "nbd://10.0.2.2:10809/export", true},
	} {
		got, err := ParseDiskSpec(tt.in, tt.allowNBD)
		if err != nil {
			t.Fatalf("ParseDiskSpec(%q): %v", tt.in, err)
		}
		if got.String() != tt.want {
			t.Errorf("ParseDiskSpec(%q).String()=%q, want %q", tt.in, got.String(), tt.want)
		}
	}
	for _, in := range []string{"raid5;/dev/a,/dev/b", "raid0;/dev/a", "nbd://"} {
		if _, err := ParseDiskSpec(in, true); err == nil {
			t.Errorf("ParseDiskSpec(%q) succeeded", in)
		}
	}
}
