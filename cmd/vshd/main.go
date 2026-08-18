// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

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
	"sync/atomic"
	"syscall"

	"github.com/mdlayher/vsock"
	"github.com/tailscale/thundersnap/cgroup"
	"github.com/tailscale/thundersnap/containerns"
	"github.com/tailscale/thundersnap/tsm"
	"github.com/tailscale/thundersnap/vshdproto"
	"github.com/tailscale/thundersnap/vshdsession"
)

// cgroupMgr applies fail-closed container and per-session cgroup v2 limits.
// It is initialized from --cgroup-parent before containerNs is used.
var cgroupMgr *cgroup.Manager

// containerNs anchors shared PID/mount/UTS/cgroup namespaces per rootfs.
var containerNs *containerns.Manager

// selectUser determines which Unix user to run as, auto-detecting when the
// caller did not request one. rootPrefix is "" for the host VM filesystem or a
// container rootfs path for VMX mode; all lookups are resolved beneath it.
// Detection order: explicit targetUser -> "ubuntu" (if /home/ubuntu exists) ->
// "user" (if its /etc/passwd home exists) -> "root".
//
// selectUser does NOT validate targetUser; callers must run validateTargetUser
// on a non-empty targetUser before relying on the result, since the returned
// name is later passed to `su - <user>` as a bare argv element and a name
// starting with '-' (e.g. "-c") would be parsed as a su flag.
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

// validateTargetUser checks that a caller-requested Unix username is safe to
// hand to `su - <user>` (or the in-container `ts su` fallback). The username is
// ultimately placed into `su`'s argv as a bare element, so the critical rule is
// that it must not start with '-': `su - -c '<cmd>'` would parse `-c` as su's
// own option and run <cmd> as root rather than as a lookup of a user named
// "-c". It also bounds the length and rejects characters outside the portable
// Unix username set so the name can never smuggle a path separator, whitespace,
// or shell metacharacter into a future caller.
//
// The auto-detected defaults produced by selectUser ("root", "user", "ubuntu")
// always satisfy this, so callers only need to validate when targetUser is
// non-empty (i.e. when the client specified a user).
func validateTargetUser(name string) error {
	if name == "" {
		return fmt.Errorf("empty username")
	}
	if len(name) > 256 {
		return fmt.Errorf("username too long (%d bytes, max 256)", len(name))
	}
	if name[0] == '-' {
		return fmt.Errorf("username must not start with '-'")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '_' || c == '-' || c == '.':
			// allowed
		default:
			return fmt.Errorf("username contains invalid character %q", rune(c))
		}
	}
	return nil
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

// exportPrefix returns a shell snippet that exports each KEY=VAL entry from
// env, e.g. "export 'KEY'='VALUE'; ". It is prepended to the command given to
// `su - user -c <cmd>` so the vars are set AFTER su clears the environment
// (a real su - resets env; ts's built-in su preserves THUNDERSNAP_* via
// identityEnv, so the prefix is redundant there but harmless). Returns "" when
// env is empty so callers can skip mangling the command entirely and preserve
// the original argv. Both key and value are single-quoted with '\” escaping so
// no metacharacter in a value (hostname, ref name, comma) can break out.
func exportPrefix(env []string) string {
	if len(env) == 0 {
		return ""
	}
	var b strings.Builder
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if !ok || k == "" {
			continue
		}
		b.WriteString("export '")
		b.WriteString(k)
		b.WriteString("'='")
		b.WriteString(strings.ReplaceAll(v, "'", "'\\''"))
		b.WriteString("'; ")
	}
	return b.String()
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

