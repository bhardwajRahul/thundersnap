// thundersnapd is a Tailscale tsnet-based SSH server that provides
// isolated container environments for each user session.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gliderlabs/ssh"
	"github.com/pborman/getopt/v2"
	"github.com/tailscale/thundersnap/thundersnap"
	"github.com/tailscale/thundersnap/tsm"
	gossh "golang.org/x/crypto/ssh"
	"tailscale.com/client/tailscale"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
)

func init() {
	// Configure tsnet to cache and reuse the netmap, which allows the server
	// to start serving requests faster when reconnecting after being offline.
	// These must be set before any tsnet.Server.Up() calls.
	os.Setenv("TS_FORCE_CACHE_NETMAP", "1")
	os.Setenv("TS_USE_CACHED_NETMAP", "1")
}

var (
	flagFsDir        *string
	flagSnapshotsDir *string
	flagVmDir        *string
	flagLibexecDir   *string
	flagPolicyPath   *string
	flagMesh         *bool
	flagNfsd         *bool
	flagNfsPort      *int

	// globalPolicy holds the loaded policy file for grant matching
	globalPolicy *PolicyFile

	// authURLFile is the path where the server writes the auth URL
	// while waiting for Tailscale login. The --activate client reads it.
	authURLFile = "/run/thundersnap/auth-url"

	// statusFile is the path where the server writes its current status
	// after successful authentication. The --status client reads it.
	statusFile = "/run/thundersnap/status"

	// controlSocket is the path for the local admin control socket.
	// Used for commands like --force-reauth that need to communicate with
	// the running daemon.
	controlSocket = "/run/thundersnap/control.sock"
)

// vmSessionManager tracks running VM sessions and allows multiple clients to share them.
type vmSessionManager struct {
	mu       sync.Mutex
	sessions map[string]*managedVMSession // key: "tailscaleUser/vmUser"
}

// managedVMSession wraps a VM session with reference counting.
type managedVMSession struct {
	session       *thundersnap.VMSession
	vsockPath     string
	refCount      int
	done          chan struct{} // closed when VM exits
	panicked      <-chan struct{} // closed when guest kernel panic is detected
	rootFS        string
	tailscaleUser string
	vmUser        string
}

var vmSessions = &vmSessionManager{
	sessions: make(map[string]*managedVMSession),
}

// activeFrames tracks which frames have active control servers (and thus active sessions).
// Key is the frame path (e.g., /fs/user/framename), value is the number of active sessions.
var activeFrames = struct {
	sync.Mutex
	count map[string]int
}{count: make(map[string]int)}

// registerActiveFrame increments the active session count for a frame.
func registerActiveFrame(framePath string) {
	activeFrames.Lock()
	activeFrames.count[framePath]++
	activeFrames.Unlock()
}

// unregisterActiveFrame decrements the active session count for a frame.
func unregisterActiveFrame(framePath string) {
	activeFrames.Lock()
	activeFrames.count[framePath]--
	if activeFrames.count[framePath] <= 0 {
		delete(activeFrames.count, framePath)
	}
	activeFrames.Unlock()
}

// getActiveFrameCount returns the number of active sessions for a frame.
func getActiveFrameCount(framePath string) int {
	activeFrames.Lock()
	defer activeFrames.Unlock()
	return activeFrames.count[framePath]
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
		panicked:      session.Panicked(),
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
	activate := getopt.BoolLong("activate", 0, "Print the Tailscale auth URL and wait for login to complete")
	showStatus := getopt.BoolLong("status", 0, "Print the current server status and exit")
	forceReauth := getopt.BoolLong("force-reauth", 0, "Force re-authentication with Tailscale")
	hostname := getopt.StringLong("hostname", 0, "thundersnap", "Tailscale hostname for this server")
	stateDir := getopt.StringLong("state-dir", 0, "", "Directory to store Tailscale state (default: ~/.config/thundersnapd)")
	flagFsDir = getopt.StringLong("fs-dir", 0, "", "Directory to store per-user live filesystems (required)")
	flagSnapshotsDir = getopt.StringLong("snapshots-dir", 0, "", "Directory to store base snapshots for cloning (required)")
	flagVmDir = getopt.StringLong("vm-dir", 0, "", "Directory containing cloud-hypervisor and vmlinux (default: <exe-dir>/vm)")
	flagLibexecDir = getopt.StringLong("libexec-dir", 0, "", "Directory containing helper binaries like ts and vshd (default: <exe-dir>)")
	flagPolicyPath = getopt.StringLong("policy", 0, "", "Path to policy file (required)")
	flagMesh = getopt.BoolLong("mesh", 0, "Enable mesh discovery: ping other thundersnap nodes and serve /bupdate/")
	flagNfsd = getopt.BoolLong("nfsd", 0, "Enable NFSv4 server to export -snapshots-dir")
	flagNfsPort = getopt.IntLong("nfs-port", 0, 2049, "Port for NFSv4 server")
	getopt.Parse()

	if *activate {
		runActivate()
		return
	}

	if *showStatus {
		runStatus()
		return
	}

	if *forceReauth {
		runForceReauth()
		return
	}

	if *flagFsDir == "" {
		log.Fatalf("-fs-dir is required")
	}
	if *flagSnapshotsDir == "" {
		log.Fatalf("-snapshots-dir is required")
	}
	if *flagPolicyPath == "" {
		log.Fatalf("-policy is required")
	}

	// Load policy file
	var err error
	globalPolicy, err = LoadPolicyFile(*flagPolicyPath)
	if err != nil {
		log.Fatalf("Failed to load policy file: %v", err)
	}
	log.Printf("Loaded policy with %d grants", len(globalPolicy.Grants))

	// Verify both directories are on btrfs and on the same filesystem
	if err := checkBtrfsFilesystems(*flagFsDir, *flagSnapshotsDir); err != nil {
		log.Fatalf("%v", err)
	}

	// Set default vm-dir and libexec-dir relative to executable
	exe, err := os.Executable()
	if err != nil {
		log.Fatalf("Failed to get executable path: %v", err)
	}
	if *flagVmDir == "" {
		*flagVmDir = filepath.Join(filepath.Dir(exe), "vm")
	}
	if *flagLibexecDir == "" {
		*flagLibexecDir = filepath.Dir(exe)
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
		UserLogf: func(format string, a ...any) {
			msg := fmt.Sprintf(format, a...)
			log.Print(msg)
			// If the log contains an auth URL, write it to a file so
			// "thundersnapd --activate" can read and display it.
			const prefix = "or go to: "
			if idx := strings.Index(msg, prefix); idx != -1 {
				url := strings.TrimSpace(msg[idx+len(prefix):])
				if url != "" {
					os.WriteFile(authURLFile, []byte(url+"\n"), 0600)
				}
			}
		},
	}
	defer srv.Close()

	// Start the tsnet server and wait for it to be ready
	log.Printf("Starting tsnet server with hostname %q...", *hostname)
	status, err := srv.Up(context.Background())
	if err != nil {
		log.Fatalf("Failed to start tsnet server: %v", err)
	}
	// Auth is complete; remove the auth URL file if it was written.
	os.Remove(authURLFile)
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

	// Write status file with current server info
	writeStatusFile(status)

	// Start admin control socket for local commands like --force-reauth
	go startAdminControlSocket(lc)

	// Create SSH server with gliderlabs/ssh and persistent host key
	forwardHandler := &ssh.ForwardedTCPHandler{}
	sshServer := &ssh.Server{
		Handler: func(s ssh.Session) {
			log.Printf("New SSH session from %s (user: %s)", s.RemoteAddr(), s.User())

			// Look up the Tailscale identity of the connecting peer
			who := getWhoIs(s.Context(), lc, s.RemoteAddr().String())
			tailscaleUser := "unknown"
			if who != nil {
				if who.Node != nil && len(who.Node.Tags) > 0 {
					tailscaleUser = fmt.Sprintf("tags: %s", strings.Join(who.Node.Tags, ", "))
				} else if who.UserProfile != nil && who.UserProfile.LoginName != "" {
					tailscaleUser = who.UserProfile.LoginName
				}
			}

			// Resolve capability from policy
			cap := ResolveCap(who, globalPolicy)
			log.Printf("Resolved cap for %s: role=%s isolation=%s", tailscaleUser, cap.Role, cap.Isolation)

			// Helper to log error to both server log and client
			logErr := func(format string, args ...any) {
				msg := fmt.Sprintf(format, args...)
				log.Print(msg)
				fmt.Fprintf(s, "* Error: %s\r\n", msg)
			}

			// Parse SSH username to extract target user and frame name.
			// Format: [<user>@]<name> or [<user>@]vm/<name> (legacy)
			// If user@ prefix is present, use that specific Unix user.
			// Otherwise, auto-detect from [ubuntu, user] or fall back to root.
			sshUser := s.User()

			// Legacy support: vm/ prefix overrides isolation to "vm"
			if strings.HasPrefix(sshUser, "vm/") {
				cap.Isolation = "vm"
				sshUser = strings.TrimPrefix(sshUser, "vm/")
			}

			// Parse optional user prefix: <user>@<name> or <name>
			targetUser := "" // empty means auto-detect
			frameName := sshUser
			if idx := strings.Index(sshUser, "@"); idx != -1 {
				targetUser = sshUser[:idx]
				frameName = sshUser[idx+1:]
			}

			rootFS := filepath.Join(*flagFsDir, sanitizeForPath(tailscaleUser), sanitizeForPath(frameName))
			runAsUser := selectTargetUser(rootFS, targetUser)

			// Route based on isolation level
			switch cap.Isolation {
			case "vm":
				fmt.Fprintf(s.Stderr(), "* Hello <%s>, connecting you to <%s> in <%s> (VM)\r\n", tailscaleUser, runAsUser, frameName)
				if err := runVMSession(s, tailscaleUser, frameName, targetUser, logErr); err != nil {
					logErr("VM session failed: %v", err)
					s.Exit(1)
				}
			case "none":
				// Direct session on host (no isolation)
				fmt.Fprintf(s.Stderr(), "* Hello <%s>, connecting you to <%s> in <%s> (no isolation)\r\n", tailscaleUser, runAsUser, frameName)
				if err := runContainerSession(s, tailscaleUser, frameName, targetUser, logErr); err != nil {
					logErr("Session failed: %v", err)
					s.Exit(1)
				}
			default: // "container" is the default
				fmt.Fprintf(s.Stderr(), "* Hello <%s>, connecting you to <%s> in <%s>\r\n", tailscaleUser, runAsUser, frameName)
				if err := runContainerSession(s, tailscaleUser, frameName, targetUser, logErr); err != nil {
					logErr("Container session failed: %v", err)
					s.Exit(1)
				}
			}

			// TODO: Handle ephemeral cleanup if cap.Ephemeral is true
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
		SubsystemHandlers: map[string]ssh.SubsystemHandler{
			"sftp": func(s ssh.Session) {
				// Handle SFTP subsystem by running sftp-server in the container.
				// This is invoked by modern scp (which uses SFTP by default).
				tailscaleUser := getTailscaleUser(s.Context(), lc, s.RemoteAddr().String())
				sshUser := s.User()

				// Parse user@container format
				targetUser := ""
				containerName := sshUser
				if idx := strings.Index(sshUser, "@"); idx != -1 {
					targetUser = sshUser[:idx]
					containerName = sshUser[idx+1:]
				}

				rootFS := filepath.Join(*flagFsDir, sanitizeForPath(tailscaleUser), sanitizeForPath(containerName))
				if err := runSFTPSession(s, rootFS, targetUser); err != nil {
					log.Printf("SFTP session failed: %v", err)
				}
			},
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
	globalMeshState = meshState // Set global for control socket access
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

// runActivate implements the --activate client mode.
// It reads the auth URL file written by the running server, prints the URL,
// then polls until the file is deleted (meaning authentication completed).
func runActivate() {
	data, err := os.ReadFile(authURLFile)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "No auth URL found at %s.\nThe server may already be authenticated, or may not be running yet.\n", authURLFile)
			os.Exit(1)
		}
		var errno syscall.Errno
		if errors.As(err, &errno) {
			fmt.Fprintf(os.Stderr, "%s: %s\n", authURLFile, errno)
		} else {
			fmt.Fprintf(os.Stderr, "%s: %v\n", authURLFile, err)
		}
		fmt.Fprintf(os.Stderr, "Try running as root.\n")
		os.Exit(1)
	}

	url := strings.TrimSpace(string(data))
	if url == "" {
		fmt.Fprintf(os.Stderr, "Auth URL file %s is empty.\n", authURLFile)
		os.Exit(1)
	}

	fmt.Printf("To authenticate this node, visit:\n\n  %s\n\nWaiting for login to complete...\n", url)

	// Poll until the file is deleted (server removes it after successful auth).
	for {
		time.Sleep(1 * time.Second)
		if _, err := os.Stat(authURLFile); os.IsNotExist(err) {
			fmt.Println("Login complete.")
			return
		}
	}
}

// runStatus implements the --status client mode.
// It reads and prints the status file written by the running server.
func runStatus() {
	data, err := os.ReadFile(statusFile)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "No status file found at %s.\nThe server may not be running or not yet authenticated.\n", statusFile)
			os.Exit(1)
		}
		var errno syscall.Errno
		if errors.As(err, &errno) {
			fmt.Fprintf(os.Stderr, "%s: %s\n", statusFile, errno)
		} else {
			fmt.Fprintf(os.Stderr, "%s: %v\n", statusFile, err)
		}
		fmt.Fprintf(os.Stderr, "Try running as root.\n")
		os.Exit(1)
	}
	fmt.Print(string(data))
}

