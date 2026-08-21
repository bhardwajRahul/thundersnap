package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolverConfigFromDHCP(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name: "virtualization framework NAT",
			content: "#PROTO: DHCP\n" +
				"domain corp.ts.net\n" +
				"nameserver 192.168.64.1\n" +
				"bootserver 192.168.64.1\n",
			want: "nameserver 192.168.64.1\n",
		},
		{
			name: "multiple resolvers",
			content: "nameserver 2001:4860:4860::8888\n" +
				"nameserver 8.8.8.8\n",
			want: "nameserver 2001:4860:4860::8888\n" +
				"nameserver 8.8.8.8\n",
		},
		{
			name:    "missing resolver falls back to passt",
			content: "#PROTO: DHCP\ndomain example.test\n",
			want:    "nameserver 10.0.2.3\n",
		},
		{
			name:    "invalid resolver is ignored",
			content: "nameserver definitely-not-an-ip\n",
			want:    "nameserver 10.0.2.3\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pnp")
			if err := os.WriteFile(path, []byte(tt.content), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := resolverConfigFromDHCP(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("resolver config = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolverConfigFromDHCPMissingFile(t *testing.T) {
	got, err := resolverConfigFromDHCP(filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "nameserver 10.0.2.3\n"; string(got) != want {
		t.Fatalf("resolver config = %q, want %q", got, want)
	}
}
