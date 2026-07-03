// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package cgroup

import (
	"strings"
	"testing"
)

// TestNewParentName confirms New stores the parent name verbatim and ParentName
// returns it, since callers build per-container leaf names from it.
func TestNewParentName(t *testing.T) {
	m := New("thundersnap-1234")
	if got := m.ParentName(); got != "thundersnap-1234" {
		t.Errorf("ParentName() = %q, want %q", got, "thundersnap-1234")
	}
}

// TestGetSystemMemoryBytes confirms the /proc/meminfo parser returns a plausible
// non-zero total on a real Linux host (the e2e/test environment is Linux).
func TestGetSystemMemoryBytes(t *testing.T) {
	mem, err := getSystemMemoryBytes()
	if err != nil {
		t.Fatalf("getSystemMemoryBytes() error: %v", err)
	}
	if mem == 0 {
		t.Error("getSystemMemoryBytes() = 0, want non-zero total memory")
	}
	// MemTotal is reported in KB and converted to bytes, so the result must be a
	// multiple of 1024 and well above a trivially small value.
	if mem%1024 != 0 {
		t.Errorf("getSystemMemoryBytes() = %d, not a multiple of 1024", mem)
	}
	if mem < 1<<20 {
		t.Errorf("getSystemMemoryBytes() = %d bytes, implausibly small", mem)
	}
}

// TestConstantsInRange guards the OOM score bias against the kernel's valid
// oom_score_adj range so a future tweak cannot silently produce a value the
// kernel rejects.
func TestConstantsInRange(t *testing.T) {
	if containerOOMScore < -1000 || containerOOMScore > 1000 {
		t.Errorf("containerOOMScore = %d, outside kernel range -1000..1000", containerOOMScore)
	}
}

// TestParentNameFormat documents the expected "thundersnap-<pid>" shape used by
// the daemon, matching what callers split on to build leaf cgroup paths.
func TestParentNameFormat(t *testing.T) {
	m := New("thundersnap-42")
	if !strings.HasPrefix(m.ParentName(), "thundersnap-") {
		t.Errorf("ParentName() = %q, want thundersnap- prefix", m.ParentName())
	}
}
