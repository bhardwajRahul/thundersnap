// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	ssh "github.com/tailscale/gliderssh"
	"github.com/tailscale/thundersnap/frameid"
	"github.com/tailscale/thundersnap/sftpfs"
	"github.com/tailscale/thundersnap/thundersnap"
	"github.com/tailscale/thundersnap/tsm"
	"github.com/tailscale/thundersnap/vshdproto"
)

// resolveFrameRootFS maps an SSH frame name to a concrete frame for the given
// tailscale user via that user's ref store, returning the frame's on-disk root
// (<fs-dir>/<user>/<uuid>) and its UUID. The frame name is resolved as a ref:
// the reserved "default" for a bare/empty login, a named ref otherwise, or a
// fresh unattached frame for an unbound default. An unknown name is an error.
// See resolveFrameForUser for the full resolution rules.
func resolveFrameRootFS(tailscaleUser, frameName string) (rootFS string, uuid frameid.ID, err error) {
	uuid, framePath, _, err := resolveFrameForUser(tailscaleUser, frameName)
	if err != nil {
		return "", frameid.Nil, err
	}
	return framePath, uuid, nil
}

// runContainerSession handles a container-based SSH session on the host.
// targetUser specifies the Unix user to run as. If empty, auto-detect from
// [ubuntu, user] based on which /home/<user> exists, or fall back to root.
//
// The host session goes through the same host vshd shim as the VM path: the
// daemon dials the host vshd's Unix socket and sends the frame's rootfs as the
// VMX request header. vshd anchors the shared PID/mount/UTS namespaces for that
// rootfs (containerns.Manager) and joins them via the in-binary `ts nsenter`,
// so the enter-container-ns code is byte-identical to the in-VM vshd.
func runContainerSession(s ssh.Session, tailscaleUser, frameName, targetUser string, logErr func(string, ...any)) error {
	// Resolve the frame name to its canonical fs/<user>/<uuid> path via the
	// user's ref store, then prepare its rootfs.
	rootFS, _, err := resolveFrameRootFS(tailscaleUser, frameName)
	if err != nil {
		return err
	}
	if err := prepareContainerRootFS(rootFS, ""); err != nil {
		return err
	}

	// Get or create shared control socket server for this container.
	_, err = controlServers.getOrCreateControlServer(rootFS)
	if err != nil {
		return fmt.Errorf("start control socket: %w", err)
	}
	defer controlServers.releaseControlServer(rootFS)

	// The frame rootfs is an absolute host path; vshd reconstructs it from the
	// VMX header as filepath.Clean("/" + framePath), so strip the leading slash.
	absRootFS, err := filepath.Abs(rootFS)
	if err != nil {
		return fmt.Errorf("get absolute path for rootFS: %w", err)
	}
	framePathHdr := strings.TrimPrefix(absRootFS, "/")

	// Ensure the host vshd process is running and dial its Unix socket. There is
	// no VM lifecycle here, so the done/panicked channels never fire.
	sockPath, err := hostVshd.ensure()
	if err != nil {
		return fmt.Errorf("start host vshd: %w", err)
	}
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return fmt.Errorf("dial host vshd: %w", err)
	}
	defer conn.Close()

	ptyReq, winCh, isPty := s.Pty()
	writeVshdRequest(conn, framePathHdr, targetUser, isPty, sessionCommand(s))

	return proxyVshdSession(s, conn, isPty, ptyReq, winCh, nil, nil)
}

// runSFTPSession handles an SFTP subsystem request using the built-in Go SFTP server.
// All paths are served relative to the container's rootFS with the user's home as
// the starting directory.
func runSFTPSession(s ssh.Session, rootFS, targetUser string) error {
	// Check if rootFS exists
	if _, err := os.Stat(rootFS); err != nil {
		return fmt.Errorf("container filesystem not found: %s", rootFS)
	}

	absRootFS, err := filepath.Abs(rootFS)
	if err != nil {
		return fmt.Errorf("get absolute path for rootFS: %w", err)
	}

	// Determine which Unix user to run as and get their home directory
	runAsUser := selectTargetUser(rootFS, targetUser)
	userInfo := tsm.LookupUser(rootFS, runAsUser)

	// Default to /home/user if we can't look up the user
	homeDir := "/home/user"
	// Files created over SFTP are created by the thundersnapd process (root),
	// so without an explicit chown they would all be owned by root. Chown new
	// files/dirs to the target user so that scp/sftp uploads land with the
	// correct ownership rather than as root.
	uid, gid := -1, -1
	if userInfo != nil {
		if userInfo.Home != "" {
			homeDir = userInfo.Home
		}
		uid = int(userInfo.UID)
		gid = int(userInfo.GID)
	}

	// Create the SFTP handler that maps paths through the container rootFS
	handler := sftpfs.NewHandler(absRootFS, homeDir, uid, gid)

	server := sftp.NewRequestServer(s, handler.Handlers(),
		sftp.WithStartDirectory(handler.HomeDir()))

	if err := server.Serve(); err != nil {
		if err != io.EOF {
			return fmt.Errorf("sftp server error: %w", err)
		}
	}
	s.Exit(0)
	return nil
}

