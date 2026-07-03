// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/tailscale/thundersnap/containerns"
)

// openContainerPTSNumber opens the container's ptmx (via /proc/<pid>/root, the
// exact path thundersnapd's openContainerPTY uses) and returns the master fd
// plus the allocated slave pts number. The caller must Close the master.
func openContainerPTSNumber(t *testing.T, initPid int) (*os.File, int) {
	t.Helper()
	ptmxPath := fmt.Sprintf("/proc/%d/root/dev/pts/ptmx", initPid)
	f, err := os.OpenFile(ptmxPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open %s: %v", ptmxPath, err)
	}
	var n int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&n))); errno != 0 {
		f.Close()
		t.Fatalf("unlock pts: %v", errno)
	}
	var ptyno uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&ptyno))); errno != 0 {
		f.Close()
		t.Fatalf("get ptn: %v", errno)
	}
	return f, int(ptyno)
}

// TestContainerSharedDevpts verifies that multiple sessions to the same
// container share ONE devpts instance with correct, distinct PTY numbering.
//
// This is a low-level guard on the shared namespace + ptmx-via-/proc/<pid>/root
// path that thundersnapd's openContainerPTY uses: concurrent opens get distinct
// numbers (0 and 1) and a sequential open after close reuses 0, with no leaked
// slave nodes.
func TestContainerSharedDevpts(t *testing.T) {
	env := newTestEnv(t)
	absFramePath, ns, initPid := setupSharedNsFrame(t, env, "shareddevpts")
	defer ns.Release(absFramePath)

	// A second GetOrCreate for the same rootFS must reuse the same init PID.
	initPid2, err := ns.GetOrCreate(absFramePath, "", "")
	if err != nil {
		t.Fatalf("second GetOrCreate: %v", err)
	}
	defer ns.Release(absFramePath)
	if initPid2 != initPid {
		t.Fatalf("sessions did not share the container namespace: initPid %d != %d", initPid2, initPid)
	}

	m1, p1 := openContainerPTSNumber(t, initPid)
	m2, p2 := openContainerPTSNumber(t, initPid)
	if p1 == p2 {
		m1.Close()
		m2.Close()
		t.Fatalf("concurrent PTYs got the same slave number %d; devpts is NOT shared", p1)
	}
	if p1 != 0 || p2 != 1 {
		t.Errorf("expected concurrent slave numbers 0 and 1, got %d and %d", p1, p2)
	}
	assertDevptsSlaves(t, initPid, []string{"0", "1"})

	m1.Close()
	m2.Close()
	m3, p3 := openContainerPTSNumber(t, initPid)
	defer m3.Close()
	if p3 != 0 {
		t.Errorf("sequential PTY after close: expected reused slave number 0, got %d", p3)
	}
	assertDevptsSlaves(t, initPid, []string{"0"})
}

// TestContainerConcurrentSessionDistinctPTS reproduces the user-reported bug:
// two SIMULTANEOUS live ssh sessions both showed /dev/pts/0 and /dev/pts
// contained only a single node, even though both process trees were alive.
//
// Root cause: the per-session command (ts drop-caps-and-run, run via nsenter
// after joining the container namespace) re-ran setupDev() on every session,
// re-mounting a fresh "newinstance" devpts on top of the one container-init
// already mounted. Each stacked instance restarts pts numbering at 0, so every
// session became pts/0 and /dev/pts showed only the topmost instance's single
// node. The fix passes --skip-mount-setup so sessions reuse the single shared
// devpts that container-init mounted.
//
// This test runs the exact production command form (nsenter + ts
// drop-caps-and-run --chroot --skip-mount-setup) for two concurrent sessions,
// each reporting its own tty via `tty`, and asserts:
//   - the two sessions report DISTINCT pts numbers, and
//   - the shared /dev/pts contains BOTH slave nodes at once.
func TestContainerConcurrentSessionDistinctPTS(t *testing.T) {
	env := newTestEnv(t)
	absFramePath, ns, initPid := setupSharedNsFrame(t, env, "concurrentpts")
	defer ns.Release(absFramePath)

	// busybox provides a static /bin/sh with `tty`, `ls`, `sleep`, and a fifo
	// so each session can report its tty and then block until released.
	installBusyboxShell(t, absFramePath)

	// Two coordination fifos in the container so the test can hold both
	// sessions alive simultaneously, then release them.
	for _, name := range []string{"go1", "go2"} {
		fifo := filepath.Join(absFramePath, "tmp", name)
		os.Remove(fifo)
		if err := syscall.Mkfifo(fifo, 0666); err != nil {
			t.Fatalf("mkfifo %s: %v", name, err)
		}
	}

	type result struct {
		tty string
		err error
	}
	run := func(gofifo string) <-chan result {
		ch := make(chan result, 1)
		go func() {
			// Mirror the production per-session command: join the namespace via
			// nsenter, then ts drop-caps-and-run --chroot --skip-mount-setup,
			// running a shell that prints its tty and blocks on the fifo.
			tsBinary := filepath.Join(absFramePath, "bin", "ts")
			script := "tty; read x < /tmp/" + gofifo
			args := []string{
				"-t", fmt.Sprintf("%d", initPid), "-p", "-m", "-u", "--",
				tsBinary, "drop-caps-and-run",
				"--chroot=" + absFramePath,
				"--skip-mount-setup",
				"--pty",
				"--", "/bin/sh", "-c", script,
			}
			cmd := exec.Command("nsenter", args...)
			cmd.Dir = "/"
			out, err := cmd.Output()
			ch <- result{tty: strings.TrimSpace(string(out)), err: err}
		}()
		return ch
	}

	ch1 := run("go1")
	ch2 := run("go2")

	// Wait until both sessions are alive (both have created their pts and are
	// blocked on the fifo) by polling the shared /dev/pts for two slave nodes.
	waitForDevptsSlaves(t, initPid, 2)

	// While both are alive, the shared devpts must contain two distinct slaves.
	slaves := devptsSlaves(t, initPid)
	if len(slaves) != 2 {
		t.Errorf("expected 2 live pts nodes while both sessions run, got %v", slaves)
	}

	// Release both sessions.
	for _, name := range []string{"go1", "go2"} {
		f, err := os.OpenFile(filepath.Join(absFramePath, "tmp", name), os.O_WRONLY, 0)
		if err != nil {
			t.Fatalf("open fifo %s for release: %v", name, err)
		}
		fmt.Fprintln(f, "go")
		f.Close()
	}

	r1 := <-ch1
	r2 := <-ch2
	if r1.err != nil {
		t.Fatalf("session 1: %v (tty=%q)", r1.err, r1.tty)
	}
	if r2.err != nil {
		t.Fatalf("session 2: %v (tty=%q)", r2.err, r2.tty)
	}
	t.Logf("session ttys: %q and %q", r1.tty, r2.tty)

	if !strings.HasPrefix(r1.tty, "/dev/pts/") || !strings.HasPrefix(r2.tty, "/dev/pts/") {
		t.Fatalf("expected pts ttys, got %q and %q", r1.tty, r2.tty)
	}
	if r1.tty == r2.tty {
		t.Errorf("two concurrent sessions reported the SAME tty %q; devpts is being re-mounted per session", r1.tty)
	}
}

