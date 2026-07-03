// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package containerns

import (
	"os/exec"
	"testing"
)

// TestReleaseUnknown verifies releasing a rootFS that was never registered is a
// no-op (does not panic, does not underflow).
func TestReleaseUnknown(t *testing.T) {
	m := New()
	m.Release("/never/registered") // must not panic
	if len(m.entries) != 0 {
		t.Errorf("entries = %d, want 0", len(m.entries))
	}
}

// TestReleaseRefcount verifies the refcount lifecycle: two references must be
// released twice before init is shut down and the entry removed. A real child
// process (cat) stands in for container-init: like the real init it exits when
// its stdin is closed, so the shutdown path (close stdin, Wait) is exercised
// for real and completes promptly.
func TestReleaseRefcount(t *testing.T) {
	m := New()

	cmd := exec.Command("cat") // reads stdin; exits on EOF when stdin closes
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start cat: %v", err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()

	const key = "/fake/rootfs"
	m.entries[key] = &entry{
		initPid:   cmd.Process.Pid,
		initStdin: stdin,
		initCmd:   cmd,
		refCount:  2,
	}

	// First release: refcount 2 -> 1, entry stays.
	m.Release(key)
	if _, ok := m.entries[key]; !ok {
		t.Fatal("entry removed after first release (refcount should be 1)")
	}
	if rc := m.entries[key].refCount; rc != 1 {
		t.Errorf("refCount = %d, want 1", rc)
	}

	// Second release: refcount 1 -> 0, init shut down and entry removed.
	m.Release(key)
	if _, ok := m.entries[key]; ok {
		t.Error("entry not removed after final release")
	}
}
