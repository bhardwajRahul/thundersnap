// thundersnapd is a Tailscale tsnet-based SSH server that provides
// isolated container environments for each user session.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/creack/pty"
	"github.com/gliderlabs/ssh"
	"github.com/tailscale/thundersnap/thundersnap"
	"tailscale.com/client/tailscale"
	"tailscale.com/tsnet"
)

var (
	flagFsDir *string
	flagVmDir *string
)

func main() {
	hostname := flag.String("hostname", "thundersnap", "Tailscale hostname for this server")
	stateDir := flag.String("state-dir", "", "Directory to store Tailscale state (default: ~/.config/thundersnapd)")
	flagFsDir = flag.String("fs-dir", "", "Directory to store per-user filesystems (required)")
	flagVmDir = flag.String("vm-dir", "", "Directory containing cloud-hypervisor and vmlinux (default: <exe-dir>/vm)")
	flag.Parse()

	if *flagFsDir == "" {
		log.Fatalf("-fs-dir is required")
	}

	// Set default vm-dir relative to executable
	if *flagVmDir == "" {
		exe, err := os.Executable()
		if err != nil {
			log.Fatalf("Failed to get executable path: %v", err)
		}
		*flagVmDir = filepath.Join(filepath.Dir(exe), "vm")
	}

	// Set up state directory
	if *stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Fatalf("Failed to get home directory: %v", err)
		}
		*stateDir = filepath.Join(home, ".config", "thundersnapd")
	}

	if err := os.MkdirAll(*stateDir, 0700); err != nil {
		log.Fatalf("Failed to create state directory: %v", err)
	}

	// Create tsnet server
	srv := &tsnet.Server{
		Hostname: *hostname,
		Dir:      *stateDir,
	}
	defer srv.Close()

	// Start the tsnet server and wait for it to be ready
	log.Printf("Starting tsnet server with hostname %q...", *hostname)
	status, err := srv.Up(context.Background())
	if err != nil {
		log.Fatalf("Failed to start tsnet server: %v", err)
	}
	log.Printf("tsnet server is up! Tailscale IP: %v", status.TailscaleIPs)

	// Listen on port 22 for SSH connections
	ln, err := srv.Listen("tcp", ":22")
	if err != nil {
		log.Fatalf("Failed to listen on :22: %v", err)
	}
	defer ln.Close()

	log.Printf("SSH server listening on port 22")

	// Get the LocalClient to look up peer info
	lc, err := srv.LocalClient()
	if err != nil {
		log.Fatalf("Failed to get LocalClient: %v", err)
	}

	// Create SSH server with gliderlabs/ssh
	sshServer := &ssh.Server{
		Handler: func(s ssh.Session) {
			log.Printf("New SSH session from %s (user: %s)", s.RemoteAddr(), s.User())

			// Look up the Tailscale identity of the connecting peer
			tailscaleUser := getTailscaleUser(s.Context(), lc, s.RemoteAddr().String())

			// Print greeting to stdout (PTY merges stdout/stderr anyway)
			fmt.Fprintf(s, "* Hello <%s>, connecting you to <%s>\r\n", tailscaleUser, s.User())

			// Helper to log error to both server log and client
			logErr := func(format string, args ...any) {
				msg := fmt.Sprintf(format, args...)
				log.Print(msg)
				fmt.Fprintf(s, "* Error: %s\r\n", msg)
			}

			// Check if this is a VM session (vm/<user>)
			sshUser := s.User()
			if strings.HasPrefix(sshUser, "vm/") {
				vmUser := strings.TrimPrefix(sshUser, "vm/")
				if err := runVMSession(s, tailscaleUser, vmUser, logErr); err != nil {
					logErr("VM session failed: %v", err)
					s.Exit(1)
				}
				return
			}

			// Container session
			if err := runContainerSession(s, tailscaleUser, sshUser, logErr); err != nil {
				logErr("Container session failed: %v", err)
				s.Exit(1)
			}
		},
		// No authentication required - Tailscale already authenticated the connection.
		// When both PasswordHandler and PublicKeyHandler are nil, gliderlabs/ssh
		// performs no client authentication.
	}

	log.Printf("Waiting for SSH connections...")

	// Serve SSH connections
	if err := sshServer.Serve(ln); err != nil {
		log.Fatalf("SSH server error: %v", err)
	}
}