// setupSharedNsFrame creates a frame from the base snapshot, copies ts into it,
// and starts a single shared container namespace, returning the abs frame path,
// the namespace manager, and the container-init PID.
func setupSharedNsFrame(t *testing.T, env *testEnv, name string) (string, *containerns.Manager, int) {
	t.Helper()
	baseSnap := env.createBaseSnapshot()
	framePath := filepath.Join(env.fsDir, "testuser", name)
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	snapPath := filepath.Join(env.snapshotsDir, baseSnap)
	if out, err := exec.Command("btrfs", "subvolume", "snapshot", snapPath, framePath).CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}
	if err := copyFile(env.tsBinary, filepath.Join(framePath, "bin/ts")); err != nil {
		t.Fatalf("copy ts: %v", err)
	}
	absFramePath, err := filepath.Abs(framePath)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	ns := containerns.New()
	initPid, err := ns.GetOrCreate(absFramePath, "", "")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	return absFramePath, ns, initPid
}

// installBusyboxShell installs a static busybox as /bin/sh inside the frame so
// shell scripts (tty, read, ls) work without glibc.
func installBusyboxShell(t *testing.T, absFramePath string) {
	t.Helper()
	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Fatalf("busybox required: %v", err)
	}
	shDst := filepath.Join(absFramePath, "bin/sh")
	if err := os.Remove(shDst); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove sh: %v", err)
	}
	if err := copyFile(busybox, shDst); err != nil {
		t.Fatalf("copy busybox sh: %v", err)
	}
	if err := os.Chmod(shDst, 0755); err != nil {
		t.Fatalf("chmod sh: %v", err)
	}
}

// devptsSlaves returns the numeric slave entries in the container's /dev/pts.
func devptsSlaves(t *testing.T, initPid int) []string {
	t.Helper()
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/root/dev/pts", initPid))
	if err != nil {
		t.Fatalf("readdir devpts: %v", err)
	}
	var out []string
	for _, e := range entries {
		if e.Name() == "ptmx" {
			continue
		}
		out = append(out, e.Name())
	}
	return out
}

// waitForDevptsSlaves polls until the container's /dev/pts has at least n slaves.
func waitForDevptsSlaves(t *testing.T, initPid, n int) {
	t.Helper()
	for i := 0; i < 200; i++ { // up to ~10s
		if len(devptsSlaves(t, initPid)) >= n {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d pts slaves; have %v", n, devptsSlaves(t, initPid))
}

// assertDevptsSlaves asserts the container's /dev/pts contains exactly the
// given numeric slave entries (ptmx is ignored).
func assertDevptsSlaves(t *testing.T, initPid int, want []string) {
	t.Helper()
	got := map[string]bool{}
	for _, s := range devptsSlaves(t, initPid) {
		got[s] = true
	}
	if len(got) != len(want) {
		t.Errorf("devpts slaves: got %v, want %v", keys(got), want)
		return
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("devpts missing slave %q; got %v, want %v", w, keys(got), want)
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
