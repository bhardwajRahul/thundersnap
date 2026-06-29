// vshd is the shell daemon used by thundersnap to run sessions inside a
// container, both on the host (over a Unix socket, --unix) and inside a VM
// (over vsock). It serves a simple null-delimited request protocol over each
// connection, spawning either an interactive PTY shell or a one-shot command.
// Two protocol variants are supported (see handleConnection): the original
// "run on this VM/host" form and the extended "VMX" form that runs inside a
// container rootfs.
//
// For container sessions vshd uses the shared-init/nsenter model
// (containerns.Manager): one "ts container-init" process anchors the
// PID/mount/UTS namespaces per container rootfs, and each session joins those
// namespaces via the CGO-free in-binary `ts nsenter` before chrooting and
// dropping caps with `ts drop-caps-and-run`. This is byte-identical on the host
// and inside a VM, so sessions sharing a container see each other's PIDs.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/creack/pty"
	"github.com/mdlayher/vsock"
	"github.com/tailscale/thundersnap/cgroup"
	"github.com/tailscale/thundersnap/containerns"
	"github.com/tailscale/thundersnap/vshdproto"
)

// containerNs anchors the shared PID/mount/UTS namespaces for container
// sessions, keyed by container rootfs path. Sessions join via `ts nsenter`.
var containerNs = containerns.New()

// cgroupMgr applies per-session resource limits (memory/pids/cpu + OOM bias) to
// each container session's child process. It is nil unless --cgroup-parent is
// passed (host mode); in a VM, resource limits come from the VM itself so vshd
// leaves cgroups alone.
var cgroupMgr *cgroup.Manager

// selectUser determines which Unix user to run as, auto-detecting when the
// caller did not request one. rootPrefix is "" for the host VM filesystem or a
// container rootfs path for VMX mode; all lookups are resolved beneath it.
// Detection order: explicit targetUser -> "ubuntu" (if /home/ubuntu exists) ->
// "user" (if its /etc/passwd home exists) -> "root".
func selectUser(rootPrefix, targetUser string) string {
	if targetUser != "" {
		return targetUser
	}

	// First check for ubuntu user (legacy behavior).
	if info, err := os.Stat(filepath.Join(rootPrefix, "home/ubuntu")); err == nil && info.IsDir() {
		return "ubuntu"
	}

	// Look up "user" in /etc/passwd and confirm their home exists.
	if userHome := lookupUserHome(rootPrefix, "user"); userHome != "" {
		if info, err := os.Stat(filepath.Join(rootPrefix, userHome)); err == nil && info.IsDir() {
			return "user"
		}
	}

	return "root"
}

// lookupUserHome reads <rootPrefix>/etc/passwd and returns the home directory
// for username. rootPrefix is "" for the host filesystem. Returns "" if the
// file doesn't exist or the user is not found.
func lookupUserHome(rootPrefix, username string) string {
	data, err := os.ReadFile(filepath.Join(rootPrefix, "etc/passwd"))
	if err != nil {
		return ""
	}
	return parsePasswdHome(string(data), username)
}

// parsePasswdHome scans /etc/passwd-formatted content and returns the home
// directory (field 6) for the first line whose first field equals username.
// Blank and comment (#) lines are skipped; lines with fewer than 6 colon-
// separated fields are ignored. Returns "" when not found.
func parsePasswdHome(passwd, username string) string {
	scanner := bufio.NewScanner(strings.NewReader(passwd))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) >= 6 && fields[0] == username {
			return fields[5] // home directory field
		}
	}
	return ""
}

// quoteArgsForSh single-quotes each argument for safe interpolation into a
// `su - <user> -c '<cmd>'` string, escaping embedded single quotes via the
// standard '\” idiom, and joins them with spaces.
func quoteArgsForSh(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
	}
	return strings.Join(quoted, " ")
}

// readField reads one null-terminated field from the protocol stream and
// returns it with the trailing '\x00' stripped. ReadString only returns a nil
// error when the delimiter was found, so slicing off the last byte is safe.
func readField(reader *bufio.Reader) (string, error) {
	s, err := reader.ReadString('\x00')
	if err != nil {
		return "", err
	}
	return s[:len(s)-1], nil
}

const vsockPort = 5222

var connectionID uint64

// tsBinaryPath is the path to the ts binary, determined at startup.
// This is set based on where vshd is located (sibling in bin/ directory).
var tsBinaryPath = "/bin/ts"