// getTailscaleUser looks up the Tailscale identity for the given remote address.
// Returns the user's login name, or tags if it's a tagged node, or the IP if lookup fails.
func getTailscaleUser(ctx context.Context, lc *tailscale.LocalClient, remoteAddr string) string {
	// Parse the IP from the remote address (format is "ip:port")
	host := remoteAddr
	if idx := strings.LastIndex(remoteAddr, ":"); idx != -1 {
		host = remoteAddr[:idx]
	}
	// Handle IPv6 addresses wrapped in brackets
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")

	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Sprintf("unknown (bad IP: %v)", err)
	}

	// Look up who owns this IP
	whois, err := lc.WhoIs(ctx, ip.String())
	if err != nil {
		return fmt.Sprintf("unknown (whois error: %v)", err)
	}

	// If it's a tagged node, return the tags
	if whois.Node != nil && len(whois.Node.Tags) > 0 {
		return fmt.Sprintf("tags: %s", strings.Join(whois.Node.Tags, ", "))
	}

	// Return the user's login name
	if whois.UserProfile != nil && whois.UserProfile.LoginName != "" {
		return whois.UserProfile.LoginName
	}

	return "unknown"
}

// setWinsize sets the size of the given pty.
func setWinsize(f *os.File, w, h int) {
	syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCSWINSZ),
		uintptr(unsafe.Pointer(&struct{ h, w, x, y uint16 }{uint16(h), uint16(w), 0, 0})))
}

// sanitizeForPath replaces characters that are unsafe for filesystem paths.
func sanitizeForPath(s string) string {
	// Replace / and null bytes, and collapse multiple replacements
	replacer := strings.NewReplacer(
		"/", "_",
		"\x00", "_",
		"..", "_",
	)
	result := replacer.Replace(s)
	// Also handle leading dots to prevent hidden directories
	result = strings.TrimLeft(result, ".")
	if result == "" {
		result = "_"
	}
	return result
}

// stripDomain removes the @domain part from a username (e.g., "user@example.com" -> "user")
func stripDomain(s string) string {
	if idx := strings.Index(s, "@"); idx != -1 {
		return s[:idx]
	}
	return s
}

// runContainerSession handles a container-based SSH session.
func runContainerSession(s ssh.Session, tailscaleUser, sshUser string, logErr func(string, ...any)) error {
	// Sanitize usernames for filesystem paths (replace unsafe chars)
	safeTailscaleUser := sanitizeForPath(tailscaleUser)
	safeSSHUser := sanitizeForPath(sshUser)

	// For home directory, strip @host from username for a cleaner look
	homeUser := stripDomain(safeTailscaleUser)

	// Set up the root filesystem for this user
	// If this is not the "base" user (stripped username), try to clone from
	// the base user's filesystem first, falling back to the clean snapshot
	rootFS := filepath.Join(*flagFsDir, safeTailscaleUser, safeSSHUser)
	baseUserFS := filepath.Join(*flagFsDir, safeTailscaleUser, homeUser)
	if err := ensureRootFS(rootFS, baseUserFS); err != nil {
		return fmt.Errorf("set up root filesystem: %w", err)
	}

	// Ensure /proc mount point exists in the rootfs
	procDir := filepath.Join(rootFS, "proc")
	if err := os.MkdirAll(procDir, 0555); err != nil {
		return fmt.Errorf("create /proc directory: %w", err)
	}

	// Create home directory inside the root filesystem
	homeDir := filepath.Join("home", homeUser)
	homeDirFull := filepath.Join(rootFS, homeDir)
	if err := os.MkdirAll(homeDirFull, 0755); err != nil {
		return fmt.Errorf("create home directory: %w", err)
	}

	// Copy ts binary into container's /sbin using btrfs reflink
	if err := copyTsBinary(rootFS); err != nil {
		return fmt.Errorf("copy ts binary: %w", err)
	}

	// Start control socket server for this container
	sockPath := filepath.Join(rootFS, "thunder.sock")
	log.Printf("Creating control socket at %s", sockPath)
	ctrlServer, err := startControlServer(sockPath)
	if err != nil {
		return fmt.Errorf("start control socket: %w", err)
	}
	defer ctrlServer.Close()
	log.Printf("Control socket created successfully")

	// Start an interactive shell
	ptyReq, winCh, isPty := s.Pty()

	// Launch shell with proc mount - the shell script mounts /proc then execs sh
	cmd := exec.Command("/bin/sh", "-c", "mount -t proc proc /proc 2>/dev/null; exec /bin/sh")
	cmd.Dir = "/" + homeDir
	cmd.Env = []string{
		"HOME=/" + homeDir,
		"USER=" + homeUser,
		"SSH_USER=" + sshUser,
		"TAILSCALE_USER=" + tailscaleUser,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"SHELL=/bin/sh",
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Chroot:     rootFS,
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS,
	}

	// Try to unmount /proc when session ends
	cleanup := func() {
		exec.Command("umount", procDir).Run()
	}

	if isPty {
		cmd.Env = append(cmd.Env, "TERM="+ptyReq.Term)
		ptmx, err := pty.Start(cmd)
		if err != nil {
			return fmt.Errorf("start shell: %w", err)
		}
		defer ptmx.Close()
		defer cleanup()

		// Handle window size changes
		go func() {
			for win := range winCh {
				setWinsize(ptmx, win.Width, win.Height)
			}
		}()

		// Copy data between SSH session and PTY
		go func() {
			io.Copy(ptmx, s) // stdin
		}()
		io.Copy(s, ptmx) // stdout

		cmd.Wait()
		s.Exit(cmd.ProcessState.ExitCode())
	} else {
		// No PTY requested, run without one
		defer cleanup()
		cmd.Stdin = s
		cmd.Stdout = s
		cmd.Stderr = s.Stderr()
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("run shell: %w", err)
		}
		s.Exit(cmd.ProcessState.ExitCode())
	}
	return nil
}

