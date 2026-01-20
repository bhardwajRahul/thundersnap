// thundersnapd is a Tailscale tsnet-based SSH server that provides
// isolated container environments for each user session.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
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
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/creack/pty"
	"github.com/gliderlabs/ssh"
	"github.com/tailscale/thundersnap/bupdate"
	"github.com/tailscale/thundersnap/thundersnap"
	gossh "golang.org/x/crypto/ssh"
	"tailscale.com/client/tailscale"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
)

var (
	flagFsDir       *string
	flagSnapshotsDir *string
	flagVmDir       *string
	flagMesh        *bool
	flagNfsd        *bool
	flagNfsPort     *int
)

// vmSessionManager tracks running VM sessions and allows multiple clients to share them.
type vmSessionManager struct {
	mu       sync.Mutex
	sessions map[string]*managedVMSession // key: "tailscaleUser/vmUser"
}

// managedVMSession wraps a VM session with reference counting.
type managedVMSession struct {
	session    *thundersnap.VMSession
	vsockPath  string
	refCount   int
	done       chan struct{} // closed when VM exits
	rootFS     string
	tailscaleUser string
	vmUser     string
}

var vmSessions = &vmSessionManager{
	sessions: make(map[string]*managedVMSession),
}

// getOrCreateVM returns an existing VM session or creates a new one.
// The caller must call releaseVM when done.
func (m *vmSessionManager) getOrCreateVM(tailscaleUser, vmUser, rootFS, vmDir string, controlHandler http.Handler) (*managedVMSession, error) {
	key := tailscaleUser + "/" + vmUser

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if session already exists
	if ms, ok := m.sessions[key]; ok {
		// Make sure it's still running
		select {
		case <-ms.done:
			// VM has exited, remove it and create a new one
			delete(m.sessions, key)
		default:
			// VM is still running, increment ref count and update handler
			ms.refCount++
			ms.session.SetControlHandler(controlHandler)
			log.Printf("VM session %s: reusing existing session (refCount=%d)", key, ms.refCount)
			return ms, nil
		}
	}

	// Create new VM session
	log.Printf("VM session %s: starting new VM", key)
	session, err := thundersnap.StartVM(thundersnap.VMConfig{
		RootFS:         rootFS,
		VMDir:          vmDir,
		ControlHandler: controlHandler,
	})
	if err != nil {
		return nil, err
	}

	ms := &managedVMSession{
		session:       session,
		vsockPath:     session.VshSocketPath(),
		refCount:      1,
		done:          make(chan struct{}),
		rootFS:        rootFS,
		tailscaleUser: tailscaleUser,
		vmUser:        vmUser,
	}

	// Monitor VM exit in background
	go func() {
		<-session.Done()
		close(ms.done)
		m.mu.Lock()
		delete(m.sessions, key)
		m.mu.Unlock()
		log.Printf("VM session %s: VM exited, removed from manager", key)
	}()

	m.sessions[key] = ms
	return ms, nil
}

// releaseVM decrements the reference count and shuts down the VM if it reaches zero.
func (m *vmSessionManager) releaseVM(tailscaleUser, vmUser string) {
	key := tailscaleUser + "/" + vmUser

	m.mu.Lock()
	defer m.mu.Unlock()

	ms, ok := m.sessions[key]
	if !ok {
		return
	}

	ms.refCount--
	log.Printf("VM session %s: released (refCount=%d)", key, ms.refCount)

	if ms.refCount <= 0 {
		log.Printf("VM session %s: no more clients, shutting down VM", key)
		ms.session.Close()
		delete(m.sessions, key)
	}
}