// connectToVshd connects to vshd in a VM via the vsock socket.
// It performs the cloud-hypervisor vsock CONNECT handshake and retries
// until vshd is ready (up to 10 seconds).
// If the panicked channel is closed, it aborts immediately.
func connectToVshd(vsockPath string, panicked <-chan struct{}) (net.Conn, error) {
	var lastErr error

	// Retry the full connection + handshake for up to 10 seconds while vshd starts up
	for i := 0; i < 100; i++ {
		// Check if VM panicked before each attempt
		select {
		case <-panicked:
			return nil, fmt.Errorf("VM kernel panic detected")
		default:
		}

		conn, err := tryConnectToVshd(vsockPath)
		if err == nil {
			return conn, nil
		}
		lastErr = err

		// Wait 100ms, but abort immediately if VM panics
		select {
		case <-panicked:
			return nil, fmt.Errorf("VM kernel panic detected")
		case <-time.After(100 * time.Millisecond):
		}
	}

	return nil, lastErr
}

// tryConnectToVshd attempts a single connection to vshd.
func tryConnectToVshd(vsockPath string) (net.Conn, error) {
	conn, err := net.Dial("unix", vsockPath)
	if err != nil {
		return nil, fmt.Errorf("dial vsock: %w", err)
	}

	// Cloud Hypervisor vsock protocol: send "CONNECT <port>\n"
	_, err = fmt.Fprintf(conn, "CONNECT %d\n", thundersnap.VshPort)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("send CONNECT: %w", err)
	}

	// Read response - should be "OK <port>\n"
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read handshake response: %w", err)
	}
	response := strings.TrimSpace(string(buf[:n]))
	if !strings.HasPrefix(response, "OK") {
		conn.Close()
		return nil, fmt.Errorf("vsock handshake failed: %s", response)
	}

	return conn, nil
}

