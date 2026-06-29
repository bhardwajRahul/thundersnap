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
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gliderlabs/ssh"
	"github.com/pkg/sftp"
	"github.com/tailscale/thundersnap/frameid"
	"github.com/tailscale/thundersnap/sftpfs"
	"github.com/tailscale/thundersnap/thundersnap"
	"github.com/tailscale/thundersnap/tsm"
)

// buildSessionCommand constructs the nsenter command that joins the container's
// PID/mount/UTS namespaces (anchored by initPid) and execs "ts drop-caps-and-run"
// to chroot into absRootFS, drop capabilities, and launch a login shell as
// runAsUser. When rawCmd is non-empty it is run via "su - <user> -c <rawCmd>";
// otherwise an interactive login shell is started. When ptyHandshake is true the
// --pty-handshake-fd=3 flag is added so the caller can complete the PTY handshake
// over fd 3.
//
// IMPORTANT: nsenter is used (not Go setns) because setns(CLONE_NEWNS) fails on
// multithreaded processes and the Go runtime is always multithreaded. We do NOT
// pass -F (--no-fork): Go programs fail to start in a joined PID namespace
// without the fork that places them in the namespace (EINVAL creating threads).
func buildSessionCommand(initPid int, tsBinary, absRootFS, runAsUser, rawCmd string, ptyHandshake bool) *exec.Cmd {
	tsArgs := []string{
		tsBinary, "drop-caps-and-run",
		"--chroot=" + absRootFS,
		// --skip-mount-setup: container-init already mounted proc/sys and a
		// devpts "newinstance" once for this namespace. Per-session setup would
		// stack a fresh devpts per session, restarting pts numbering at 0 so
		// every session sees /dev/pts/0 (bug #11); skip it for joining sessions.
		"--skip-mount-setup",
	}
	if ptyHandshake {
		// The fd number is 3 (after stdin=0, stdout=1, stderr=2).
		tsArgs = append(tsArgs, "--pty-handshake-fd=3")
	}
	// Run as the target user via a login shell ("su -") so HOME and the working
	// directory are the user's home; without it commands run from "/" with the
	// wrong HOME, which breaks tools like rsync that start in $HOME.
	tsArgs = append(tsArgs, "--", "su", "-", runAsUser)
	if rawCmd != "" {
		tsArgs = append(tsArgs, "-c", rawCmd)
	}

	// nsenter joins the PID (-p), mount (-m), and UTS (-u) namespaces of initPid.
	nsenterArgs := append([]string{
		"-t", fmt.Sprintf("%d", initPid),
		"-p", "-m", "-u",
		"--",
	}, tsArgs...)

	cmd := exec.Command("nsenter", nsenterArgs...)
	cmd.Dir = "/"
	return cmd
}

