// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// Package tspkgs builds thundersnap release packages for distribution.
package tspkgs

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Target is something that can be built.
type Target interface {
	String() string
	Build(b *Build) ([]string, error)
}

// Build is a build context for Targets.
type Build struct {
	// Repo is a path to the root Go module for the build.
	Repo string
	// Out is where build artifacts are written.
	Out string
	// Verbose is whether to print all command output, rather than just
	// failed commands.
	Verbose bool

	// Tmp is a temporary directory that gets deleted when the Build is closed.
	Tmp string
	// Go is the path to the Go binary to use for building.
	Go string
	// Version is a sanitized version string used for package file names
	// by the tgz and rpm targets (see getVersion). It strips a leading "v"
	// and wraps a bare commit hash (no tags) as "0.0.0-<hash>" so the file
	// name is sortable. The deb targets use EmbedVersion instead (see
	// pkgs.go), so dpkg's Version field matches `ts version` and the filename.
	//
	// For tagged builds Version and EmbedVersion are the same string; they
	// differ only for bare-commit builds, which cannot occur once any tag
	// exists.
	Version string
	// EmbedVersion is the raw version string baked into binaries via
	// -ldflags (X version.Version), i.e. what `ts --version` and `ts version`
	// print (computed by scripts/version.sh, which strips a leading "v").
	// The deb targets also use this for both the dpkg Version field and the
	// .deb filename so all three report the same string.
	EmbedVersion string
	// Time is the timestamp of the build.
	Time time.Time

	goBuilds     sync.Map
	goBuildLimit chan struct{}
}

// NewBuild creates a new Build rooted at repo, and writing artifacts to out.
func NewBuild(repo, out string) (*Build, error) {
	if err := os.MkdirAll(out, 0750); err != nil {
		return nil, fmt.Errorf("creating out dir: %w", err)
	}
	tmp, err := os.MkdirTemp("", "tspkgs-*")
	if err != nil {
		return nil, fmt.Errorf("creating tempdir: %w", err)
	}
	repo, err = findModRoot(repo)
	if err != nil {
		return nil, fmt.Errorf("finding module root: %w", err)
	}
	goTool, err := findGo(repo)
	if err != nil {
		return nil, fmt.Errorf("finding go binary: %w", err)
	}
	return &Build{
		Repo:         repo,
		Tmp:          tmp,
		Out:          out,
		Go:           goTool,
		Version:      getVersion(repo),
		EmbedVersion: rawVersion(repo),
		Time:         time.Now().UTC(),
		goBuildLimit: make(chan struct{}, runtime.NumCPU()),
	}, nil
}

// Close cleans up temporary files.
func (b *Build) Close() error {
	return os.RemoveAll(b.Tmp)
}

// Build builds all targets concurrently.
func (b *Build) Build(targets []Target) (files []string, err error) {
	if len(targets) == 0 {
		return nil, errors.New("no targets specified")
	}
	log.Printf("Building %d targets: %v", len(targets), targets)
	var (
		wg         sync.WaitGroup
		errs       = make([]error, len(targets))
		buildFiles = make([][]string, len(targets))
	)
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t Target) {
			var err error
			defer func() {
				if err != nil {
					err = fmt.Errorf("%s: %w", t, err)
				}
				errs[i] = err
				wg.Done()
			}()
			fs, err := t.Build(b)
			buildFiles[i] = fs
		}(i, t)
	}
	wg.Wait()

	for _, fs := range buildFiles {
		files = append(files, fs...)
	}
	sort.Strings(files)

	return files, errors.Join(errs...)
}

type goBuildResult struct {
	path string
	err  error
	done chan struct{} // closed when build is complete
}

// BuildGoBinary builds the Go binary at path and returns the path to the
// binary. Builds are cached by path and env, so each build only happens once
// per process execution.
func (b *Build) BuildGoBinary(path string, env map[string]string) (string, error) {
	var envStrs []string
	for k, v := range env {
		envStrs = append(envStrs, k+"="+v)
	}
	sort.Strings(envStrs)
	cacheKey := path + "\x00" + strings.Join(envStrs, "\x00")

	r := &goBuildResult{done: make(chan struct{})}
	if v, loaded := b.goBuilds.LoadOrStore(cacheKey, r); loaded {
		// Another goroutine is already building (or has built) this binary.
		r = v.(*goBuildResult)
		<-r.done
		return r.path, r.err
	}

	// We won the race; do the actual build.
	defer close(r.done)

	b.goBuildLimit <- struct{}{}
	defer func() { <-b.goBuildLimit }()

	log.Printf("Building %s (with env %s)", path, strings.Join(envStrs, " "))
	buildDir := b.TmpDir()
	// Bake the version into the binary (X version.Version), so `ts --version`
	// and the client/server version check work on shipped packages. The value
	// is quoted-inert for go's -ldflags argument passing (no spaces).
	ldflags := "-X github.com/tailscale/thundersnap/version.Version=" + b.EmbedVersion
	cmd := exec.Command(b.Go, "build", "-v", "-trimpath", "-ldflags", ldflags, "-o", buildDir, path)
	cmd.Dir = b.Repo
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if b.Verbose {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	} else {
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
	}
	r.err = cmd.Run()
	r.path = filepath.Join(buildDir, filepath.Base(path))
	return r.path, r.err
}