// runForceReauth implements the --force-reauth client mode.
// It connects to the control socket and requests re-authentication.
func runForceReauth() {
	conn, err := net.Dial("unix", controlSocket)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "Control socket not found at %s.\nThe server may not be running.\n", controlSocket)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Failed to connect to control socket: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	// Send reauth command
	fmt.Fprintln(conn, "reauth")

	// Read response
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read response: %v\n", err)
		os.Exit(1)
	}

	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "OK") {
		fmt.Println(line)
	} else {
		fmt.Fprintf(os.Stderr, "%s\n", line)
		os.Exit(1)
	}
}

// writeStatusFile writes the current server status to the status file.
func writeStatusFile(status *ipnstate.Status) {
	// Ensure the directory exists
	if err := os.MkdirAll(filepath.Dir(statusFile), 0755); err != nil {
		log.Printf("Warning: failed to create status file directory: %v", err)
		return
	}

	// Determine the logged-in identity (user login or tags)
	login := "unknown"
	if status.Self != nil {
		if status.Self.Tags != nil && status.Self.Tags.Len() > 0 {
			var tags []string
			for i := range status.Self.Tags.Len() {
				tags = append(tags, status.Self.Tags.At(i))
			}
			login = strings.Join(tags, ",")
		} else if status.Self.UserID != 0 {
			if user, ok := status.User[status.Self.UserID]; ok {
				login = user.LoginName
			}
		}
	}

	// Get the actual hostname from control server (DNSName without trailing dot)
	hostname := "unknown"
	if status.Self != nil && status.Self.DNSName != "" {
		hostname = strings.TrimSuffix(status.Self.DNSName, ".")
	}

	// Format IP addresses
	var ips []string
	for _, ip := range status.TailscaleIPs {
		ips = append(ips, ip.String())
	}

	// Build status content
	var buf strings.Builder
	buf.WriteString(fmt.Sprintf("state: %s\n", status.BackendState))
	buf.WriteString(fmt.Sprintf("hostname: %s\n", hostname))
	buf.WriteString(fmt.Sprintf("login: %s\n", login))
	buf.WriteString(fmt.Sprintf("tailscale-ips: %s\n", strings.Join(ips, " ")))

	if err := os.WriteFile(statusFile, []byte(buf.String()), 0644); err != nil {
		log.Printf("Warning: failed to write status file: %v", err)
	}
}

// startAdminControlSocket starts a Unix socket for local admin commands.
func startAdminControlSocket(lc *tailscale.LocalClient) {
	// Remove any stale socket file
	os.Remove(controlSocket)

	// Ensure the directory exists
	if err := os.MkdirAll(filepath.Dir(controlSocket), 0755); err != nil {
		log.Printf("Warning: failed to create control socket directory: %v", err)
		return
	}

	ln, err := net.Listen("unix", controlSocket)
	if err != nil {
		log.Printf("Warning: failed to start admin control socket: %v", err)
		return
	}

	// Make socket accessible only to root
	if err := os.Chmod(controlSocket, 0600); err != nil {
		log.Printf("Warning: failed to chmod control socket: %v", err)
	}

	log.Printf("Admin control socket listening on %s", controlSocket)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("Admin control socket accept error: %v", err)
			continue
		}
		go handleAdminConnection(conn, lc)
	}
}

// handleAdminConnection handles a single connection to the admin control socket.
func handleAdminConnection(conn net.Conn, lc *tailscale.LocalClient) {
	defer conn.Close()

	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		log.Printf("Admin control: failed to read command: %v", err)
		return
	}

	cmd := strings.TrimSpace(line)
	log.Printf("Admin control: received command: %s", cmd)

	switch cmd {
	case "reauth":
		if err := lc.StartLoginInteractive(context.Background()); err != nil {
			fmt.Fprintf(conn, "ERROR: %v\n", err)
			return
		}
		fmt.Fprintln(conn, "OK: re-authentication started, use --activate to complete")
	default:
		fmt.Fprintf(conn, "ERROR: unknown command: %s\n", cmd)
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

// getWhoIs looks up the full WhoIs response for the given remote address.
// Returns the WhoIs response, or nil if lookup fails.
func getWhoIs(ctx context.Context, lc *tailscale.LocalClient, remoteAddr string) *apitype.WhoIsResponse {
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
		return nil
	}

	// Look up who owns this IP
	whois, err := lc.WhoIs(ctx, ip.String())
	if err != nil {
		return nil
	}

	return whois
}

// getTailscaleUser looks up the Tailscale identity for the given remote address.
// Returns the user's login name, or tags if it's a tagged node, or the IP if lookup fails.
func getTailscaleUser(ctx context.Context, lc *tailscale.LocalClient, remoteAddr string) string {
	whois := getWhoIs(ctx, lc, remoteAddr)
	if whois == nil {
		return "unknown"
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

// Resource limit constants for container isolation.
// These provide defense against runaway processes while allowing efficient memory sharing.
const (
	// containerOOMScore is the OOM score adjustment applied to container processes.
	// Higher values make processes more likely to be killed by the OOM killer.
	// Range is -1000 to +1000; default is 0. We use +500 to make containers much more
	// likely to be killed than the host OS or thundersnapd itself during memory pressure.
	containerOOMScore = 500

	// parentCgroupName is the cgroup that contains all thundersnap containers.
	// System-wide limits are applied here as a backstop.
	parentCgroupName = "thundersnap"

	// parentMemoryMaxPercent is the percentage of system RAM that all thundersnap
	// containers combined can use. This is a hard limit to protect the host OS.
	parentMemoryMaxPercent = 80

	// parentCPUWeight is the CPU weight for all thundersnap containers relative to
	// other work on the system. Default is 100; we use 50 so non-thundersnap work
	// gets priority when CPU is contested. When CPU is idle, containers can still
	// use all available CPU.
	parentCPUWeight = 50

	// containerMemoryHighPercent is the percentage of system RAM for the soft limit
	// per container. When exceeded, the kernel aggressively reclaims memory from
	// the container (swapping, dropping caches) but doesn't OOM kill. This allows
	// containers to burst above their "fair share" when memory is available.
	// With 8 containers, each gets ~10% as a soft limit but can burst higher.
	containerMemoryHighPercent = 10

	// containerPidsMax limits the number of processes per container.
	// This is the primary defense against fork bombs.
	containerPidsMax = 2000

	// containerCPUWeight is the CPU weight for each container relative to other
	// containers. All containers get equal weight (100 = default).
	containerCPUWeight = 100
)

// cgroupInitialized tracks whether the parent cgroup has been set up.
var cgroupInitialized bool

// getSystemMemoryBytes returns the total system memory in bytes.
func getSystemMemoryBytes() (uint64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, err := strconv.ParseUint(fields[1], 10, 64)
				if err != nil {
					return 0, err
				}
				return kb * 1024, nil // Convert KB to bytes
			}
		}
	}
	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}

// initParentCgroup creates the parent thundersnap cgroup with system-wide limits.
// This should be called once at startup. Errors are logged but not fatal.
func initParentCgroup() {
	if cgroupInitialized {
		return
	}

	cgroupPath := filepath.Join("/sys/fs/cgroup", parentCgroupName)

	// Create parent cgroup directory
	if err := os.MkdirAll(cgroupPath, 0755); err != nil {
		log.Printf("warning: failed to create parent cgroup %s: %v", cgroupPath, err)
		return
	}

	// Enable controllers for child cgroups
	// We need to enable controllers in the parent so children can use them
	subtreeControl := filepath.Join(cgroupPath, "cgroup.subtree_control")
	if err := os.WriteFile(subtreeControl, []byte("+memory +pids +cpu"), 0644); err != nil {
		log.Printf("warning: failed to enable cgroup controllers: %v", err)
		// Continue anyway - some controllers might already be enabled
	}

	// Set CPU weight (lower priority than default)
	cpuWeight := filepath.Join(cgroupPath, "cpu.weight")
	if err := os.WriteFile(cpuWeight, []byte(strconv.Itoa(parentCPUWeight)), 0644); err != nil {
		log.Printf("warning: failed to set parent cpu.weight: %v", err)
	}

	// Set memory.max as hard backstop (percentage of system RAM)
	totalMem, err := getSystemMemoryBytes()
	if err != nil {
		log.Printf("warning: failed to get system memory: %v", err)
	} else {
		memMax := totalMem * parentMemoryMaxPercent / 100
		memMaxPath := filepath.Join(cgroupPath, "memory.max")
		if err := os.WriteFile(memMaxPath, []byte(strconv.FormatUint(memMax, 10)), 0644); err != nil {
			log.Printf("warning: failed to set parent memory.max: %v", err)
		} else {
			log.Printf("Configured parent cgroup %s: memory.max=%dMB, cpu.weight=%d",
				parentCgroupName, memMax/(1024*1024), parentCPUWeight)
		}
	}

	cgroupInitialized = true
}

// setProcessOOMScore sets the OOM score adjustment for a process.
// Higher scores make the process more likely to be killed during memory pressure.
// Errors are logged but not fatal since OOM adjustment is best-effort.
func setProcessOOMScore(pid int, score int) {
	path := fmt.Sprintf("/proc/%d/oom_score_adj", pid)
	if err := os.WriteFile(path, []byte(strconv.Itoa(score)), 0644); err != nil {
		log.Printf("warning: failed to set OOM score for pid %d: %v", pid, err)
	}
}

// setupContainerCgroup creates a cgroup for the container process with resource limits.
// Limits applied:
//   - memory.high: soft memory limit (kernel reclaims aggressively above this)
//   - memory.oom.group: kill entire container on OOM, not just one process
//   - pids.max: limit process count (fork bomb protection)
//   - cpu.weight: fair sharing among containers
//
// Errors are logged but not fatal since cgroup setup is best-effort.
func setupContainerCgroup(pid int, cgroupName string) {
	// Ensure parent cgroup exists with system-wide limits
	initParentCgroup()

	// Use cgroup v2 unified hierarchy
	cgroupPath := filepath.Join("/sys/fs/cgroup", cgroupName)

	// Create cgroup directory
	if err := os.MkdirAll(cgroupPath, 0755); err != nil {
		log.Printf("warning: failed to create cgroup %s: %v", cgroupPath, err)
		return
	}

	// Set memory.high (soft limit) - kernel reclaims aggressively above this
	totalMem, err := getSystemMemoryBytes()
	if err == nil {
		memHigh := totalMem * containerMemoryHighPercent / 100
		memHighPath := filepath.Join(cgroupPath, "memory.high")
		if err := os.WriteFile(memHighPath, []byte(strconv.FormatUint(memHigh, 10)), 0644); err != nil {
			log.Printf("warning: failed to set memory.high for %s: %v", cgroupName, err)
		}
	}

	// Enable memory.oom.group=1 so OOM kills the entire cgroup
	oomGroupPath := filepath.Join(cgroupPath, "memory.oom.group")
	if err := os.WriteFile(oomGroupPath, []byte("1"), 0644); err != nil {
		log.Printf("warning: failed to set memory.oom.group for %s: %v", cgroupName, err)
	}

	// Set pids.max (fork bomb protection)
	pidsMaxPath := filepath.Join(cgroupPath, "pids.max")
	if err := os.WriteFile(pidsMaxPath, []byte(strconv.Itoa(containerPidsMax)), 0644); err != nil {
		log.Printf("warning: failed to set pids.max for %s: %v", cgroupName, err)
	}

	// Set cpu.weight for fair sharing among containers
	cpuWeightPath := filepath.Join(cgroupPath, "cpu.weight")
	if err := os.WriteFile(cpuWeightPath, []byte(strconv.Itoa(containerCPUWeight)), 0644); err != nil {
		log.Printf("warning: failed to set cpu.weight for %s: %v", cgroupName, err)
	}

	// Move the process into the cgroup
	procsPath := filepath.Join(cgroupPath, "cgroup.procs")
	if err := os.WriteFile(procsPath, []byte(strconv.Itoa(pid)), 0644); err != nil {
		log.Printf("warning: failed to add pid %d to cgroup %s: %v", pid, cgroupName, err)
		return
	}
}