// monitorLifecycleFd reads from the given fd until EOF (or error), then exits
// the process. This ties vshd's lifetime to the parent: the parent creates a
// pipe, passes the read end as lifecycleFd, and keeps the write end open. When
// the parent exits (or crashes), the write end closes, we see EOF, and exit.
func monitorLifecycleFd(fd int) {
	f := os.NewFile(uintptr(fd), "lifecycle-fd")
	if f == nil {
		log.Printf("lifecycle-fd %d is invalid, ignoring", fd)
		return
	}
	defer f.Close()

	// Block until EOF or error
	buf := make([]byte, 1)
	for {
		_, err := f.Read(buf)
		if err != nil {
			log.Printf("lifecycle-fd closed, exiting")
			os.Exit(0)
		}
	}
}

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

	// Env block (appended after args by writeVshdRequest): a null-delimited
	// count of KEY=VAL entries to inject into the session's environment (e.g.
	// THUNDERSNAP_HOST, THUNDERSNAP_FRAME). The daemon — the only place that
	// knows the tsnet hostname and the user's refs — computes these; vshd
	// treats them as opaque strings and propagates them via the command's Env,
	// which flows through nsenter -> session-serve -> the final shell because
	// every exec in that chain uses os.Environ().
	sessionEnvs, err := readEnv(reader)
	if err != nil {
		log.Printf("[conn %d] failed to read env: %v", id, err)
		return
	}

	// Validate a caller-specified user before it reaches `su`. A name starting
	// with '-' (e.g. "-c") would be parsed as a su flag and run the supplied
	// command as root; see validateTargetUser. Auto-detection (targetUser == "")
	// always yields a safe default, so only non-empty requests are checked.
	if targetUser != "" {
		if err := validateTargetUser(targetUser); err != nil {
			log.Printf("[conn %d] rejecting invalid target user %q: %v", id, targetUser, err)
			vshdproto.WriteFrame(conn, vshdproto.FrameStderr,
				[]byte(fmt.Sprintf("vshd: invalid user %q: %v\n", targetUser, err)))
			vshdproto.WriteFrame(conn, vshdproto.FrameExit, vshdproto.EncodeExit(1))
			return
		}
	}

	runAsUser := selectUser(rootPrefix, targetUser)
	log.Printf("[conn %d] running as user %q (requested: %q, rootPrefix: %q, pty: %v, args: %v, env: %v)",
		id, runAsUser, targetUser, rootPrefix, wantPTY, cmdArgs, sessionEnvs)

	cmd, release, err := buildSessionCmd(rootPrefix, runAsUser, cmdArgs, wantPTY, sessionEnvs)
	if err != nil {
		log.Printf("[conn %d] failed to build session command: %v", id, err)
		vshdproto.WriteFrame(conn, vshdproto.FrameStderr, []byte(fmt.Sprintf("vshd: %v\n", err)))
		vshdproto.WriteFrame(conn, vshdproto.FrameExit, vshdproto.EncodeExit(1))
		return
	}
	if release != nil {
		defer release()
	}

	// Container sessions get a leaf inside the container's delegated cgroup.
	// Because vshd is outside the container cgroup namespace it addresses the
	// complete path; inside the container the same cgroup appears as /.
	var postStart func(pid int) error
	var postDisconnect func()
	var postExit func()
	if cgroupMgr != nil && rootPrefix != "" && cgroup.CanCloneInto() {
		leaf := filepath.Join(containerNs.CgroupName(rootPrefix), "sessions", fmt.Sprintf("%d", id))
		postStart = func(pid int) error {
			return cgroupMgr.ConfigureContainer(pid, leaf)
		}
		postDisconnect = func() {
			if err := cgroupMgr.KillSession(leaf); err != nil {
				log.Printf("[conn %d] failed to kill remaining session processes: %v", id, err)
			}
		}
		postExit = func() {
			// Repeat the kill synchronously before removal. The disconnect callback
			// runs in the input-copy goroutine and may race the output side exiting.
			if err := cgroupMgr.KillSession(leaf); err != nil {
				log.Printf("[conn %d] failed to kill remaining session processes at exit: %v", id, err)
			}
			if err := cgroupMgr.RemoveSession(leaf); err != nil {
				log.Printf("[conn %d] failed to remove session cgroup: %v", id, err)
			}
		}
	}

	logf := func(format string, args ...any) {
		log.Printf("[conn %d] "+format, append([]any{id}, args...)...)
	}

	if rootPrefix != "" {
		// Container session: the wrapped inner `ts session-serve` (which runs
		// after nsenter+chroot, inside the container's mount namespace) is the
		// TLV endpoint. vshd just splices the raw TLV bytes through in both
		// directions; it does not interpret the protocol. This is what lets the
		// pty be opened from inside the container so /dev/pts/N is visible there.
		spliceContainerSession(id, conn, reader, cmd, postStart, postDisconnect)
		if postExit != nil {
			postExit()
		}
		return
	}

	// Direct VM/host session (no container): vshd is the TLV endpoint.
	ptyOwnerUID := -1
	if wantPTY {
		if ui := tsm.LookupUser("/", runAsUser); ui != nil {
			ptyOwnerUID = int(ui.UID)
		} else if runAsUser == "root" {
			ptyOwnerUID = 0
		}
	}
	vshdsession.Serve(conn, reader, cmd, wantPTY, ptyOwnerUID, postStart, logf)
}

