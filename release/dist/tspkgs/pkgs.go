// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package tspkgs

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/goreleaser/nfpm/v2"
	"github.com/goreleaser/nfpm/v2/files"
)

// symlinkLatest creates a thundersnap_latest_<arch>.<ext> symlink pointing to filename.
// filename is e.g. "thundersnap_1.2.3_amd64.deb"; the symlink replaces the version with "latest".
func symlinkLatest(outDir, filename, arch, ext string) {
	link := fmt.Sprintf("thundersnap_latest_%s.%s", arch, ext)
	linkPath := filepath.Join(outDir, link)
	os.Remove(linkPath)
	if err := os.Symlink(filename, linkPath); err != nil {
		log.Printf("Warning: failed to create symlink %s: %v", link, err)
	}
}

type tgzTarget struct {
	goEnv map[string]string
}

func (t *tgzTarget) os() string   { return t.goEnv["GOOS"] }
func (t *tgzTarget) arch() string { return t.goEnv["GOARCH"] }

func (t *tgzTarget) String() string {
	return fmt.Sprintf("%s/%s/tgz", t.os(), t.arch())
}

func (t *tgzTarget) Build(b *Build) ([]string, error) {
	ts, err := b.BuildGoBinary("github.com/tailscale/thundersnap/cmd/ts", t.goEnv)
	if err != nil {
		return nil, err
	}
	tsd, err := b.BuildGoBinary("github.com/tailscale/thundersnap/cmd/thundersnapd", t.goEnv)
	if err != nil {
		return nil, err
	}
	vshd, err := b.BuildGoBinary("github.com/tailscale/thundersnap/cmd/vshd", t.goEnv)
	if err != nil {
		return nil, err
	}
	vsh, err := b.BuildGoBinary("github.com/tailscale/thundersnap/cmd/vsh", t.goEnv)
	if err != nil {
		return nil, err
	}

	filename := fmt.Sprintf("thundersnap_%s_%s.tgz", b.Version, t.arch())
	log.Printf("Building %s", filename)

	out := filepath.Join(b.Out, filename)
	f, err := os.Create(out)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gw := gzip.NewWriter(f)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	addFile := func(src, dst string, mode int64) error {
		sf, err := os.Open(src)
		if err != nil {
			return err
		}
		defer sf.Close()
		fi, err := sf.Stat()
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:    dst,
			Size:    fi.Size(),
			Mode:    mode,
			ModTime: b.Time,
			Uid:     0,
			Gid:     0,
			Uname:   "root",
			Gname:   "root",
		}); err != nil {
			return err
		}
		_, err = io.Copy(tw, sf)
		return err
	}
	addDir := func(name string) error {
		return tw.WriteHeader(&tar.Header{
			Name:    name + "/",
			Mode:    0755,
			ModTime: b.Time,
			Uid:     0,
			Gid:     0,
			Uname:   "root",
			Gname:   "root",
		})
	}

	dir := strings.TrimSuffix(filename, ".tgz")
	if err := addDir(dir); err != nil {
		return nil, err
	}
	if err := addFile(tsd, filepath.Join(dir, "thundersnapd"), 0755); err != nil {
		return nil, err
	}
	if err := addFile(vsh, filepath.Join(dir, "vsh"), 0755); err != nil {
		return nil, err
	}
	libexecDir := filepath.Join(dir, "libexec")
	if err := addDir(libexecDir); err != nil {
		return nil, err
	}
	if err := addFile(ts, filepath.Join(libexecDir, "ts"), 0755); err != nil {
		return nil, err
	}
	if err := addFile(vshd, filepath.Join(libexecDir, "vshd"), 0755); err != nil {
		return nil, err
	}

	// Include systemd files.
	sysDir := filepath.Join(dir, "systemd")
	if err := addDir(sysDir); err != nil {
		return nil, err
	}
	thundersnapdDir, err := b.GoPkg("github.com/tailscale/thundersnap/cmd/thundersnapd")
	if err != nil {
		return nil, err
	}
	if err := addFile(filepath.Join(thundersnapdDir, "thundersnapd.service"), filepath.Join(sysDir, "thundersnapd.service"), 0644); err != nil {
		return nil, err
	}
	if err := addFile(filepath.Join(thundersnapdDir, "thundersnapd.defaults"), filepath.Join(sysDir, "thundersnapd.defaults"), 0644); err != nil {
		return nil, err
	}
	if err := addFile(filepath.Join(thundersnapdDir, "policy.jsonc"), filepath.Join(sysDir, "policy.jsonc"), 0644); err != nil {
		return nil, err
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	symlinkLatest(b.Out, filename, t.arch(), "tgz")
	return []string{filename}, nil
}