// sessionEnv builds the environment for a container session command. When term
// is non-empty a TERM entry is appended (PTY sessions); otherwise it is omitted.
func sessionEnv(sshUser, tailscaleUser, term string) []string {
	env := []string{
		"SSH_USER=" + sshUser,
		"TAILSCALE_USER=" + tailscaleUser,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	if term != "" {
		env = append(env, "TERM="+term)
	}
	return env
}

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

// runContainerSession handles a container-based SSH session.
// targetUser specifies the Unix user to run as. If empty, auto-detect from
// [ubuntu, user] based on which /home/<user> exists, or fall back to root.
func runContainerSession(s ssh.Session, tailscaleUser, frameName, targetUser string, logErr func(string, ...any)) error {
	// Resolve the frame name to its canonical fs/<user>/<uuid> path via the
	// user's ref store. A fresh unattached default frame is created on demand
	// below by ensureRootFS (falling back to the base snapshot).
	rootFS, uuid, err := resolveFrameRootFS(tailscaleUser, frameName)
	if err != nil {
		return err
	}
	if err := prepareContainerRootFS(rootFS, ""); err != nil {
		return err
	}

	// Get or create shared control socket server for this container
	_, err = controlServers.getOrCreateControlServer(rootFS)
	if err != nil {
		return fmt.Errorf("start control socket: %w", err)
	}
	defer controlServers.releaseControlServer(rootFS)

	// Check if a command was requested
	ptyReq, winCh, isPty := s.Pty()
	rawCmd := s.RawCommand()

	// Determine which Unix user to run as
	runAsUser := selectTargetUser(rootFS, targetUser)
	log.Printf("Container session: running as user %q (requested: %q)", runAsUser, targetUser)

	// Get absolute path for the rootFS
	absRootFS, err := filepath.Abs(rootFS)
	if err != nil {
		return fmt.Errorf("get absolute path for rootFS: %w", err)
	}

	// Determine hostname and domainname for the container namespace
	var hostname, domainname string
	if globalMeshState != nil && globalMeshState.myFQDN != "" {
		fqdn := globalMeshState.myFQDN
		if idx := strings.Index(fqdn, "."); idx > 0 {
			hostname = fqdn[:idx]
			domainname = fqdn[idx+1:]
		} else {
			hostname = fqdn
		}
	}

	// Get or create shared PID/mount/UTS namespaces for this container.
	// A single "init" process per rootFS creates and anchors the namespaces;
	// all sessions join these existing namespaces rather than creating new ones.
	// This allows processes from different sessions to see each other via /proc.
	initPid, err := containerNs.GetOrCreate(rootFS, hostname, domainname)
	if err != nil {
		return fmt.Errorf("create container namespace: %w", err)
	}
	defer containerNs.Release(rootFS)

	// Use nsenter to join the existing namespaces, then exec ts drop-caps-and-run.
	// We use nsenter instead of trying to do setns() in Go because:
	// - setns(CLONE_NEWNS) fails on multithreaded processes (EINVAL)
	// - Go programs are always multithreaded due to the runtime
	// - nsenter is a single-threaded C program that handles this correctly
	//
	// After nsenter joins the namespaces, ts drop-caps-and-run does:
	// - Chroot into the container rootfs (needed because joining mount ns doesn't change root)
	// - Drop dangerous capabilities
	// - Exec the final command
	tsBinary := filepath.Join(absRootFS, "bin", "ts")

	// Build cgroup name for this container (used for OOM group killing)
	// Uses the cgroup manager's parent name, which includes the daemon PID to
	// avoid conflicts with other instances.
	cgroupName := fmt.Sprintf("%s/%s/%s", cgroupManager.ParentName(), sanitizeForPath(tailscaleUser), uuid.String())

	// The nsenter command is built per-branch: the PTY branch needs the handshake
	// fd and TERM, the non-PTY branch does not.
	if isPty {
		// For PTY sessions, we allocate the PTY from outside the namespace by
		// opening the container's devpts ptmx directly. This gives us the master
		// fd for direct I/O and window size control, eliminating the need for
		// file-based communication and an intermediary process.
		//
		// The handshake protocol:
		// 1. We create a pipe and pass the write end to ts as --pty-handshake-fd
		// 2. nsenter joins namespaces, execs ts which does chroot and writes "READY\n"
		// 3. We open /proc/<pid>/root/dev/pts/ptmx, get the slave path
		//    (works because nsenter joined the mount namespace)
		// 4. We write the slave path back to ts via the pipe
		// 5. ts opens the slave as its controlling terminal and execs the shell

		// Create handshake socket pair for bidirectional communication
		fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
		if err != nil {
			return fmt.Errorf("create handshake socketpair: %w", err)
		}
		handshakeOurs := os.NewFile(uintptr(fds[0]), "handshake-ours")
		handshakeTheirs := os.NewFile(uintptr(fds[1]), "handshake-theirs")
		defer handshakeOurs.Close()

		// Same command as the non-PTY branch but with the PTY handshake fd so ts
		// waits for the slave path; TERM is added to the environment.
		cmd := buildSessionCommand(initPid, tsBinary, absRootFS, runAsUser, rawCmd, true)
		cmd.Env = sessionEnv(frameName, tailscaleUser, ptyReq.Term)

		// Pass their end of the socket as extra fd (fd 3)
		cmd.ExtraFiles = []*os.File{handshakeTheirs}

		// Start the command - it will set up devpts and wait for the PTY slave path
		if err := cmd.Start(); err != nil {
			handshakeTheirs.Close()
			return fmt.Errorf("start shell: %w", err)
		}
		handshakeTheirs.Close() // Close our copy of their end

		// Configure resource limits: OOM priority, memory soft limit, fork bomb
		// protection (pids.max), and CPU fairness
		cgroupManager.ConfigureContainer(cmd.Process.Pid, cgroupName)

		// Wait for "READY\n" from ts indicating devpts is mounted
		readyBuf := make([]byte, 64)
		n, err := handshakeOurs.Read(readyBuf)
		if err != nil {
			cmd.Process.Kill()
			cmd.Wait()
			return fmt.Errorf("handshake read: %w", err)
		}
		if !strings.HasPrefix(string(readyBuf[:n]), "READY") {
			cmd.Process.Kill()
			cmd.Wait()
			return fmt.Errorf("unexpected handshake: %q", string(readyBuf[:n]))
		}

		// Now open the container's ptmx via /proc/<pid>/root - this lets us
		// see the container's mount namespace including the devpts mount.
		// IMPORTANT: We use initPid (the container-init process) not cmd.Process.Pid
		// because nsenter forks (we don't use -F), so cmd.Process.Pid is the parent
		// nsenter process which hasn't chrooted. The container-init has the proper
		// rootfs and devpts setup.
		ptmx, slavePath, err := openContainerPTY(initPid)
		if err != nil {
			cmd.Process.Kill()
			cmd.Wait()
			return fmt.Errorf("open container PTY: %w", err)
		}
		defer ptmx.Close()

		// Set initial window size
		setPTYWinsize(ptmx, int(ptyReq.Window.Width), int(ptyReq.Window.Height))

		// Send the slave path back to ts
		if _, err := handshakeOurs.Write([]byte(slavePath + "\n")); err != nil {
			cmd.Process.Kill()
			cmd.Wait()
			return fmt.Errorf("send slave path: %w", err)
		}

		// Handle window size changes - now we can do this directly on the master
		go func() {
			for win := range winCh {
				setPTYWinsize(ptmx, int(win.Width), int(win.Height))
			}
		}()

		// Proxy I/O between SSH session and PTY master
		go func() {
			io.Copy(ptmx, s) // SSH -> PTY
		}()
		go func() {
			io.Copy(s, ptmx) // PTY -> SSH
		}()

		// Wait for the command to complete
		cmd.Wait()
		s.Exit(cmd.ProcessState.ExitCode())
	} else {
		// No PTY requested, run without one (no handshake fd, no TERM).
		cmd := buildSessionCommand(initPid, tsBinary, absRootFS, runAsUser, rawCmd, false)
		cmd.Env = sessionEnv(frameName, tailscaleUser, "")

		// Set up pipes for stdin/stdout/stderr
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return fmt.Errorf("create stdin pipe: %w", err)
		}
		cmd.Stdout = s
		cmd.Stderr = s.Stderr()

		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start command: %w", err)
		}

		// Configure resource limits: OOM priority, memory soft limit, fork bomb
		// protection (pids.max), and CPU fairness
		cgroupManager.ConfigureContainer(cmd.Process.Pid, cgroupName)

		// Copy stdin from SSH session to command in background
		go func() {
			io.Copy(stdin, s)
			stdin.Close()
		}()

		// Wait for the command to complete
		if err := cmd.Wait(); err != nil {
			// Check if it's just a non-zero exit code
			if exitErr, ok := err.(*exec.ExitError); ok {
				s.Exit(exitErr.ExitCode())
				return nil
			}
			return fmt.Errorf("run command: %w", err)
		}
		s.Exit(0)
	}
	return nil
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

	// Send VMX protocol: VMX\0framePath\0targetUser\0argCount\0args...
	// framePath is the frame's UUID, relative to the virtiofs root (userFsDir,
	// i.e. fs/<user>): the frame lives at fs/<user>/<uuid> on the host.
	writeVshdRequest(conn, uuid.String(), targetUser, s.Command())

	// Proxy SSH I/O (same as runVMSession)
	return proxyVMSession(s, conn, ms.done, ms.panicked)
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

	// Send original vshd protocol (no VMX prefix) - shell directly in VM
	writeVshdRequest(conn, "", targetUser, s.Command())

	// Proxy SSH I/O
	return proxyVMSession(s, conn, ms.done, ms.panicked)
}