// configureContainerResources sets up resource limits for a container process.
// It adjusts OOM score to prioritize killing containers over host services,
// and sets up a cgroup with memory limits, fork bomb protection, and CPU fairness.
func configureContainerResources(pid int, cgroupName string) {
	setProcessOOMScore(pid, containerOOMScore)
	setupContainerCgroup(pid, cgroupName)
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
// Otherwise, auto-detect:
//  1. Check if "ubuntu" user's home exists -> use ubuntu
//  2. Ensure "user" exists in /etc/passwd (add if missing, with home=/home)
//  3. If user's home directory exists -> use user
//  4. Fall back to root
func selectTargetUser(rootFS, targetUser string) string {
	if targetUser != "" {
		return targetUser
	}

	// First check for ubuntu user (legacy behavior)
	ubuntuHome := filepath.Join(rootFS, "home", "ubuntu")
	if info, err := os.Stat(ubuntuHome); err == nil && info.IsDir() {
		return "ubuntu"
	}

	// Ensure "user" exists in /etc/passwd, adding it if necessary.
	// This returns the home directory from passwd (e.g., "/home" for new entries).
	userHome, err := tsm.EnsureUserInPasswd(rootFS)
	if err != nil {
		log.Printf("Warning: failed to ensure user in passwd: %v", err)
	}

	// If we have a home path, check if it exists in the rootfs
	if userHome != "" {
		// userHome is absolute (e.g., "/home"), join with rootFS
		homePath := filepath.Join(rootFS, userHome)
		if info, err := os.Stat(homePath); err == nil && info.IsDir() {
			return "user"
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

	// Copy ts binary into container's /bin using btrfs reflink
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
	ptyReq, _, isPty := s.Pty()
	rawCmd := s.RawCommand()

	// Determine which Unix user to run as
	runAsUser := selectTargetUser(rootFS, targetUser)
	log.Printf("Container session: running as user %q (requested: %q)", runAsUser, targetUser)

	// Prepare the command to execute using su to switch to the target user.
	// For interactive sessions (no command): su - <user> (login shell)
	// For command execution: su <user> -c '<command>' (non-login shell)

	// Build arguments for "ts drop-caps-and-run" which handles all container
	// initialization via syscalls (no external commands needed):
	//   - Makes mounts private (prevents propagation to host)
	//   - Chroots into the container rootfs
	//   - Mounts /proc and /sys
	//   - Sets hostname/domainname
	//   - Drops dangerous capabilities
	//
	// The chroot is done by ts rather than via SysProcAttr so that we can
	// make mounts private BEFORE the chroot (while / is still a mount point).
	// The ts binary path must be absolute since we exec it from the host.
	absRootFS, err := filepath.Abs(rootFS)
	if err != nil {
		return fmt.Errorf("get absolute path for rootFS: %w", err)
	}
	tsBinary := filepath.Join(absRootFS, "bin", "ts")
	tsArgs := []string{"--chroot=" + absRootFS}

	// Set hostname and domainname in the new UTS namespace based on tsnet FQDN.
	// e.g., "hotdog.corp.ts.net" -> hostname "hotdog", domainname "corp.ts.net"
	if globalMeshState != nil && globalMeshState.myFQDN != "" {
		fqdn := globalMeshState.myFQDN
		if idx := strings.Index(fqdn, "."); idx > 0 {
			tsArgs = append(tsArgs, "--hostname="+fqdn[:idx])
			tsArgs = append(tsArgs, "--domainname="+fqdn[idx+1:])
		} else {
			tsArgs = append(tsArgs, "--hostname="+fqdn)
		}
	}

	var cmd *exec.Cmd
	if rawCmd != "" {
		// Execute the requested command as the target user (non-login shell).
		// The SSH protocol sends a raw command string that the user expects to
		// be interpreted by a shell, so we pass it directly to su -c without
		// any re-quoting. This avoids shell quoting bugs and matches standard
		// SSH server behavior.
		tsArgs = append(tsArgs, "--", "su", runAsUser, "-c", rawCmd)
		cmd = exec.Command(tsBinary, append([]string{"drop-caps-and-run"}, tsArgs...)...)
	} else {
		// Launch interactive login shell as the target user
		tsArgs = append(tsArgs, "--", "su", "-", runAsUser)
		cmd = exec.Command(tsBinary, append([]string{"drop-caps-and-run"}, tsArgs...)...)
	}

	cmd.Dir = "/"
	cmd.Env = []string{
		"SSH_USER=" + sshUser,
		"TAILSCALE_USER=" + tailscaleUser,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS,
	}

	// Build cgroup name for this container (used for OOM group killing)
	cgroupName := fmt.Sprintf("thundersnap/%s/%s", safeTailscaleUser, safeSSHUser)

	if isPty {
		// For PTY sessions, we need to allocate the PTY INSIDE the container
		// after devpts is mounted, otherwise the PTY won't be visible in
		// /dev/pts inside the container. We pass --pty to ts drop-caps-and-run
		// which handles PTY allocation after setting up the mount namespace.
		//
		// Rebuild the command with --pty flag inserted before --
		tsArgsWithPty := make([]string, 0, len(tsArgs)+1)
		for _, arg := range tsArgs {
			if arg == "--" {
				tsArgsWithPty = append(tsArgsWithPty, "--pty", "--")
			} else {
				tsArgsWithPty = append(tsArgsWithPty, arg)
			}
		}
		if rawCmd != "" {
			cmd = exec.Command(tsBinary, append([]string{"drop-caps-and-run"}, tsArgsWithPty...)...)
		} else {
			cmd = exec.Command(tsBinary, append([]string{"drop-caps-and-run"}, tsArgsWithPty...)...)
		}
		cmd.Dir = "/"
		cmd.Env = []string{
			"SSH_USER=" + sshUser,
			"TAILSCALE_USER=" + tailscaleUser,
			"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			"TERM=" + ptyReq.Term,
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS,
		}

		// Set up pipes - ts --pty will proxy between stdin/stdout and the PTY
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return fmt.Errorf("create stdin pipe: %w", err)
		}
		cmd.Stdout = s
		cmd.Stderr = s.Stderr()

		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start shell: %w", err)
		}

		// Configure resource limits: OOM priority, memory soft limit, fork bomb
		// protection (pids.max), and CPU fairness
		configureContainerResources(cmd.Process.Pid, cgroupName)

		// Copy stdin from SSH session to command
		go func() {
			io.Copy(stdin, s)
			stdin.Close()
		}()

		// Wait for the command to complete
		cmd.Wait()
		s.Exit(cmd.ProcessState.ExitCode())
	} else {
		// No PTY requested, run without one

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
		configureContainerResources(cmd.Process.Pid, cgroupName)

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

// runSFTPSession handles an SFTP subsystem request by running sftp-server in the container.
func runSFTPSession(s ssh.Session, rootFS, targetUser string) error {
	// Check if rootFS exists
	if _, err := os.Stat(rootFS); err != nil {
		return fmt.Errorf("container filesystem not found: %s", rootFS)
	}

	// Determine which Unix user to run as
	runAsUser := selectTargetUser(rootFS, targetUser)

	// Build the command to run sftp-server inside the container
	absRootFS, err := filepath.Abs(rootFS)
	if err != nil {
		return fmt.Errorf("get absolute path for rootFS: %w", err)
	}
	tsBinary := filepath.Join(absRootFS, "bin", "ts")
	tsArgs := []string{"--chroot=" + absRootFS}

	// Find sftp-server - it's typically in /usr/lib/openssh/ or /usr/libexec/
	sftpServerPaths := []string{
		"/usr/lib/openssh/sftp-server",
		"/usr/libexec/openssh/sftp-server",
		"/usr/lib/sftp-server",
		"/usr/libexec/sftp-server",
	}
	sftpServer := ""
	for _, p := range sftpServerPaths {
		if _, err := os.Stat(filepath.Join(rootFS, p)); err == nil {
			sftpServer = p
			break
		}
	}
	if sftpServer == "" {
		return fmt.Errorf("sftp-server not found in container (tried %v)", sftpServerPaths)
	}

	// Run sftp-server as the target user
	tsArgs = append(tsArgs, "--", "su", runAsUser, "-c", sftpServer)
	cmd := exec.Command(tsBinary, append([]string{"drop-caps-and-run"}, tsArgs...)...)

	cmd.Dir = "/"
	cmd.Env = []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS,
	}

	// SFTP uses stdin/stdout for the protocol, no PTY
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("create stdin pipe: %w", err)
	}
	cmd.Stdout = s
	cmd.Stderr = s.Stderr()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start sftp-server: %w", err)
	}

	// Configure resource limits: OOM priority, memory soft limit, fork bomb
	// protection (pids.max), and CPU fairness.
	// Derive cgroup name from rootFS path (e.g., /fs/user/container -> thundersnap/user/container)
	cgroupName := "thundersnap/" + filepath.Base(filepath.Dir(rootFS)) + "/" + filepath.Base(rootFS)
	configureContainerResources(cmd.Process.Pid, cgroupName)

	// Copy stdin from SSH session to sftp-server
	go func() {
		io.Copy(stdin, s)
		stdin.Close()
	}()

	// Wait for sftp-server to complete
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			s.Exit(exitErr.ExitCode())
			return nil
		}
		return fmt.Errorf("sftp-server error: %w", err)
	}
	s.Exit(0)
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
	// Pass panic channel so we abort immediately if VM panics during connection
	conn, err := connectToVshd(ms.vsockPath, ms.panicked)
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
	copyDone := make(chan struct{}, 2) // buffered to avoid goroutine leak

	log.Printf("SSH proxy: starting io.Copy goroutines, ms.panicked=%v", ms.panicked)

	// SSH stdin -> vshd
	go func() {
		n, err := io.Copy(conn, s)
		log.Printf("SSH proxy: stdin->vshd finished: %d bytes, err=%v", n, err)
		// When SSH session closes, close our write side to vshd
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

	log.Printf("SSH proxy: entering select, watching copyDone, ms.done, ms.panicked, s.Context().Done()")

	// Wait for either:
	// - vshd connection to close (shell exited normally)
	// - VM to exit unexpectedly
	// - VM kernel panic (disconnect immediately)
	// - SSH session to be closed by client (e.g., ~. escape sequence)
	select {
	case <-copyDone:
		log.Printf("SSH proxy: select triggered by copyDone (vshd connection closed)")
	case <-ms.done:
		log.Printf("SSH proxy: select triggered by ms.done (VM exited)")
	case <-ms.panicked:
		log.Printf("SSH proxy: select triggered by ms.panicked (VM kernel panic)")
	case <-s.Context().Done():
		log.Printf("SSH proxy: select triggered by s.Context().Done() (SSH session closed by client)")
	}

	log.Printf("SSH proxy: closing conn to unblock goroutines")
	// Close conn to unblock any io.Copy goroutines still running
	conn.Close()

	// Wait briefly for goroutines to finish, but don't block forever
	log.Printf("SSH proxy: waiting for goroutines to finish")
	timeout := time.After(100 * time.Millisecond)
	for i := 0; i < 2; i++ {
		select {
		case <-copyDone:
			log.Printf("SSH proxy: goroutine %d finished", i+1)
		case <-timeout:
			log.Printf("SSH proxy: Warning: io.Copy goroutine did not finish promptly")
			goto done
		}
	}
done:

	log.Printf("SSH proxy: calling s.Exit(0)")
	s.Exit(0)
	log.Printf("SSH proxy: returning")
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
// If a frame.jsonc file exists at rootFS+".jsonc", the frame model is used:
// - The rootfs, home, and work snaps are cloned to create a three-component frame
// - Nested /home and /work subvolumes are created within the rootfs
// - Taints are computed as the union of all component snaps' taints
//
// The snapshotting flow (legacy single-component):
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

	// Check if a frame.jsonc exists specifying the frame composition
	frameMeta, err := readFrameMeta(rootFS)
	if err != nil {
		return fmt.Errorf("reading frame meta: %w", err)
	}

	if frameMeta != nil && frameMeta.Rootfs != "" {
		// Use the new three-component frame model
		return ensureFrameFS(rootFS, frameMeta)
	}

	// Legacy single-component mode
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

	// Verify the snapshot source exists before trying to clone it.
	if _, err := os.Stat(snapshotSource); err != nil {
		if os.IsNotExist(err) && snapshotSource == defaultSnapshot {
			return fmt.Errorf("%s does not exist; create a base filesystem snapshot there before starting", snapshotSource)
		}
		return fmt.Errorf("snapshot source %s: %w", snapshotSource, err)
	}

	// Step 1: Create intermediate snapshot in snapshots-dir with fidx
	// (no progress reporting for ensureRootFS - happens at SSH login time)
	// The snapshot ID is based on the TSM SHA-256, so duplicates are detected.
	intermediateID, err := createSnapshotWithFidx(snapshotSource, baseStampID, nil, false)
	if err != nil {
		return fmt.Errorf("create intermediate snapshot from %s: %w", snapshotSource, err)
	}
	intermediatePath := filepath.Join(*flagSnapshotsDir, intermediateID)

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

	// Apply the simplified UID model to the live filesystem before any
	// session enters it. All non-root users in /etc/passwd resolve to the
	// shared UID, all non-root groups in /etc/group to the shared GID, and
	// every file owned by a non-root UID/GID is chowned to match. This
	// runs once per fresh fs-dir clone, never against the read-only
	// snapshots-dir subvolumes. See strip-all-uids-design.md.
	stripOpts := tsm.StripOptions{ChownFiles: true}
	if err := tsm.StripRootfs(rootFS, stripOpts); err != nil {
		log.Printf("Warning: strip-uids on %s: %v", rootFS, err)
	}

	// Ensure resolv.conf exists for DNS resolution inside the frame
	if err := ensureResolvConf(rootFS); err != nil {
		log.Printf("Warning: ensure resolv.conf on %s: %v", rootFS, err)
	}

	// Ensure /tmp has correct permissions (1777 with sticky bit)
	if err := ensureTmpDir(rootFS); err != nil {
		log.Printf("Warning: ensure /tmp on %s: %v", rootFS, err)
	}

	return nil
}