// runVMSession handles a VM-based SSH session using cloud-hypervisor.
func runVMSession(s ssh.Session, tailscaleUser, vmUser string, logErr func(string, ...any)) error {
	// Sanitize usernames for filesystem paths
	safeTailscaleUser := sanitizeForPath(tailscaleUser)
	safeVMUser := sanitizeForPath(vmUser)

	// Set up the root filesystem for this VM (same as container for now)
	homeUser := stripDomain(safeTailscaleUser)
	rootFS := filepath.Join(*flagFsDir, safeTailscaleUser, safeVMUser)
	baseUserFS := filepath.Join(*flagFsDir, safeTailscaleUser, homeUser)
	if err := ensureRootFS(rootFS, baseUserFS); err != nil {
		return fmt.Errorf("set up root filesystem: %w", err)
	}

	// Copy ts binary into VM's /sbin
	// (ts detects vsock via /dev/vsock and connects directly to the host)
	if err := copyTsBinary(rootFS); err != nil {
		return fmt.Errorf("copy ts binary: %w", err)
	}

	// Create control handler for vsock
	controlMux := http.NewServeMux()
	controlMux.HandleFunc("/ping", handlePing)

	// Start the VM - use PTY because SSH sessions don't provide a real TTY
	session, err := thundersnap.StartVM(thundersnap.VMConfig{
		RootFS:         rootFS,
		VMDir:          *flagVmDir,
		Stdin:          s,
		Stdout:         s,
		UsePTY:         true,
		ControlHandler: controlMux,
	})
	if err != nil {
		return fmt.Errorf("start VM: %w", err)
	}

	// Wait for either the VM to exit or the SSH session to close (stdin EOF)
	select {
	case <-session.Done():
		log.Printf("VM exited on its own")
	case <-session.StdinClosed():
		log.Printf("SSH session closed, terminating VM")
		session.Close()
	}

	s.Exit(0)
	return nil
}

// ensureRootFS ensures the root filesystem exists at the given path.
// If it doesn't exist, it clones from baseUserFS if that exists,
// otherwise falls back to /snapshots/1.
func ensureRootFS(rootFS, baseUserFS string) error {
	// Check if the directory already exists
	if _, err := os.Stat(rootFS); err == nil {
		return nil // Already exists
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking rootfs: %w", err)
	}

	// Ensure the parent directory exists
	parentDir := filepath.Dir(rootFS)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		return fmt.Errorf("creating parent directory: %w", err)
	}

	// Determine which snapshot to clone from:
	// 1. If baseUserFS exists and is different from rootFS, use it
	// 2. Otherwise fall back to /snapshots/1
	snapshotSource := "/snapshots/1"
	if baseUserFS != rootFS {
		if _, err := os.Stat(baseUserFS); err == nil {
			snapshotSource = baseUserFS
		}
	}

	// Clone using btrfs subvolume snapshot
	cmd := exec.Command("btrfs", "subvolume", "snapshot", snapshotSource, rootFS)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("btrfs snapshot from %s failed: %w\noutput: %s", snapshotSource, err, string(output))
	}

	return nil
}

// copyTsBinary copies the ts binary into the container's /sbin using btrfs reflink (COW copy).
func copyTsBinary(rootFS string) error {
	// Find the ts binary next to the current executable
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("get executable path: %w", err)
	}
	tsSrc := filepath.Join(filepath.Dir(exe), "ts")

	// Destination in container
	tsDst := filepath.Join(rootFS, "sbin", "ts")

	// Check if source exists
	if _, err := os.Stat(tsSrc); err != nil {
		return fmt.Errorf("ts binary not found at %s: %w", tsSrc, err)
	}

	// Remove existing destination if present (reflink won't overwrite)
	os.Remove(tsDst)

	// Use cp --reflink=always for btrfs COW copy
	cmd := exec.Command("cp", "--reflink=always", tsSrc, tsDst)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cp --reflink=always failed: %w\noutput: %s", err, string(output))
	}

	// Make it executable
	if err := os.Chmod(tsDst, 0755); err != nil {
		return fmt.Errorf("chmod ts binary: %w", err)
	}

	return nil
}

