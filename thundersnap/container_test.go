package thundersnap

import (
	"os/exec"
	"strings"
	"testing"
)

// TestBuildNsenterCmd verifies the single source of truth for the
// `nsenter ... ts drop-caps-and-run` invocation that RunInContainerNs and
// StartInContainerNs share, including the tsBinary default.
func TestBuildNsenterCmd(t *testing.T) {
	cmd := buildNsenterCmd("/abs/root", "", 4242, "/bin/sh", "-c", "echo hi")

	if got := cmd.Path; !strings.HasSuffix(got, "nsenter") {
		t.Errorf("cmd.Path = %q, want it to end in nsenter", got)
	}
	if cmd.Dir != "/" {
		t.Errorf("cmd.Dir = %q, want /", cmd.Dir)
	}

	want := []string{
		"nsenter",
		"-t", "4242", "-p", "-m", "-u", "--",
		"/abs/root/bin/ts", "drop-caps-and-run", "--chroot=/abs/root", "--",
		"/bin/sh", "-c", "echo hi",
	}
	if !equalArgs(cmd.Args, want) {
		t.Errorf("cmd.Args =\n  %v\nwant\n  %v", cmd.Args, want)
	}
}

// TestBuildNsenterCmdExplicitTSBinary verifies an explicit tsBinary overrides
// the <rootFS>/bin/ts default.
func TestBuildNsenterCmdExplicitTSBinary(t *testing.T) {
	cmd := buildNsenterCmd("/abs/root", "/custom/ts", 1, "true")
	want := []string{
		"nsenter",
		"-t", "1", "-p", "-m", "-u", "--",
		"/custom/ts", "drop-caps-and-run", "--chroot=/abs/root", "--",
		"true",
	}
	if !equalArgs(cmd.Args, want) {
		t.Errorf("cmd.Args =\n  %v\nwant\n  %v", cmd.Args, want)
	}
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestReleaseContainerNsUnknown verifies releasing a rootFS that was never
// registered is a no-op (does not panic, does not underflow).
func TestReleaseContainerNsUnknown(t *testing.T) {
	m := NewContainerNsManager()
	m.ReleaseContainerNs("/never/registered") // must not panic
	if len(m.entries) != 0 {
		t.Errorf("entries = %d, want 0", len(m.entries))
	}
}

// TestReleaseContainerNsRefcount verifies the refcount lifecycle: two
// references must be released twice before init is shut down and the entry
// removed. A real child process (cat) stands in for container-init: like the
// real init it exits when its stdin is closed, so the shutdown path
// (close stdin, Wait) is exercised for real and completes promptly.
func TestReleaseContainerNsRefcount(t *testing.T) {
	m := NewContainerNsManager()

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
	m.entries[key] = &containerNsEntry{
		initPid:   cmd.Process.Pid,
		initStdin: stdin,
		initCmd:   cmd,
		refCount:  2,
	}

	// First release: refcount 2 -> 1, entry stays.
	m.ReleaseContainerNs(key)
	if _, ok := m.entries[key]; !ok {
		t.Fatal("entry removed after first release (refcount should be 1)")
	}
	if rc := m.entries[key].refCount; rc != 1 {
		t.Errorf("refCount = %d, want 1", rc)
	}

	// Second release: refcount 1 -> 0, init shut down and entry removed.
	m.ReleaseContainerNs(key)
	if _, ok := m.entries[key]; ok {
		t.Error("entry not removed after final release")
	}
}