// ensureFrameFS creates a three-component frame from the given FrameMeta.
// It creates:
// - rootFS: the rootfs subvolume (the frame directory itself)
// - rootFS/home: nested home subvolume
// - rootFS/work: nested work subvolume
func ensureFrameFS(rootFS string, meta *FrameMeta) error {
	// Ensure the parent directory exists
	parentDir := filepath.Dir(rootFS)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		return fmt.Errorf("creating parent directory: %w", err)
	}

	// Step 1: Clone rootfs component from snapshot
	rootfsSnapPath := filepath.Join(*flagSnapshotsDir, meta.Rootfs)
	if _, err := os.Stat(rootfsSnapPath); err != nil {
		return fmt.Errorf("rootfs snap %s: %w", meta.Rootfs, err)
	}

	cmd := exec.Command("btrfs", "subvolume", "snapshot", rootfsSnapPath, rootFS)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("btrfs snapshot rootfs from %s to %s failed: %w\noutput: %s",
			rootfsSnapPath, rootFS, err, string(output))
	}

	// Step 2: Create or clone home subvolume
	homePath := filepath.Join(rootFS, "home")
	// Remove existing /home directory if it's not a subvolume (from the rootfs snap)
	if fi, err := os.Stat(homePath); err == nil && fi.IsDir() && !isSubvolume(homePath) {
		if err := os.RemoveAll(homePath); err != nil {
			log.Printf("Warning: failed to remove existing /home directory: %v", err)
		}
	}

	if meta.Home != "" {
		// Clone from home snap
		homeSnapPath := filepath.Join(*flagSnapshotsDir, meta.Home)
		if _, err := os.Stat(homeSnapPath); err != nil {
			return fmt.Errorf("home snap %s: %w", meta.Home, err)
		}
		cmd := exec.Command("btrfs", "subvolume", "snapshot", homeSnapPath, homePath)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("btrfs snapshot home from %s to %s failed: %w\noutput: %s",
				homeSnapPath, homePath, err, string(output))
		}
	} else {
		// Create empty home subvolume
		cmd := exec.Command("btrfs", "subvolume", "create", homePath)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("btrfs subvolume create home at %s failed: %w\noutput: %s",
				homePath, err, string(output))
		}
		// Chown to user 1000 (the standard container user)
		if err := os.Chown(homePath, 1000, 1000); err != nil {
			log.Printf("Warning: failed to chown home subvolume: %v", err)
		}
	}

	// Step 3: Create or clone work subvolume
	workPath := filepath.Join(rootFS, "work")
	// Remove existing /work directory if it's not a subvolume
	if fi, err := os.Stat(workPath); err == nil && fi.IsDir() && !isSubvolume(workPath) {
		if err := os.RemoveAll(workPath); err != nil {
			log.Printf("Warning: failed to remove existing /work directory: %v", err)
		}
	}

	if meta.Work != "" {
		// Clone from work snap
		workSnapPath := filepath.Join(*flagSnapshotsDir, meta.Work)
		if _, err := os.Stat(workSnapPath); err != nil {
			return fmt.Errorf("work snap %s: %w", meta.Work, err)
		}
		cmd := exec.Command("btrfs", "subvolume", "snapshot", workSnapPath, workPath)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("btrfs snapshot work from %s to %s failed: %w\noutput: %s",
				workSnapPath, workPath, err, string(output))
		}
	} else {
		// Create empty work subvolume
		cmd := exec.Command("btrfs", "subvolume", "create", workPath)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("btrfs subvolume create work at %s failed: %w\noutput: %s",
				workPath, err, string(output))
		}
		// Chown to user 1000 (the standard container user)
		if err := os.Chown(workPath, 1000, 1000); err != nil {
			log.Printf("Warning: failed to chown work subvolume: %v", err)
		}
	}

	// Step 4: Compute taints as union of all component snaps' taints
	rootfsTaints := getSnapTaints(*flagSnapshotsDir, meta.Rootfs)
	homeTaints := getSnapTaints(*flagSnapshotsDir, meta.Home)
	workTaints := getSnapTaints(*flagSnapshotsDir, meta.Work)
	meta.Taints = UnionTaints(rootfsTaints, homeTaints, workTaints)

	// Step 5: Write frame.jsonc with updated taints
	if err := writeFrameMeta(rootFS, meta); err != nil {
		log.Printf("Warning: failed to write frame.jsonc for %s: %v", rootFS, err)
	}

	// Step 6: Write stamp file (rootfs snap ID for compatibility)
	if err := writeStampFile(rootFS, meta.Rootfs); err != nil {
		log.Printf("Warning: failed to write stamp file for %s: %v", rootFS, err)
	}

	// Step 7: Apply strip-uids to the rootfs
	stripOpts := tsm.StripOptions{ChownFiles: true}
	if err := tsm.StripRootfs(rootFS, stripOpts); err != nil {
		log.Printf("Warning: strip-uids on %s: %v", rootFS, err)
	}

	// Step 8: Ensure resolv.conf exists for DNS resolution inside the frame
	if err := ensureResolvConf(rootFS); err != nil {
		log.Printf("Warning: ensure resolv.conf on %s: %v", rootFS, err)
	}

	// Step 9: Ensure /tmp has correct permissions (1777 with sticky bit)
	if err := ensureTmpDir(rootFS); err != nil {
		log.Printf("Warning: ensure /tmp on %s: %v", rootFS, err)
	}

	// Step 10: Create /id subvolume for frame-local secrets (never persisted in snapshots)
	// This is always created fresh and empty, never cloned from a snapshot.
	// Since it's a btrfs subvolume, it's automatically excluded from snapshots.
	idPath := filepath.Join(rootFS, "id")
	// Remove existing /id directory if it's not a subvolume (from the rootfs snap)
	if fi, err := os.Stat(idPath); err == nil && fi.IsDir() && !isSubvolume(idPath) {
		if err := os.RemoveAll(idPath); err != nil {
			log.Printf("Warning: failed to remove existing /id directory: %v", err)
		}
	}
	if !isSubvolume(idPath) {
		cmd := exec.Command("btrfs", "subvolume", "create", idPath)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("btrfs subvolume create id at %s failed: %w\noutput: %s",
				idPath, err, string(output))
		}
		// Set permissions: 0700 (only root can access)
		if err := os.Chmod(idPath, 0700); err != nil {
			log.Printf("Warning: failed to chmod /id subvolume: %v", err)
		}
	}

	log.Printf("Created frame %s with rootfs:%s home:%s work:%s taints:%v",
		rootFS, meta.Rootfs, meta.Home, meta.Work, meta.Taints)
	return nil
}

// isSubvolume returns true if the path is a btrfs subvolume.
func isSubvolume(path string) bool {
	cmd := exec.Command("btrfs", "subvolume", "show", path)
	err := cmd.Run()
	return err == nil
}

// isDirEmpty returns true if the directory contains no files (ignoring . and ..).
func isDirEmpty(path string) bool {
	entries, err := os.ReadDir(path)
	if err != nil {
		return true // treat errors as empty
	}
	return len(entries) == 0
}

// ensureResolvConf copies the host's /etc/resolv.conf into the frame if
// the frame doesn't already have one. If there's an existing resolv.conf,
// it's backed up to resolv.conf.orig (but only if .orig doesn't exist).
func ensureResolvConf(rootFS string) error {
	frameResolvConf := filepath.Join(rootFS, "etc", "resolv.conf")
	frameResolvConfOrig := frameResolvConf + ".orig"
	hostResolvConf := "/etc/resolv.conf"

	// Read the host's resolv.conf
	hostData, err := os.ReadFile(hostResolvConf)
	if err != nil {
		return fmt.Errorf("reading host resolv.conf: %w", err)
	}

	// Check if frame already has a resolv.conf
	frameData, err := os.ReadFile(frameResolvConf)
	if err == nil {
		// Frame has an existing resolv.conf - check if it matches host
		if string(frameData) == string(hostData) {
			// Already matches, nothing to do
			return nil
		}
		// Different content - back up to .orig if .orig doesn't exist
		if _, err := os.Stat(frameResolvConfOrig); os.IsNotExist(err) {
			if err := os.WriteFile(frameResolvConfOrig, frameData, 0644); err != nil {
				log.Printf("Warning: failed to backup resolv.conf to %s: %v", frameResolvConfOrig, err)
			}
		}
	}

	// Ensure /etc directory exists
	etcDir := filepath.Join(rootFS, "etc")
	if err := os.MkdirAll(etcDir, 0755); err != nil {
		return fmt.Errorf("creating /etc directory: %w", err)
	}

	// Write the host's resolv.conf to the frame
	if err := os.WriteFile(frameResolvConf, hostData, 0644); err != nil {
		return fmt.Errorf("writing resolv.conf: %w", err)
	}

	return nil
}

// ensureTmpDir ensures /tmp exists with the correct permissions (1777 with sticky bit).
// Docker images sometimes have /tmp with wrong permissions, which breaks apt-get and
// other tools that need to create temp files.
func ensureTmpDir(rootFS string) error {
	tmpDir := filepath.Join(rootFS, "tmp")

	// Create /tmp if it doesn't exist
	if err := os.MkdirAll(tmpDir, 0777); err != nil {
		return fmt.Errorf("creating /tmp: %w", err)
	}

	// Set correct permissions: 1777 (sticky bit + world writable)
	// The sticky bit (01000) ensures users can only delete their own files
	if err := os.Chmod(tmpDir, 01777); err != nil {
		return fmt.Errorf("chmod /tmp: %w", err)
	}

	return nil
}