// generateRandomID generates a random hex string for snapshot naming.
func generateRandomID() (string, error) {
	b := make([]byte, 16) // 32 hex characters
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// prepareVMXRootFS creates a minimal rootfs for the VMX outer VM.
// This is a "mini frame" containing only the statically-linked binaries
// needed to run vshd and spawn containers (ts, vshd).
func prepareVMXRootFS(vmxRootFS string) error {
	// Check if already exists
	if _, err := os.Stat(vmxRootFS); err == nil {
		// Exists - ensure binaries are up to date
		if err := copyTsBinary(vmxRootFS); err != nil {
			return fmt.Errorf("update ts binary: %w", err)
		}
		if err := copyVshdBinary(vmxRootFS); err != nil {
			return fmt.Errorf("update vshd binary: %w", err)
		}
		return nil
	}

	log.Printf("Creating VMX rootfs at %s", vmxRootFS)

	// Create minimal directory structure
	dirs := []string{
		"bin", "sbin", "dev", "proc", "sys", "tmp", "etc", "run", "root",
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(vmxRootFS, dir), 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}

	// Set /tmp permissions (sticky bit)
	if err := os.Chmod(filepath.Join(vmxRootFS, "tmp"), 01777); err != nil {
		return fmt.Errorf("chmod tmp: %w", err)
	}

	// Create minimal /etc/passwd with root user
	passwd := "root:x:0:0:root:/root:/bin/sh\n"
	if err := os.WriteFile(filepath.Join(vmxRootFS, "etc/passwd"), []byte(passwd), 0644); err != nil {
		return fmt.Errorf("write passwd: %w", err)
	}

	// Create minimal /etc/group with root group
	group := "root:x:0:\n"
	if err := os.WriteFile(filepath.Join(vmxRootFS, "etc/group"), []byte(group), 0644); err != nil {
		return fmt.Errorf("write group: %w", err)
	}

	// Copy statically-linked binaries
	if err := copyTsBinary(vmxRootFS); err != nil {
		return fmt.Errorf("copy ts binary: %w", err)
	}
	if err := copyVshdBinary(vmxRootFS); err != nil {
		return fmt.Errorf("copy vshd binary: %w", err)
	}

	// Symlink /bin/sh -> ts (relative symlink to ts in same directory). The ts
	// binary *is* the shell: when invoked with argv0 "sh" it enters shell mode,
	// so the VM's init/shell and any "/bin/sh -c ..." spawned in the VMX rootfs
	// run ts itself rather than needing a separate shell binary.
	shPath := filepath.Join(vmxRootFS, "bin/sh")
	if err := os.Symlink("ts", shPath); err != nil && !os.IsExist(err) {
		return fmt.Errorf("symlink sh: %w", err)
	}

	log.Printf("VMX rootfs created at %s", vmxRootFS)
	return nil
}

// runVMXSession handles a VMX session: a container running inside a shared VM.
// Multiple frames can share the same outer VM (keyed by tailscaleUser/isolationName).
func runVMXSession(s ssh.Session, tailscaleUser, isolationName, frameName, targetUser string, logErr func(string, ...any)) error {
	safeTailscaleUser := sanitizeForPath(tailscaleUser)
	safeIsolationName := sanitizeForPath(isolationName)

	// The user's fs directory (becomes virtiofs root)
	userFsDir := filepath.Join(*flagFsDir, safeTailscaleUser)

	// Resolve the frame name to its canonical fs/<user>/<uuid> path via the
	// user's ref store, then prepare the frame's rootfs (same as container mode).
	frameRootFS, uuid, err := resolveFrameRootFS(tailscaleUser, frameName)
	if err != nil {
		return err
	}
	if err := prepareContainerRootFS(frameRootFS, ""); err != nil {
		return fmt.Errorf("prepare frame rootfs: %w", err)
	}

	// Prepare the outer VM's minimal rootfs
	initPrefix := ".vmx-" + safeIsolationName
	vmxRootFS := filepath.Join(userFsDir, initPrefix)
	if err := prepareVMXRootFS(vmxRootFS); err != nil {
		return fmt.Errorf("prepare VMX rootfs: %w", err)
	}

	// Create control handler for this frame
	controlMux := makeVMXControlHandler(frameRootFS)

	// Get or create the shared VMX session
	ms, err := vmxSessions.getOrCreateVMX(safeTailscaleUser, safeIsolationName, userFsDir, initPrefix, *flagVmDir, controlMux)
	if err != nil {
		return fmt.Errorf("start VMX: %w", err)
	}
	defer vmxSessions.releaseVMX(safeTailscaleUser, safeIsolationName)

	// Connect to vshd in the VM
	conn, err := connectToVshd(ms.vsockPath, ms.panicked)
	if err != nil {
		return fmt.Errorf("connect to vshd: %w", err)
	}
	defer conn.Close()

	// Send VMX protocol header: VMX\0framePath\0targetUser\0pty\0argCount\0args...
	// framePath is the frame's UUID, relative to the virtiofs root (userFsDir,
	// i.e. fs/<user>): the frame lives at fs/<user>/<uuid> on the host.
	ptyReq, winCh, isPty := s.Pty()
	writeVshdRequest(conn, uuid.String(), targetUser, isPty, sessionCommand(s))

	// Proxy SSH I/O over the vshdproto TLV stream.
	return proxyVshdSession(s, conn, isPty, ptyReq, winCh, ms.done, ms.panicked)
}

// runVMXOuterShell handles a direct shell into the outer VMX VM (no container).
// This is useful for debugging the VMX environment.
func runVMXOuterShell(s ssh.Session, tailscaleUser, isolationName, targetUser string, logErr func(string, ...any)) error {
	safeTailscaleUser := sanitizeForPath(tailscaleUser)
	safeIsolationName := sanitizeForPath(isolationName)

	// The user's fs directory (becomes virtiofs root)
	userFsDir := filepath.Join(*flagFsDir, safeTailscaleUser)

	// Prepare the outer VM's minimal rootfs
	initPrefix := ".vmx-" + safeIsolationName
	vmxRootFS := filepath.Join(userFsDir, initPrefix)
	if err := prepareVMXRootFS(vmxRootFS); err != nil {
		return fmt.Errorf("prepare VMX rootfs: %w", err)
	}

	// Create control handler (for outer shell, use the vmx rootfs)
	controlMux := makeVMXControlHandler(vmxRootFS)

	// Get or create the shared VMX session
	ms, err := vmxSessions.getOrCreateVMX(safeTailscaleUser, safeIsolationName, userFsDir, initPrefix, *flagVmDir, controlMux)
	if err != nil {
		return fmt.Errorf("start VMX: %w", err)
	}
	defer vmxSessions.releaseVMX(safeTailscaleUser, safeIsolationName)

	// Connect to vshd in the VM
	conn, err := connectToVshd(ms.vsockPath, ms.panicked)
	if err != nil {
		return fmt.Errorf("connect to vshd: %w", err)
	}
	defer conn.Close()

	// Send original vshd protocol header (no VMX prefix) - shell directly in VM
	ptyReq, winCh, isPty := s.Pty()
	writeVshdRequest(conn, "", targetUser, isPty, sessionCommand(s))

	// Proxy SSH I/O over the vshdproto TLV stream.
	return proxyVshdSession(s, conn, isPty, ptyReq, winCh, ms.done, ms.panicked)
}

// makeVMXControlHandler creates the HTTP handler for VMX control requests.
func makeVMXControlHandler(frameRootFS string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", handlePing)
	mux.HandleFunc("/snap", makeSnapHandler(frameRootFS))
	return mux
}

// sessionCommand maps an SSH session's command to the argv sent to vshd. SSH
// exec semantics run the supplied command through the user's login shell
// (sh -c "<command>"), so a non-empty command is wrapped as ["sh","-c",raw]
// rather than naively word-split: this preserves shell metacharacters such as
// redirections (>), pipes (|) and operators (&&) instead of passing them as
// literal argv elements. An empty command (interactive login) yields nil, which
// tells vshd to start a login shell.
func sessionCommand(s ssh.Session) []string {
	raw := s.RawCommand()
	if raw == "" {
		return nil
	}
	return []string{"sh", "-c", raw}
}

// writeVshdRequest writes a vshd session request header to conn using the
// null-delimited wire protocol. When framePath is non-empty it emits the VMX
// form ("VMX\0framePath\0user\0pty\0argc\0args...") which spawns a container at
// framePath inside the VM; when framePath is empty it emits the plain form
// ("user\0pty\0argc\0args...") for a shell directly in the VM. pty signals
// whether the client requested a terminal. After this header the connection
// carries vshdproto TLV frames in both directions.
func writeVshdRequest(conn net.Conn, framePath, targetUser string, pty bool, cmdArgs []string) {
	if framePath != "" {
		fmt.Fprintf(conn, "VMX\x00%s\x00", framePath)
	}
	ptyFlag := "0"
	if pty {
		ptyFlag = "1"
	}
	fmt.Fprintf(conn, "%s\x00%s\x00%d\x00", targetUser, ptyFlag, len(cmdArgs))
	for _, arg := range cmdArgs {
		fmt.Fprintf(conn, "%s\x00", arg)
	}
}

// proxyVshdSessionGeneric proxies a session to/from a vshd connection using
// vshdproto TLV framing. This is the core proxy logic shared by SSH sessions
// and control socket /enter sessions.
//
// Parameters:
//   - clientIn: reader for client input (stdin)
//   - clientOut: writer for session stdout
//   - clientErr: writer for session stderr
//   - vshdConn: connection to vshd
//   - isPty: whether this is a PTY session
//   - initialWinsize: initial window size for PTY sessions (ignored if !isPty)
//   - winCh: channel of window size changes (nil for non-PTY or no resize support)
//   - clientClosed: channel that fires when the client disconnects (may be nil)
//   - done: channel that fires when VM exits (may be nil, for host containers)
//   - panicked: channel that fires when VM panics (may be nil, for host containers)
//
// Returns the exit code from the remote process.
func proxyVshdSessionGeneric(
	clientIn io.Reader,
	clientOut, clientErr io.Writer,
	vshdConn net.Conn,
	isPty bool,
	initialWinsize vshdproto.Winsize,
	winCh <-chan vshdproto.Winsize,
	clientClosed <-chan struct{},
	done <-chan struct{},
	panicked <-chan struct{},
) int {
	// Stdin and winsize frames are written from independent goroutines; a
	// mutex keeps each frame's header+payload contiguous on the wire.
	var writeMu sync.Mutex
	writeFrame := func(typ uint8, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return vshdproto.WriteFrame(vshdConn, typ, payload)
	}

	// For PTY sessions, send the initial window size first so the guest pty is
	// created/resized to the client's terminal rather than the default 80x24.
	if isPty {
		writeFrame(vshdproto.FrameWinsize, vshdproto.EncodeWinsize(initialWinsize))
	}

	// guestDone fires when the guest->host direction ends (FrameExit received or
	// the guest closed the connection). This is the authoritative signal that the
	// session is over: the remote command has exited and all its output has been
	// delivered. The host->guest stdin copy ending (e.g. immediate EOF for a
	// non-interactive exec with no stdin) must NOT tear the session down, or we
	// race the command's output frames.
	guestDone := make(chan struct{})

	// Host -> guest: frame client stdin as FrameStdin; relay winsize changes.
	// This goroutine exits when stdin EOFs; it does not signal session end.
	go func() {
		if isPty && winCh != nil {
			go func() {
				for win := range winCh {
					writeFrame(vshdproto.FrameWinsize, vshdproto.EncodeWinsize(win))
				}
			}()
		}
		buf := make([]byte, 32*1024)
		for {
			n, rerr := clientIn.Read(buf)
			if n > 0 {
				if werr := writeFrame(vshdproto.FrameStdin, buf[:n]); werr != nil {
					break
				}
			}
			if rerr != nil {
				break
			}
		}
	}()

	// Guest -> host: decode TLV frames; route stdout/stderr/exit. Closing
	// guestDone signals the session is complete.
	exitCode := 0
	go func() {
		defer close(guestDone)
		for {
			typ, payload, err := vshdproto.ReadFrame(vshdConn)
			if err != nil {
				break
			}
			switch typ {
			case vshdproto.FrameStdout:
				clientOut.Write(payload)
			case vshdproto.FrameStderr:
				clientErr.Write(payload)
			case vshdproto.FrameExit:
				if code, derr := vshdproto.DecodeExit(payload); derr == nil {
					exitCode = int(code)
				}
			}
		}
	}()

	// Wait for session end
	select {
	case <-guestDone:
		log.Printf("proxy: vshd connection closed")
	case <-done:
		log.Printf("proxy: VM exited")
	case <-panicked:
		log.Printf("proxy: VM kernel panic")
	case <-clientClosed:
		log.Printf("proxy: client disconnected")
	}

	vshdConn.Close()

	// Closing conn unblocks the guest->host reader if it is still running (e.g.
	// when we exited the select via done/panicked/client-close rather than
	// guestDone). Wait briefly so any in-flight output is flushed, bounded so a
	// wedged reader cannot block teardown.
	select {
	case <-guestDone:
	case <-time.After(100 * time.Millisecond):
	}

	return exitCode
}

// proxyVshdSession proxies an SSH session to/from a vshd connection using the
// vshdproto TLV framing. Host -> guest: SSH stdin is framed as FrameStdin and,
// for PTY sessions, the initial window size plus every winCh change is sent as
// FrameWinsize. Guest -> host: FrameStdout/FrameStderr are written to the SSH
// stdout/stderr channels and FrameExit carries the real exit status.
//
// It is shared by all vshd-backed sessions (VMX container, VMX outer shell, and
// any future host-vshd shim).
func proxyVshdSession(s ssh.Session, conn net.Conn, isPty bool, ptyReq ssh.Pty, winCh <-chan ssh.Window, done <-chan struct{}, panicked <-chan struct{}) error {
	// The session's pty (line discipline, OPOST/ONLCR cooking, raw mode) lives
	// inside the container/VM. gliderlabs/ssh's default "minimal PTY emulation"
	// would re-cook our output host-side, rewriting \n -> \r\n on every
	// FrameStdout write and ignoring raw mode. Disable it so this relay is a
	// transparent byte pipe and the guest pty owns line discipline (classic
	// ssh behaviour). Must precede any s.Write below.
	s.DisablePTYEmulation()

	// Convert SSH window size to vshdproto.Winsize
	initialWinsize := vshdproto.Winsize{
		Rows: uint16(ptyReq.Window.Height),
		Cols: uint16(ptyReq.Window.Width),
	}

	// Adapt ssh.Window channel to vshdproto.Winsize channel
	var vshdWinCh <-chan vshdproto.Winsize
	if isPty && winCh != nil {
		ch := make(chan vshdproto.Winsize)
		vshdWinCh = ch
		go func() {
			defer close(ch)
			for win := range winCh {
				ch <- vshdproto.Winsize{
					Rows: uint16(win.Height),
					Cols: uint16(win.Width),
				}
			}
		}()
	}

	// Use session context done channel as client disconnect signal
	clientClosed := s.Context().Done()

	exitCode := proxyVshdSessionGeneric(
		s,          // clientIn (ssh.Session implements io.Reader)
		s,          // clientOut (ssh.Session implements io.Writer)
		s.Stderr(), // clientErr
		conn,       // vshdConn
		isPty,      // isPty
		initialWinsize,
		vshdWinCh,
		clientClosed,
		done,
		panicked,
	)

	s.Exit(exitCode)
	return nil
}
