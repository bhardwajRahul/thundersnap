// vshd is the in-guest shell daemon. It listens on a vsock port and serves a
// simple null-delimited request protocol over each connection, spawning either
// an interactive PTY shell or a one-shot command. Two protocol variants are
// supported (see handleConnection): the original "run on this VM" form and the
// extended "VMX" form that chroots into a container rootfs via
// `ts drop-caps-and-run` before exec'ing.
package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/creack/pty"
	"github.com/mdlayher/vsock"
)

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

func handleConnection(conn *vsock.Conn) {
	id := atomic.AddUint64(&connectionID, 1)
	log.Printf("[conn %d] new connection from %v", id, conn.RemoteAddr())
	defer func() {
		conn.Close()
		log.Printf("[conn %d] connection closed", id)
	}()

	// Read the command protocol:
	// Original protocol:
	//   targetUser\0argCount\0arg1\0...argN\0
	// Extended VMX protocol:
	//   VMX\0framePath\0targetUser\0argCount\0arg1\0...argN\0
	reader := bufio.NewReader(conn)

	firstField, err := readField(reader)
	if err != nil {
		log.Printf("[conn %d] failed to read first field: %v", id, err)
		return
	}

	// Check if this is the VMX protocol
	if firstField == "VMX" {
		handleVMXConnection(id, conn, reader)
		return
	}

	// Original protocol: firstField is targetUser
	targetUser := firstField

	cmdArgs, err := readArgs(reader)
	if err != nil {
		log.Printf("[conn %d] failed to read args: %v", id, err)
		return
	}

	// Determine which user to run as
	runAsUser := selectUser("", targetUser)
	log.Printf("[conn %d] running as user %q (requested: %q)", id, runAsUser, targetUser)

	if len(cmdArgs) > 0 {
		log.Printf("[conn %d] running command: %v", id, cmdArgs)
		runCommand(id, conn, reader, runAsUser, cmdArgs)
	} else {
		log.Printf("[conn %d] starting interactive shell", id)
		runInteractiveShell(id, conn, reader, runAsUser)
	}
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

// handleVMXConnection handles the VMX protocol for spawning containers inside the VM.
// Protocol: VMX\0framePath\0targetUser\0argCount\0arg1\0...argN\0
func handleVMXConnection(id uint64, conn *vsock.Conn, reader *bufio.Reader) {
	// Read framePath (path to container rootfs relative to virtiofs root)
	framePath, err := readField(reader)
	if err != nil {
		log.Printf("[conn %d] VMX: failed to read frame path: %v", id, err)
		return
	}

	// Read target user
	targetUser, err := readField(reader)
	if err != nil {
		log.Printf("[conn %d] VMX: failed to read target user: %v", id, err)
		return
	}

	// Read arg count + arguments
	cmdArgs, err := readArgs(reader)
	if err != nil {
		log.Printf("[conn %d] VMX: failed to read args: %v", id, err)
		return
	}

	// The frame rootfs is at /<framePath> from the virtiofs root
	// (virtiofs is mounted as / in the VM)
	containerRootFS := filepath.Clean("/" + framePath)
	log.Printf("[conn %d] VMX: spawning container at %s (user: %q, args: %v)", id, containerRootFS, targetUser, cmdArgs)

	if len(cmdArgs) > 0 {
		runContainerCommand(id, conn, reader, containerRootFS, targetUser, cmdArgs)
	} else {
		runContainerShell(id, conn, reader, containerRootFS, targetUser)
	}
}

// runContainerShell spawns an interactive shell inside a container.
// Uses ts drop-caps-and-run to set up namespaces and chroot.
func runContainerShell(id uint64, conn *vsock.Conn, reader *bufio.Reader, containerRootFS, targetUser string) {
	// Determine which user to run as (may auto-detect from container's /etc/passwd)
	runAsUser := selectUser(containerRootFS, targetUser)

	// Build ts command
	// ts drop-caps-and-run --chroot=<path> -- su - <user>
	var tsArgs []string
	if runAsUser == "root" {
		tsArgs = []string{"drop-caps-and-run", "--chroot=" + containerRootFS, "--", "/bin/sh", "-l"}
	} else {
		tsArgs = []string{"drop-caps-and-run", "--chroot=" + containerRootFS, "--", "su", "-", runAsUser}
	}

	cmd := exec.Command(tsBinaryPath, tsArgs...)
	// New PID/mount/UTS namespaces give the container its own process tree,
	// mount table, and hostname; `ts drop-caps-and-run` then performs the
	// chroot and drops capabilities inside them.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS,
	}
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		log.Printf("[conn %d] VMX: failed to start container shell: %v", id, err)
		fmt.Fprintf(conn, "vshd: failed to start container shell: %v\n", err)
		return
	}
	defer ptmx.Close()
	log.Printf("[conn %d] VMX: container shell started with PID %d (user: %s)", id, cmd.Process.Pid, runAsUser)

	// Copy data between vsock and pty
	done := make(chan struct{}, 2)

	go func() {
		n, err := io.Copy(ptmx, reader)
		log.Printf("[conn %d] VMX: stdin copy ended: %d bytes, err=%v", id, n, err)
		done <- struct{}{}
	}()

	go func() {
		n, err := io.Copy(conn, ptmx)
		log.Printf("[conn %d] VMX: stdout copy ended: %d bytes, err=%v", id, n, err)
		done <- struct{}{}
	}()

	// One copy goroutine returning means the session is ending (the guest shell
	// exited, closing the pty, or the vsock peer went away). SIGHUP tells the
	// shell its controlling terminal hung up so it tears down its job-control
	// children before we reap it with Wait.
	<-done
	log.Printf("[conn %d] VMX: signaling container to exit", id)
	cmd.Process.Signal(syscall.SIGHUP)
	cmd.Wait()
	log.Printf("[conn %d] VMX: container exited", id)
}