// initTsBinaryPath determines the path to the ts binary based on vshd's location.
// If vshd is at /foo/sbin/vshd, then ts is expected at /foo/bin/ts.
// This supports VMX mode where vshd runs at /.vmx-<isolation>/sbin/vshd.
func initTsBinaryPath() {
	exe, err := os.Executable()
	if err != nil {
		log.Printf("warning: could not determine executable path, using default ts path: %v", err)
		return
	}
	// Resolve symlinks to get the real path
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		log.Printf("warning: could not resolve symlinks for executable path, using default ts path: %v", err)
		return
	}
	// vshd is at <prefix>/sbin/vshd, ts is at <prefix>/bin/ts
	dir := filepath.Dir(exe)    // <prefix>/sbin
	prefix := filepath.Dir(dir) // <prefix>
	tsPath := filepath.Join(prefix, "bin", "ts")
	if _, err := os.Stat(tsPath); err == nil {
		tsBinaryPath = tsPath
		log.Printf("using ts binary at %s", tsBinaryPath)
	} else {
		log.Printf("ts binary not found at %s, using default /bin/ts", tsPath)
	}
}

// handleConnection serves one vshd session over conn. conn is an
// io.ReadWriteCloser so the same handler serves both a VM vsock connection and
// (in host mode) a Unix-socket connection.
//
// Request header (null-delimited), read before any TLV framing:
//
//	original: targetUser\0pty\0argCount\0arg1\0...argN\0
//	VMX:      VMX\0framePath\0targetUser\0pty\0argCount\0arg1\0...argN\0
//
// where pty is "1" for a PTY session and "0" otherwise. After the header the
// connection carries vshdproto TLV frames in both directions.
func handleConnection(conn io.ReadWriteCloser) {
	id := atomic.AddUint64(&connectionID, 1)
	log.Printf("[conn %d] new connection", id)
	defer func() {
		conn.Close()
		log.Printf("[conn %d] connection closed", id)
	}()

	reader := bufio.NewReader(conn)

	firstField, err := readField(reader)
	if err != nil {
		log.Printf("[conn %d] failed to read first field: %v", id, err)
		return
	}

	// rootPrefix is "" for the host/VM filesystem, or the container rootfs for
	// the VMX protocol.
	rootPrefix := ""
	if firstField == "VMX" {
		framePath, err := readField(reader)
		if err != nil {
			log.Printf("[conn %d] VMX: failed to read frame path: %v", id, err)
			return
		}
		// The frame rootfs is at /<framePath> from the virtiofs root
		// (virtiofs is mounted as / in the VM).
		rootPrefix = filepath.Clean("/" + framePath)
		// The next field is the target user, read below.
		firstField, err = readField(reader)
		if err != nil {
			log.Printf("[conn %d] VMX: failed to read target user: %v", id, err)
			return
		}
	}

	targetUser := firstField
	wantPTY, err := readBool(reader)
	if err != nil {
		log.Printf("[conn %d] failed to read pty flag: %v", id, err)
		return
	}
	cmdArgs, err := readArgs(reader)
	if err != nil {
		log.Printf("[conn %d] failed to read args: %v", id, err)
		return
	}

	runAsUser := selectUser(rootPrefix, targetUser)
	log.Printf("[conn %d] running as user %q (requested: %q, rootPrefix: %q, pty: %v, args: %v)",
		id, runAsUser, targetUser, rootPrefix, wantPTY, cmdArgs)

	cmd, release, err := buildSessionCmd(rootPrefix, runAsUser, cmdArgs, wantPTY)
	if err != nil {
		log.Printf("[conn %d] failed to build session command: %v", id, err)
		vshdproto.WriteFrame(conn, vshdproto.FrameStderr, []byte(fmt.Sprintf("vshd: %v\n", err)))
		vshdproto.WriteFrame(conn, vshdproto.FrameExit, vshdproto.EncodeExit(1))
		return
	}
	if release != nil {
		defer release()
	}

	// For container sessions (rootPrefix != "") in host mode (cgroupMgr set),
	// apply per-session cgroup limits to the started child. The leaf name is
	// <parent>/<rootfs-base>/<conn-id> so sessions sharing a container land
	// under the same intermediate dir while each gets its own leaf. VM sessions
	// (cgroupMgr nil) and outer/non-container sessions skip this entirely.
	var postStart func(pid int)
	if cgroupMgr != nil && rootPrefix != "" {
		leaf := fmt.Sprintf("%s/%s/%d", cgroupMgr.ParentName(), filepath.Base(rootPrefix), id)
		postStart = func(pid int) {
			cgroupMgr.ConfigureContainer(pid, leaf)
		}
	}

	serveSession(id, conn, reader, cmd, wantPTY, postStart)
}