// spliceContainerSession runs the wrapped inner `ts session-serve` command and
// splices the raw vshdproto byte stream between the network connection and the
// child's stdin/stdout. The inner ts is the protocol endpoint (it opens the pty
// inside the container, frames stdout/stderr, and sends FrameExit), so vshd
// performs no framing here. postStart, when non-nil, applies cgroup limits to
// the child once started.
func spliceContainerSession(id uint64, conn io.Writer, reader io.Reader, cmd *exec.Cmd, postStart func(pid int) error, postDisconnect func()) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("[conn %d] stdin pipe: %v", id, err)
		vshdproto.WriteFrame(conn, vshdproto.FrameStderr, []byte(fmt.Sprintf("vshd: %v\n", err)))
		vshdproto.WriteFrame(conn, vshdproto.FrameExit, vshdproto.EncodeExit(1))
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("[conn %d] stdout pipe: %v", id, err)
		vshdproto.WriteFrame(conn, vshdproto.FrameStderr, []byte(fmt.Sprintf("vshd: %v\n", err)))
		vshdproto.WriteFrame(conn, vshdproto.FrameExit, vshdproto.EncodeExit(1))
		return
	}
	// The inner ts owns stderr framing only for its own startup diagnostics; the
	// session's stderr travels as FrameStderr inside the spliced stdout stream.
	// Surface any raw inner-ts stderr to vshd's log to aid debugging.
	cmd.Stderr = &logWriter{id: id}

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		log.Printf("[conn %d] start command: %v", id, err)
		vshdproto.WriteFrame(conn, vshdproto.FrameStderr, []byte(fmt.Sprintf("vshd: %v\n", err)))
		vshdproto.WriteFrame(conn, vshdproto.FrameExit, vshdproto.EncodeExit(1))
		return
	}
	if postStart != nil {
		if err := postStart(cmd.Process.Pid); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			log.Printf("[conn %d] failed to confine container session: %v", id, err)
			vshdproto.WriteFrame(conn, vshdproto.FrameStderr, []byte("vshd: failed to confine session: "+err.Error()+"\n"))
			vshdproto.WriteFrame(conn, vshdproto.FrameExit, vshdproto.EncodeExit(1))
			return
		}
	}
	log.Printf("[conn %d] container session started with PID %d", id, cmd.Process.Pid)

	killSession := func() {
		if postDisconnect != nil {
			postDisconnect()
			return
		}
		// Nested or namespace-relative cgroup setups cannot create a session leaf.
		// The wrapper retains a shared process group across nsenter, so killing the
		// group also closes the inner protocol endpoint and triggers its own reap.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	// Client -> inner ts: copy the raw TLV byte stream to the child's stdin.
	go func() {
		io.Copy(stdin, reader)
		stdin.Close()
		killSession()
	}()

	// Inner ts -> client: copy the raw TLV byte stream (stdout, stderr frames,
	// and the final FrameExit) back to the client.
	io.Copy(conn, stdout)

	// The inner ts already framed and sent FrameExit before closing stdout, so
	// just reap the child.
	cmd.Wait()
	log.Printf("[conn %d] container session exited", id)
}

// logWriter forwards a child's raw stderr to vshd's log, line-buffered loosely
// (each Write is logged as-is).
type logWriter struct{ id uint64 }