// runContainerCommand runs a command inside a container.
func runContainerCommand(id uint64, conn *vsock.Conn, reader *bufio.Reader, containerRootFS, targetUser string, cmdArgs []string) {
	// Determine which user to run as
	runAsUser := selectUser(containerRootFS, targetUser)

	// Build ts command
	// ts drop-caps-and-run --chroot=<path> -- su - <user> -c '<command>'
	var tsArgs []string
	if runAsUser == "root" {
		// For root, run command directly
		tsArgs = append([]string{"drop-caps-and-run", "--chroot=" + containerRootFS, "--"}, cmdArgs...)
	} else {
		// For non-root, use su -c
		cmdStr := quoteArgsForSh(cmdArgs)
		tsArgs = []string{"drop-caps-and-run", "--chroot=" + containerRootFS, "--", "su", "-", runAsUser, "-c", cmdStr}
	}

	cmd := exec.Command(tsBinaryPath, tsArgs...)
	// See runContainerShell for why these three namespaces.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS,
	}
	cmd.Env = os.Environ()

	// Set up pipes
	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("[conn %d] VMX: failed to create stdin pipe: %v", id, err)
		fmt.Fprintf(conn, "vshd: failed to create stdin pipe: %v\n", err)
		return
	}
	cmd.Stdout = conn
	cmd.Stderr = conn

	if err := cmd.Start(); err != nil {
		log.Printf("[conn %d] VMX: failed to start container command: %v", id, err)
		fmt.Fprintf(conn, "vshd: %v\n", err)
		return
	}
	log.Printf("[conn %d] VMX: container command started with PID %d (user: %s)", id, cmd.Process.Pid, runAsUser)

	// Copy stdin in background
	go func() {
		io.Copy(stdin, reader)
		stdin.Close()
	}()

	// Wait for command to complete
	err = cmd.Wait()
	if err != nil {
		log.Printf("[conn %d] VMX: container command exited with error: %v", id, err)
	} else {
		log.Printf("[conn %d] VMX: container command exited successfully", id)
	}
}