// GoPkg returns the directory on disk of pkg.
func (b *Build) GoPkg(pkg string) (string, error) {
	cmd := exec.Command(b.Go, "list", "-f", "{{.Dir}}", pkg)
	cmd.Dir = b.Repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("finding package %q: %w\n%s", pkg, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// TmpDir creates and returns a new empty temporary directory.
func (b *Build) TmpDir() string {
	ret, err := os.MkdirTemp(b.Tmp, "")
	if err != nil {
		panic(fmt.Sprintf("creating temp dir: %v", err))
	}
	return ret
}

// FilterTargets returns the subset of targets that match any of the filters.
// If filters is empty, returns all targets.
func FilterTargets(targets []Target, filters []string) ([]Target, error) {
	var filts []*regexp.Regexp
	for _, f := range filters {
		if f == "all" {
			return targets, nil
		}
		filt, err := regexp.Compile(f)
		if err != nil {
			return nil, fmt.Errorf("invalid filter %q: %w", f, err)
		}
		filts = append(filts, filt)
	}
	var ret []Target
	for _, t := range targets {
		for _, filt := range filts {
			if filt.MatchString(t.String()) {
				ret = append(ret, t)
				break
			}
		}
	}
	return ret, nil
}

func findModRoot(path string) (string, error) {
	for {
		if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
			return path, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", fmt.Errorf("no go.mod found in %q or any parent", path)
		}
		path = parent
	}
}

// findGo returns the path to the Go binary.
// It first looks in the "tool" directory of the repo, then in $PATH.
func findGo(repo string) (string, error) {
	tool := filepath.Join(repo, "tool", "go")
	if _, err := os.Stat(tool); err == nil {
		return tool, nil
	}
	p, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("go not found in PATH: %w", err)
	}
	return p, nil
}

// getVersion returns a version string for the current build for use as the
// tgz and rpm file-name version. It runs "git describe --tags --always
// --dirty", strips a leading "v", and — if there are no tags so the result is
// just a bare commit hash like "60d15db" — wraps it as "0.0.0-60d15db" so the
// file name is still sortable. Tagged versions like "v1.2.3" are returned as
// "1.2.3".
//
// This is intentionally distinct from rawVersion (EmbedVersion), which is
// what `ts --version`/`ts version` print and what the deb targets use for
// both dpkg's Version field and the .deb filename; for tagged builds the two
// are identical, differing only in the no-tag bare-hash wrapping here.
func getVersion(repo string) string {
	cmd := exec.Command("git", "describe", "--tags", "--always", "--dirty")
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "0.0.0-dev"
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "0.0.0-dev"
	}
	v = strings.TrimPrefix(v, "v")
	// If the version is just a bare commit hash (no tags), wrap it.
	if !strings.Contains(v, ".") {
		return "0.0.0-" + v
	}
	return v
}

// rawVersion returns the raw version string for embedding into binaries via
// -ldflags (X version.Version), i.e. what `ts --version` and `ts version`
// print. It prefers scripts/version.sh — which runs `git describe --tags
// --always --dirty`, strips a leading "v" so a tag "v0.03" yields "0.03",
// handles the jj-only fallback and the THUNDERSNAP_VERSION override, and
// falls back to an inline "git describe" (then "unknown") if the script is
// missing (older/dist checkouts) so a build always gets a string.
//
// The deb targets use this (as b.EmbedVersion) for both the dpkg Version field
// (with nfpm VersionSchema "none" to prevent semver re-serialization) and the
// .deb filename, so dpkg, the filename, and `ts version` all match.
func rawVersion(repo string) string {
	if script, err := filepath.Abs(filepath.Join(repo, "scripts", "version.sh")); err == nil {
		out, err := exec.Command(script).Output()
		if err == nil {
			if v := strings.TrimSpace(string(out)); v != "" {
				return v
			}
		}
	}
	cmd := exec.Command("git", "describe", "--tags", "--always", "--dirty")
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "unknown"
	}
	if v := strings.TrimSpace(string(out)); v != "" {
		return v
	}
	return "unknown"
}
