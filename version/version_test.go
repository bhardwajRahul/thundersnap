// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package version

import (
	"testing"
)

func TestDefaultVersion(t *testing.T) {
	// Version is only overwritten by -ldflags at build time; the package itself
	// leaves it at the "(devel)" default so a bare `go build` always reports a
	// non-empty, honest string.
	if Version == "" {
		t.Errorf("Version is empty; default should be \"(devel)\"")
	}
	if got := String(); got != Version {
		t.Errorf("String() = %q, want %q", got, Version)
	}
}
