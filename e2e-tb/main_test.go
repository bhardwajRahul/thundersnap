//go:build e2e

package e2e_tb

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorageLayouts(t *testing.T) {
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	thunderboot := filepath.Join(root, "bin", "thunderboot")
	// Start the rebased infiniblock NBD server on the passt host-loopback
	// gateway. Port 0 avoids colliding with developer services and parallel runs.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	nbdBacking := filepath.Join(t.TempDir(), "infiniblock.sparse")
	infiniblock := exec.Command(filepath.Join(root, "bin", "infiniblockd"),
		"--addr", fmt.Sprintf("127.0.0.1:%d", port),
		"--backing", nbdBacking,
		"--size", fmt.Sprint(512<<20))
	infiniblock.Stdout, infiniblock.Stderr = os.Stdout, os.Stderr
	if err := infiniblock.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = infiniblock.Process.Kill(); _ = infiniblock.Wait() })

	layouts := []struct {
		name, cache, disk string
	}{

		{"single", "", "disk.img"},
		{"disk-raid0", "", "raid0;d0.img,d1.img"},
		{"disk-raid1", "", "raid1;d0.img,d1.img"},
		{"bcache", "cache.img", "disk.img"},
		{"bcache-raids", "raid0;c0.img,c1.img", "raid1;d0.img,d1.img"},
		{"bcache-nbd", "cache.img", fmt.Sprintf("nbd://10.0.2.2:%d/default", port)},
	}
	for _, tt := range layouts {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			rewrite := func(spec string) string {
				if spec == "" || strings.HasPrefix(spec, "nbd://") {
					return spec
				}
				prefix, devices := "", spec
				if p, d, ok := strings.Cut(spec, ";"); ok {
					prefix, devices = p+";", d
				}
				parts := strings.Split(devices, ",")
				for i := range parts {
					parts[i] = filepath.Join(dir, parts[i])
				}
				return prefix + strings.Join(parts, ",")
			}
			args := []string{"--appliance", "--testonly", "--vmdir", filepath.Join(root, "vm"), "--initramfs", filepath.Join(root, "thunderboot-out/initramfs.cpio"), "--memory-mb", "2048", "--disk-size", "512M", "--disk", rewrite(tt.disk)}
			if tt.cache != "" {
				args = append(args, "--cache", rewrite(tt.cache))
			}
			cmd := exec.Command(thunderboot, args...)
			cmd.Env = os.Environ()
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("thunderboot: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), "THUNDERBOOT STORAGE OK") {
				t.Fatalf("missing success marker:\n%s", out)
			}
			t.Logf("%s", fmt.Sprintf("%s storage booted", tt.name))
		})
	}
}
