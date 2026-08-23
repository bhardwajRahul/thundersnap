// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// Package version holds the thundersnap build version string.
//
// The version is computed by scripts/version.sh at build time — from "git
// describe" when a git repository (or a jj repo colocated with git) is present,
// with a jj-only fallback and an explicit override — and injected via -ldflags:
//
//	go build -ldflags "-X github.com/tailscale/thundersnap/version.Version=<v>"
//
// When built without that flag (e.g. a bare `go build ./cmd/ts` with no
// Makefile), Version keeps its default "(devel)" so the binary still reports a
// sensible, honest string instead of an empty one.
package version

import (
	"fmt"
	"os"
)

// Version is the build version string, injected via -ldflags at build time.
// It is "(devel)" when no version was injected.
var Version = "(devel)"

// String returns the version string.
func String() string {
	return Version
}

// HandleFlag implements the universal "--version" / "-version" flag for
// thundersnap binaries that do not otherwise parse flags (e.g. tiny transport
// helpers and the dist tool). It scans os.Args for an exact "--version" or
// "-version" token (also the "--version=true" form) appearing before the first
// non-option argument and, if present, prints "<prog> <version>" to stdout and
// exits 0.
//
// Call it at the top of main(), before any flag parsing or required-argument
// checks, so "<binary> --version" always works quickly and offline even in a
// binary that otherwise needs flags, a control socket, or root. Binaries that
// already use a flag library should register --version as a real flag there
// (so it appears in --help) rather than calling this; this helper is for the
// no-flag-library case.
func HandleFlag(prog string) {
	for _, a := range os.Args[1:] {
		switch a {
		case "--version", "-version", "--version=true", "-version=true":
			fmt.Printf("%s %s\n", prog, Version)
			os.Exit(0)
		}
		// Stop at the first positional/non-option argument so
		// "<binary> <subcommand> --version" is not mistaken for the global
		// flag and is instead handled by the subcommand.
		if len(a) == 0 || a[0] != '-' {
			return
		}
	}
}
