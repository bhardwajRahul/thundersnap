//go:build e2e

package e2e

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tailscale/thundersnap/vshdproto"
)

// TestVshdContainerPTYDevpts reproduces the user-reported bug: an SSH PTY
// session into a container gets a pty whose slave is NOT visible as
// /dev/pts/N inside the container ("ps" shows "?" for the tty), because vshd
// opened the pty in ITS OWN mount namespace rather than the container's
// (separate "newinstance" devpts).
//
// It drives the REAL production session path end-to-end: a host-mode vshd
// (vshd --unix --ts=...) listening on a plain Unix socket, exactly as the
// daemon's runContainerSession speaks to it. vshd splices the raw vshdproto
// TLV stream to an inner `ts session-serve` that nsenter+chroots into the
// container and opens the pty THERE (so the slave lands in the container's own
// devpts and is visible as /dev/pts/N).
//
// All observations are made from INSIDE the container (each session's own
// shell reports its tty and lists /dev/pts on demand), so the test does not
// depend on knowing vshd's container-init PID. The user's exact spec:
//   - after session 1 starts: it sees /dev/pts/0 and NOT /dev/pts/1;
//   - after session 2 starts: BOTH sessions see /dev/pts/0 and /dev/pts/1, and
//     the two sessions are on distinct ptys.
//
// This FAILS before the fix (vshd's pty.Start opened the slave in vshd's
// devpts; the container's /dev/pts stays empty and `tty` reports "not a tty").
func TestVshdContainerPTYDevpts(t *testing.T) {
	env := newTestEnv(t)
	absFramePath := setupFrameNoNs(t, env, "vshddevpts")

	// busybox supplies a static /bin/sh with `tty`, `ls`, `read`.
	installBusyboxShell(t, absFramePath)

	// Per-session, per-phase fifos let the test step each session's shell
	// through: print tty -> (gate g1) -> list /dev/pts -> (gate g2) -> exit.
	for _, name := range []string{"s1g1", "s1g2", "s2g1", "s2g2"} {
		fifo := filepath.Join(absFramePath, "tmp", name)
		os.Remove(fifo)
		if err := syscall.Mkfifo(fifo, 0666); err != nil {
			t.Fatalf("mkfifo %s: %v", name, err)
		}
	}

	sock := startHostVshd(t, env)

	// framePath header is the rootfs path relative to "/" (vshd reconstructs
	// rootPrefix = "/"+framePath), matching the daemon's writeVshdRequest.
	framePathHdr := strings.TrimPrefix(absFramePath, "/")

	// startSession opens a PTY session over vshd. The shell prints its tty
	// (terminated by TTYDONE), blocks on gate g1, lists /dev/pts (terminated by
	// PTSDONE), then blocks on gate g2 before exiting. Markers are split with
	// '' so the pty's echo of the command never contains the marker we wait on.
	startSession := func(g1, g2 string) *vshdPTYSession {
		ws := vshdproto.Winsize{Rows: 24, Cols: 80}
		script := "tty; echo TTY''DONE; " +
			"read a < /tmp/" + g1 + "; " +
			"ls /dev/pts | tr '\\n' ' '; echo PTS''DONE; " +
			"read b < /tmp/" + g2
		s, err := startHostVshdPTY(sock, framePathHdr, "root", ws, "/bin/sh", "-c", script)
		if err != nil {
			t.Fatalf("start vshd pty session: %v", err)
		}
		return s
	}
	release := func(g string) {
		f, err := os.OpenFile(filepath.Join(absFramePath, "tmp", g), os.O_WRONLY, 0)
		if err != nil {
			t.Fatalf("open fifo %s: %v", g, err)
		}
		f.WriteString("go\n")
		f.Close()
	}

	// --- Session 1 ---
	s1 := startSession("s1g1", "s1g2")
	defer s1.close()
	out1, err := s1.readUntil("TTYDONE", 15*time.Second)
	if err != nil {
		t.Fatalf("session 1 tty: %v", err)
	}
	tty1 := firstDevLine(out1)
	if !strings.HasPrefix(tty1, "/dev/pts/") {
		t.Fatalf("session 1 did not get a container pts: tty=%q (full: %q)", tty1, out1)
	}

	// Session 1 alone: it must see exactly /dev/pts/0, not /dev/pts/1.
	release("s1g1")
	ptsA, err := s1.readUntil("PTSDONE", 15*time.Second)
	if err != nil {
		t.Fatalf("session 1 first pts listing: %v", err)
	}
	slavesA := parsePtsListing(ptsA)
	if len(slavesA) != 1 || slavesA[0] != "0" {
		t.Fatalf("after session 1: expected exactly [0], got %v (raw %q)", slavesA, ptsA)
	}

	// --- Session 2 ---
	s2 := startSession("s2g1", "s2g2")
	defer s2.close()
	out2, err := s2.readUntil("TTYDONE", 15*time.Second)
	if err != nil {
		t.Fatalf("session 2 tty: %v", err)
	}
	tty2 := firstDevLine(out2)
	if !strings.HasPrefix(tty2, "/dev/pts/") {
		t.Fatalf("session 2 did not get a container pts: tty=%q (full: %q)", tty2, out2)
	}
	if tty1 == tty2 {
		t.Errorf("two concurrent sessions reported the SAME tty %q", tty1)
	}

	// With both sessions alive, each must see BOTH /dev/pts/0 and /dev/pts/1.
	release("s2g1")
	ptsFrom2, err := s2.readUntil("PTSDONE", 15*time.Second)
	if err != nil {
		t.Fatalf("session 2 pts listing: %v", err)
	}
	if got := parsePtsListing(ptsFrom2); !equalStrs(got, []string{"0", "1"}) {
		t.Errorf("session 2 view: expected [0 1], got %v (raw %q)", got, ptsFrom2)
	}

	// Release both sessions' exit gates so the shells terminate cleanly.
	release("s1g2")
	release("s2g2")
}