// copyTsBinary copies the ts binary into the container's /bin using btrfs reflink (COW copy).
// If the container has no /bin/sh, it also creates a symlink /bin/sh -> ts so that
// SSH commands work (ssh invokes /bin/sh -c "command"). The ts binary has a minimal
// shell mode that handles this case.
func copyTsBinary(rootFS string) error {
	// Remove legacy /sbin/ts if present (we moved to /bin/ts for PATH sanity).
	os.Remove(filepath.Join(rootFS, "sbin", "ts"))

	if err := copyBinaryToRootFS(rootFS, "ts", "bin/ts"); err != nil {
		return err
	}

	// If there's no shell, symlink /bin/sh -> ts so SSH command execution works.
	// ts has a minimal shell mode that activates when invoked as "sh".
	shPath := filepath.Join(rootFS, "bin", "sh")
	if _, err := os.Lstat(shPath); os.IsNotExist(err) {
		// No shell exists - create symlink to ts
		if err := os.Symlink("ts", shPath); err != nil {
			// Non-fatal: log but don't fail
			log.Printf("Warning: failed to create /bin/sh symlink: %v", err)
		}
	}

	return nil
}

// copyVshdBinary copies the vshd binary into the VM's /sbin using btrfs reflink (COW copy).
func copyVshdBinary(rootFS string) error {
	return copyBinaryToRootFS(rootFS, "vshd", "sbin/vshd")
}

// copyBinaryToRootFS copies a binary from the libexec directory into the rootfs.
func copyBinaryToRootFS(rootFS, binaryName, destPath string) error {
	src := filepath.Join(*flagLibexecDir, binaryName)

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
	unregisterActiveFrame(c.rootFS)
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
	mux.HandleFunc("/create", cs.handleCreate)
	mux.HandleFunc("/taint", cs.handleTaint)
	mux.HandleFunc("/delete-snap", handleDeleteSnap)
	mux.HandleFunc("/delete-frame", cs.handleDeleteFrame)
	mux.HandleFunc("/list-snaps", handleListSnaps)
	mux.HandleFunc("/list-frames", handleListFrames)
	mux.HandleFunc("/download-docker", handleDownloadDocker)
	mux.HandleFunc("/download-snap", handleDownloadSnap)
	mux.HandleFunc("/who-has", handleWhoHas)
	mux.HandleFunc("/ts/servers.json", handleServersJSONControl)
	cs.handler = mux

	go cs.serve()

	registerActiveFrame(rootFS)
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

// TaintRequest is the request body for /taint
type TaintRequest struct {
	TaintName string `json:"taint_name"`
}

// TaintResponse is the response from /taint
type TaintResponse struct {
	Status  string   `json:"status"`
	Message string   `json:"message,omitempty"`
	Taints  []string `json:"taints,omitempty"`
}

// handleTaint handles POST /taint - add a taint to the current frame
func (c *controlServer) handleTaint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req TaintRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.TaintName == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(TaintResponse{
			Status:  "error",
			Message: "taint_name is required",
		})
		return
	}

	// Read existing frame metadata
	frameMeta, err := readFrameMeta(c.rootFS)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(TaintResponse{
			Status:  "error",
			Message: fmt.Sprintf("read frame meta: %v", err),
		})
		return
	}

	// Create default frame metadata if none exists
	if frameMeta == nil {
		frameMeta = &FrameMeta{
			Rootfs: readStampFile(c.rootFS), // Use stamp file as rootfs ID
		}
	}

	// Add the taint if not already present
	found := false
	for _, t := range frameMeta.Taints {
		if t == req.TaintName {
			found = true
			break
		}
	}
	if !found {
		frameMeta.Taints = append(frameMeta.Taints, req.TaintName)
		// Keep taints sorted
		frameMeta.Taints = UnionTaints(frameMeta.Taints)
	}

	// Write updated frame metadata
	if err := writeFrameMeta(c.rootFS, frameMeta); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(TaintResponse{
			Status:  "error",
			Message: fmt.Sprintf("write frame meta: %v", err),
		})
		return
	}

	log.Printf("Added taint %q to %s, taints now: %v", req.TaintName, c.rootFS, frameMeta.Taints)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(TaintResponse{
		Status: "ok",
		Taints: frameMeta.Taints,
	})
}

// DeleteSnapRequest is the request body for /delete-snap
type DeleteSnapRequest struct {
	SnapshotID string `json:"snapshot_id"`
}

// DeleteSnapResponse is the response from /delete-snap
type DeleteSnapResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// handleDeleteSnap handles POST /delete-snap - delete a snapshot
// When deleting a snap that has children, updates the children to point
// to the deleted snap's parent, maintaining the parent chain integrity.
func handleDeleteSnap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req DeleteSnapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.SnapshotID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(DeleteSnapResponse{
			Status:  "error",
			Message: "snapshot_id is required",
		})
		return
	}

	// Check that the snapshot exists
	snapPath := filepath.Join(*flagSnapshotsDir, req.SnapshotID)
	if _, err := os.Stat(snapPath); os.IsNotExist(err) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(DeleteSnapResponse{
			Status:  "error",
			Message: fmt.Sprintf("snapshot %q not found", req.SnapshotID),
		})
		return
	}

	// Read the snap's metadata to get its parent
	snapMeta, _ := readSnapMeta(*flagSnapshotsDir, req.SnapshotID)
	var deletedParent string
	if snapMeta != nil {
		deletedParent = snapMeta.Parent
	}

	// Find all snaps that have this snap as their parent and update them
	if err := relinkSnapChildren(*flagSnapshotsDir, req.SnapshotID, deletedParent); err != nil {
		log.Printf("Warning: failed to relink children of %s: %v", req.SnapshotID, err)
	}

	// Delete the snapshot directory (btrfs subvolume)
	cmd := exec.Command("btrfs", "subvolume", "delete", snapPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(DeleteSnapResponse{
			Status:  "error",
			Message: fmt.Sprintf("btrfs subvolume delete failed: %v\noutput: %s", err, string(output)),
		})
		return
	}

	// Delete associated files
	os.Remove(snapPath + ".jsonc")  // metadata
	os.Remove(snapPath + ".stamp")  // stamp file
	os.Remove(snapPath + ".tsm")    // tsm manifest
	os.Remove(snapPath + ".tsc")    // tsc manifest

	log.Printf("Deleted snapshot %s", req.SnapshotID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(DeleteSnapResponse{
		Status: "ok",
	})
}

// relinkSnapChildren finds all snaps that have oldParent as their parent
// and updates them to point to newParent instead.
func relinkSnapChildren(snapshotsDir, oldParent, newParent string) error {
	entries, err := os.ReadDir(snapshotsDir)
	if err != nil {
		return fmt.Errorf("read snapshots dir: %w", err)
	}

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".jsonc") {
			continue
		}

		snapID := strings.TrimSuffix(entry.Name(), ".jsonc")
		if snapID == oldParent {
			continue // Skip the snap being deleted
		}

		meta, err := readSnapMeta(snapshotsDir, snapID)
		if err != nil || meta == nil {
			continue
		}

		if meta.Parent == oldParent {
			meta.Parent = newParent
			if err := writeSnapMeta(snapshotsDir, snapID, meta); err != nil {
				log.Printf("Warning: failed to update parent for snap %s: %v", snapID, err)
			} else {
				log.Printf("Relinked snap %s: parent changed from %s to %s", snapID, oldParent, newParent)
			}
		}
	}

	return nil
}

// DeleteFrameRequest is the request body for /delete-frame
type DeleteFrameRequest struct {
	FrameName string `json:"frame_name"`
}

// DeleteFrameResponse is the response from /delete-frame
type DeleteFrameResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// handleDeleteFrame handles POST /delete-frame - delete a frame
func (c *controlServer) handleDeleteFrame(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req DeleteFrameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.FrameName == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(DeleteFrameResponse{
			Status:  "error",
			Message: "frame_name is required",
		})
		return
	}

	// Extract tailscale user from rootFS path: /fs-dir/<tailscale-user>/<frame>
	rootFSRel, _ := filepath.Rel(*flagFsDir, c.rootFS)
	parts := strings.Split(rootFSRel, string(filepath.Separator))
	if len(parts) < 2 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(DeleteFrameResponse{
			Status:  "error",
			Message: "cannot determine tailscale user from rootFS path",
		})
		return
	}
	safeTailscaleUser := parts[0]

	// Sanitize frame name for filesystem path
	safeFrameName := sanitizeForPath(req.FrameName)

	// Build the target path
	framePath := filepath.Join(*flagFsDir, safeTailscaleUser, safeFrameName)

	// Check if frame exists
	if _, err := os.Stat(framePath); os.IsNotExist(err) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(DeleteFrameResponse{
			Status:  "error",
			Message: fmt.Sprintf("frame %q not found", req.FrameName),
		})
		return
	}

	// Prevent deleting the current frame
	if framePath == c.rootFS {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(DeleteFrameResponse{
			Status:  "error",
			Message: "cannot delete the currently active frame",
		})
		return
	}

	// Delete nested subvolumes first (home, work, id) if they exist
	homePath := filepath.Join(framePath, "home")
	workPath := filepath.Join(framePath, "work")
	idPath := filepath.Join(framePath, "id")

	if isSubvolume(homePath) {
		cmd := exec.Command("btrfs", "subvolume", "delete", homePath)
		if output, err := cmd.CombinedOutput(); err != nil {
			log.Printf("Warning: failed to delete home subvolume: %v\noutput: %s", err, string(output))
		}
	}

	if isSubvolume(workPath) {
		cmd := exec.Command("btrfs", "subvolume", "delete", workPath)
		if output, err := cmd.CombinedOutput(); err != nil {
			log.Printf("Warning: failed to delete work subvolume: %v\noutput: %s", err, string(output))
		}
	}

	if isSubvolume(idPath) {
		cmd := exec.Command("btrfs", "subvolume", "delete", idPath)
		if output, err := cmd.CombinedOutput(); err != nil {
			log.Printf("Warning: failed to delete id subvolume: %v\noutput: %s", err, string(output))
		}
	}

	// Delete the frame directory (btrfs subvolume)
	cmd := exec.Command("btrfs", "subvolume", "delete", framePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(DeleteFrameResponse{
			Status:  "error",
			Message: fmt.Sprintf("btrfs subvolume delete failed: %v\noutput: %s", err, string(output)),
		})
		return
	}

	// Delete the frame metadata file
	os.Remove(framePath + ".jsonc")

	log.Printf("Deleted frame %s", framePath)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(DeleteFrameResponse{
		Status: "ok",
	})
}

// ListSnapsResponse is the response from /list-snaps
type ListSnapsResponse struct {
	Status string     `json:"status"`
	Snaps  []SnapInfo `json:"snaps,omitempty"`
	Error  string     `json:"error,omitempty"`
}

// SnapInfo contains info about a single snapshot
type SnapInfo struct {
	ID   string `json:"id"`
	Size uint64 `json:"size"` // Total size in bytes from TSM header
}

// handleListSnaps handles GET /list-snaps - list all snapshots with sizes
func handleListSnaps(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	entries, err := os.ReadDir(*flagSnapshotsDir)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(ListSnapsResponse{
			Status: "error",
			Error:  fmt.Sprintf("read snapshots dir: %v", err),
		})
		return
	}

	var snaps []SnapInfo
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".tsm") {
			continue
		}

		snapID := strings.TrimSuffix(entry.Name(), ".tsm")

		// Read TSM to get size from header
		tsmPath := filepath.Join(*flagSnapshotsDir, entry.Name())
		tsmReader, err := tsm.ReadTSM(tsmPath)
		if err != nil {
			// If we can't read the TSM, skip this snap
			log.Printf("Warning: failed to read TSM for %s: %v", snapID, err)
			continue
		}

		snaps = append(snaps, SnapInfo{
			ID:   snapID,
			Size: tsmReader.Header.TotalSize,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ListSnapsResponse{
		Status: "ok",
		Snaps:  snaps,
	})
}