// makeVMXControlHandler creates the HTTP handler for VMX control requests.
func makeVMXControlHandler(frameRootFS string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", handlePing)
	mux.HandleFunc("/snap", makeSnapHandler(frameRootFS))
	return mux
}

// writeVshdRequest writes a vshd session request to conn using the null-delimited
// wire protocol. When framePath is non-empty it emits the VMX form
// ("VMX\0framePath\0user\0argc\0args...") which spawns a container at framePath
// inside the VM; when framePath is empty it emits the plain form
// ("user\0argc\0args...") for a shell directly in the VM.
func writeVshdRequest(conn net.Conn, framePath, targetUser string, cmdArgs []string) {
	if framePath != "" {
		fmt.Fprintf(conn, "VMX\x00%s\x00", framePath)
	}
	fmt.Fprintf(conn, "%s\x00%d\x00", targetUser, len(cmdArgs))
	for _, arg := range cmdArgs {
		fmt.Fprintf(conn, "%s\x00", arg)
	}
}

// proxyVMSession proxies SSH I/O to/from a vshd connection.
// This is shared between runVMSession and runVMX* functions.
func proxyVMSession(s ssh.Session, conn net.Conn, done <-chan struct{}, panicked <-chan struct{}) error {
	copyDone := make(chan struct{}, 2)

	// SSH stdin -> vshd
	go func() {
		n, err := io.Copy(conn, s)
		log.Printf("SSH proxy: stdin->vshd finished: %d bytes, err=%v", n, err)
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		copyDone <- struct{}{}
	}()

	// vshd -> SSH stdout
	go func() {
		n, err := io.Copy(s, conn)
		log.Printf("SSH proxy: vshd->stdout finished: %d bytes, err=%v", n, err)
		copyDone <- struct{}{}
	}()

	// Wait for session end
	select {
	case <-copyDone:
		log.Printf("SSH proxy: vshd connection closed")
	case <-done:
		log.Printf("SSH proxy: VM exited")
	case <-panicked:
		log.Printf("SSH proxy: VM kernel panic")
	case <-s.Context().Done():
		log.Printf("SSH proxy: SSH session closed by client")
	}

	conn.Close()

	// Closing conn unblocks the io.Copy goroutines; wait briefly for both to
	// finish, but don't block the session teardown if one is wedged (e.g. the
	// SSH side of the copy not unblocking). The shared deadline bounds the total
	// wait at ~100ms regardless of how many goroutines have already returned.
	deadline := time.After(100 * time.Millisecond)
drain:
	for i := 0; i < 2; i++ {
		select {
		case <-copyDone:
		case <-deadline:
			break drain
		}
	}

	s.Exit(0)
	return nil
}