// setupFrameNoNs creates a frame from the base snapshot and copies ts into it,
// WITHOUT starting a container namespace (vshd creates the single shared
// namespace itself). Returns the absolute frame path.
func setupFrameNoNs(t *testing.T, env *testEnv, name string) string {
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
	return absFramePath
}

// startHostVshd launches a host-mode vshd on a plain Unix socket and returns
// the socket path. It listens with --unix and uses --ts pointing at the outer
// ts binary used for nsenter, mirroring the daemon's hostVshd.ensure().
func startHostVshd(t *testing.T, env *testEnv) string {
	t.Helper()
	vshdBinary := env.requireBinary("vshd")
	sock := filepath.Join(env.root, "host-vshd.sock")
	os.Remove(sock)

	cmd := exec.Command(vshdBinary,
		"--unix="+sock,
		"--ts="+env.tsBinary,
	)
	cmd.Stdout = &testLogWriter{t: t, prefix: "vshd"}
	cmd.Stderr = &testLogWriter{t: t, prefix: "vshd"}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start host vshd: %v", err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		os.Remove(sock)
	})

	// Wait for the socket to appear (vshd creates it on listen).
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("host vshd did not create socket %s within 5s", sock)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return sock
}

// startHostVshdPTY dials the host vshd Unix socket (a plain socket, no
// CONNECT/OK handshake) and starts a PTY session, mirroring startVshdPTY but
// over unix rather than vsock.
func startHostVshdPTY(sock, framePath, user string, ws vshdproto.Winsize, args ...string) (*vshdPTYSession, error) {
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, err
	}
	if err := writeVshdHeader(conn, framePath, user, true, args); err != nil {
		conn.Close()
		return nil, err
	}
	// The leading FrameWinsize signals PTY mode and sizes the pty.
	if err := vshdproto.WriteFrame(conn, vshdproto.FrameWinsize, vshdproto.EncodeWinsize(ws)); err != nil {
		conn.Close()
		return nil, err
	}
	return &vshdPTYSession{conn: conn}, nil
}

// firstDevLine returns the first line of out that names a device (e.g. the tty
// path) or the literal "not a tty".
func firstDevLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "/dev/") || line == "not a tty" {
			return line
		}
	}
	return strings.TrimSpace(out)
}

// parsePtsListing extracts the numeric /dev/pts slave entries from the `ls
// /dev/pts` line emitted just before the PTSDONE marker. ptmx is ignored.
func parsePtsListing(out string) []string {
	// The listing line is the one immediately preceding "PTSDONE".
	lines := strings.Split(out, "\n")
	var listing string
	for i, line := range lines {
		if strings.Contains(line, "PTSDONE") {
			// The numbers may be on the same line (echo with no trailing \n
			// from tr) or the preceding line; check both.
			listing = strings.TrimSpace(strings.Replace(line, "PTSDONE", "", 1))
			if listing == "" && i > 0 {
				listing = strings.TrimSpace(lines[i-1])
			}
			break
		}
	}
	var out2 []string
	for _, f := range strings.Fields(listing) {
		if f == "ptmx" || f == "" {
			continue
		}
		out2 = append(out2, f)
	}
	sort.Strings(out2)
	return out2
}

func equalStrs(a, b []string) bool {
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

// testLogWriter forwards a subprocess's output to the test log.
type testLogWriter struct {
	t      *testing.T
	prefix string
}

func (w *testLogWriter) Write(p []byte) (int, error) {
	w.t.Logf("[%s] %s", w.prefix, strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