// buildSessionCmd constructs the *exec.Cmd for a session and, for container
// sessions, a release func that drops the caller's reference on the shared
// namespace (nil otherwise). rootPrefix is "" for a direct VM/host shell or a
// container rootfs path (the container rootfs for the VMX protocol, or a frame
// rootfs in host mode). When cmdArgs is empty an interactive login shell is
// started; otherwise the command is run (via `su - user -c` for non-root).
//
// For a container session the command joins the shared PID/mount/UTS namespaces
// anchored by `ts container-init` (containerNs.GetOrCreate) via the in-binary
// `ts nsenter`, then chroots and drops caps with `ts drop-caps-and-run
// --skip-mount-setup`. This is byte-identical to the daemon's host per-session
// form, so host and VM sessions sharing a container rootfs see each other's
// PIDs.
func buildSessionCmd(rootPrefix, runAsUser string, cmdArgs []string, wantPTY bool) (*exec.Cmd, func(), error) {
	// argv is what we ultimately exec (before any container wrapper).
	var argv []string
	switch {
	case len(cmdArgs) == 0 && runAsUser == "root":
		// Interactive root shell: run /bin/sh -l directly (avoids needing su).
		argv = []string{"/bin/sh", "-l"}
	case len(cmdArgs) == 0:
		// Interactive login shell for a non-root user.
		argv = []string{"su", "-", runAsUser}
	case runAsUser == "root":
		// Run the command directly as root.
		argv = cmdArgs
	default:
		// Run the command via a login shell for the target user.
		argv = []string{"su", "-", runAsUser, "-c", quoteArgsForSh(cmdArgs)}
	}

	if rootPrefix == "" {
		// Direct shell/command in this filesystem (outer VM or host, no
		// container).
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = sessionEnv(wantPTY)
		return cmd, nil, nil
	}

	// Container session (VMX or host): join the shared namespaces anchored by
	// container-init, then chroot + drop caps. GetOrCreate refcounts the init
	// per rootfs; release drops our reference when the session ends.
	initPid, err := containerNs.GetOrCreate(rootPrefix, "", "")
	if err != nil {
		return nil, nil, fmt.Errorf("create container namespace: %w", err)
	}
	release := func() { containerNs.Release(rootPrefix) }

	// The inner ts lives in the frame rootfs; nsenter is run by the outer ts
	// (tsBinaryPath) which is always present on the host/outer-VM filesystem.
	innerTs := filepath.Join(rootPrefix, "bin", "ts")
	dropCapsArgs := append([]string{
		"drop-caps-and-run",
		"--chroot=" + rootPrefix,
		"--skip-mount-setup",
		"--",
	}, argv...)
	nsenterArgs := append([]string{
		"nsenter",
		"-t", strconv.Itoa(initPid), "-p", "-m", "-u", "--",
		innerTs,
	}, dropCapsArgs...)

	cmd := exec.Command(tsBinaryPath, nsenterArgs...)
	cmd.Env = sessionEnv(wantPTY)
	return cmd, release, nil
}

// sessionEnv returns the environment for a session command, adding TERM for PTY
// sessions.
func sessionEnv(wantPTY bool) []string {
	env := os.Environ()
	if wantPTY {
		env = append(env, "TERM=xterm-256color")
	}
	return env
}

// readBool reads a null-terminated field expected to be "1" or "0".
func readBool(reader *bufio.Reader) (bool, error) {
	s, err := readField(reader)
	if err != nil {
		return false, err
	}
	return s == "1", nil
}

// readArgs reads a null-delimited "argCount\0arg1\0...argN\0" sequence shared by
// both protocol variants. A non-numeric or negative count is rejected up front
// so a malformed request fails fast instead of blocking on a never-arriving
// field.
func readArgs(reader *bufio.Reader) ([]string, error) {
	countStr, err := readField(reader)
	if err != nil {
		return nil, fmt.Errorf("read arg count: %w", err)
	}
	argCount, err := strconv.Atoi(countStr)
	if err != nil {
		return nil, fmt.Errorf("invalid arg count %q: %w", countStr, err)
	}
	if argCount < 0 {
		return nil, fmt.Errorf("negative arg count %d", argCount)
	}
	cmdArgs := make([]string, 0, argCount)
	for i := 0; i < argCount; i++ {
		arg, err := readField(reader)
		if err != nil {
			return nil, fmt.Errorf("read arg %d: %w", i, err)
		}
		cmdArgs = append(cmdArgs, arg)
	}
	return cmdArgs, nil
}