// ListFramesResponse is the response from /list-frames
type ListFramesResponse struct {
	Status string      `json:"status"`
	Frames []FrameInfo `json:"frames,omitempty"`
	Error  string      `json:"error,omitempty"`
}

// FrameInfo contains info about a single frame
type FrameInfo struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "stopped" or number of processes
}

// handleListFrames handles GET /list-frames - list all frames with status
func handleListFrames(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Walk the fs-dir to find all frames
	// Structure: fs-dir/<user>/<frame-name>/ with <frame-name>.jsonc metadata
	var frames []FrameInfo

	userEntries, err := os.ReadDir(*flagFsDir)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(ListFramesResponse{
			Status: "error",
			Error:  fmt.Sprintf("read fs dir: %v", err),
		})
		return
	}

	for _, userEntry := range userEntries {
		if !userEntry.IsDir() {
			continue
		}
		userName := userEntry.Name()
		userDir := filepath.Join(*flagFsDir, userName)

		frameEntries, err := os.ReadDir(userDir)
		if err != nil {
			continue
		}

		for _, frameEntry := range frameEntries {
			if !frameEntry.IsDir() {
				continue
			}
			frameName := frameEntry.Name()
			framePath := filepath.Join(userDir, frameName)

			// Check if metadata file exists to confirm it's a frame
			if _, err := os.Stat(framePath + ".jsonc"); os.IsNotExist(err) {
				continue
			}

			// Determine status based on active control servers
			sessionCount := getActiveFrameCount(framePath)
			status := "stopped"
			if sessionCount > 0 {
				status = fmt.Sprintf("%d", sessionCount)
			}

			frames = append(frames, FrameInfo{
				Name:   frameName,
				Status: status,
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ListFramesResponse{
		Status: "ok",
		Frames: frames,
	})
}

// CreateRequest is the request body for /create
type CreateRequest struct {
	FrameName  string `json:"frame_name"`
	SnapshotID string `json:"snapshot_id"` // Can be single ID or frame spec "rootfs:home:work"

	// Frame-specific fields (alternative to parsing snapshot_id)
	RootfsSnap string `json:"rootfs,omitempty"`    // Rootfs snap ID
	HomeSnap   string `json:"home,omitempty"`      // Home snap ID (empty = new empty subvolume)
	WorkSnap   string `json:"work,omitempty"`      // Work snap ID (empty = new empty subvolume)
	Isolation  string `json:"isolation,omitempty"` // "vm", "container", "none"
}

// parseFrameSpec parses a frame spec string "rootfs:home:work" into components.
// Returns rootfs, home, work snap IDs.
// The string "nil" is treated as empty (allows explicit empty components in frame specs).
func parseFrameSpec(spec string) (rootfs, home, work string) {
	parts := strings.Split(spec, ":")
	if len(parts) >= 1 {
		rootfs = parts[0]
		if rootfs == "nil" {
			rootfs = ""
		}
	}
	if len(parts) >= 2 {
		home = parts[1]
		if home == "nil" {
			home = ""
		}
	}
	if len(parts) >= 3 {
		work = parts[2]
		if work == "nil" {
			work = ""
		}
	}
	return
}

// isFrameSpec returns true if the spec contains ":" indicating a frame spec.
func isFrameSpec(spec string) bool {
	return strings.Contains(spec, ":")
}

// CreateResponse is the response from /create
type CreateResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	Path    string `json:"path,omitempty"`
}

// handleCreate handles POST /create - create a new frame from a snapshot
func (c *controlServer) handleCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Allow either snapshot_id or rootfs field for specifying the rootfs
	if req.RootfsSnap != "" && req.SnapshotID == "" {
		req.SnapshotID = req.RootfsSnap
	}

	if req.FrameName == "" || req.SnapshotID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(CreateResponse{
			Status:  "error",
			Message: "frame_name and snapshot_id (or rootfs) are required",
		})
		return
	}

	// Extract tailscale user from rootFS path: /fs-dir/<tailscale-user>/<frame>
	// The tailscale user is the second-to-last path component
	rootFSRel, _ := filepath.Rel(*flagFsDir, c.rootFS)
	parts := strings.Split(rootFSRel, string(filepath.Separator))
	if len(parts) < 2 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(CreateResponse{
			Status:  "error",
			Message: "cannot determine tailscale user from rootFS path",
		})
		return
	}
	safeTailscaleUser := parts[0]

	// Sanitize frame name for filesystem path
	safeFrameName := sanitizeForPath(req.FrameName)

	// Build the target path
	framePath := filepath.Join(*flagFsDir, safeTailscaleUser, safeFrameName)

	// Check if frame already exists
	if _, err := os.Stat(framePath); err == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(CreateResponse{
			Status:  "error",
			Message: fmt.Sprintf("frame %q already exists", req.FrameName),
		})
		return
	}

	// Check if streaming is requested
	stream := r.URL.Query().Get("stream") == "1"
	isTTY := r.URL.Query().Get("tty") == "1"

	if stream {
		handleCreateStreaming(w, req, framePath, isTTY)
		return
	}

	// Non-streaming mode - no auto-download, just check existence of rootfs snap
	rootfsSnap := req.SnapshotID
	if isFrameSpec(req.SnapshotID) {
		rootfsSnap, _, _ = parseFrameSpec(req.SnapshotID)
	}
	snapshotPath := filepath.Join(*flagSnapshotsDir, rootfsSnap)
	if _, err := os.Stat(snapshotPath); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(CreateResponse{
			Status:  "error",
			Message: fmt.Sprintf("snapshot %q not found", rootfsSnap),
		})
		return
	}

	// Create frame from the snapshot/frame spec
	if err := createFrame(framePath, req.SnapshotID, req.HomeSnap, req.WorkSnap, req.Isolation); err != nil {
		log.Printf("create frame failed: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(CreateResponse{
			Status:  "error",
			Message: err.Error(),
		})
		return
	}

	log.Printf("Created frame %s from snapshot %s", framePath, req.SnapshotID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(CreateResponse{
		Status: "ok",
		Path:   framePath,
	})
}

// CreateStreamEvent is an event in the streaming create response
type CreateStreamEvent struct {
	Type    string `json:"type"`              // "progress" or "result"
	Message string `json:"message,omitempty"` // progress message
	Status  string `json:"status,omitempty"`  // "ok" or "error" (for result)
	Path    string `json:"path,omitempty"`    // frame path (for result)
}

// handleCreateStreaming handles streaming create with auto-download
func handleCreateStreaming(w http.ResponseWriter, req CreateRequest, framePath string, isTTY bool) {
	w.Header().Set("Content-Type", "application/x-ndjson")

	// Enable streaming mode immediately
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	pw := &createProgressWriter{w: w, encoder: json.NewEncoder(w)}
	if f, ok := w.(http.Flusher); ok {
		pw.flusher = f
	}

	// Check if rootfs snapshot exists locally (parse frame spec if needed)
	rootfsSnap := req.SnapshotID
	if isFrameSpec(req.SnapshotID) {
		rootfsSnap, _, _ = parseFrameSpec(req.SnapshotID)
	}
	snapshotPath := filepath.Join(*flagSnapshotsDir, rootfsSnap)
	if _, err := os.Stat(snapshotPath); err != nil {
		// Snapshot doesn't exist - try to download it
		pw.writeProgress(fmt.Sprintf("Snapshot %s not found locally, downloading from mesh peers...", rootfsSnap))

		result, err := doDownloadSnap(rootfsSnap, pw, isTTY)
		if err != nil {
			log.Printf("create: auto-download of snapshot %s failed: %v", rootfsSnap, err)
			pw.writeResult("error", "", fmt.Sprintf("failed to download snapshot: %v", err))
			return
		}

		if result.AlreadyExists {
			pw.writeProgress("Snapshot already present locally")
		} else {
			pw.writeProgress("Downloaded snapshot from mesh peer")
		}
	}

	// Create frame from the snapshot/frame spec
	pw.writeProgress("Creating frame...")
	if err := createFrame(framePath, req.SnapshotID, req.HomeSnap, req.WorkSnap, req.Isolation); err != nil {
		log.Printf("create frame failed: %v", err)
		pw.writeResult("error", "", err.Error())
		return
	}

	log.Printf("Created frame %s from snapshot %s", framePath, req.SnapshotID)
	pw.writeResult("ok", framePath, "")
}

// createProgressWriter wraps ResponseWriter to write progress events
type createProgressWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	encoder *json.Encoder
}

func (pw *createProgressWriter) Write(p []byte) (n int, err error) {
	msg := strings.TrimSpace(string(p))
	if msg == "" {
		return len(p), nil
	}
	pw.writeProgress(msg)
	return len(p), nil
}

func (pw *createProgressWriter) writeProgress(msg string) {
	event := CreateStreamEvent{
		Type:    "progress",
		Message: msg,
	}
	pw.encoder.Encode(event)
	if pw.flusher != nil {
		pw.flusher.Flush()
	}
}

func (pw *createProgressWriter) writeResult(status, path, message string) {
	event := CreateStreamEvent{
		Type:    "result",
		Status:  status,
		Path:    path,
		Message: message,
	}
	pw.encoder.Encode(event)
	if pw.flusher != nil {
		pw.flusher.Flush()
	}
}

// createFrameFromSnapshot creates a new frame by cloning from a snapshot.
// This is similar to ensureRootFS but uses a specific snapshot ID instead of
// auto-detecting the source.
//
// If snapshotID contains ":" it's treated as a frame spec "rootfs:home:work".
func createFrameFromSnapshot(framePath, snapshotID string) error {
	return createFrame(framePath, snapshotID, "", "", "")
}

// createFrame creates a frame with explicit components.
// If homeSnap or workSnap are empty, empty subvolumes are created.
// If isolation is non-empty, it's stored in the frame metadata.
func createFrame(framePath, snapshotID, homeSnap, workSnap, isolation string) error {
	// Check if snapshotID is a frame spec
	rootfsSnap := snapshotID
	if isFrameSpec(snapshotID) {
		rootfsSnap, homeSnap, workSnap = parseFrameSpec(snapshotID)
	}

	// If we have any frame components, use the frame model
	if homeSnap != "" || workSnap != "" || strings.Contains(snapshotID, ":") {
		meta := &FrameMeta{
			Rootfs:    rootfsSnap,
			Home:      homeSnap,
			Work:      workSnap,
			Isolation: isolation,
		}
		// Write the frame.jsonc first so ensureFrameFS can find it
		if err := writeFrameMeta(framePath, meta); err != nil {
			return fmt.Errorf("write frame meta: %w", err)
		}
		if err := ensureFrameFS(framePath, meta); err != nil {
			return err
		}
		// Copy ts binary into the frame
		if err := copyTsBinary(framePath); err != nil {
			log.Printf("Warning: failed to copy ts binary to %s: %v", framePath, err)
		}
		return nil
	}

	// Legacy single-snapshot mode
	snapshotPath := filepath.Join(*flagSnapshotsDir, rootfsSnap)

	// Ensure the parent directory exists
	parentDir := filepath.Dir(framePath)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		return fmt.Errorf("creating parent directory: %w", err)
	}

	// Clone from the snapshot to the frame path
	cmd := exec.Command("btrfs", "subvolume", "snapshot", snapshotPath, framePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("btrfs snapshot from %s to %s failed: %w\noutput: %s",
			snapshotPath, framePath, err, string(output))
	}

	// Write stamp file for the live filesystem
	// The stamp contains the snapshot ID we cloned from
	if err := writeStampFile(framePath, rootfsSnap); err != nil {
		log.Printf("Warning: failed to write stamp file for %s: %v", framePath, err)
	}

	// Copy ts binary into the frame
	if err := copyTsBinary(framePath); err != nil {
		log.Printf("Warning: failed to copy ts binary to %s: %v", framePath, err)
	}

	// Apply the simplified UID model so this frame works regardless of
	// whether the source snapshot was stripped already. Idempotent: calling
	// it on an already-stripped tree leaves it unchanged.
	if err := tsm.StripRootfs(framePath, tsm.StripOptions{ChownFiles: true}); err != nil {
		log.Printf("Warning: strip-uids on %s: %v", framePath, err)
	}

	// Ensure resolv.conf exists for DNS resolution inside the frame
	if err := ensureResolvConf(framePath); err != nil {
		log.Printf("Warning: ensure resolv.conf on %s: %v", framePath, err)
	}

	// Ensure /tmp has correct permissions (1777 with sticky bit)
	if err := ensureTmpDir(framePath); err != nil {
		log.Printf("Warning: ensure /tmp on %s: %v", framePath, err)
	}

	return nil
}