type debTarget struct {
	goEnv map[string]string
}

func (t *debTarget) os() string   { return t.goEnv["GOOS"] }
func (t *debTarget) arch() string { return t.goEnv["GOARCH"] }

func (t *debTarget) String() string {
	return fmt.Sprintf("linux/%s/deb", t.arch())
}

func (t *debTarget) Build(b *Build) ([]string, error) {
	if t.os() != "linux" {
		return nil, errors.New("deb only supported on linux")
	}

	ts, err := b.BuildGoBinary("github.com/tailscale/thundersnap/cmd/ts", t.goEnv)
	if err != nil {
		return nil, err
	}
	tsd, err := b.BuildGoBinary("github.com/tailscale/thundersnap/cmd/thundersnapd", t.goEnv)
	if err != nil {
		return nil, err
	}
	vshd, err := b.BuildGoBinary("github.com/tailscale/thundersnap/cmd/vshd", t.goEnv)
	if err != nil {
		return nil, err
	}
	vsh, err := b.BuildGoBinary("github.com/tailscale/thundersnap/cmd/vsh", t.goEnv)
	if err != nil {
		return nil, err
	}

	thundersnapdDir, err := b.GoPkg("github.com/tailscale/thundersnap/cmd/thundersnapd")
	if err != nil {
		return nil, err
	}

	arch := debArch(t.arch())
	contents, err := files.PrepareForPackager(files.Contents{
		&files.Content{
			Type:        files.TypeFile,
			Source:      ts,
			Destination: "/usr/libexec/thundersnap/ts",
		},
		&files.Content{
			Type:        files.TypeFile,
			Source:      vshd,
			Destination: "/usr/libexec/thundersnap/vshd",
		},
		&files.Content{
			Type:        files.TypeFile,
			Source:      tsd,
			Destination: "/usr/sbin/thundersnapd",
		},
		&files.Content{
			Type:        files.TypeFile,
			Source:      vsh,
			Destination: "/usr/bin/vsh",
		},
		&files.Content{
			Type:        files.TypeFile,
			Source:      filepath.Join(thundersnapdDir, "thundersnapd.service"),
			Destination: "/lib/systemd/system/thundersnapd.service",
		},
		&files.Content{
			Type:        files.TypeConfigNoReplace,
			Source:      filepath.Join(thundersnapdDir, "thundersnapd.defaults"),
			Destination: "/etc/default/thundersnapd",
		},
		&files.Content{
			Type:        files.TypeConfigNoReplace,
			Source:      filepath.Join(thundersnapdDir, "policy.jsonc"),
			Destination: "/etc/thundersnap/policy.jsonc",
		},
	}, 0, "deb", false, b.Time)
	if err != nil {
		return nil, err
	}
	info := nfpm.WithDefaults(&nfpm.Info{
		Name:        "thundersnap",
		Arch:        arch,
		Platform:    "linux",
		Version:     b.Version,
		Maintainer:  "Tailscale Inc <info@tailscale.com>",
		Description: "Thundersnap: tsnet-based SSH server with isolated container environments",
		Homepage:    "https://github.com/tailscale/thundersnap",
		License:     "BSD-3-Clause",
		Section:     "net",
		Priority:    "extra",
		Overridables: nfpm.Overridables{
			Contents: contents,
			Scripts: nfpm.Scripts{
				PostInstall: filepath.Join(b.Repo, "release/deb/debian.postinst.sh"),
				PreRemove:   filepath.Join(b.Repo, "release/deb/debian.prerm.sh"),
				PostRemove:  filepath.Join(b.Repo, "release/deb/debian.postrm.sh"),
			},
		},
	})
	pkg, err := nfpm.Get("deb")
	if err != nil {
		return nil, err
	}

	filename := fmt.Sprintf("thundersnap_%s_%s.deb", b.Version, arch)
	log.Printf("Building %s", filename)
	f, err := os.Create(filepath.Join(b.Out, filename))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := pkg.Package(info, f); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	symlinkLatest(b.Out, filename, arch, "deb")
	return []string{filename}, nil
}

type rpmTarget struct {
	goEnv map[string]string
}

func (t *rpmTarget) os() string   { return t.goEnv["GOOS"] }
func (t *rpmTarget) arch() string { return t.goEnv["GOARCH"] }