// serveSession runs cmd, proxying it to the client over conn using vshdproto TLV
// framing. For a PTY session (wantPTY) the command is started on a pty whose
// size tracks FrameWinsize frames from the client; for a non-PTY session stdin
// is fed from FrameStdin frames and stdout/stderr are framed separately. In both
// cases the child's exit code is sent as a FrameExit frame before the connection
// is closed.
// postStart, when non-nil, is invoked with the started child's PID immediately
// after the command starts (used to apply cgroup limits in host mode).
func serveSession(id uint64, conn io.Writer, reader io.Reader, cmd *exec.Cmd, wantPTY bool, postStart func(pid int)) {
	if wantPTY {
		servePTYSession(id, conn, reader, cmd, postStart)
	} else {
		servePipeSession(id, conn, reader, cmd, postStart)
	}
}

// servePTYSession starts cmd on a pty and bridges it to the TLV stream:
// FrameStdin -> pty, FrameWinsize -> pty.Setsize, pty output -> FrameStdout.
func servePTYSession(id uint64, conn io.Writer, reader io.Reader, cmd *exec.Cmd, postStart func(pid int)) {
	ptmx, err := pty.Start(cmd)
	if err != nil {
		log.Printf("[conn %d] failed to start pty: %v", id, err)
		vshdproto.WriteFrame(conn, vshdproto.FrameStderr, []byte(fmt.Sprintf("vshd: failed to start shell: %v\n", err)))
		vshdproto.WriteFrame(conn, vshdproto.FrameExit, vshdproto.EncodeExit(1))
		return
	}
	defer ptmx.Close()
	if postStart != nil {
		postStart(cmd.Process.Pid)
	}
	log.Printf("[conn %d] pty session started with PID %d", id, cmd.Process.Pid)

	// Client -> child: decode TLV frames, route stdin to the pty and winsize to
	// the pty size. Runs until the client closes (EOF) or sends a malformed frame.
	go func() {
		for {
			typ, payload, err := vshdproto.ReadFrame(reader)
			if err != nil {
				if err != io.EOF {
					log.Printf("[conn %d] read frame: %v", id, err)
				}
				return
			}
			switch typ {
			case vshdproto.FrameStdin:
				if _, werr := ptmx.Write(payload); werr != nil {
					return
				}
			case vshdproto.FrameWinsize:
				ws, derr := vshdproto.DecodeWinsize(payload)
				if derr != nil {
					log.Printf("[conn %d] bad winsize: %v", id, derr)
					continue
				}
				pty.Setsize(ptmx, &pty.Winsize{Rows: ws.Rows, Cols: ws.Cols, X: ws.X, Y: ws.Y})
			}
		}
	}()

	// Child -> client: frame pty output as FrameStdout.
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				if werr := vshdproto.WriteFrame(conn, vshdproto.FrameStdout, buf[:n]); werr != nil {
					break
				}
			}
			if rerr != nil {
				break
			}
		}
		close(done)
	}()

	<-done
	log.Printf("[conn %d] signaling pty session to exit", id)
	cmd.Process.Signal(syscall.SIGHUP)
	code := waitExitCode(cmd)
	vshdproto.WriteFrame(conn, vshdproto.FrameExit, vshdproto.EncodeExit(code))
	log.Printf("[conn %d] pty session exited (code %d)", id, code)
}