// runInteractiveShell spawns an interactive login shell as the specified user with PTY.
// For root, runs /bin/sh directly. For other users, uses "su - <user>".
func runInteractiveShell(id uint64, conn *vsock.Conn, reader *bufio.Reader, runAsUser string) {
	var cmd *exec.Cmd
	if runAsUser == "root" {
		// When running as root, start a shell directly without su.
		// This avoids the need for a dynamically-linked su binary in minimal containers.
		cmd = exec.Command("/bin/sh", "-l")
	} else {
		// Use su - <user> for a login shell (sets HOME, reads profile, etc.)
		cmd = exec.Command("su", "-", runAsUser)
	}
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		log.Printf("[conn %d] failed to start pty: %v", id, err)
		fmt.Fprintf(conn, "vshd: failed to start shell as %s: %v\n", runAsUser, err)
		return
	}
	defer ptmx.Close()
	log.Printf("[conn %d] shell started with PID %d (user: %s)", id, cmd.Process.Pid, runAsUser)

	// Copy data between vsock and pty
	done := make(chan struct{}, 2)

	go func() {
		n, err := io.Copy(ptmx, reader)
		log.Printf("[conn %d] stdin copy ended: %d bytes, err=%v", id, n, err)
		done <- struct{}{}
	}()

	go func() {
		n, err := io.Copy(conn, ptmx)
		log.Printf("[conn %d] stdout copy ended: %d bytes, err=%v", id, n, err)
		done <- struct{}{}
	}()

	// See runContainerShell: one copy ending means the session is over; SIGHUP
	// hangs up the shell's controlling terminal before we reap it.
	<-done
	log.Printf("[conn %d] signaling shell to exit", id)
	cmd.Process.Signal(syscall.SIGHUP)
	cmd.Wait()
	log.Printf("[conn %d] shell exited", id)
}

// runCommand executes a command as the specified user without PTY and exits when done.
// For root, runs the command directly. For other users, uses "su - <user> -c" for a
// login shell that sets HOME and changes to the user's home directory.
func runCommand(id uint64, conn *vsock.Conn, reader *bufio.Reader, runAsUser string, cmdArgs []string) {
	var cmd *exec.Cmd

	if runAsUser == "root" {
		// When running as root, execute the command directly without su.
		// This avoids the need for a dynamically-linked su binary in minimal containers.
		cmd = exec.Command(cmdArgs[0], cmdArgs[1:]...)
	} else {
		// For non-root users, use su - to switch users with a login shell.
		// The login shell changes to the home directory, sets HOME, reads profile, etc.
		cmdStr := quoteArgsForSh(cmdArgs)
		cmd = exec.Command("su", "-", runAsUser, "-c", cmdStr)
	}
	cmd.Env = os.Environ()

	// Set up pipes for stdin/stdout/stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("[conn %d] failed to create stdin pipe: %v", id, err)
		fmt.Fprintf(conn, "vshd: failed to create stdin pipe: %v\n", err)
		return
	}
	cmd.Stdout = conn
	cmd.Stderr = conn

	if err := cmd.Start(); err != nil {
		log.Printf("[conn %d] failed to start command: %v", id, err)
		fmt.Fprintf(conn, "vshd: %v\n", err)
		return
	}
	log.Printf("[conn %d] command started with PID %d (user: %s)", id, cmd.Process.Pid, runAsUser)

	// Copy stdin in background
	go func() {
		io.Copy(stdin, reader)
		stdin.Close()
	}()

	// Wait for command to complete
	err = cmd.Wait()
	if err != nil {
		log.Printf("[conn %d] command exited with error: %v", id, err)
	} else {
		log.Printf("[conn %d] command exited successfully", id)
	}
}

func main() {
	log.Printf("vshd starting up")

	// Determine ts binary path based on vshd's location
	initTsBinaryPath()

	l, err := vsock.Listen(vsockPort, nil)
	if err != nil {
		log.Fatalf("failed to listen on vsock port %d: %v", vsockPort, err)
	}
	defer l.Close()

	log.Printf("vshd listening on vsock port %d", vsockPort)

	for {
		conn, err := l.Accept()
		if err != nil {
			log.Printf("accept error: %v", err)
			continue
		}

		go handleConnection(conn.(*vsock.Conn))
	}
}