func (t *rpmTarget) String() string {
	return fmt.Sprintf("linux/%s/rpm", t.arch())
}

func (t *rpmTarget) Build(b *Build) ([]string, error) {
	if t.os() != "linux" {
		return nil, errors.New("rpm only supported on linux")
	}

	ts, err := b.BuildGoBinary("github.com/tailscale/thundersnap/cmd/ts", t.goEnv)
	if err != nil {
		return nil, err
	}
	tsd, err := b.BuildGoBinary("github.com/tailscale/thundersnap/cmd/thundersnapd", t.goEnv)
	if err != nil {
		return nil, err
	}
	vshd, err := b.BuildGoBinary("github.com/tailscale/thundersnap/cmd/vshd", t.goEnv)
	if err != nil {
		return nil, err
	}
	vsh, err := b.BuildGoBinary("github.com/tailscale/thundersnap/cmd/vsh", t.goEnv)
	if err != nil {
		return nil, err
	}

	thundersnapdDir, err := b.GoPkg("github.com/tailscale/thundersnap/cmd/thundersnapd")
	if err != nil {
		return nil, err
	}

	arch := rpmArch(t.arch())
	contents, err := files.PrepareForPackager(files.Contents{
		&files.Content{
			Type:        files.TypeFile,
			Source:      ts,
			Destination: "/usr/libexec/thundersnap/ts",
		},
		&files.Content{
			Type:        files.TypeFile,
			Source:      vshd,
			Destination: "/usr/libexec/thundersnap/vshd",
		},
		&files.Content{
			Type:        files.TypeFile,
			Source:      tsd,
			Destination: "/usr/sbin/thundersnapd",
		},
		&files.Content{
			Type:        files.TypeFile,
			Source:      vsh,
			Destination: "/usr/bin/vsh",
		},
		&files.Content{
			Type:        files.TypeFile,
			Source:      filepath.Join(thundersnapdDir, "thundersnapd.service"),
			Destination: "/lib/systemd/system/thundersnapd.service",
		},
		&files.Content{
			Type:        files.TypeConfigNoReplace,
			Source:      filepath.Join(thundersnapdDir, "thundersnapd.defaults"),
			Destination: "/etc/default/thundersnapd",
		},
		&files.Content{
			Type:        files.TypeConfigNoReplace,
			Source:      filepath.Join(thundersnapdDir, "policy.jsonc"),
			Destination: "/etc/thundersnap/policy.jsonc",
		},
		&files.Content{
			Type:        files.TypeDir,
			Destination: "/var/cache/thundersnap",
		},
	}, 0, "rpm", false, b.Time)
	if err != nil {
		return nil, err
	}
	info := nfpm.WithDefaults(&nfpm.Info{
		Name:        "thundersnap",
		Arch:        arch,
		Platform:    "linux",
		Version:     b.Version,
		Maintainer:  "Tailscale Inc <info@tailscale.com>",
		Description: "Thundersnap: tsnet-based SSH server with isolated container environments",
		Homepage:    "https://github.com/tailscale/thundersnap",
		License:     "BSD-3-Clause",
		Overridables: nfpm.Overridables{
			Contents: contents,
			Scripts: nfpm.Scripts{
				PostInstall: filepath.Join(b.Repo, "release/rpm/rpm.postinst.sh"),
				PreRemove:   filepath.Join(b.Repo, "release/rpm/rpm.prerm.sh"),
				PostRemove:  filepath.Join(b.Repo, "release/rpm/rpm.postrm.sh"),
			},
			RPM: nfpm.RPM{
				Group: "Network",
			},
		},
	})
	pkg, err := nfpm.Get("rpm")
	if err != nil {
		return nil, err
	}

	filename := fmt.Sprintf("thundersnap_%s_%s.rpm", b.Version, arch)
	log.Printf("Building %s", filename)

	f, err := os.Create(filepath.Join(b.Out, filename))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := pkg.Package(info, f); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	symlinkLatest(b.Out, filename, arch, "rpm")
	return []string{filename}, nil
}

// debArch returns the debian arch name for the given Go arch name.
func debArch(goarch string) string {
	switch goarch {
	case "386":
		return "i386"
	case "arm":
		return "armhf"
	default:
		return goarch
	}
}

// rpmArch returns the RPM arch name for the given Go arch name.
func rpmArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "386":
		return "i386"
	case "arm":
		return "armv7hl"
	case "arm64":
		return "aarch64"
	default:
		return goarch
	}
}