func main() {
	hostname := flag.String("hostname", "thundersnap", "Tailscale hostname for this server")
	stateDir := flag.String("state-dir", "", "Directory to store Tailscale state (default: ~/.config/thundersnapd)")
	flagFsDir = flag.String("fs-dir", "", "Directory to store per-user live filesystems (required)")
	flagSnapshotsDir = flag.String("snapshots-dir", "", "Directory to store base snapshots for cloning (required)")
	flagVmDir = flag.String("vm-dir", "", "Directory containing cloud-hypervisor and vmlinux (default: <exe-dir>/vm)")
	flagMesh = flag.Bool("mesh", false, "Enable mesh discovery: ping other thundersnap nodes and serve /bupdate/")
	flagNfsd = flag.Bool("nfsd", false, "Enable NFSv4 server to export -snapshots-dir")
	flagNfsPort = flag.Int("nfs-port", 2049, "Port for NFSv4 server (default: 2049)")
	flag.Parse()

	if *flagFsDir == "" {
		log.Fatalf("-fs-dir is required")
	}
	if *flagSnapshotsDir == "" {
		log.Fatalf("-snapshots-dir is required")
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

	// Ensure SSH host key exists
	hostKeyPath := filepath.Join(*stateDir, "ssh_host_ed25519_key")
	if err := ensureHostKey(hostKeyPath); err != nil {
		log.Fatalf("Failed to ensure host key: %v", err)
	}

	// Get the LocalClient to look up peer info
	lc, err := srv.LocalClient()
	if err != nil {
		log.Fatalf("Failed to get LocalClient: %v", err)
	}

	// Create SSH server with gliderlabs/ssh and persistent host key
	forwardHandler := &ssh.ForwardedTCPHandler{}
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

			// Parse SSH username: can be "<container>" or "<user>@<container>"
			// If user@ prefix is present, use that specific user
			// Otherwise, auto-detect from [ubuntu, user] or fall back to root
			sshUser := s.User()
			targetUser := "" // empty means auto-detect
			if idx := strings.Index(sshUser, "@"); idx != -1 {
				targetUser = sshUser[:idx]
				sshUser = sshUser[idx+1:]
			}

			// Check if this is a VM session (vm/<user>)
			if strings.HasPrefix(sshUser, "vm/") {
				vmUser := strings.TrimPrefix(sshUser, "vm/")
				if err := runVMSession(s, tailscaleUser, vmUser, targetUser, logErr); err != nil {
					logErr("VM session failed: %v", err)
					s.Exit(1)
				}
				return
			}

			// Container session
			if err := runContainerSession(s, tailscaleUser, sshUser, targetUser, logErr); err != nil {
				logErr("Container session failed: %v", err)
				s.Exit(1)
			}
		},
		// No authentication required - Tailscale already authenticated the connection.
		// When both PasswordHandler and PublicKeyHandler are nil, gliderlabs/ssh
		// performs no client authentication.

		// Enable port forwarding
		LocalPortForwardingCallback: ssh.LocalPortForwardingCallback(func(ctx ssh.Context, dhost string, dport uint32) bool {
			log.Printf("Accepted local forward to %s:%d", dhost, dport)
			return true
		}),
		ReversePortForwardingCallback: ssh.ReversePortForwardingCallback(func(ctx ssh.Context, host string, port uint32) bool {
			log.Printf("Accepted reverse forward on %s:%d", host, port)
			return true
		}),
		RequestHandlers: map[string]ssh.RequestHandler{
			"tcpip-forward":        forwardHandler.HandleSSHRequest,
			"cancel-tcpip-forward": forwardHandler.HandleSSHRequest,
		},
	}

	// Load the persistent host key
	if err := ssh.HostKeyFile(hostKeyPath)(sshServer); err != nil {
		log.Fatalf("Failed to load host key: %v", err)
	}

	// Start HTTP server on port 7575 for mesh discovery and bupdate
	httpLn, err := srv.Listen("tcp", ":7575")
	if err != nil {
		log.Fatalf("Failed to listen on :7575: %v", err)
	}
	defer httpLn.Close()

	// Get our own FQDN for mesh pings
	status, err = srv.Up(context.Background())
	if err != nil {
		log.Fatalf("Failed to get status: %v", err)
	}
	myFQDN := ""
	if status.Self != nil && status.Self.DNSName != "" {
		myFQDN = strings.TrimSuffix(status.Self.DNSName, ".")
	}

	meshState := newMeshState(myFQDN)
	httpMux := http.NewServeMux()

	// Mesh discovery endpoint
	httpMux.HandleFunc("/ts/ping", meshState.handleTsPing)

	// List of known servers (JSON)
	httpMux.HandleFunc("/ts/servers.json", meshState.handleServersJSON)

	// Web UI showing connected hosts
	httpMux.HandleFunc("/", meshState.handleIndex)

	// File server for bupdate (serves -snapshots-dir contents)
	bupdateServer := &bupdateFileServer{root: *flagSnapshotsDir}
	httpMux.Handle("/bupdate/", http.StripPrefix("/bupdate", bupdateServer))

	httpServer := &http.Server{Handler: httpMux}
	go func() {
		log.Printf("HTTP server listening on port 7575")
		if err := httpServer.Serve(httpLn); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	// Start mesh ping loop if enabled
	if *flagMesh {
		go meshState.pingLoop(context.Background(), srv, lc)
	}

	// Start NFSv3 server if enabled
	if *flagNfsd {
		// Start portmapper on port 111 so clients can discover NFS/MOUNT ports
		pmLn, err := srv.Listen("tcp", ":111")
		if err != nil {
			log.Fatalf("Failed to listen on portmapper port 111: %v", err)
		}
		defer pmLn.Close()

		// Also listen on UDP for portmapper (required by some clients)
		pmUDP, err := srv.ListenPacket("udp", ":111")
		if err != nil {
			log.Fatalf("Failed to listen on UDP portmapper: %v", err)
		}
		defer pmUDP.Close()
		log.Printf("UDP portmapper listening on %v", pmUDP.LocalAddr())

		startPortmapper(pmLn, pmUDP, *flagNfsPort)

		// Start NFS server
		nfsLn, err := srv.Listen("tcp", fmt.Sprintf(":%d", *flagNfsPort))
		if err != nil {
			log.Fatalf("Failed to listen on NFS port %d: %v", *flagNfsPort, err)
		}
		defer nfsLn.Close()

		nfsSrv, err := startNFSServer(*flagSnapshotsDir, nfsLn)
		if err != nil {
			log.Fatalf("Failed to start NFS server: %v", err)
		}
		go func() {
			log.Printf("NFSv3 server listening on tsnet port %d", *flagNfsPort)
			if err := nfsSrv.Serve(); err != nil {
				log.Printf("NFS server error: %v", err)
			}
		}()
	}

	log.Printf("Waiting for SSH connections...")

	// Serve SSH connections
	if err := sshServer.Serve(ln); err != nil {
		log.Fatalf("SSH server error: %v", err)
	}
}

// ensureHostKey ensures an SSH host key exists at the given path.
// If the file doesn't exist, generates a new ED25519 key pair and saves it.
func ensureHostKey(keyPath string) error {
	// Check if key already exists
	if _, err := os.Stat(keyPath); err == nil {
		log.Printf("Using existing SSH host key: %s", keyPath)
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking host key: %w", err)
	}

	// Generate new ED25519 key pair
	log.Printf("Generating new SSH host key: %s", keyPath)
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generating ED25519 key: %w", err)
	}

	// Marshal the private key to OpenSSH PEM format
	pemBlock, err := gossh.MarshalPrivateKey(privateKey, "")
	if err != nil {
		return fmt.Errorf("marshaling private key: %w", err)
	}

	// Write key file with restricted permissions
	keyFile, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("creating host key file: %w", err)
	}
	defer keyFile.Close()

	if err := pem.Encode(keyFile, pemBlock); err != nil {
		return fmt.Errorf("writing host key: %w", err)
	}

	// Create signer to get fingerprint for logging
	sshPrivateKey, err := gossh.NewSignerFromKey(privateKey)
	if err != nil {
		return fmt.Errorf("creating SSH signer: %w", err)
	}

	log.Printf("SSH host key generated successfully (fingerprint: %s)", gossh.FingerprintSHA256(sshPrivateKey.PublicKey()))
	return nil
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