// handleServersJSONControl handles GET /ts/servers.json on the control socket
// This allows ts inside containers to access the mesh peer list
var globalMeshState *meshState

func handleServersJSONControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if globalMeshState == nil {
		// Mesh not enabled, return empty list
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]meshPeer{})
		return
	}

	peers := globalMeshState.getPeersIncludingSelf()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(peers)
}

// WhoHasRequest is the request body for /who-has
type WhoHasRequest struct {
	SnapshotID string `json:"snapshot_id"`
}

// WhoHasResponse is the response from /who-has
type WhoHasResponse struct {
	Status string           `json:"status"`
	Peers  []WhoHasPeerInfo `json:"peers,omitempty"`
	Error  string           `json:"error,omitempty"`
}

// WhoHasPeerInfo represents a peer that has the snapshot
type WhoHasPeerInfo struct {
	Hostname string `json:"hostname"`
	URL      string `json:"url"`
}

// handleWhoHas handles POST /who-has - find which peers have a snapshot
func handleWhoHas(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req WhoHasRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.SnapshotID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(WhoHasResponse{
			Status: "error",
			Error:  "snapshot_id is required",
		})
		return
	}

	// Get mesh peers
	if globalMeshState == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(WhoHasResponse{
			Status: "error",
			Error:  "mesh not enabled",
		})
		return
	}

	meshPeers := globalMeshState.getPeersIncludingSelf()
	if len(meshPeers) == 0 {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(WhoHasResponse{
			Status: "error",
			Error:  "no mesh peers available",
		})
		return
	}

	// Convert to tsm.PeerInfo
	peers := make([]tsm.PeerInfo, len(meshPeers))
	for i, p := range meshPeers {
		peers[i] = tsm.PeerInfo{
			URL:      p.URL,
			Hostname: p.Hostname,
		}
	}

	// Check all peers for the snapshot
	results := tsm.CheckPeersForSnapshot(peers, req.SnapshotID)

	// Filter to peers that have the snapshot
	var peersWithSnap []WhoHasPeerInfo
	for _, r := range results {
		if r.HasSnap {
			peersWithSnap = append(peersWithSnap, WhoHasPeerInfo{
				Hostname: r.Hostname,
				URL:      r.PeerURL,
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(WhoHasResponse{
		Status: "ok",
		Peers:  peersWithSnap,
	})
}

// DownloadSnapRequest is the request body for /download-snap
type DownloadSnapRequest struct {
	SnapshotID string `json:"snapshot_id"`
}

// DownloadSnapResponse is the response from /download-snap
type DownloadSnapResponse struct {
	Status       string `json:"status"`
	Message      string `json:"message,omitempty"`
	SnapshotPath string `json:"snapshot_path,omitempty"`
	AlreadyHad   bool   `json:"already_had,omitempty"`
}

// DownloadSnapStreamEvent is an event in the streaming download response
type DownloadSnapStreamEvent struct {
	Type         string `json:"type"`                    // "progress" or "result"
	Message      string `json:"message,omitempty"`       // progress message
	Status       string `json:"status,omitempty"`        // "ok" or "error" (for result)
	SnapshotPath string `json:"snapshot_path,omitempty"` // path (for result)
	AlreadyHad   bool   `json:"already_had,omitempty"`   // if already present
}

// handleDownloadSnap handles POST /download-snap - download a snapshot from mesh peers
func handleDownloadSnap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req DownloadSnapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.SnapshotID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(DownloadSnapResponse{
			Status:  "error",
			Message: "snapshot_id is required",
		})
		return
	}

	// Check if streaming is requested
	stream := r.URL.Query().Get("stream") == "1"
	isTTY := r.URL.Query().Get("tty") == "1"

	if stream {
		handleDownloadSnapStreaming(w, req.SnapshotID, isTTY)
		return
	}

	// Non-streaming mode
	result, err := doDownloadSnap(req.SnapshotID, nil, false)
	if err != nil {
		log.Printf("download-snap failed for %s: %v", req.SnapshotID, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(DownloadSnapResponse{
			Status:  "error",
			Message: err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(DownloadSnapResponse{
		Status:       "ok",
		SnapshotPath: result.SnapshotPath,
		AlreadyHad:   result.AlreadyExists,
	})
}

// handleDownloadSnapStreaming handles streaming download with progress
func handleDownloadSnapStreaming(w http.ResponseWriter, snapshotID string, isTTY bool) {
	w.Header().Set("Content-Type", "application/x-ndjson")

	// Enable streaming mode immediately
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	pw := &downloadProgressWriter{w: w, encoder: json.NewEncoder(w)}
	if f, ok := w.(http.Flusher); ok {
		pw.flusher = f
	}

	result, err := doDownloadSnap(snapshotID, pw, isTTY)
	if err != nil {
		log.Printf("download-snap failed for %s: %v", snapshotID, err)
		pw.encoder.Encode(DownloadSnapStreamEvent{
			Type:    "result",
			Status:  "error",
			Message: err.Error(),
		})
		return
	}

	pw.encoder.Encode(DownloadSnapStreamEvent{
		Type:         "result",
		Status:       "ok",
		SnapshotPath: result.SnapshotPath,
		AlreadyHad:   result.AlreadyExists,
	})
}

// downloadProgressWriter wraps ResponseWriter to write progress events
type downloadProgressWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	encoder *json.Encoder
}

func (pw *downloadProgressWriter) Write(p []byte) (n int, err error) {
	msg := strings.TrimSpace(string(p))
	if msg == "" {
		return len(p), nil
	}
	event := DownloadSnapStreamEvent{
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

// doDownloadSnap performs the actual download operation using TSM/TSC format.
func doDownloadSnap(snapshotID string, progressWriter io.Writer, isTTY bool) (*tsm.DownloadResult, error) {
	// Check if snapshot already exists
	snapshotPath := filepath.Join(*flagSnapshotsDir, snapshotID)
	if _, err := os.Stat(snapshotPath); err == nil {
		return &tsm.DownloadResult{
			SnapshotPath:  snapshotPath,
			AlreadyExists: true,
		}, nil
	}

	// Get mesh peers
	if globalMeshState == nil {
		return nil, fmt.Errorf("mesh not enabled")
	}

	meshPeers := globalMeshState.getPeers()
	if len(meshPeers) == 0 {
		return nil, fmt.Errorf("no mesh peers available")
	}

	// Convert to tsm.PeerInfo
	peers := make([]tsm.PeerInfo, len(meshPeers))
	for i, p := range meshPeers {
		peers[i] = tsm.PeerInfo{
			URL:      p.URL,
			Hostname: p.Hostname,
		}
	}

	// Find a peer with the snapshot
	results := tsm.CheckPeersForSnapshot(peers, snapshotID)
	var peersWithSnap []tsm.PeerResult
	for _, r := range results {
		if r.HasSnap {
			peersWithSnap = append(peersWithSnap, r)
		}
	}

	if len(peersWithSnap) == 0 {
		return nil, fmt.Errorf("no peer has snapshot %s", snapshotID)
	}

	// Sort by hostname for determinism, pick first
	sort.Slice(peersWithSnap, func(i, j int) bool {
		return peersWithSnap[i].Hostname < peersWithSnap[j].Hostname
	})

	peer := peersWithSnap[0]
	baseURL := strings.TrimSuffix(peer.PeerURL, "/")

	// Download using TSM/TSC format
	opts := tsm.DownloadOptions{
		SnapshotID:     snapshotID,
		SnapshotsDir:   *flagSnapshotsDir,
		BaseURL:        baseURL,
		ProgressWriter: progressWriter,

		// Create the target directory as a btrfs subvolume.
		CreateTargetDir: func(path, parentStamp string) error {
			return createDownloadTargetDir(path, parentStamp, progressWriter)
		},

		// Clean up using btrfs subvolume delete since we created a subvolume
		CleanupTargetDir: func(path string) {
			exec.Command("btrfs", "subvolume", "delete", path).Run()
		},

		// Delete files that exist in the cloned parent but not in the new snapshot
		PrepareForFiles: func(targetDir string, fileList []string) error {
			return prepareDownloadDir(targetDir, fileList, progressWriter)
		},
	}

	return tsm.Download(opts)
}

// createDownloadTargetDir creates a btrfs subvolume for downloading a snapshot.
// If parentStamp (or one of its historical parents) exists locally as a subvolume,
// we clone from it instead of creating an empty subvolume - this allows unchanged
// files to be skipped during download.
func createDownloadTargetDir(path, parentStamp string, progress io.Writer) error {
	// Walk the parent chain to find a local ancestor we can clone from
	localAncestor := findLocalAncestor(parentStamp)

	if localAncestor != "" {
		// Clone from the local ancestor
		if progress != nil {
			fmt.Fprintf(progress, "Cloning from local ancestor %s...\n", filepath.Base(localAncestor))
		}
		cmd := exec.Command("btrfs", "subvolume", "snapshot", localAncestor, path)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("btrfs subvolume snapshot from %s to %s: %w\noutput: %s",
				localAncestor, path, err, output)
		}
		return nil
	}

	// No local ancestor found, create a fresh subvolume
	cmd := exec.Command("btrfs", "subvolume", "create", path)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("btrfs subvolume create %s: %w\noutput: %s", path, err, output)
	}
	return nil
}

// findLocalAncestor walks the parent chain starting from stampID and returns
// the path to the first snapshot that exists locally as a btrfs subvolume.
// Returns empty string if no local ancestor is found.
func findLocalAncestor(stampID string) string {
	// Limit the search depth to avoid infinite loops from circular references
	const maxDepth = 100

	currentID := stampID
	for i := 0; i < maxDepth && currentID != "" && currentID != "1"; i++ {
		snapPath := filepath.Join(*flagSnapshotsDir, currentID)

		// Check if this snapshot exists locally and is a btrfs subvolume
		if _, err := os.Stat(snapPath); err == nil {
			if isSubvolume(snapPath) {
				return snapPath
			}
			// Exists but not a subvolume - check its parent instead
		}

		// Look up this snapshot's parent from its stamp file
		currentID = readStampFile(snapPath)
	}

	// Also check if the base "1" snapshot exists (common ancestor for all)
	basePath := filepath.Join(*flagSnapshotsDir, "1")
	if _, err := os.Stat(basePath); err == nil && isSubvolume(basePath) {
		return basePath
	}

	return ""
}

// prepareDownloadDir removes files from targetDir that are not in fileList.
// This is used when we've cloned from a parent snapshot and need to delete
// files that were removed in the new snapshot.
func prepareDownloadDir(targetDir string, fileList []string, progress io.Writer) error {
	// Build a set of files that should exist
	shouldExist := make(map[string]bool)
	for _, f := range fileList {
		shouldExist[f] = true
		// Also mark parent directories as "should exist" to avoid deleting them
		dir := filepath.Dir(f)
		for dir != "." && dir != "/" {
			shouldExist[dir] = true
			dir = filepath.Dir(dir)
		}
	}

	// Walk the directory and find files to delete
	var toDelete []string
	err := filepath.Walk(targetDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == targetDir {
			return nil
		}

		relPath, err := filepath.Rel(targetDir, path)
		if err != nil {
			return err
		}

		if !shouldExist[relPath] {
			toDelete = append(toDelete, path)
			if info.IsDir() {
				return filepath.SkipDir // Don't recurse into directories we'll delete
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("scanning directory: %w", err)
	}

	// Delete files/dirs that shouldn't exist (in reverse order to handle nested dirs)
	for i := len(toDelete) - 1; i >= 0; i-- {
		path := toDelete[i]
		if err := os.RemoveAll(path); err != nil {
			log.Printf("Warning: failed to remove %s: %v", path, err)
		}
	}

	if len(toDelete) > 0 && progress != nil {
		fmt.Fprintf(progress, "Removed %d files/dirs not in new snapshot\n", len(toDelete))
	}

	return nil
}

// createSnapshot creates a read-only snapshot of the given rootFS in snapshots-dir.
// Returns the snapshot ID (based on the fidx checksum).
// If progressWriter is non-nil, progress updates are written to it.
func createSnapshot(rootFS string, progressWriter io.Writer, isTTY bool) (string, error) {
	// Check if this is a three-component frame (has nested home/work subvolumes)
	homePath := filepath.Join(rootFS, "home")
	workPath := filepath.Join(rootFS, "work")
	hasHomeSubvol := isSubvolume(homePath)
	hasWorkSubvol := isSubvolume(workPath)

	// Read the frame metadata for taints
	frameMeta, _ := readFrameMeta(rootFS)
	var frameTaints []string
	if frameMeta != nil {
		frameTaints = frameMeta.Taints
	}

	// Read the base stamp from the source rootFS to find parent snapshot
	baseStampID := readStampFile(rootFS)
	if baseStampID == "" {
		baseStampID = "1" // default
	}

	// Snapshot the rootfs (btrfs automatically excludes nested subvolumes)
	rootfsID, err := createSnapshotWithTaints(rootFS, baseStampID, frameTaints, progressWriter, isTTY)
	if err != nil {
		return "", fmt.Errorf("snapshot rootfs: %w", err)
	}

	// Update the source rootFS's stamp to point to the new snapshot
	if err := writeStampFile(rootFS, rootfsID); err != nil {
		log.Printf("Warning: failed to update stamp file for %s: %v", rootFS, err)
	}

	// If no nested subvolumes, return single ID (legacy format)
	if !hasHomeSubvol && !hasWorkSubvol {
		return rootfsID, nil
	}

	// Snapshot home if it's a subvolume and not empty
	homeID := ""
	if hasHomeSubvol && !isDirEmpty(homePath) {
		homeParent := ""
		if frameMeta != nil && frameMeta.Home != "" {
			homeParent = frameMeta.Home
		}
		homeID, err = createSnapshotWithTaints(homePath, homeParent, frameTaints, progressWriter, isTTY)
		if err != nil {
			return "", fmt.Errorf("snapshot home: %w", err)
		}
	}

	// Snapshot work if it's a subvolume and not empty
	workID := ""
	if hasWorkSubvol && !isDirEmpty(workPath) {
		workParent := ""
		if frameMeta != nil && frameMeta.Work != "" {
			workParent = frameMeta.Work
		}
		workID, err = createSnapshotWithTaints(workPath, workParent, frameTaints, progressWriter, isTTY)
		if err != nil {
			return "", fmt.Errorf("snapshot work: %w", err)
		}
	}

	// Update frame metadata with new snap IDs
	if frameMeta == nil {
		frameMeta = &FrameMeta{}
	}
	frameMeta.Rootfs = rootfsID
	frameMeta.Home = homeID
	frameMeta.Work = workID
	if err := writeFrameMeta(rootFS, frameMeta); err != nil {
		log.Printf("Warning: failed to update frame.jsonc for %s: %v", rootFS, err)
	}

	// Return frame spec format: rootfs:home:work
	// Use "nil" for empty components to avoid ambiguity with colons
	homeStr := homeID
	if homeStr == "" {
		homeStr = "nil"
	}
	workStr := workID
	if workStr == "" {
		workStr = "nil"
	}
	return fmt.Sprintf("%s:%s:%s", rootfsID, homeStr, workStr), nil
}

// createSnapshotWithFidx creates a read-only snapshot in snapshots-dir and generates
// fidx and tsm files for it. The snapshot is named after the SHA-256 of its TSM manifest.
// If a snapshot with the same SHA-256 already exists, it returns the existing ID
// and discards the new snapshot, performing taint intersection on the metadata.
//
// The process is:
// 1. Create btrfs snapshot to a random tmp name
// 2. Create mfidx (with --ref to parent if exists)
// 3. Create TSM/TSC manifests
// 4. Load TSM to get its SHA-256, use that as the final snapshot ID
// 5. If snapshot already exists with that ID, perform taint intersection and discard new one
// 6. Otherwise rename all files to the SHA-256-based final names
// 7. Create fidx of the fidx and write snap.jsonc metadata
func createSnapshotWithFidx(source, parentStampID string, progressWriter io.Writer, isTTY bool) (string, error) {
	return createSnapshotWithTaints(source, parentStampID, nil, progressWriter, isTTY)
}

// createSnapshotWithTaints is like createSnapshotWithFidx but accepts explicit taints.
// If taints is nil, taints are inherited from the parent snap.
func createSnapshotWithTaints(source, parentStampID string, taints []string, progressWriter io.Writer, isTTY bool) (string, error) {
	// Generate a random temporary ID for the work-in-progress snapshot
	tmpID, err := generateRandomID()
	if err != nil {
		return "", fmt.Errorf("generating temporary ID: %w", err)
	}

	tmpPath := filepath.Join(*flagSnapshotsDir, tmpID+".tmp")
	tmpTSMPath := tmpPath + ".tsm"
	tmpTSCPath := tmpPath + ".tsc"

	// Cleanup helper
	cleanupTmp := func() {
		exec.Command("btrfs", "subvolume", "delete", tmpPath).Run()
		os.Remove(tmpPath + ".stamp")
		os.Remove(tmpTSMPath)
		os.Remove(tmpTSCPath)
	}

	// Step 1: Create read-only btrfs snapshot to tmp path
	cmd := exec.Command("btrfs", "subvolume", "snapshot", "-r", source, tmpPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("btrfs snapshot failed: %w\noutput: %s", err, string(output))
	}

	// Write stamp file for the snapshot (in tmp location)
	if err := writeStampFile(tmpPath, parentStampID); err != nil {
		cleanupTmp()
		return "", fmt.Errorf("write stamp file: %w", err)
	}

	// Step 2: Create TSM/TSC manifests in tmp location
	tsmOpts := tsm.IndexerOptions{
		Progress:       false,
		ProgressWriter: progressWriter,
		IsTTY:          isTTY,
	}
	if err := tsm.Create(tmpPath, tmpPath, tsmOpts); err != nil {
		cleanupTmp()
		return "", fmt.Errorf("create tsm/tsc: %w", err)
	}

	// Step 3: Load TSM to get its SHA-256, which becomes the snapshot ID
	tsmReader, err := tsm.ReadTSM(tmpTSMPath)
	if err != nil {
		cleanupTmp()
		return "", fmt.Errorf("read tsm for checksum: %w", err)
	}
	snapshotID := hex.EncodeToString(tsmReader.SHA256[:])

	finalPath := filepath.Join(*flagSnapshotsDir, snapshotID)
	finalTSMPath := finalPath + ".tsm"
	finalTSCPath := finalPath + ".tsc"

	// Determine taints for this snapshot
	if taints == nil {
		// Inherit from parent if available
		taints = getSnapTaints(*flagSnapshotsDir, parentStampID)
	}

	// Step 4: Check if a snapshot with this SHA-256 already exists
	if _, err := os.Stat(finalPath); err == nil {
		// Snapshot already exists! Perform taint intersection and discard the new one.
		log.Printf("Snapshot %s already exists, checking taints", snapshotID)

		existingMeta, _ := readSnapMeta(*flagSnapshotsDir, snapshotID)
		if existingMeta != nil && len(taints) > 0 {
			// Taint intersection: if we can produce the same content with fewer taints,
			// the removed taints are not inherent to the content.
			intersected := IntersectTaints(existingMeta.Taints, taints)
			if !taintsEqual(existingMeta.Taints, intersected) {
				existingMeta.Taints = intersected
				if err := writeSnapMeta(*flagSnapshotsDir, snapshotID, existingMeta); err != nil {
					log.Printf("Warning: failed to update snap meta for taint intersection: %v", err)
				} else {
					log.Printf("Taint intersection for %s: %v", snapshotID, intersected)
				}
			}
		}

		cleanupTmp()
		return snapshotID, nil
	}

	// Step 5: Rename all to final names (order matters for consistency)
	// First the directory, then stamp, then index files
	if err := os.Rename(tmpPath, finalPath); err != nil {
		cleanupTmp()
		return "", fmt.Errorf("rename snapshot: %w", err)
	}
	// Also rename the stamp file
	os.Rename(tmpPath+".stamp", finalPath+".stamp")

	if err := os.Rename(tmpTSMPath, finalTSMPath); err != nil {
		log.Printf("Warning: failed to rename tsm: %v", err)
	}
	if err := os.Rename(tmpTSCPath, finalTSCPath); err != nil {
		log.Printf("Warning: failed to rename tsc: %v", err)
	}

	// Step 6: Write snap.jsonc metadata
	snapMeta := &SnapMeta{
		Parent: parentStampID,
		Taints: taints,
	}
	if err := writeSnapMeta(*flagSnapshotsDir, snapshotID, snapMeta); err != nil {
		log.Printf("Warning: failed to write snap.jsonc for %s: %v", snapshotID, err)
	}

	log.Printf("Created snapshot %s (SHA-256) with tsm/tsc", snapshotID)
	return snapshotID, nil
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

// getPeers returns a list of all known peers (excluding self)
func (m *meshState) getPeers() []meshPeer {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make([]meshPeer, 0, len(m.peers))
	for _, p := range m.peers {
		result = append(result, *p)
	}
	return result
}

// getPeersIncludingSelf returns a list of all known peers plus the local node.
// This is used by who-has to also check if the snapshot exists locally.
func (m *meshState) getPeersIncludingSelf() []meshPeer {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Start with self if we have a URL
	result := make([]meshPeer, 0, len(m.peers)+1)
	if m.myURL != "" {
		result = append(result, meshPeer{
			URL:      m.myURL,
			Hostname: m.myFQDN,
			LastSeen: time.Now(),
		})
	}

	// Add all known peers
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

// btrfsMagic is the magic number for btrfs filesystems (from statfs).
const btrfsMagic = 0x9123683E

// checkBtrfsFilesystems verifies that both directories exist, are on btrfs,
// and are on the same btrfs filesystem (required for subvolume snapshots).
func checkBtrfsFilesystems(fsDir, snapshotsDir string) error {
	// Ensure both directories exist
	if err := os.MkdirAll(fsDir, 0755); err != nil {
		return fmt.Errorf("creating fs-dir %s: %w", fsDir, err)
	}
	if err := os.MkdirAll(snapshotsDir, 0755); err != nil {
		return fmt.Errorf("creating snapshots-dir %s: %w", snapshotsDir, err)
	}

	// Check that fs-dir is on btrfs
	var fsDirStatfs syscall.Statfs_t
	if err := syscall.Statfs(fsDir, &fsDirStatfs); err != nil {
		return fmt.Errorf("statfs on fs-dir %s: %w", fsDir, err)
	}
	if fsDirStatfs.Type != btrfsMagic {
		return fmt.Errorf("-fs-dir %s is not on a btrfs filesystem (type=0x%x, need btrfs=0x%x)", fsDir, fsDirStatfs.Type, btrfsMagic)
	}

	// Check that snapshots-dir is on btrfs
	var snapshotsDirStatfs syscall.Statfs_t
	if err := syscall.Statfs(snapshotsDir, &snapshotsDirStatfs); err != nil {
		return fmt.Errorf("statfs on snapshots-dir %s: %w", snapshotsDir, err)
	}
	if snapshotsDirStatfs.Type != btrfsMagic {
		return fmt.Errorf("-snapshots-dir %s is not on a btrfs filesystem (type=0x%x, need btrfs=0x%x)", snapshotsDir, snapshotsDirStatfs.Type, btrfsMagic)
	}

	// Check that both are on the same filesystem by comparing device IDs
	var fsDirStat syscall.Stat_t
	if err := syscall.Stat(fsDir, &fsDirStat); err != nil {
		return fmt.Errorf("stat on fs-dir %s: %w", fsDir, err)
	}

	var snapshotsDirStat syscall.Stat_t
	if err := syscall.Stat(snapshotsDir, &snapshotsDirStat); err != nil {
		return fmt.Errorf("stat on snapshots-dir %s: %w", snapshotsDir, err)
	}

	if fsDirStat.Dev != snapshotsDirStat.Dev {
		return fmt.Errorf("-fs-dir and -snapshots-dir must be on the same btrfs filesystem for subvolume snapshots; fs-dir device=%d, snapshots-dir device=%d", fsDirStat.Dev, snapshotsDirStat.Dev)
	}

	return nil
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