func (w *logWriter) Write(p []byte) (int, error) {
	log.Printf("[conn %d] inner-ts stderr: %s", w.id, strings.TrimRight(string(p), "\n"))
	return len(p), nil
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
// `ts join-and-run --chroot --keep-dev-caps`. This is byte-identical to the daemon's host per-session
// form, so host and VM sessions sharing a container rootfs see each other's
// PIDs.
func buildSessionCmd(rootPrefix, runAsUser string, cmdArgs []string, wantPTY bool, env []string) (*exec.Cmd, func(), error) {
	// argv is what we ultimately exec (before any container wrapper).
	var argv []string
	switch {
	case len(cmdArgs) == 0 && runAsUser == "root":
		// Force interactive mode rather than relying on each minimal image's sh
		// to infer it from the pty. In particular, BusyBox ash otherwise exits
		// when the terminal line discipline delivers Ctrl-C at the prompt.
		argv = []string{"/bin/sh", "-il"}
	case len(cmdArgs) == 0:
		// Use our in-frame su directly rather than the image's su. In particular,
		// do not use `su - user -c "...; exec sh -l"`: util-linux su puts its
		// -c child in a process-group arrangement that makes dash disable job
		// control, and both su's login shell and the exec'd shell read profiles.
		// ts su drops uid/gid and execs the user's login shell directly, preserving
		// THUNDERSNAP_* through identityEnv and reading the profile exactly once.
		argv = []string{"/bin/ts", "su", "-", runAsUser}
	case runAsUser == "root":
		// Run the command directly as root.
		argv = cmdArgs
	default:
		// Use the same deterministic in-frame privilege drop for command sessions.
		// identityEnv preserves session descriptors, so no extra login-shell export
		// wrapper is needed.
		argv = []string{"/bin/ts", "su", "-", runAsUser, "-c", quoteArgsForSh(cmdArgs)}
	}

	if rootPrefix == "" {
		// Direct shell/command in this filesystem (outer VM or host, no
		// container).
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = sessionEnv(wantPTY, env)
		return cmd, nil, nil
	}

	// Container session (VMX or host): join the shared namespaces anchored by
	// container-init, then chroot + drop caps. GetOrCreate refcounts the init
	// per rootfs; release drops our reference when the session ends.
	if containerNs == nil {
		return nil, nil, fmt.Errorf("container cgroup manager is not initialized")
	}
	initPid, err := containerNs.GetOrCreate(rootPrefix, "", "")
	if err != nil {
		return nil, nil, fmt.Errorf("create container namespace: %w", err)
	}
	release := func() { containerNs.Release(rootPrefix) }

	// The inner ts lives in the frame rootfs; nsenter is run by the outer ts
	// (tsBinaryPath) which is always present on the host/outer-VM filesystem.
	//
	// Inside the container we exec `ts session-serve` rather than the shell
	// directly: session-serve runs AFTER chroot, so when it opens the pty the
	// slave is allocated from the container's own devpts instance and is visible
	// as /dev/pts/N inside the container. vshd then just splices the TLV byte
	// stream to/from this inner ts (see spliceContainerSession). The pty flag and
	// the final argv are passed through to session-serve.
	innerTs := filepath.Join(rootPrefix, "bin", "ts")
	ptyFlag := "0"
	if wantPTY {
		ptyFlag = "1"
	}
	serveArgs := append([]string{
		"session-serve", ptyFlag, runAsUser, strconv.Itoa(len(argv)),
	}, argv...)
	// TODO: --keep-dev-caps is currently always passed to allow running
	// thundersnap recursively inside a thundersnap container (for development).
	// This retains CAP_MKNOD so nested thundersnap can mount devtmpfs and create
	// device nodes. This should be made configurable per-frame or per-session
	// once we have a mechanism to request it (e.g., a frame metadata flag or
	// SSH user prefix like "dev@frame").
	//
	// Always join the container-init's mount namespace. container-init bind-mounts
	// chrootPath to itself before chroot to ensure it's in the mount table for
	// processes that later join via setns(CLONE_NEWNS). This approach ensures
	// identical behavior in all environments (host, nested container, VM).
	dropCapsArgs := append([]string{
		"join-and-run",
		"--chroot=" + rootPrefix,
		"--keep-dev-caps",
		"--",
		"/bin/ts",
	}, serveArgs...)
	nsenterArgs := append([]string{
		"nsenter",
		"-t", strconv.Itoa(initPid), "-p", "-m", "-u", "-C", "--",
		innerTs,
	}, dropCapsArgs...)

	cmd := exec.Command(tsBinaryPath, nsenterArgs...)
	cmd.Env = sessionEnv(wantPTY, env)
	return cmd, release, nil
}

const sessionPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// sessionEnv returns the environment for a session command, setting a
// deterministic PATH, adding TERM for PTY sessions, and merging in any
// daemon-supplied KEY=VAL entries. The session must not inherit PATH from vshd:
// service managers may omit /bin, but a minimal frame contains ts only as
// /bin/ts. The merged env propagates to the final shell because every exec in
// the nsenter -> session-serve chain uses os.Environ().
func sessionEnv(wantPTY bool, env []string) []string {
	out := mergeEnv(os.Environ(), env)
	if wantPTY {
		out = mergeEnv(out, []string{"TERM=xterm-256color"})
	}
	return mergeEnv(out, []string{"PATH=" + sessionPath})
}

// mergeEnv returns base with each KEY=VAL from extra overriding any existing
// entry with the same key (extra takes precedence), preserving base order for
// surviving base entries and appending extras in order. This keeps a stale
// value inherited from vshd's own environment from shadowing a fresh value
// supplied by the daemon for this session.
func mergeEnv(base, extra []string) []string {
	extraKeys := make(map[string]bool, len(extra))
	for _, e := range extra {
		if i := strings.IndexByte(e, '='); i >= 0 {
			extraKeys[e[:i]] = true
		}
	}
	merged := make([]string, 0, len(base)+len(extra))
	for _, e := range base {
		if i := strings.IndexByte(e, '='); i >= 0 && extraKeys[e[:i]] {
			continue // overridden by extra
		}
		merged = append(merged, e)
	}
	return append(merged, extra...)
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

// readEnv reads a null-delimited "envCount\0KEY=VAL\0...\0" block appended
// after the args by writeVshdRequest. The entries are opaque KEY=VAL strings
// the daemon injects into the session's environment (e.g. THUNDERSNAP_HOST,
// THUNDERSNAP_FRAME). A non-numeric or negative count is rejected up front so
// a malformed request fails fast instead of blocking on a never-arriving
// field.
func readEnv(reader *bufio.Reader) ([]string, error) {
	countStr, err := readField(reader)
	if err != nil {
		return nil, fmt.Errorf("read env count: %w", err)
	}
	envCount, err := strconv.Atoi(countStr)
	if err != nil {
		return nil, fmt.Errorf("invalid env count %q: %w", countStr, err)
	}
	if envCount < 0 {
		return nil, fmt.Errorf("negative env count %d", envCount)
	}
	env := make([]string, 0, envCount)
	for i := 0; i < envCount; i++ {
		entry, err := readField(reader)
		if err != nil {
			return nil, fmt.Errorf("read env %d: %w", i, err)
		}
		env = append(env, entry)
	}
	return env, nil
}

func main() {
	unixPath := flag.String("unix", "", "listen on this Unix socket path (host mode) instead of vsock (VM mode)")
	tsPath := flag.String("ts", "", "path to the ts binary used for nsenter (default: derived from vshd's location)")
	cgroupParent := flag.String("cgroup-parent", "", "parent cgroup name for per-session resource limits (host mode; empty disables)")
	lifecycleFd := flag.Int("lifecycle-fd", -1, "file descriptor to monitor; vshd exits when this fd closes (used for parent-death cleanup)")
	flag.Parse()

	log.Printf("vshd starting up")

	// If a lifecycle fd is provided, monitor it in a goroutine and exit when it
	// closes. This ties vshd's lifetime to the parent process: when the parent
	// dies (or explicitly closes the fd), we exit cleanly instead of orphaning.
	if *lifecycleFd >= 0 {
		go monitorLifecycleFd(*lifecycleFd)
	}

	// In host mode the daemon passes its cgroup parent name so vshd can apply
	// per-session memory/pids/cpu limits to each container child. In a VM the
	// flag is unset and resource limits come from the VM itself.
	if *cgroupParent == "" && *unixPath == "" {
		// VM-mode vshd has its own cgroup2 hierarchy and no daemon-supplied
		// parent name. PID is stable enough to distinguish concurrent daemons.
		*cgroupParent = fmt.Sprintf("thundersnap-vm-%d", os.Getpid())
	}
	if *cgroupParent != "" {
		cgroupMgr = cgroup.New(*cgroupParent)
		containerNs = containerns.New(cgroupMgr)
		log.Printf("container and per-session cgroups enabled under parent %q", *cgroupParent)
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