// selectTargetUser determines which Unix user to run as in a container/VM.
// If targetUser is non-empty, it's used directly (caller specified it).
// Otherwise, auto-detect by checking if /home/<user> exists for each candidate
// in order: [ubuntu, user]. If none exist, fall back to root.
func selectTargetUser(rootFS, targetUser string) string {
	if targetUser != "" {
		return targetUser
	}
	// Auto-detect: check candidates in order
	for _, candidate := range []string{"ubuntu", "user"} {
		homeDir := filepath.Join(rootFS, "home", candidate)
		if info, err := os.Stat(homeDir); err == nil && info.IsDir() {
			return candidate
		}
	}
	return "root"
}

// runContainerSession handles a container-based SSH session.
// targetUser specifies the Unix user to run as. If empty, auto-detect from
// [ubuntu, user] based on which /home/<user> exists, or fall back to root.
func runContainerSession(s ssh.Session, tailscaleUser, sshUser, targetUser string, logErr func(string, ...any)) error {
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

	// Copy ts binary into container's /sbin using btrfs reflink
	if err := copyTsBinary(rootFS); err != nil {
		return fmt.Errorf("copy ts binary: %w", err)
	}

	// Start control socket server for this container
	sockPath := filepath.Join(rootFS, "thunder.sock")
	log.Printf("Creating control socket at %s", sockPath)
	ctrlServer, err := startControlServer(sockPath, rootFS)
	if err != nil {
		return fmt.Errorf("start control socket: %w", err)
	}
	defer ctrlServer.Close()
	log.Printf("Control socket created successfully")

	// Check if a command was requested
	ptyReq, winCh, isPty := s.Pty()
	cmdArgs := s.Command()

	// Determine which Unix user to run as
	runAsUser := selectTargetUser(rootFS, targetUser)
	log.Printf("Container session: running as user %q (requested: %q)", runAsUser, targetUser)

	// Prepare the command to execute using su to switch to the target user.
	// For interactive sessions (no command): su - <user> (login shell)
	// For command execution: su <user> -c '<command>' (non-login shell)
	var cmd *exec.Cmd
	if len(cmdArgs) > 0 {
		// Execute the requested command as the target user (non-login shell)
		// Mount /proc first, then use su to run the command
		// We pass each arg through sh to handle quoting properly
		quotedArgs := make([]string, len(cmdArgs))
		for i, arg := range cmdArgs {
			quotedArgs[i] = "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
		}
		suCmd := fmt.Sprintf("su %s -c %s", runAsUser, strings.Join(quotedArgs, " "))
		cmd = exec.Command("/bin/sh", "-c", "mount -t proc proc /proc 2>/dev/null; exec "+suCmd)
	} else {
		// Launch interactive login shell as the target user
		cmd = exec.Command("/bin/sh", "-c", "mount -t proc proc /proc 2>/dev/null; exec su - "+runAsUser)
	}

	cmd.Dir = "/"
	cmd.Env = []string{
		"SSH_USER=" + sshUser,
		"TAILSCALE_USER=" + tailscaleUser,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
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

// runVMSession handles a VM-based SSH session using cloud-hypervisor.
// Multiple SSH connections to the same VM (same tailscaleUser + vmUser) share
// the same VM instance. The VM is only shut down when all clients disconnect.
// targetUser specifies the Unix user to run as. If empty, auto-detect from
// [ubuntu, user] based on which /home/<user> exists, or fall back to root.
func runVMSession(s ssh.Session, tailscaleUser, vmUser, targetUser string, logErr func(string, ...any)) error {
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

	// Copy vshd binary into VM's /sbin for shell access via vsock
	if err := copyVshdBinary(rootFS); err != nil {
		return fmt.Errorf("copy vshd binary: %w", err)
	}

	// Create control handler for vsock
	controlMux := http.NewServeMux()
	controlMux.HandleFunc("/ping", handlePing)
	// Create snap handler with closure over rootFS
	controlMux.HandleFunc("/snap", makeSnapHandler(rootFS))

	// Get or create VM session (reuses existing VM if one is running)
	ms, err := vmSessions.getOrCreateVM(safeTailscaleUser, safeVMUser, rootFS, *flagVmDir, controlMux)
	if err != nil {
		return fmt.Errorf("start VM: %w", err)
	}
	// Release our reference when done (may shut down VM if we're the last client)
	defer vmSessions.releaseVM(safeTailscaleUser, safeVMUser)

	// Connect to vshd in the VM via vsock
	conn, err := connectToVshd(ms.vsockPath)
	if err != nil {
		return fmt.Errorf("connect to vshd: %w", err)
	}
	defer conn.Close()

	// Send command protocol to vshd:
	// First: target username terminated by \0 (empty = auto-detect)
	// Then: argument count terminated by \0 (0 = interactive shell)
	// Then: each argument terminated by \0
	cmdArgs := s.Command()
	fmt.Fprintf(conn, "%s\x00", targetUser)
	fmt.Fprintf(conn, "%d\x00", len(cmdArgs))
	for _, arg := range cmdArgs {
		fmt.Fprintf(conn, "%s\x00", arg)
	}

	// Proxy the SSH session to vshd
	done := make(chan struct{})

	// SSH stdin -> vshd
	go func() {
		io.Copy(conn, s)
		// When SSH session closes, close our write side to vshd
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	// vshd -> SSH stdout
	go func() {
		io.Copy(s, conn)
		close(done)
	}()

	// Wait for either:
	// - vshd connection to close (shell exited normally)
	// - VM to exit unexpectedly
	// - SSH session to be closed by client (e.g., ~. escape sequence)
	select {
	case <-done:
		log.Printf("vshd connection closed")
	case <-ms.done:
		log.Printf("VM exited")
	case <-s.Context().Done():
		log.Printf("SSH session closed by client")
	}

	s.Exit(0)
	return nil
}

// connectToVshd connects to vshd in a VM via the vsock socket.
// It performs the cloud-hypervisor vsock CONNECT handshake and retries
// until vshd is ready (up to 10 seconds).
func connectToVshd(vsockPath string) (net.Conn, error) {
	var lastErr error

	// Retry the full connection + handshake for up to 10 seconds while vshd starts up
	for i := 0; i < 100; i++ {
		conn, err := tryConnectToVshd(vsockPath)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
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

// readStampFile reads the snapshot ID from a .stamp file.
// Returns empty string if file doesn't exist.
func readStampFile(path string) string {
	data, err := os.ReadFile(path + ".stamp")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// writeStampFile writes a snapshot ID to a .stamp file atomically.
func writeStampFile(path, snapshotID string) error {
	stampPath := path + ".stamp"
	tmpPath := stampPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(snapshotID+"\n"), 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, stampPath)
}

// ensureRootFS ensures the root filesystem exists at the given path.
// If it doesn't exist, it first creates an intermediate snapshot in snapshots-dir,
// then clones from that to the destination. This ensures snapshots-dir contains
// stable reference points while fs-dir contains the live, changing filesystems.
//
// The snapshotting flow:
// 1. Determine source: baseUserFS (if exists) or $snapshots-dir/1
// 2. Create intermediate snapshot in $snapshots-dir with random hex ID
// 3. Clone from intermediate snapshot to rootFS in $fs-dir
// 4. Create .stamp files tracking the base snapshot ID
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

	// Determine which source to clone from and what stamp ID to use:
	// 1. If baseUserFS exists and is different from rootFS, use it
	//    (inherit the stamp from the source's .stamp file)
	// 2. Otherwise fall back to $snapshots-dir/1 (stamp ID = "1")
	defaultSnapshot := filepath.Join(*flagSnapshotsDir, "1")
	snapshotSource := defaultSnapshot
	baseStampID := "1" // default base is "1"

	if baseUserFS != rootFS {
		if _, err := os.Stat(baseUserFS); err == nil {
			snapshotSource = baseUserFS
			// Inherit stamp from the source (fs-dir has .stamp files)
			if stamp := readStampFile(baseUserFS); stamp != "" {
				baseStampID = stamp
			}
		}
	}

	// Generate random ID for the intermediate snapshot
	intermediateID, err := generateRandomID()
	if err != nil {
		return fmt.Errorf("generating snapshot ID: %w", err)
	}
	intermediatePath := filepath.Join(*flagSnapshotsDir, intermediateID)

	// Step 1: Create intermediate snapshot in snapshots-dir with fidx
	// (no progress reporting for ensureRootFS - happens at SSH login time)
	if err := createSnapshotWithFidx(snapshotSource, intermediateID, baseStampID, nil, false); err != nil {
		return fmt.Errorf("create intermediate snapshot: %w", err)
	}

	// Step 2: Clone from intermediate snapshot to rootFS
	cmd := exec.Command("btrfs", "subvolume", "snapshot", intermediatePath, rootFS)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("btrfs snapshot from %s to %s failed: %w\noutput: %s",
			intermediatePath, rootFS, err, string(output))
	}

	// Write stamp file for the live filesystem
	// For fs-dir snapshots, the stamp contains the intermediate snapshot ID
	// (which is the basename of the snapshot in snapshots-dir)
	if err := writeStampFile(rootFS, intermediateID); err != nil {
		log.Printf("Warning: failed to write stamp file for %s: %v", rootFS, err)
	}

	return nil
}

// copyTsBinary copies the ts binary into the container's /sbin using btrfs reflink (COW copy).
func copyTsBinary(rootFS string) error {
	return copyBinaryToRootFS(rootFS, "ts", "sbin/ts")
}

// copyVshdBinary copies the vshd binary into the VM's /sbin using btrfs reflink (COW copy).
func copyVshdBinary(rootFS string) error {
	return copyBinaryToRootFS(rootFS, "vshd", "sbin/vshd")
}

// copyBinaryToRootFS copies a binary from the executable directory into the rootfs.
func copyBinaryToRootFS(rootFS, binaryName, destPath string) error {
	// Find the binary next to the current executable
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("get executable path: %w", err)
	}
	src := filepath.Join(filepath.Dir(exe), binaryName)

	// Destination in rootfs
	dst := filepath.Join(rootFS, destPath)

	// Check if source exists
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("%s binary not found at %s: %w", binaryName, src, err)
	}

	// Ensure destination directory exists
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("create destination directory: %w", err)
	}

	// Remove existing destination if present (reflink won't overwrite)
	os.Remove(dst)

	// Use cp --reflink=always for btrfs COW copy
	cmd := exec.Command("cp", "--reflink=always", src, dst)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cp --reflink=always failed: %w\noutput: %s", err, string(output))
	}

	// Make it executable
	if err := os.Chmod(dst, 0755); err != nil {
		return fmt.Errorf("chmod %s binary: %w", binaryName, err)
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
	rootFS   string // the rootFS this control server is associated with
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
func startControlServer(sockPath, rootFS string) (*controlServer, error) {
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

	cs := &controlServer{
		listener: ln,
		sockPath: sockPath,
		rootFS:   rootFS,
		done:     make(chan struct{}),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ping", handlePing)
	mux.HandleFunc("/snap", cs.handleSnap)
	cs.handler = mux

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
		log.Printf("control socket: handling %s %s", req.Method, req.URL.Path)
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
	conn          net.Conn
	headers       http.Header
	statusCode    int
	body          []byte
	headerWritten bool
	streaming     bool // if true, writes go directly to conn
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
	if w.streaming {
		// In streaming mode, write headers on first write, then write directly
		if !w.headerWritten {
			if err := w.writeHeaders(); err != nil {
				return 0, err
			}
		}
		return w.conn.Write(data)
	}
	// Buffered mode: accumulate in body
	w.body = append(w.body, data...)
	return len(data), nil
}

func (w *controlResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
}

// Flush implements http.Flusher for streaming responses
func (w *controlResponseWriter) Flush() {
	// Enable streaming mode on first flush
	if !w.streaming {
		w.streaming = true
	}
	// For net.Conn, writes are typically unbuffered, so nothing extra to do
}

// writeHeaders writes the HTTP status and headers to the connection
func (w *controlResponseWriter) writeHeaders() error {
	if w.headerWritten {
		return nil
	}
	w.headerWritten = true

	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}

	// Write status line
	statusText := http.StatusText(w.statusCode)
	if _, err := fmt.Fprintf(w.conn, "HTTP/1.0 %d %s\r\n", w.statusCode, statusText); err != nil {
		return err
	}

	// For streaming, use chunked-style (no content-length)
	// Write headers (skip Content-Length for streaming)
	for key, values := range w.headers {
		if w.streaming && key == "Content-Length" {
			continue
		}
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

	return nil
}

func (w *controlResponseWriter) finish() error {
	if w.streaming {
		// Already wrote everything directly
		return nil
	}

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

// SnapResponse is the response from the /snap endpoint
type SnapResponse struct {
	Status     string `json:"status"`
	SnapshotID string `json:"snapshot_id,omitempty"`
	Message    string `json:"message,omitempty"`
}

// SnapStreamEvent is a single event in the streaming snap response (NDJSON format).
// Type is "progress" for intermediate progress or "result" for the final result.
type SnapStreamEvent struct {
	Type       string `json:"type"`                  // "progress" or "result"
	Message    string `json:"message,omitempty"`     // progress message
	Status     string `json:"status,omitempty"`      // "ok" or "error" (for result)
	SnapshotID string `json:"snapshot_id,omitempty"` // snapshot ID (for result)
}

// snapProgressWriter wraps an http.ResponseWriter to write progress events
type snapProgressWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	encoder *json.Encoder
}

func newSnapProgressWriter(w http.ResponseWriter) *snapProgressWriter {
	pw := &snapProgressWriter{
		w:       w,
		encoder: json.NewEncoder(w),
	}
	if f, ok := w.(http.Flusher); ok {
		pw.flusher = f
	}
	return pw
}

func (pw *snapProgressWriter) Write(p []byte) (n int, err error) {
	// Each write from the progress tracker is a line of progress text
	msg := strings.TrimSpace(string(p))
	if msg == "" {
		return len(p), nil
	}
	event := SnapStreamEvent{
		Type:    "progress",
		Message: msg,
	}
	if err := pw.encoder.Encode(event); err != nil {
		return 0, err
	}
	if pw.flusher != nil {
		pw.flusher.Flush()
	}
	return len(p), nil
}

// makeSnapHandler creates a /snap handler for the given rootFS.
// This is used by both container (controlServer) and VM handlers.
func makeSnapHandler(rootFS string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Check if client wants streaming progress
		stream := r.URL.Query().Get("stream") == "1"
		isTTY := r.URL.Query().Get("tty") == "1"

		if stream {
			handleSnapStreaming(w, rootFS, isTTY)
			return
		}

		// Non-streaming: original behavior
		snapshotID, err := createSnapshot(rootFS, nil, false)
		if err != nil {
			log.Printf("snap failed for %s: %v", rootFS, err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(SnapResponse{
				Status:  "error",
				Message: err.Error(),
			})
			return
		}

		log.Printf("Created snapshot %s from %s", snapshotID, rootFS)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(SnapResponse{
			Status:     "ok",
			SnapshotID: snapshotID,
		})
	}
}

// handleSnapStreaming handles the streaming version of /snap
func handleSnapStreaming(w http.ResponseWriter, rootFS string, isTTY bool) {
	w.Header().Set("Content-Type", "application/x-ndjson")

	// Enable streaming mode immediately by flushing
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	pw := newSnapProgressWriter(w)
	encoder := json.NewEncoder(w)

	snapshotID, err := createSnapshot(rootFS, pw, isTTY)
	if err != nil {
		log.Printf("snap failed for %s: %v", rootFS, err)
		encoder.Encode(SnapStreamEvent{
			Type:    "result",
			Status:  "error",
			Message: err.Error(),
		})
		return
	}

	log.Printf("Created snapshot %s from %s", snapshotID, rootFS)
	encoder.Encode(SnapStreamEvent{
		Type:       "result",
		Status:     "ok",
		SnapshotID: snapshotID,
	})
}

// handleSnap handles POST /snap - create a snapshot of the container's rootFS
func (c *controlServer) handleSnap(w http.ResponseWriter, r *http.Request) {
	makeSnapHandler(c.rootFS)(w, r)
}

// createSnapshot creates a read-only snapshot of the given rootFS in snapshots-dir.
// Returns the new snapshot ID (random hex string).
// If progressWriter is non-nil, progress updates are written to it.
func createSnapshot(rootFS string, progressWriter io.Writer, isTTY bool) (string, error) {
	// Read the base stamp from the source rootFS to find parent snapshot
	baseStampID := readStampFile(rootFS)
	if baseStampID == "" {
		baseStampID = "1" // default
	}

	// Generate random ID for the snapshot
	snapshotID, err := generateRandomID()
	if err != nil {
		return "", fmt.Errorf("generating snapshot ID: %w", err)
	}

	// Create snapshot with fidx
	if err := createSnapshotWithFidx(rootFS, snapshotID, baseStampID, progressWriter, isTTY); err != nil {
		return "", err
	}

	// Update the source rootFS's stamp to point to the new snapshot
	// This makes future snapshots use the new snapshot as their parent for faster fidx
	if err := writeStampFile(rootFS, snapshotID); err != nil {
		log.Printf("Warning: failed to update stamp file for %s: %v", rootFS, err)
	}

	return snapshotID, nil
}

// createSnapshotWithFidx creates a read-only snapshot in snapshots-dir and generates
// fidx files for it. The process is:
// 1. Create btrfs snapshot to $snapshotID.tmp
// 2. Create mfidx $snapshotID.tmp.fidx (with --ref to parent if exists)
// 3. Create fidx of the fidx: $snapshotID.tmp.fidx.fidx
// 4. Rename all three to final names atomically
func createSnapshotWithFidx(source, snapshotID, parentStampID string, progressWriter io.Writer, isTTY bool) error {
	tmpPath := filepath.Join(*flagSnapshotsDir, snapshotID+".tmp")
	tmpFidxPath := tmpPath + ".fidx"
	tmpFidxFidxPath := tmpFidxPath + ".fidx"

	finalPath := filepath.Join(*flagSnapshotsDir, snapshotID)
	finalFidxPath := finalPath + ".fidx"
	finalFidxFidxPath := finalFidxPath + ".fidx"

	// Step 1: Create read-only btrfs snapshot to tmp path
	cmd := exec.Command("btrfs", "subvolume", "snapshot", "-r", source, tmpPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("btrfs snapshot failed: %w\noutput: %s", err, string(output))
	}

	// Write stamp file for the snapshot (in tmp location)
	if err := writeStampFile(tmpPath, parentStampID); err != nil {
		// Clean up on error
		exec.Command("btrfs", "subvolume", "delete", tmpPath).Run()
		os.Remove(tmpPath + ".stamp")
		return fmt.Errorf("write stamp file: %w", err)
	}

	// Step 2: Create mfidx for the snapshot
	// Check if parent snapshot exists and has a fidx we can use as reference
	var refPath string
	if parentStampID != "" {
		parentFidxPath := filepath.Join(*flagSnapshotsDir, parentStampID+".fidx")
		if _, err := os.Stat(parentFidxPath); err == nil {
			refPath = parentFidxPath
		}
	}

	log.Printf("Creating fidx for snapshot %s (ref: %s)", snapshotID, refPath)
	fidxOpts := bupdate.IndexerOptions{
		RefPath:        refPath,
		Progress:       false, // don't use stderr directly
		ProgressWriter: progressWriter,
		IsTTY:          isTTY,
	}
	if err := bupdate.CreateFidx(tmpPath, tmpFidxPath, fidxOpts); err != nil {
		// Clean up on error
		exec.Command("btrfs", "subvolume", "delete", tmpPath).Run()
		os.Remove(tmpPath + ".stamp")
		return fmt.Errorf("create fidx: %w", err)
	}

	// Step 3: Create fidx of the fidx (single file fidx)
	fidxFidxOpts := bupdate.IndexerOptions{
		Progress: false,
	}
	if err := bupdate.CreateSingleFidx(tmpFidxPath, tmpFidxFidxPath, fidxFidxOpts); err != nil {
		// Clean up on error
		exec.Command("btrfs", "subvolume", "delete", tmpPath).Run()
		os.Remove(tmpPath + ".stamp")
		os.Remove(tmpFidxPath)
		return fmt.Errorf("create fidx.fidx: %w", err)
	}

	// Step 4: Rename all to final names (order matters for consistency)
	// First the directory, then fidx, then fidx.fidx
	if err := os.Rename(tmpPath, finalPath); err != nil {
		// Clean up on error
		exec.Command("btrfs", "subvolume", "delete", tmpPath).Run()
		os.Remove(tmpPath + ".stamp")
		os.Remove(tmpFidxPath)
		os.Remove(tmpFidxFidxPath)
		return fmt.Errorf("rename snapshot: %w", err)
	}
	// Also rename the stamp file
	os.Rename(tmpPath+".stamp", finalPath+".stamp")

	if err := os.Rename(tmpFidxPath, finalFidxPath); err != nil {
		// Snapshot is already renamed, just log the error
		log.Printf("Warning: failed to rename fidx: %v", err)
	}

	if err := os.Rename(tmpFidxFidxPath, finalFidxFidxPath); err != nil {
		// Snapshot and fidx are already renamed, just log the error
		log.Printf("Warning: failed to rename fidx.fidx: %v", err)
	}

	log.Printf("Created snapshot %s with fidx", snapshotID)
	return nil
}

// meshPort is the HTTP port for mesh discovery (TSTS in leetspeak = 7575)
const meshPort = 7575

// MeshPing is the JSON format for /ts/ping requests and responses
type MeshPing struct {
	URL      string `json:"url"`      // Full URL including tsnet FQDN, e.g., "http://host.xxx.ts.net:7575"
	Hostname string `json:"hostname"` // Just the hostname part
}

// meshPeer tracks a peer that has successfully pinged or been pinged
type meshPeer struct {
	URL      string    `json:"url"`
	Hostname string    `json:"hostname"`
	LastSeen time.Time `json:"last_seen"`
}

// meshState tracks mesh discovery state
type meshState struct {
	mu      sync.Mutex
	myURL   string
	myFQDN  string
	peers   map[string]*meshPeer // keyed by hostname
}

func newMeshState(myFQDN string) *meshState {
	myURL := ""
	if myFQDN != "" {
		myURL = fmt.Sprintf("http://%s:%d", myFQDN, meshPort)
	}
	return &meshState{
		myURL:  myURL,
		myFQDN: myFQDN,
		peers:  make(map[string]*meshPeer),
	}
}

// recordPeer records or updates a peer that has been seen
func (m *meshState) recordPeer(ping MeshPing) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.peers[ping.Hostname] = &meshPeer{
		URL:      ping.URL,
		Hostname: ping.Hostname,
		LastSeen: time.Now(),
	}
}

// getPeers returns a list of all known peers
func (m *meshState) getPeers() []meshPeer {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make([]meshPeer, 0, len(m.peers))
	for _, p := range m.peers {
		result = append(result, *p)
	}
	return result
}

// handleTsPing handles POST /ts/ping - receive a ping from another node
func (m *meshState) handleTsPing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var ping MeshPing
	if err := json.NewDecoder(r.Body).Decode(&ping); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if ping.URL == "" || ping.Hostname == "" {
		http.Error(w, "url and hostname required", http.StatusBadRequest)
		return
	}

	// Record this peer
	m.recordPeer(ping)
	log.Printf("Mesh ping received from %s (%s)", ping.Hostname, ping.URL)

	// Respond with our own info
	resp := MeshPing{
		URL:      m.myURL,
		Hostname: m.myFQDN,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleServersJSON handles GET /ts/servers.json - list known peers
func (m *meshState) handleServersJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	peers := m.getPeers()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(peers)
}

// handleIndex handles GET / - show web UI
func (m *meshState) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	peers := m.getPeers()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head>
<title>Thundersnap Mesh</title>
<style>
body { font-family: sans-serif; margin: 2em; }
table { border-collapse: collapse; }
th, td { border: 1px solid #ccc; padding: 0.5em 1em; text-align: left; }
th { background: #f0f0f0; }
.stale { color: #999; }
</style>
</head>
<body>
<h1>Thundersnap Mesh</h1>
<p>My URL: <code>%s</code></p>
<h2>Known Peers (%d)</h2>
`, m.myURL, len(peers))

	if len(peers) == 0 {
		fmt.Fprintf(w, "<p>No peers discovered yet.</p>")
	} else {
		fmt.Fprintf(w, `<table>
<tr><th>Hostname</th><th>URL</th><th>Last Seen</th></tr>
`)
		for _, p := range peers {
			age := time.Since(p.LastSeen)
			class := ""
			if age > 2*time.Minute {
				class = ` class="stale"`
			}
			fmt.Fprintf(w, `<tr%s><td>%s</td><td><a href="%s">%s</a></td><td>%s ago</td></tr>
`, class, p.Hostname, p.URL, p.URL, age.Round(time.Second))
		}
		fmt.Fprintf(w, "</table>\n")
	}

	fmt.Fprintf(w, `<p><a href="/ts/servers.json">JSON API</a></p>
</body>
</html>
`)
}

// pingLoop runs the mesh discovery loop
func (m *meshState) pingLoop(ctx context.Context, srv *tsnet.Server, lc *tailscale.LocalClient) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// Create an HTTP client that uses tsnet for dialing (not the host's network)
	tsClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return srv.Dial(ctx, network, addr)
			},
		},
		Timeout: 10 * time.Second,
	}

	// Run immediately, then on ticker
	m.pingAllPeers(ctx, lc, tsClient)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.pingAllPeers(ctx, lc, tsClient)
		}
	}
}

// pingAllPeers discovers all Tailscale nodes and pings them
func (m *meshState) pingAllPeers(ctx context.Context, lc *tailscale.LocalClient, tsClient *http.Client) {
	status, err := lc.Status(ctx)
	if err != nil {
		log.Printf("Mesh: failed to get tailscale status: %v", err)
		return
	}

	// Get our own tags and user ID
	var myTags []string
	var myUserID tailcfg.UserID
	if status.Self != nil {
		if status.Self.Tags != nil {
			myTags = status.Self.Tags.AsSlice()
		}
		myUserID = status.Self.UserID
	}

	// Build our ping message
	ping := MeshPing{
		URL:      m.myURL,
		Hostname: m.myFQDN,
	}
	pingBody, _ := json.Marshal(ping)

	// Ping peers that match our tags or user
	for _, peer := range status.Peer {
		if peer.DNSName == "" {
			continue
		}
		// Skip ourselves
		fqdn := strings.TrimSuffix(peer.DNSName, ".")
		if fqdn == m.myFQDN {
			continue
		}

		// Filter: only ping peers that are in our "mesh group"
		if !shouldPingPeer(myTags, myUserID, peer) {
			continue
		}

		go m.pingPeer(ctx, fqdn, pingBody, tsClient)
	}
}

// shouldPingPeer returns true if the peer should be pinged based on tag/user matching.
// If we are tagged: peer must share at least one tag with us.
// If we are not tagged: peer must have the same user ID and no tags.
func shouldPingPeer(myTags []string, myUserID tailcfg.UserID, peer *ipnstate.PeerStatus) bool {
	var peerTags []string
	if peer.Tags != nil {
		peerTags = peer.Tags.AsSlice()
	}

	if len(myTags) > 0 {
		// We are tagged: peer must share at least one tag
		for _, myTag := range myTags {
			for _, peerTag := range peerTags {
				if myTag == peerTag {
					return true
				}
			}
		}
		return false
	}

	// We are not tagged: peer must have same user and no tags
	return len(peerTags) == 0 && peer.UserID == myUserID
}

// pingPeer sends a ping to a single peer using the tsnet HTTP client
func (m *meshState) pingPeer(ctx context.Context, fqdn string, pingBody []byte, tsClient *http.Client) {
	url := fmt.Sprintf("http://%s:%d/ts/ping", fqdn, meshPort)
	log.Printf("Mesh: pinging %s", fqdn)

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(pingBody))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := tsClient.Do(req)
	if err != nil {
		// Peer might not be running thundersnapd, that's fine
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return
	}

	var peerPing MeshPing
	if err := json.NewDecoder(resp.Body).Decode(&peerPing); err != nil {
		return
	}

	if peerPing.URL != "" && peerPing.Hostname != "" {
		m.recordPeer(peerPing)
		log.Printf("Mesh ping successful: %s", peerPing.Hostname)
	}
}

// bupdateFileServer serves files from -snapshots-dir with range request support
type bupdateFileServer struct {
	root string
}

func (fs *bupdateFileServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Clean the path to prevent directory traversal
	cleanPath := filepath.Clean(r.URL.Path)
	if strings.HasPrefix(cleanPath, "..") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	fullPath := filepath.Join(fs.root, cleanPath)

	// Ensure the path is within root
	if !strings.HasPrefix(fullPath, fs.root) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	// Use Lstat to check file type without following symlinks
	stat, err := os.Lstat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
		} else {
			http.Error(w, "error stating file", http.StatusInternalServerError)
		}
		return
	}

	mode := stat.Mode()

	// Only allow regular files and symlinks
	if !mode.IsRegular() && mode&os.ModeSymlink == 0 {
		http.Error(w, "not a regular file or symlink", http.StatusForbidden)
		return
	}

	// Handle symlinks: return the readlink() result as content
	if mode&os.ModeSymlink != 0 {
		target, err := os.Readlink(fullPath)
		if err != nil {
			http.Error(w, "error reading symlink", http.StatusInternalServerError)
			return
		}

		content := []byte(target)
		fileSize := int64(len(content))

		rangeHeader := r.Header.Get("Range")
		if rangeHeader == "" {
			w.Header().Set("Content-Length", strconv.FormatInt(fileSize, 10))
			w.Header().Set("Accept-Ranges", "bytes")
			w.WriteHeader(http.StatusOK)
			w.Write(content)
			return
		}

		// Handle range request for symlink content
		start, end, err := parseRangeHeader(rangeHeader, fileSize)
		if err != nil {
			http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}

		contentLength := end - start + 1
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, fileSize))
		w.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusPartialContent)
		w.Write(content[start : end+1])
		return
	}

	// Regular file: open with O_NOFOLLOW and O_NONBLOCK
	fd, err := syscall.Open(fullPath, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if err == syscall.ELOOP {
			http.Error(w, "unexpected symlink", http.StatusForbidden)
		} else if err == syscall.ENOENT {
			http.Error(w, "not found", http.StatusNotFound)
		} else {
			http.Error(w, "error opening file", http.StatusInternalServerError)
		}
		return
	}
	f := os.NewFile(uintptr(fd), fullPath)
	defer f.Close()

	fileSize := stat.Size()

	// Check for Range header
	rangeHeader := r.Header.Get("Range")
	if rangeHeader == "" {
		// No range request - serve entire file
		w.Header().Set("Content-Length", strconv.FormatInt(fileSize, 10))
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusOK)
		io.Copy(w, f)
		return
	}

	// Parse Range header
	start, end, err := parseRangeHeader(rangeHeader, fileSize)
	if err != nil {
		http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	// Seek to start position
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		http.Error(w, "seek error", http.StatusInternalServerError)
		return
	}

	// Calculate content length
	contentLength := end - start + 1

	// Set headers for partial content
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, fileSize))
	w.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	w.WriteHeader(http.StatusPartialContent)

	// Copy the requested range
	io.CopyN(w, f, contentLength)
}

// parseRangeHeader parses a Range header like "bytes=0-99" and returns start and end positions.
func parseRangeHeader(header string, fileSize int64) (start, end int64, err error) {
	// Must start with "bytes="
	if !strings.HasPrefix(header, "bytes=") {
		return 0, 0, fmt.Errorf("invalid range prefix")
	}

	rangeSpec := strings.TrimPrefix(header, "bytes=")

	// Split on comma for multiple ranges (we only support single range)
	ranges := strings.Split(rangeSpec, ",")
	if len(ranges) != 1 {
		return 0, 0, fmt.Errorf("multiple ranges not supported")
	}

	// Parse the range
	parts := strings.Split(strings.TrimSpace(ranges[0]), "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid range format")
	}

	startStr := strings.TrimSpace(parts[0])
	endStr := strings.TrimSpace(parts[1])

	if startStr == "" {
		// Suffix range: -500 means last 500 bytes
		if endStr == "" {
			return 0, 0, fmt.Errorf("invalid range: empty")
		}
		suffixLen, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid suffix length")
		}
		start = fileSize - suffixLen
		if start < 0 {
			start = 0
		}
		end = fileSize - 1
	} else {
		// Regular range: start-end
		start, err = strconv.ParseInt(startStr, 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid start")
		}

		if endStr == "" {
			// Open-ended range: start-
			end = fileSize - 1
		} else {
			end, err = strconv.ParseInt(endStr, 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("invalid end")
			}
		}
	}

	// Validate range
	if start < 0 || start >= fileSize {
		return 0, 0, fmt.Errorf("start out of range")
	}
	if end >= fileSize {
		end = fileSize - 1
	}
	if start > end {
		return 0, 0, fmt.Errorf("start > end")
	}

	return start, end, nil
}