// servePipeSession runs cmd without a pty, feeding FrameStdin frames to the
// child's stdin and framing its stdout/stderr separately (FrameStdout/
// FrameStderr), then sending FrameExit.
func servePipeSession(id uint64, conn io.Writer, reader io.Reader, cmd *exec.Cmd, postStart func(pid int)) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("[conn %d] stdin pipe: %v", id, err)
		vshdproto.WriteFrame(conn, vshdproto.FrameStderr, []byte(fmt.Sprintf("vshd: %v\n", err)))
		vshdproto.WriteFrame(conn, vshdproto.FrameExit, vshdproto.EncodeExit(1))
		return
	}
	// stdout and stderr are framed onto the same connection from independent
	// goroutines; a shared mutex keeps each frame's header+payload contiguous.
	var writeMu sync.Mutex
	cmd.Stdout = &frameWriter{conn: conn, typ: vshdproto.FrameStdout, mu: &writeMu}
	cmd.Stderr = &frameWriter{conn: conn, typ: vshdproto.FrameStderr, mu: &writeMu}

	if err := cmd.Start(); err != nil {
		log.Printf("[conn %d] start command: %v", id, err)
		vshdproto.WriteFrame(conn, vshdproto.FrameStderr, []byte(fmt.Sprintf("vshd: %v\n", err)))
		vshdproto.WriteFrame(conn, vshdproto.FrameExit, vshdproto.EncodeExit(1))
		return
	}
	if postStart != nil {
		postStart(cmd.Process.Pid)
	}
	log.Printf("[conn %d] command started with PID %d", id, cmd.Process.Pid)

	// Client -> child stdin: decode FrameStdin frames. Other frame types (e.g.
	// stray winsize) are ignored in pipe mode. Close stdin on EOF.
	go func() {
		defer stdin.Close()
		for {
			typ, payload, err := vshdproto.ReadFrame(reader)
			if err != nil {
				return
			}
			if typ == vshdproto.FrameStdin {
				if _, werr := stdin.Write(payload); werr != nil {
					return
				}
			}
		}
	}()

	code := waitExitCode(cmd)
	vshdproto.WriteFrame(conn, vshdproto.FrameExit, vshdproto.EncodeExit(code))
	log.Printf("[conn %d] command exited (code %d)", id, code)
}

// waitExitCode waits for cmd and returns its exit code (0 on success, the
// process exit status on a normal non-zero exit, or 1 for other failures).
func waitExitCode(cmd *exec.Cmd) int32 {
	err := cmd.Wait()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				return int32(128 + int(ws.Signal()))
			}
			return int32(ws.ExitStatus())
		}
		return int32(ee.ExitCode())
	}
	return 1
}

// frameWriter wraps a connection so that each Write is emitted as one vshdproto
// frame of a fixed type. Used to frame a child's stdout/stderr in pipe mode.
type frameWriter struct {
	conn io.Writer
	typ  uint8
	mu   *sync.Mutex // optional; serialises frames sharing one conn
}

func (fw *frameWriter) Write(p []byte) (int, error) {
	if fw.mu != nil {
		fw.mu.Lock()
		defer fw.mu.Unlock()
	}
	if err := vshdproto.WriteFrame(fw.conn, fw.typ, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func main() {
	unixPath := flag.String("unix", "", "listen on this Unix socket path (host mode) instead of vsock (VM mode)")
	tsPath := flag.String("ts", "", "path to the ts binary used for nsenter (default: derived from vshd's location)")
	cgroupParent := flag.String("cgroup-parent", "", "parent cgroup name for per-session resource limits (host mode; empty disables)")
	flag.Parse()

	log.Printf("vshd starting up")

	// In host mode the daemon passes its cgroup parent name so vshd can apply
	// per-session memory/pids/cpu limits to each container child. In a VM the
	// flag is unset and resource limits come from the VM itself.
	if *cgroupParent != "" {
		cgroupMgr = cgroup.New(*cgroupParent)
		log.Printf("per-session cgroups enabled under parent %q", *cgroupParent)
	}

	// Determine ts binary path. An explicit --ts wins (host mode, where vshd is
	// not laid out as <prefix>/sbin/vshd); otherwise derive it from vshd's own
	// location (VM/VMX mode).
	if *tsPath != "" {
		tsBinaryPath = *tsPath
		log.Printf("using ts binary at %s (from --ts)", tsBinaryPath)
	} else {
		initTsBinaryPath()
	}

	var l net.Listener
	if *unixPath != "" {
		// Host mode: listen on a Unix socket. Remove any stale socket first.
		os.Remove(*unixPath)
		ul, err := net.Listen("unix", *unixPath)
		if err != nil {
			log.Fatalf("failed to listen on unix socket %s: %v", *unixPath, err)
		}
		l = ul
		log.Printf("vshd listening on unix socket %s", *unixPath)
	} else {
		// VM mode: listen on vsock.
		vl, err := vsock.Listen(vsockPort, nil)
		if err != nil {
			log.Fatalf("failed to listen on vsock port %d: %v", vsockPort, err)
		}
		l = vl
		log.Printf("vshd listening on vsock port %d", vsockPort)
	}
	defer l.Close()

	for {
		conn, err := l.Accept()
		if err != nil {
			log.Printf("accept error: %v", err)
			continue
		}

		go handleConnection(conn)
	}
}