// thunderPort is the vsock port used for the thunder control protocol.
const thunderPort = 5223

// controlServer wraps the HTTP server and listener for cleanup.
type controlServer struct {
	handler  http.Handler
	listener net.Listener
	sockPath string
	done     chan struct{}
}

// Close shuts down the control server and removes the socket file.
func (c *controlServer) Close() error {
	c.listener.Close()
	<-c.done
	os.Remove(c.sockPath)
	return nil
}

// startControlServer starts the HTTP control server on a Unix socket.
// The server expects a vsock-style handshake (CONNECT/OK) before HTTP.
func startControlServer(sockPath string) (*controlServer, error) {
	// Remove existing socket file if it exists
	os.Remove(sockPath)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listen on control socket %s: %w", sockPath, err)
	}

	// Make socket accessible
	if err := os.Chmod(sockPath, 0666); err != nil {
		log.Printf("Warning: failed to chmod control socket: %v", err)
	}

	log.Printf("Control socket listening on %s", sockPath)

	mux := http.NewServeMux()
	mux.HandleFunc("/ping", handlePing)

	cs := &controlServer{
		handler:  mux,
		listener: ln,
		sockPath: sockPath,
		done:     make(chan struct{}),
	}

	go cs.serve()

	return cs, nil
}

// serve accepts connections and handles the vsock handshake before HTTP.
func (c *controlServer) serve() {
	defer close(c.done)
	for {
		conn, err := c.listener.Accept()
		if err != nil {
			return
		}
		go c.handleConn(conn)
	}
}

// handleConn handles a single connection with vsock handshake then HTTP.
func (c *controlServer) handleConn(conn net.Conn) {
	defer conn.Close()

	// Read vsock-style CONNECT handshake
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		log.Printf("control socket: failed to read handshake: %v", err)
		return
	}

	// Parse "CONNECT <port>\n"
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "CONNECT ") {
		log.Printf("control socket: invalid handshake: %s", line)
		fmt.Fprintf(conn, "ERROR invalid handshake\n")
		return
	}
	portStr := strings.TrimPrefix(line, "CONNECT ")
	port, err := strconv.Atoi(portStr)
	if err != nil || port != thunderPort {
		log.Printf("control socket: invalid port: %s", portStr)
		fmt.Fprintf(conn, "ERROR invalid port\n")
		return
	}

	// Send OK response
	fmt.Fprintf(conn, "OK %d\n", port)

	// Now serve HTTP on this connection
	for {
		req, err := http.ReadRequest(reader)
		if err != nil {
			if err != io.EOF {
				log.Printf("control socket: failed to read request: %v", err)
			}
			return
		}

		// Create response writer
		rw := newControlResponseWriter(conn)
		c.handler.ServeHTTP(rw, req)
		if err := rw.finish(); err != nil {
			log.Printf("control socket: failed to write response: %v", err)
			return
		}

		// HTTP/1.0 style: close after one request
		return
	}
}

// controlResponseWriter implements http.ResponseWriter for control socket connections.
type controlResponseWriter struct {
	conn       net.Conn
	headers    http.Header
	statusCode int
	body       []byte
}

func newControlResponseWriter(conn net.Conn) *controlResponseWriter {
	return &controlResponseWriter{
		conn:    conn,
		headers: make(http.Header),
	}
}

func (w *controlResponseWriter) Header() http.Header {
	return w.headers
}

func (w *controlResponseWriter) Write(data []byte) (int, error) {
	w.body = append(w.body, data...)
	return len(data), nil
}

func (w *controlResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
}

func (w *controlResponseWriter) finish() error {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}

	// Write status line
	statusText := http.StatusText(w.statusCode)
	if _, err := fmt.Fprintf(w.conn, "HTTP/1.0 %d %s\r\n", w.statusCode, statusText); err != nil {
		return err
	}

	// Write content-length header
	w.headers.Set("Content-Length", strconv.Itoa(len(w.body)))

	// Write headers
	for key, values := range w.headers {
		for _, value := range values {
			if _, err := fmt.Fprintf(w.conn, "%s: %s\r\n", key, value); err != nil {
				return err
			}
		}
	}

	// End headers
	if _, err := w.conn.Write([]byte("\r\n")); err != nil {
		return err
	}

	// Write body
	if _, err := w.conn.Write(w.body); err != nil {
		return err
	}

	return nil
}

// ControlRequest represents a request to the control socket.
type ControlRequest struct {
	Command string `json:"command"`
}

// ControlResponse represents a response from the control socket.
type ControlResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

func handlePing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ControlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Command != "ping" {
		http.Error(w, "unknown command", http.StatusBadRequest)
		return
	}

	resp := ControlResponse{
		Status:  "ok",
		Message: "pong",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
