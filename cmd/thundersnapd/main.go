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
	"errors"
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

	"github.com/pborman/getopt/v2"
	ssh "github.com/tailscale/gliderssh"
	"github.com/tailscale/thundersnap/btrfsutil"
	"github.com/tailscale/thundersnap/cgroup"
	"github.com/tailscale/thundersnap/frameid"
	"github.com/tailscale/thundersnap/frames"
	"github.com/tailscale/thundersnap/refs"
	"github.com/tailscale/thundersnap/snapsubdir"
	"github.com/tailscale/thundersnap/thunderproto"
	"github.com/tailscale/thundersnap/thundersnap"
	"github.com/tailscale/thundersnap/tsm"
	gossh "golang.org/x/crypto/ssh"
	"tailscale.com/client/tailscale"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn"
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
	flagDataDir *string
	// flagFsDir and flagSnapsDir are derived from --data-dir
	// (<data-dir>/fs and <data-dir>/snaps) rather than being flags themselves.
	flagFsDir      *string
	flagSnapsDir   *string
	flagVmDir      *string
	flagLibexecDir *string
	flagPolicyPath *string
	flagMesh       *bool
	flagNfsd       *bool
	flagNfsPort    *int

	// globalPolicy holds the loaded policy file for grant matching
	globalPolicy *PolicyFile

	// authURLFile is the path where the server writes the auth URL
	// while waiting for Tailscale login. The --activate client reads it.
	authURLFile = "/run/thundersnap/auth-url"

	// statusFiles are the paths where the server writes its current status.
	// We write to both locations: /run for humans, /var/lib for --status client
	// (since /run/thundersnap/ is wiped by systemd when the service exits).
	statusFiles = []string{
		"/run/thundersnap/status",
		"/var/lib/thundersnap/status",
	}

	// controlSocket is the path for the local admin control socket.
	// Used for commands like --force-reauth that need to communicate with
	// the running daemon.
	controlSocket = "/run/thundersnap/control.sock"

	// globalTsnetHostname stores the tsnet hostname (FQDN) for use by VM sessions.
	// Set after tsnet.Server.Up() completes. Protected by globalTsnetHostnameMu.
	globalTsnetHostname   string
	globalTsnetHostnameMu sync.RWMutex

	// fsDirLibexec is the path to $fs-dir/libexec/ where binaries are cached
	// for btrfs reflink copying into frames. This is on the same filesystem
	// as frames, allowing reflinks to work even when the original libexec-dir
	// is on a different filesystem.
	fsDirLibexec string

	// testModeUser is set via --test-user and overrides the identity lookup
	// for all SSH connections. When non-empty, the daemon is in test mode.
	testModeUser string
)

// controlServerManager manages shared control servers for container sessions.
// Multiple SSH sessions to the same rootFS share one control server.
type controlServerManager struct {
	mu      sync.Mutex
	servers map[string]*managedControlServer // key: rootFS path
}

type managedControlServer struct {
	server   *controlServer
	refCount int
}

var controlServers = &controlServerManager{
	servers: make(map[string]*managedControlServer),
}

// getOrCreateControlServer returns an existing control server or creates a new one.
// The caller must call releaseControlServer when done.
func (m *controlServerManager) getOrCreateControlServer(rootFS string) (*controlServer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if ms, ok := m.servers[rootFS]; ok {
		ms.refCount++
		log.Printf("Reusing control server for %s (refCount=%d)", rootFS, ms.refCount)
		return ms.server, nil
	}

	// Create new control server with socket inside the rootFS.
	// Use short relative name "thunder.sock" to avoid Unix socket path length limits.
	sockPath := filepath.Join(rootFS, "thunder.sock")
	cs, err := startControlServer(sockPath, rootFS)
	if err != nil {
		return nil, err
	}

	m.servers[rootFS] = &managedControlServer{
		server:   cs,
		refCount: 1,
	}
	log.Printf("Created new control server for %s", rootFS)
	return cs, nil
}

// releaseControlServer decrements the reference count and closes the server if zero.
func (m *controlServerManager) releaseControlServer(rootFS string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ms, ok := m.servers[rootFS]
	if !ok {
		return
	}

	ms.refCount--
	log.Printf("Released control server for %s (refCount=%d)", rootFS, ms.refCount)
	if ms.refCount <= 0 {
		ms.server.Close()
		delete(m.servers, rootFS)
		log.Printf("Closed control server for %s", rootFS)
	}
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

// getTsnetHostname returns the current tsnet hostname (FQDN).
func getTsnetHostname() string {
	globalTsnetHostnameMu.RLock()
	defer globalTsnetHostnameMu.RUnlock()
	return globalTsnetHostname
}

// vmxSessionManager tracks running VMX isolation VMs.
// Each VM hosts multiple frames as containers.
// Keyed by "tailscaleUser/isolationName" (not vmUser like vmSessionManager).
type vmxSessionManager struct {
	mu       sync.Mutex
	sessions map[string]*managedVMXSession
}

// managedVMXSession represents an outer VM that hosts containers.
type managedVMXSession struct {
	session       *thundersnap.VMSession
	vsockPath     string
	refCount      int
	done          chan struct{}
	panicked      <-chan struct{}
	vmRootFS      string // the outer VM's minimal rootfs (fs-dir/<user>/.vmx-<isolation>/)
	tailscaleUser string
	isolationName string
}

var vmxSessions = &vmxSessionManager{
	sessions: make(map[string]*managedVMXSession),
}

// getOrCreateVMX returns an existing VMX session or creates a new one.
// The caller must call releaseVMX when done.
// userFsDir is the path to fs-dir/<user>/ which becomes the virtiofs root.
// initPrefix is the subdirectory within userFsDir containing the VMX rootfs (e.g., ".vmx-<isolation>").
func (m *vmxSessionManager) getOrCreateVMX(tailscaleUser, isolationName, userFsDir, initPrefix, vmDir string, controlHandler http.Handler) (*managedVMXSession, error) {
	key := tailscaleUser + "/" + isolationName

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
			log.Printf("VMX session %s: reusing existing session (refCount=%d)", key, ms.refCount)
			return ms, nil
		}
	}

	// Create new VMX session
	// The virtiofs root is fs-dir/<user>/, so:
	// - VMX rootfs binaries are at /<initPrefix>/bin/ts, /<initPrefix>/sbin/vshd
	// - Frame rootfs directories are at /<frame>/
	log.Printf("VMX session %s: starting new VM (userFsDir=%s, initPrefix=%s)", key, userFsDir, initPrefix)
	session, err := thundersnap.StartVM(thundersnap.VMConfig{
		RootFS:         userFsDir,
		VMDir:          vmDir,
		ControlHandler: controlHandler,
		Hostname:       getTsnetHostname(),
		InitPrefix:     initPrefix,
	})
	if err != nil {
		return nil, err
	}

	ms := &managedVMXSession{
		session:       session,
		vsockPath:     session.VshSocketPath(),
		refCount:      1,
		done:          make(chan struct{}),
		panicked:      session.Panicked(),
		vmRootFS:      filepath.Join(userFsDir, initPrefix),
		tailscaleUser: tailscaleUser,
		isolationName: isolationName,
	}

	// Monitor VM exit in background
	go func() {
		<-session.Done()
		close(ms.done)
		m.mu.Lock()
		delete(m.sessions, key)
		m.mu.Unlock()
		log.Printf("VMX session %s: VM exited, removed from manager", key)
	}()

	m.sessions[key] = ms
	return ms, nil
}

// releaseVMX decrements the reference count and shuts down the VM if it reaches zero.
func (m *vmxSessionManager) releaseVMX(tailscaleUser, isolationName string) {
	key := tailscaleUser + "/" + isolationName

	m.mu.Lock()
	defer m.mu.Unlock()

	ms, ok := m.sessions[key]
	if !ok {
		return
	}

	ms.refCount--
	log.Printf("VMX session %s: released (refCount=%d)", key, ms.refCount)

	if ms.refCount <= 0 {
		log.Printf("VMX session %s: no more clients, shutting down VM", key)
		ms.session.Close()
		delete(m.sessions, key)
	}
}

// hostVshdManager runs a single host-mode vshd process listening on a Unix
// socket. Host container sessions dial this socket and speak the same vshd wire
// protocol as VM sessions, so runContainerSession and runVMXSession share one
// transport (proxyVshdSession). The vshd process itself anchors the shared
// PID/mount/UTS namespaces per container rootfs via containerns.Manager, exactly
// as the in-VM vshd does, keeping host and VM enter-container-ns code identical.
//
// One vshd serves every frame: per-session the daemon sends the frame rootfs in
// the VMX request header, and vshd's buildSessionCmd joins/creates that frame's
// namespace. The process is started lazily on first session and reused.
type hostVshdManager struct {
	mu       sync.Mutex
	cmd      *exec.Cmd
	sockPath string
}

var hostVshd = &hostVshdManager{}

// ensure starts the host vshd process (idempotently) and returns the Unix
// socket path that sessions should dial.
func (m *hostVshdManager) ensure() (sockPath string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cmd != nil && m.cmd.Process != nil {
		// Verify it is still alive (signal 0 is an existence probe).
		if perr := m.cmd.Process.Signal(syscall.Signal(0)); perr == nil {
			return m.sockPath, nil
		}
		// Died - reap and restart below.
		log.Printf("host vshd died, restarting")
		m.cmd.Wait()
		m.cmd = nil
	}

	vshdBin := filepath.Join(fsDirLibexec, "vshd")
	tsBin := filepath.Join(fsDirLibexec, "ts")
	sockDir := filepath.Dir(controlSocket) // /run/thundersnap
	if err := os.MkdirAll(sockDir, 0755); err != nil {
		return "", fmt.Errorf("create host vshd socket dir: %w", err)
	}
	sockPath = filepath.Join(sockDir, "host-vshd.sock")
	os.Remove(sockPath)

	// Pass the daemon's cgroup parent so host vshd applies the same per-session
	// memory/pids/cpu limits the daemon used to apply itself, preserving
	// fork-bomb/OOM protection now that the session child is spawned by vshd.
	cmd := exec.Command(vshdBin,
		"--unix="+sockPath,
		"--ts="+tsBin,
		"--cgroup-parent="+cgroupManager.ParentName(),
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start host vshd: %w", err)
	}

	// Wait for the socket to appear (up to ~5s) so the first dial does not race
	// vshd's listen.
	for i := 0; i < 50; i++ {
		if _, statErr := os.Stat(sockPath); statErr == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	m.cmd = cmd
	m.sockPath = sockPath
	log.Printf("host vshd started (pid %d) listening on %s", cmd.Process.Pid, sockPath)
	return sockPath, nil
}

func main() {
	activate := getopt.BoolLong("activate", 0, "Print the Tailscale auth URL and wait for login to complete")
	showStatus := getopt.BoolLong("status", 0, "Print the current server status and exit")
	forceReauth := getopt.BoolLong("force-reauth", 0, "Force re-authentication with Tailscale")
	hostname := getopt.StringLong("hostname", 0, "thundersnap", "Tailscale hostname for this server")
	stateDir := getopt.StringLong("state-dir", 0, "", "Directory to store Tailscale state (default: ~/.config/thundersnapd)")
	flagDataDir = getopt.StringLong("data-dir", 0, "/var/lib/thundersnap", "Directory holding all thundersnap data (live filesystems in <data-dir>/fs and base snapshots in <data-dir>/snaps)")
	flagVmDir = getopt.StringLong("vm-dir", 0, "", "Directory containing cloud-hypervisor and vmlinux (default: <exe-dir>/vm)")
	flagLibexecDir = getopt.StringLong("libexec-dir", 0, "", "Directory containing helper binaries like ts and vshd (default: <exe-dir>)")
	flagPolicyPath = getopt.StringLong("policy", 0, "", "Path to policy file (required)")
	flagMesh = getopt.BoolLong("mesh", 0, "Enable mesh discovery: ping other thundersnap nodes and serve /bupdate/")
	flagNfsd = getopt.BoolLong("nfsd", 0, "Enable NFSv4 server to export the snaps directory")
	flagNfsPort = getopt.IntLong("nfs-port", 0, 2049, "Port for NFSv4 server")
	testListen := getopt.StringLong("test-listen", 0, "", "Test mode: listen on this local TCP address (e.g. 127.0.0.1:2222) instead of tsnet")
	testUser := getopt.StringLong("test-user", 0, "", "Test mode: use this identity for all SSH connections (e.g. test@example.com)")
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

	if *flagDataDir == "" {
		log.Fatalf("-data-dir is required")
	}
	// Derive the live-filesystem and snapshot directories from the single
	// --data-dir. Keeping them as separate internal variables avoids touching
	// the many call sites that already use fs-dir/snaps-dir paths, and
	// guarantees both live on the same btrfs filesystem.
	fsDir, snapsDir := deriveDataDirs(*flagDataDir)
	flagFsDir = &fsDir
	flagSnapsDir = &snapsDir
	if *flagPolicyPath == "" {
		log.Fatalf("-policy is required")
	}

	// Load policy file
	var err error
	globalPolicy, err = LoadPolicyFile(*flagPolicyPath)
	if err != nil {
		fatalWithStatus("Failed to load policy file: %v", err)
	}
	log.Printf("Loaded policy with %d grants", len(globalPolicy.Grants))

	// Verify both directories are on btrfs and on the same filesystem
	if err := checkBtrfsFilesystems(*flagFsDir, *flagSnapsDir); err != nil {
		fatalWithStatus("%v", err)
	}

	// Set default vm-dir and libexec-dir relative to executable
	// (must be done before setupFsDirLibexec)
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

	// Set up fs-dir/libexec directory with copies of binaries.
	// This ensures binaries are on the same btrfs filesystem as frames,
	// allowing reflink copies to work even when the original libexec-dir
	// is on a different filesystem.
	if err := setupFsDirLibexec(); err != nil {
		fatalWithStatus("Failed to set up fs-dir libexec: %v", err)
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

	// Initialize ref and frame stores for the new UUID-based API. These are
	// per-user: a per-user refs.Store appends "refs/<user>" and a per-user
	// frames.Store appends "fs/<user>", so both are rooted at the data dir
	// (NOT the fs dir, which would double the "fs" component).
	initRefStore(*flagDataDir)
	initFrameStore(*flagDataDir)

	// In test mode (--test-listen), skip tsnet and listen on local TCP.
	// testModeUser will be used for identity instead of WhoIs.
	if *testListen != "" {
		if *testUser == "" {
			log.Fatalf("--test-listen requires --test-user")
		}
		testModeUser = *testUser
		log.Printf("TEST MODE: listening on %s as user %q", *testListen, testModeUser)
		runTestMode(*testListen, *stateDir)
		return
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
			// Also write status file to indicate we're waiting for auth.
			const prefix = "or go to: "
			if idx := strings.Index(msg, prefix); idx != -1 {
				url := strings.TrimSpace(msg[idx+len(prefix):])
				if url != "" {
					os.WriteFile(authURLFile, []byte(url+"\n"), 0600)
					writeStatusWaitingForAuth(url)
				}
			}
		},
	}
	defer srv.Close()

	// Start the tsnet server and wait for it to be ready
	log.Printf("Starting tsnet server with hostname %q...", *hostname)
	status, err := srv.Up(context.Background())
	if err != nil {
		fatalWithStatus("Failed to start tsnet server: %v", err)
	}
	// Auth is complete; remove the auth URL file if it was written.
	os.Remove(authURLFile)
	log.Printf("tsnet server is up! Tailscale IP: %v", status.TailscaleIPs)

	// Store the tsnet hostname (FQDN) globally for VM sessions
	if status.Self != nil && status.Self.DNSName != "" {
		globalTsnetHostnameMu.Lock()
		globalTsnetHostname = strings.TrimSuffix(status.Self.DNSName, ".")
		globalTsnetHostnameMu.Unlock()
		log.Printf("tsnet hostname: %s", globalTsnetHostname)
	}

	// Listen on port 22 for SSH connections
	ln, err := srv.Listen("tcp", ":22")
	if err != nil {
		fatalWithStatus("Failed to listen on :22: %v", err)
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

	// Ensure the hostname is set with the control server.
	// When the node already has state, tsnet.Server.Hostname only affects the initial
	// registration. We need to call EditPrefs to update the hostname for existing nodes.
	if _, err := lc.EditPrefs(context.Background(), &ipn.MaskedPrefs{
		Prefs:       ipn.Prefs{Hostname: *hostname},
		HostnameSet: true,
	}); err != nil {
		log.Printf("Warning: failed to set hostname via EditPrefs: %v", err)
	} else {
		log.Printf("Hostname set to %q via EditPrefs", *hostname)
	}

	// Write status file with current server info
	writeStatusFile(status)

	// Start admin control socket for local commands like --force-reauth
	go startAdminControlSocket(lc)

	// Create SSH server with gliderlabs/ssh and persistent host key
	sshServer := newSSHServer(lc)

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

	// File server for bupdate (serves -snaps-dir contents)
	bupdateServer := &bupdateFileServer{root: *flagSnapsDir}
	httpMux.Handle("/bupdate/", http.StripPrefix("/bupdate", bupdateServer))

	// Prometheus metrics endpoint (OS-level + thundersnap counts).
	registerMetrics(httpMux, *flagFsDir, *flagSnapsDir)

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

		nfsSrv, err := startNFSServer(*flagSnapsDir, nfsLn)
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

// runTestMode runs the daemon in test mode: listening on a local TCP port
// instead of tsnet, using testModeUser for identity. This enables e2e tests
// to connect via SSH without a real Tailscale network.
func runTestMode(listenAddr, stateDir string) {
	// In test mode, use stateDir for the control socket instead of /run/thundersnap
	// so each test daemon instance has its own socket path.
	controlSocket = filepath.Join(stateDir, "control.sock")

	// Ensure SSH host key exists
	hostKeyPath := filepath.Join(stateDir, "ssh_host_ed25519_key")
	if err := ensureHostKey(hostKeyPath); err != nil {
		log.Fatalf("Failed to ensure host key: %v", err)
	}

	// Listen on local TCP
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", listenAddr, err)
	}
	defer ln.Close()

	log.Printf("SSH server listening on %s (test mode)", listenAddr)

	// Create SSH server (no LocalClient needed - testModeUser is set)
	sshServer := newSSHServer(nil)

	// Load the persistent host key
	if err := ssh.HostKeyFile(hostKeyPath)(sshServer); err != nil {
		log.Fatalf("Failed to load host key: %v", err)
	}

	// Serve SSH connections
	log.Printf("Waiting for SSH connections...")
	if err := sshServer.Serve(ln); err != nil {
		log.Fatalf("SSH server error: %v", err)
	}
}

// newSSHServer creates an SSH server with the standard handler configuration.
// The LocalClient lc may be nil in test mode (when testModeUser is set).
func newSSHServer(lc *tailscale.LocalClient) *ssh.Server {
	forwardHandler := &ssh.ForwardedTCPHandler{}
	return &ssh.Server{
		Handler: func(s ssh.Session) {
			log.Printf("New SSH session from %s (user: %s)", s.RemoteAddr(), s.User())

			// Look up the Tailscale identity of the connecting peer.
			// In test mode (--test-user), use the configured test user.
			var who *apitype.WhoIsResponse
			tailscaleUser := "unknown"
			if testModeUser != "" {
				tailscaleUser = testModeUser
				// In test mode, who stays nil and we use DefaultCap
			} else {
				who = getWhoIs(s.Context(), lc, s.RemoteAddr().String())
				if who != nil {
					if who.Node != nil && len(who.Node.Tags) > 0 {
						tailscaleUser = fmt.Sprintf("tags: %s", strings.Join(who.Node.Tags, ", "))
					} else if who.UserProfile != nil && who.UserProfile.LoginName != "" {
						tailscaleUser = who.UserProfile.LoginName
					}
				}
			}

			// Resolve capability from policy (in test mode uses DefaultCap)
			cap := ResolveCap(who, globalPolicy)
			log.Printf("Resolved cap for %s: role=%s isolation=%s", tailscaleUser, cap.Role, cap.Isolation)

			// Helper to log error to both server log and client
			logErr := func(format string, args ...any) {
				msg := fmt.Sprintf(format, args...)
				log.Print(msg)
				fmt.Fprintf(s, "* Error: %s\r\n", msg)
			}

			// Parse SSH username to extract target user and frame name.
			// See parseSSHUser for format documentation.
			parsedIsolation, vmxIsolation, targetUser, frameName := parseSSHUser(s.User())
			if parsedIsolation != "container" {
				cap.Isolation = parsedIsolation
			}

			// Resolve the frame name to its canonical fs/<user>/<uuid> path via
			// the user's ref store solely to choose the Unix user shown in the
			// greeting. Resolution errors (e.g. an unknown frame name) are
			// ignored here and surfaced authoritatively by the session function
			// below; the greeting just falls back to the requested target user.
			runAsUser := targetUser
			if rootFS, _, rerr := resolveFrameRootFS(tailscaleUser, frameName); rerr == nil {
				runAsUser = selectTargetUser(rootFS, targetUser)
			}

			// Only greet the user in interactive mode. When a subcommand is
			// supplied (e.g. `ssh host -- some-cmd`, scp, rsync) the greeting
			// would corrupt the program's output, so suppress it.
			interactive := isInteractiveSession(s.RawCommand())
			greet := func(format string, args ...any) {
				if interactive {
					fmt.Fprintf(s.Stderr(), format, args...)
				}
			}

			// Route based on isolation level
			switch cap.Isolation {
			case "vmx":
				if frameName == "" {
					// Direct shell into outer VM
					greet("* Hello <%s>, connecting you to outer VM <%s>\r\n", tailscaleUser, vmxIsolation)
					if err := runVMXOuterShell(s, tailscaleUser, vmxIsolation, targetUser, logErr); err != nil {
						logErr("VMX outer shell failed: %v", err)
						s.Exit(1)
					}
				} else {
					// Container inside VM
					greet("* Hello <%s>, connecting you to <%s> in <%s> (VMX/%s)\r\n", tailscaleUser, runAsUser, frameName, vmxIsolation)
					if err := runVMXSession(s, tailscaleUser, vmxIsolation, frameName, targetUser, logErr); err != nil {
						logErr("VMX session failed: %v", err)
						s.Exit(1)
					}
				}
			default:
				// "container" is the default; "none" (no host isolation) takes
				// the same code path and differs only in the greeting suffix.
				suffix := ""
				if cap.Isolation == "none" {
					suffix = " (no isolation)"
				}
				greet("* Hello <%s>, connecting you to <%s> in <%s>%s\r\n", tailscaleUser, runAsUser, frameName, suffix)
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
				tailscaleUser := testModeUser
				if tailscaleUser == "" {
					tailscaleUser = getTailscaleUser(s.Context(), lc, s.RemoteAddr().String())
				}
				sshUser := s.User()

				// Parse user@container format
				targetUser := ""
				containerName := sshUser
				if idx := strings.Index(sshUser, "@"); idx != -1 {
					targetUser = sshUser[:idx]
					containerName = sshUser[idx+1:]
				}

				// Resolve the frame name to its canonical fs/<user>/<uuid> path
				// via the user's ref store, then set up the root filesystem
				// (same setup as container sessions).
				rootFS, _, err := resolveFrameRootFS(tailscaleUser, containerName)
				if err != nil {
					log.Printf("SFTP session failed: %v", err)
					return
				}
				if err := prepareContainerRootFS(rootFS, ""); err != nil {
					log.Printf("SFTP session failed: %v", err)
					return
				}

				if err := runSFTPSession(s, rootFS, targetUser); err != nil {
					log.Printf("SFTP session failed: %v", err)
				}
			},
		},
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
// Reads from the persistent location (/var/lib) since /run is wiped on exit.
func runStatus() {
	// Try each status file location, preferring the persistent one
	var data []byte
	var lastErr error
	var lastPath string
	for _, path := range statusFiles {
		var err error
		data, err = os.ReadFile(path)
		if err == nil {
			fmt.Print(string(data))
			return
		}
		lastErr = err
		lastPath = path
	}

	// All locations failed
	if os.IsNotExist(lastErr) {
		fmt.Fprintf(os.Stderr, "No status file found.\nThe server may not be running or not yet authenticated.\n")
		os.Exit(1)
	}
	var errno syscall.Errno
	if errors.As(lastErr, &errno) {
		fmt.Fprintf(os.Stderr, "%s: %s\n", lastPath, errno)
	} else {
		fmt.Fprintf(os.Stderr, "%s: %v\n", lastPath, lastErr)
	}
	fmt.Fprintf(os.Stderr, "Try running as root.\n")
	os.Exit(1)
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

// writeStatusToAllFiles writes content to all status file locations.
// We write to both /run (for humans) and /var/lib (for --status client,
// since /run/thundersnap/ is wiped by systemd when the service exits).
func writeStatusToAllFiles(content string) {
	for _, path := range statusFiles {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			log.Printf("Warning: failed to create status file directory %s: %v", filepath.Dir(path), err)
			continue
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			log.Printf("Warning: failed to write status file %s: %v", path, err)
		}
	}
}

// writeStatusFile writes the current server status to all status files.
func writeStatusFile(status *ipnstate.Status) {
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

	writeStatusToAllFiles(buf.String())
}

// writeStatusWaitingForAuth writes a status file indicating auth is needed.
func writeStatusWaitingForAuth(authURL string) {
	var buf strings.Builder
	buf.WriteString("state: waiting_for_auth\n")
	buf.WriteString(fmt.Sprintf("auth_url: %s\n", authURL))
	writeStatusToAllFiles(buf.String())
}

// writeStatusError writes a fatal error to the status file before exiting.
func writeStatusError(errMsg string) {
	writeStatusToAllFiles(fmt.Sprintf("error: %s\n", errMsg))
}

// fatalWithStatus logs a fatal error, writes it to the status file, and exits.
func fatalWithStatus(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	writeStatusError(msg)
	log.Fatalf(format, a...)
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
// In test mode (--test-user set), returns the configured test user instead.
func getTailscaleUser(ctx context.Context, lc *tailscale.LocalClient, remoteAddr string) string {
	// In test mode, return the configured test user for all connections.
	if testModeUser != "" {
		return testModeUser
	}

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

// cgroupManager owns the per-instance parent cgroup and applies resource limits
// to container processes. The parent name embeds the daemon PID so multiple or
// nested instances on the same machine do not collide.
var cgroupManager = cgroup.New(fmt.Sprintf("thundersnap-%d", os.Getpid()))

// btrfsCreateSubvol creates a new empty btrfs subvolume at path.
func btrfsCreateSubvol(path string) error {
	return btrfsutil.CreateSubvol(path)
}

// btrfsSnapshot creates a btrfs snapshot of src at dst. When readonly is true
// the snapshot is created with -r (used for the immutable snaps-dir entries).
func btrfsSnapshot(src, dst string, readonly bool) error {
	return btrfsutil.Snapshot(src, dst, readonly)
}

// btrfsDeleteSubvol deletes the btrfs subvolume at path.
func btrfsDeleteSubvol(path string) error {
	return btrfsutil.DeleteSubvol(path)
}

// deriveDataDirs splits the single --data-dir into the live-filesystem
// ("<dataDir>/fs") and snapshot ("<dataDir>/snaps") directories. Both are
// children of the same data dir, which is what guarantees they land on the
// same btrfs filesystem (load-bearing for the same-filesystem snapshot check).
func deriveDataDirs(dataDir string) (fsDir, snapsDir string) {
	return filepath.Join(dataDir, "fs"), filepath.Join(dataDir, "snaps")
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

// tailscaleUserFromRootFS extracts the tailscale user from a frame's rootFS
// path, which has the shape "<fsDir>/<tailscale-user>/<frame>". The user is the
// first path component relative to flagFsDir; an error is returned when the
// relative path has fewer than two components (so the user can't be determined).
func tailscaleUserFromRootFS(rootFS string) (string, error) {
	rootFSRel, _ := filepath.Rel(*flagFsDir, rootFS)
	parts := strings.Split(rootFSRel, string(filepath.Separator))
	if len(parts) < 2 {
		return "", fmt.Errorf("cannot determine tailscale user from rootFS path")
	}
	return parts[0], nil
}

// stripDomain removes the @domain part from a username (e.g., "user@example.com" -> "user")
func stripDomain(s string) string {
	if idx := strings.Index(s, "@"); idx != -1 {
		return s[:idx]
	}
	return s
}

// parseSSHUser parses an SSH username into isolation mode and frame information.
// Returns (isolation, vmxIsolation, targetUser, frameName).
//
// Formats:
//   - <user>@vmx/<isolation>/<frame>  - container inside named VM, as user
//   - vmx/<isolation>/<user>@<frame>  - same, alternate syntax
//   - vmx/<isolation>/<frame>         - container inside named VM
//   - <user>@vmx/<isolation>          - shell into outer VM, as user
//   - vmx/<isolation>                 - shell into outer VM directly
//   - <user>@vm/<frame>               - sugar for vmx/default/<frame>, as user
//   - vm/<user>@<frame>               - same, alternate syntax
//   - vm/<frame>                      - sugar for vmx/default/<frame>
//   - <user>@<frame>                  - container as user
//   - <frame>                         - container (default)
//
// isInteractiveSession reports whether an SSH session is interactive (a login
// shell) rather than a subcommand invocation. rawCommand is the raw command
// string from the SSH session (ssh.Session.RawCommand): it is empty for an
// interactive shell and non-empty when a command was supplied (e.g.
// `ssh host -- cmd`, scp, or rsync). The greeting banner is only shown for
// interactive sessions, since printing it would corrupt subcommand output.
func isInteractiveSession(rawCommand string) bool {
	return rawCommand == ""
}

func parseSSHUser(sshUser string) (isolation, vmxIsolation, targetUser, frameName string) {
	isolation = "container"

	// First, check if there's a user@ prefix before the mode prefix (vmx/ or vm/)
	// This handles cases like "root@vmx/dev/frame" or "root@vm/frame"
	var modePrefix string
	if idx := strings.Index(sshUser, "@"); idx != -1 {
		afterAt := sshUser[idx+1:]
		if strings.HasPrefix(afterAt, "vmx/") || strings.HasPrefix(afterAt, "vm/") {
			targetUser = sshUser[:idx]
			sshUser = afterAt
		}
	}

	// Check for vmx/ prefix (containers inside named VMs)
	if strings.HasPrefix(sshUser, "vmx/") {
		isolation = "vmx"
		rest := strings.TrimPrefix(sshUser, "vmx/")
		if slashIdx := strings.Index(rest, "/"); slashIdx >= 0 {
			vmxIsolation = rest[:slashIdx]
			modePrefix = rest[slashIdx+1:] // frame name (may be empty for outer shell)
		} else {
			vmxIsolation = rest
			modePrefix = "" // direct shell into outer VM
		}
	} else if strings.HasPrefix(sshUser, "vm/") {
		// Legacy support: vm/<frame> becomes vmx/default/<frame>
		isolation = "vmx"
		vmxIsolation = "default"
		modePrefix = strings.TrimPrefix(sshUser, "vm/")
	} else {
		// No mode prefix, just frame name (possibly with user@)
		modePrefix = sshUser
	}

	// Parse optional user prefix in frame: <user>@<name> or <name>
	// (only if we didn't already extract user from before the mode prefix)
	frameName = modePrefix
	if targetUser == "" {
		if idx := strings.Index(modePrefix, "@"); idx != -1 {
			targetUser = modePrefix[:idx]
			frameName = modePrefix[idx+1:]
		}
	}

	return isolation, vmxIsolation, targetUser, frameName
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

// isSubvolume returns true if the path is a btrfs subvolume.
func isSubvolume(path string) bool {
	return btrfsutil.IsSubvolume(path)
}

// isDirEmpty returns true if the directory contains no files (ignoring . and ..).
func isDirEmpty(path string) bool {
	entries, err := os.ReadDir(path)
	if err != nil {
		return true // treat errors as empty
	}
	return len(entries) == 0
}

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
//
// To avoid Unix socket path length limits (~108 chars), the socket is bound
// using a relative path from within rootFS. The full sockPath is still stored
// for cleanup.
func startControlServer(sockPath, rootFS string) (*controlServer, error) {
	// Remove existing socket file if it exists
	os.Remove(sockPath)

	// Bind using a relative path to avoid socket path length limits.
	// Unix socket paths are limited to ~108 characters; deep test paths can exceed this.
	// By chdir'ing to rootFS first, we use a short relative name for the bind.
	sockName := filepath.Base(sockPath) // e.g., "ctrl.sock"
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("get cwd: %w", err)
	}
	if err := os.Chdir(rootFS); err != nil {
		return nil, fmt.Errorf("chdir to rootFS %s: %w", rootFS, err)
	}
	ln, listenErr := net.Listen("unix", sockName)
	os.Chdir(cwd) // restore cwd regardless of error
	if listenErr != nil {
		return nil, fmt.Errorf("listen on control socket %s: %w", sockPath, listenErr)
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
	mux.HandleFunc("/list-frames", cs.handleListFrames)
	mux.HandleFunc("/download-docker", handleDownloadDocker)
	mux.HandleFunc("/download-snap", handleDownloadSnap)
	mux.HandleFunc("/who-has", handleWhoHas)
	mux.HandleFunc("/ts/servers.json", handleServersJSONControl)
	// Ref and frame UUID-based API handlers (per-user, keyed off the frame's
	// rootFS path).
	mux.HandleFunc("/ref/create", cs.handleRefCreate)
	mux.HandleFunc("/ref/move", cs.handleRefMove)
	mux.HandleFunc("/ref/delete", cs.handleRefDelete)
	mux.HandleFunc("/refs", cs.handleListRefs)
	mux.HandleFunc("/reflog", cs.handleReflog)
	mux.HandleFunc("/log", cs.handleLog)
	mux.HandleFunc("/autorun", cs.handleAutorun)
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

	// The in-container `ts` client uses the same code path to reach two kinds of
	// control sockets: a real cloud-hypervisor vsock (for VM/VMX frames) and this
	// Unix socket (for plain container frames). Cloud-hypervisor's vsock requires
	// a "CONNECT <port>\n" / "OK <port>\n" text handshake before the real stream,
	// so we emulate that handshake here over the Unix socket and only then speak
	// HTTP, letting the client be oblivious to which transport it actually got.
	reader := bufio.NewReader(conn)
	if err := thunderproto.ReadServerHandshake(conn, reader); err != nil {
		log.Printf("control socket: handshake failed: %v", err)
		return
	}

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

// requireMethod enforces the HTTP method for a handler. It writes a 405 and
// returns false when the method does not match, in which case the caller should
// return immediately.
func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

// writeJSON writes v as a JSON response with the given status code. A status of
// 0 or http.StatusOK leaves the default 200 in place (no explicit WriteHeader),
// matching the success paths that never called WriteHeader.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	if code != 0 && code != http.StatusOK {
		w.WriteHeader(code)
	}
	json.NewEncoder(w).Encode(v)
}

func handlePing(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
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

	writeJSON(w, http.StatusOK, ControlResponse{
		Status:  "ok",
		Message: "pong",
	})
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
// progressEmitter is the shared machinery for the NDJSON streaming progress
// writers (/snap, /create, /download-snap, /download-docker). It owns the JSON
// encoder and the optional flusher, and emits each event as a line of NDJSON
// followed by a flush. Each endpoint embeds it and supplies its own typed event
// structs.
type progressEmitter struct {
	flusher http.Flusher
	encoder *json.Encoder
}

func newProgressEmitter(w http.ResponseWriter) progressEmitter {
	pe := progressEmitter{encoder: json.NewEncoder(w)}
	if f, ok := w.(http.Flusher); ok {
		pe.flusher = f
	}
	return pe
}

// emit writes v as one NDJSON line and flushes if possible.
func (pe *progressEmitter) emit(v any) error {
	if err := pe.encoder.Encode(v); err != nil {
		return err
	}
	if pe.flusher != nil {
		pe.flusher.Flush()
	}
	return nil
}

type snapProgressWriter struct {
	progressEmitter
}

func newSnapProgressWriter(w http.ResponseWriter) *snapProgressWriter {
	return &snapProgressWriter{newProgressEmitter(w)}
}

func (pw *snapProgressWriter) Write(p []byte) (n int, err error) {
	// Each write from the progress tracker is a line of progress text
	msg := strings.TrimSpace(string(p))
	if msg == "" {
		return len(p), nil
	}
	if err := pw.emit(SnapStreamEvent{Type: "progress", Message: msg}); err != nil {
		return 0, err
	}
	return len(p), nil
}

// makeSnapHandler creates a /snap handler for the given rootFS.
// This is used by both container (controlServer) and VM handlers.
func makeSnapHandler(rootFS string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}

		// Check if client wants streaming progress
		stream := r.URL.Query().Get("stream") == "1"
		isTTY := r.URL.Query().Get("tty") == "1"
		subdir := r.URL.Query().Get("subdir")

		if stream {
			handleSnapStreaming(w, rootFS, subdir, isTTY)
			return
		}

		// Non-streaming fallback: a single JSON response with no progress
		// events. The in-tree `ts` client always requests stream=1, so this
		// branch only serves plain HTTP clients that omit it.
		snapshotID, err := createSnapshotSubdir(rootFS, subdir, nil, false)
		if err != nil {
			log.Printf("snap failed for %s: %v", rootFS, err)
			writeJSON(w, http.StatusInternalServerError, SnapResponse{
				Status:  "error",
				Message: err.Error(),
			})
			return
		}

		log.Printf("Created snapshot %s from %s", snapshotID, rootFS)

		writeJSON(w, http.StatusOK, SnapResponse{
			Status:     "ok",
			SnapshotID: snapshotID,
		})
	}
}

// handleSnapStreaming handles the streaming version of /snap
func handleSnapStreaming(w http.ResponseWriter, rootFS, subdir string, isTTY bool) {
	w.Header().Set("Content-Type", "application/x-ndjson")

	// Enable streaming mode immediately by flushing
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	pw := newSnapProgressWriter(w)
	encoder := json.NewEncoder(w)

	snapshotID, err := createSnapshotSubdir(rootFS, subdir, pw, isTTY)
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
	if !requireMethod(w, r, http.MethodPost) {
		return
	}

	var req TaintRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.TaintName == "" {
		writeJSON(w, http.StatusBadRequest, TaintResponse{
			Status:  "error",
			Message: "taint_name is required",
		})
		return
	}

	// Read existing frame metadata
	frameMeta, err := readFrameSidecar(c.rootFS)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, TaintResponse{
			Status:  "error",
			Message: fmt.Sprintf("read frame meta: %v", err),
		})
		return
	}

	// Create default frame metadata if none exists
	if frameMeta == nil {
		frameMeta = &frames.Frame{
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
	if err := writeFrameSidecar(c.rootFS, frameMeta); err != nil {
		writeJSON(w, http.StatusInternalServerError, TaintResponse{
			Status:  "error",
			Message: fmt.Sprintf("write frame meta: %v", err),
		})
		return
	}

	log.Printf("Added taint %q to %s, taints now: %v", req.TaintName, c.rootFS, frameMeta.Taints)

	writeJSON(w, http.StatusOK, TaintResponse{
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
	if !requireMethod(w, r, http.MethodPost) {
		return
	}

	var req DeleteSnapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.SnapshotID == "" {
		writeJSON(w, http.StatusBadRequest, DeleteSnapResponse{
			Status:  "error",
			Message: "snapshot_id is required",
		})
		return
	}

	// Check that the snapshot exists
	snapPath := filepath.Join(*flagSnapsDir, req.SnapshotID)
	if _, err := os.Stat(snapPath); os.IsNotExist(err) {
		writeJSON(w, http.StatusNotFound, DeleteSnapResponse{
			Status:  "error",
			Message: fmt.Sprintf("snapshot %q not found", req.SnapshotID),
		})
		return
	}

	// Read the snap's metadata to get its parent
	snapMeta, _ := readSnapMeta(*flagSnapsDir, req.SnapshotID)
	var deletedParent string
	if snapMeta != nil {
		deletedParent = snapMeta.Parent
	}

	// Find all snaps that have this snap as their parent and update them
	if err := relinkSnapChildren(*flagSnapsDir, req.SnapshotID, deletedParent); err != nil {
		log.Printf("Warning: failed to relink children of %s: %v", req.SnapshotID, err)
	}

	// Delete the snapshot directory (btrfs subvolume)
	if err := btrfsDeleteSubvol(snapPath); err != nil {
		writeJSON(w, http.StatusInternalServerError, DeleteSnapResponse{
			Status:  "error",
			Message: err.Error(),
		})
		return
	}

	// Delete associated files
	os.Remove(snapPath + ".jsonc") // metadata
	os.Remove(snapPath + ".stamp") // stamp file
	os.Remove(snapPath + ".tsm")   // tsm manifest
	os.Remove(snapPath + ".tsc")   // tsc manifest

	log.Printf("Deleted snapshot %s", req.SnapshotID)

	writeJSON(w, http.StatusOK, DeleteSnapResponse{
		Status: "ok",
	})
}

// relinkSnapChildren finds all snaps that have oldParent as their parent
// and updates them to point to newParent instead.
func relinkSnapChildren(snapsDir, oldParent, newParent string) error {
	entries, err := os.ReadDir(snapsDir)
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

		meta, err := readSnapMeta(snapsDir, snapID)
		if err != nil || meta == nil {
			continue
		}

		if meta.Parent == oldParent {
			meta.Parent = newParent
			if err := writeSnapMeta(snapsDir, snapID, meta); err != nil {
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
	if !requireMethod(w, r, http.MethodPost) {
		return
	}

	var req DeleteFrameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.FrameName == "" {
		writeJSON(w, http.StatusBadRequest, DeleteFrameResponse{
			Status:  "error",
			Message: "frame_name is required",
		})
		return
	}

	// Extract tailscale user from rootFS path: fs/<tailscale-user>/<uuid>
	user, err := tailscaleUserFromRootFS(c.rootFS)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, DeleteFrameResponse{
			Status:  "error",
			Message: err.Error(),
		})
		return
	}

	refStore := userRefStore(user)
	frameStore := userFrameStore(user)

	// Resolve the frame name (a ref) to its UUID. Frames are addressed by ref
	// in this API; there is no name-based fallback path on disk.
	ref, err := refStore.Get(req.FrameName)
	if err != nil {
		if err == refs.ErrRefNotFound {
			writeJSON(w, http.StatusNotFound, DeleteFrameResponse{
				Status:  "error",
				Message: fmt.Sprintf("no such frame %q", req.FrameName),
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, DeleteFrameResponse{
			Status:  "error",
			Message: err.Error(),
		})
		return
	}
	uuid := ref.UUID
	framePath := framePathForUserUUID(user, uuid)

	// Prevent deleting the current frame
	if framePath == c.rootFS {
		writeJSON(w, http.StatusBadRequest, DeleteFrameResponse{
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
		if err := btrfsDeleteSubvol(homePath); err != nil {
			log.Printf("Warning: failed to delete home subvolume: %v", err)
		}
	}

	if isSubvolume(workPath) {
		if err := btrfsDeleteSubvol(workPath); err != nil {
			log.Printf("Warning: failed to delete work subvolume: %v", err)
		}
	}

	if isSubvolume(idPath) {
		if err := btrfsDeleteSubvol(idPath); err != nil {
			log.Printf("Warning: failed to delete id subvolume: %v", err)
		}
	}

	// Delete the frame directory (btrfs subvolume)
	if err := btrfsDeleteSubvol(framePath); err != nil {
		writeJSON(w, http.StatusInternalServerError, DeleteFrameResponse{
			Status:  "error",
			Message: err.Error(),
		})
		return
	}

	// Delete the frame metadata sidecar via the store, then drop the ref.
	if err := frameStore.Delete(uuid); err != nil && err != frames.ErrFrameNotFound {
		log.Printf("Warning: delete frame metadata for %s: %v", uuid, err)
	}
	if err := refStore.Delete(req.FrameName); err != nil && err != refs.ErrRefNotFound {
		log.Printf("Warning: delete ref %s: %v", req.FrameName, err)
	}

	log.Printf("Deleted frame %s (ref %q, user %s)", framePath, req.FrameName, user)

	writeJSON(w, http.StatusOK, DeleteFrameResponse{
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
	if !requireMethod(w, r, http.MethodGet) {
		return
	}

	entries, err := os.ReadDir(*flagSnapsDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ListSnapsResponse{
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
		tsmPath := filepath.Join(*flagSnapsDir, entry.Name())
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

	writeJSON(w, http.StatusOK, ListSnapsResponse{
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

// handleListFrames handles GET /list-frames - list the requesting user's
// frames with status. Frames are stored at fs/<user>/<uuid> with a
// fs/<user>/<uuid>.jsonc sidecar; each is reported by its bound ref name when
// one exists, otherwise by its UUID. Legacy non-UUID dirs are ignored by the
// per-user frames.Store.List().
func (c *controlServer) handleListFrames(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}

	user, err := tailscaleUserFromRootFS(c.rootFS)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ListFramesResponse{
			Status: "error",
			Error:  err.Error(),
		})
		return
	}

	frameStore := userFrameStore(user)
	refStore := userRefStore(user)

	uuids, err := frameStore.List()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ListFramesResponse{
			Status: "error",
			Error:  fmt.Sprintf("list frames: %v", err),
		})
		return
	}

	// Build a UUID -> ref-name map so frames are reported by their ref.
	refByUUID := map[frameid.ID]string{}
	if names, err := refStore.List(); err == nil {
		for _, name := range names {
			if ref, err := refStore.Get(name); err == nil {
				refByUUID[ref.UUID] = name
			}
		}
	}

	var frameInfos []FrameInfo
	for _, uuid := range uuids {
		name := uuid.String()
		if refName, ok := refByUUID[uuid]; ok {
			name = refName
		}

		// Determine status based on active control servers.
		sessionCount := getActiveFrameCount(framePathForUserUUID(user, uuid))
		status := "stopped"
		if sessionCount > 0 {
			status = fmt.Sprintf("%d", sessionCount)
		}

		frameInfos = append(frameInfos, FrameInfo{
			Name:   name,
			Status: status,
		})
	}

	writeJSON(w, http.StatusOK, ListFramesResponse{
		Status: "ok",
		Frames: frameInfos,
	})
}

// CreateRequest is the request body for /create
type CreateRequest struct {
	// New UUID-based API fields
	SnapshotSpec string `json:"snapshot_spec,omitempty"` // <rootfs>:<home>:<work> format
	RefName      string `json:"ref_name,omitempty"`      // Optional ref to create pointing at new frame

	// Legacy API fields (for backward compatibility)
	FrameName  string `json:"frame_name,omitempty"`
	SnapshotID string `json:"snapshot_id,omitempty"` // Can be single ID or frame spec "rootfs:home:work"

	// Frame-specific fields (alternative to parsing snapshot_id/snapshot_spec)
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

// hasBlankRootfs checks if the frame spec has an empty or "nil" rootfs component.
// This is used to detect when a blank container is being requested.
// Returns (isBlank, isExplicitNil) where isExplicitNil means the user wrote "nil".
func hasBlankRootfs(spec string) (isBlank, isExplicitNil bool) {
	if !isFrameSpec(spec) {
		return false, false
	}
	parts := strings.Split(spec, ":")
	if len(parts) == 0 {
		return false, false
	}
	rootfs := parts[0]
	if rootfs == "nil" {
		return true, true
	}
	if rootfs == "" {
		return true, false
	}
	return false, false
}

// CreateResponse is the response from /create
type CreateResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	UUID    string `json:"uuid,omitempty"` // The new frame's UUID
	Path    string `json:"path,omitempty"`
}

// handleCreate handles POST /create - create a new frame from a snapshot
func (c *controlServer) handleCreate(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}

	var req CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Check if streaming is requested (needed early for error responses)
	stream := r.URL.Query().Get("stream") == "1"
	isTTY := r.URL.Query().Get("tty") == "1"

	// Allow either snapshot_spec or the legacy rootfs/snapshot_id fields to
	// specify the rootfs; everything funnels through the UUID-based create.
	if req.SnapshotSpec == "" {
		if req.RootfsSnap != "" && req.SnapshotID == "" {
			req.SnapshotID = req.RootfsSnap
		}
		req.SnapshotSpec = req.SnapshotID
	}

	if req.SnapshotSpec == "" {
		writeJSON(w, http.StatusBadRequest, CreateResponse{
			Status:  "error",
			Message: "snapshot_spec (or rootfs) is required",
		})
		return
	}

	c.handleCreateWithUUID(w, req, stream, isTTY)
}

// handleCreateWithUUID handles frame creation using the UUID-based API.
// Frames are created at fs/<user>/<uuid>/ and optionally a ref is bound.
func (c *controlServer) handleCreateWithUUID(w http.ResponseWriter, req CreateRequest, stream, isTTY bool) {
	user, err := tailscaleUserFromRootFS(c.rootFS)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, CreateResponse{
			Status:  "error",
			Message: err.Error(),
		})
		return
	}

	// Parse the snapshot spec
	rootfsSpec, homeSpec, workSpec := parseFrameSpec(req.SnapshotSpec)

	// Check if this is a blank container request
	isBlank, isExplicitNil := hasBlankRootfs(req.SnapshotSpec)
	if isBlank && !isExplicitNil {
		writeJSON(w, http.StatusBadRequest, CreateResponse{
			Status:  "error",
			Message: "rootfs component is required (use 'nil' for blank container)",
		})
		return
	}

	// For non-blank containers, verify the rootfs snapshot exists
	if !isBlank {
		snapshotPath := filepath.Join(*flagSnapsDir, rootfsSpec)
		if _, err := os.Stat(snapshotPath); err != nil {
			writeJSON(w, http.StatusNotFound, CreateResponse{
				Status:  "error",
				Message: fmt.Sprintf("rootfs snapshot %q not found", rootfsSpec),
			})
			return
		}
	}

	frameStore := userFrameStore(user)
	refStore := userRefStore(user)

	// If a ref name is provided, validate it and check it doesn't already exist
	if req.RefName != "" {
		if refStore.Exists(req.RefName) {
			writeJSON(w, http.StatusConflict, CreateResponse{
				Status:  "error",
				Message: fmt.Sprintf("ref %q already exists", req.RefName),
			})
			return
		}
	}

	// Generate a new UUID for this frame
	uuid, err := frameid.New()
	if err != nil {
		log.Printf("failed to generate UUID: %v", err)
		writeJSON(w, http.StatusInternalServerError, CreateResponse{
			Status:  "error",
			Message: "failed to generate frame UUID",
		})
		return
	}

	// Build the per-user frame path using UUID.
	framePath := framePathForUserUUID(user, uuid)

	frameMeta := &frames.Frame{
		Rootfs:    rootfsSpec,
		Home:      homeSpec,
		Work:      workSpec,
		Isolation: req.Isolation,
	}

	// Persist the metadata sidecar through the per-user store FIRST: it writes
	// fs/<user>/<uuid>.jsonc (stamping CreatedAt), which buildFrameFS then reads
	// to assemble the rootfs/home/work subvolumes. (Doing the FS build first
	// would write the sidecar twice and collide with store.Create.)
	if err := frameStore.Create(uuid, frameMeta); err != nil {
		log.Printf("failed to store frame metadata: %v", err)
		writeJSON(w, http.StatusInternalServerError, CreateResponse{
			Status:  "error",
			Message: err.Error(),
		})
		return
	}

	// For streaming mode, delegate to the streaming handler.
	if stream {
		handleCreateStreamingWithUUID(w, req, framePath, frameMeta, uuid, refStore, isTTY)
		return
	}

	// Assemble the frame's filesystem from the (already-written) sidecar.
	if err := buildFrameFS(framePath, frameMeta); err != nil {
		log.Printf("create frame failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, CreateResponse{
			Status:  "error",
			Message: err.Error(),
		})
		return
	}

	// Create the ref if requested
	if req.RefName != "" {
		if err := refStore.Create(req.RefName, uuid); err != nil {
			log.Printf("failed to create ref %s: %v", req.RefName, err)
			// Frame was created but ref failed - log warning
		} else {
			log.Printf("Created ref %s -> %s for user %s", req.RefName, uuid, user)
		}
	}

	log.Printf("Created frame %s (UUID: %s) from snapshot spec %s", framePath, uuid, req.SnapshotSpec)

	writeJSON(w, http.StatusOK, CreateResponse{
		Status: "ok",
		UUID:   uuid.String(),
		Path:   framePath,
	})
}

// handleCreateStreamingWithUUID handles streaming create for UUID-based frames.
// The metadata sidecar has already been written by the caller via the per-user
// frames.Store; this only assembles the filesystem and binds the optional ref.
func handleCreateStreamingWithUUID(w http.ResponseWriter, req CreateRequest, framePath string, frameMeta *frames.Frame, uuid frameid.ID, refStore *refs.Store, isTTY bool) {
	w.Header().Set("Content-Type", "application/x-ndjson")

	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	pw := &createProgressWriter{newProgressEmitter(w)}

	// Check if this is a blank container
	isBlank, _ := hasBlankRootfs(req.SnapshotSpec)

	// For non-blank containers, check/download the rootfs snap
	if !isBlank {
		snapshotPath := filepath.Join(*flagSnapsDir, frameMeta.Rootfs)
		if _, err := os.Stat(snapshotPath); err != nil {
			// Try to download from mesh
			pw.writeProgress(fmt.Sprintf("Snapshot %s not found locally, downloading from mesh peers...", frameMeta.Rootfs))
			result, err := doDownloadSnap(frameMeta.Rootfs, pw, isTTY)
			if err != nil {
				log.Printf("create: auto-download of snapshot %s failed: %v", frameMeta.Rootfs, err)
				pw.writeResultWithUUID("error", "", fmt.Sprintf("failed to download snapshot: %v", err), "")
				return
			}
			if !result.AlreadyExists {
				pw.writeProgress("Downloaded snapshot from mesh peer")
			}
		}
	}

	pw.writeProgress("Creating frame...")

	// Assemble the frame's filesystem from the (already-written) sidecar.
	if err := buildFrameFS(framePath, frameMeta); err != nil {
		pw.writeResultWithUUID("error", "", err.Error(), "")
		return
	}

	// Create the ref if requested
	if req.RefName != "" {
		if err := refStore.Create(req.RefName, uuid); err != nil {
			log.Printf("failed to create ref %s: %v", req.RefName, err)
		} else {
			log.Printf("Created ref %s -> %s", req.RefName, uuid)
		}
	}

	log.Printf("Created frame %s (UUID: %s) from snapshot spec %s", framePath, uuid, req.SnapshotSpec)
	pw.writeResultWithUUID("ok", uuid.String(), "", framePath)
}

// CreateStreamEvent is an event in the streaming create response
type CreateStreamEvent struct {
	Type    string `json:"type"`              // "progress" or "result"
	Message string `json:"message,omitempty"` // progress message
	Status  string `json:"status,omitempty"`  // "ok" or "error" (for result)
	UUID    string `json:"uuid,omitempty"`    // frame UUID (for result)
	Path    string `json:"path,omitempty"`    // frame path (for result)
}

// createProgressWriter wraps ResponseWriter to write progress events
type createProgressWriter struct {
	progressEmitter
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
	pw.emit(CreateStreamEvent{Type: "progress", Message: msg})
}

func (pw *createProgressWriter) writeResult(status, path, message string) {
	pw.emit(CreateStreamEvent{
		Type:    "result",
		Status:  status,
		Path:    path,
		Message: message,
	})
}

func (pw *createProgressWriter) writeResultWithUUID(status, uuid, message, path string) {
	pw.emit(CreateStreamEvent{
		Type:    "result",
		Status:  status,
		UUID:    uuid,
		Path:    path,
		Message: message,
	})
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
		meta := &frames.Frame{
			Rootfs:    rootfsSnap,
			Home:      homeSnap,
			Work:      workSnap,
			Isolation: isolation,
		}
		// Write the sidecar first so ensureFrameFS can find it
		if err := writeFrameSidecar(framePath, meta); err != nil {
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
	snapshotPath := filepath.Join(*flagSnapsDir, rootfsSnap)

	// Ensure the parent directory exists
	parentDir := filepath.Dir(framePath)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		return fmt.Errorf("creating parent directory: %w", err)
	}

	// Clone from the snapshot to the frame path
	if err := btrfsSnapshot(snapshotPath, framePath, false); err != nil {
		return err
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

	// Ensure the "user" account, sudoers, resolv.conf, and /tmp are set up.
	finalizeFrameRootfs(framePath)

	return nil
}

// buildFrameFS assembles a frame's on-disk filesystem (rootfs/home/work
// subvolumes) from a metadata sidecar that the caller has ALREADY written
// (e.g. via the per-user frames.Store). Unlike createFrame, it does not write
// the sidecar itself, so it is safe to call after frames.Store.Create without
// colliding on fs/<user>/<uuid>.jsonc.
func buildFrameFS(framePath string, meta *frames.Frame) error {
	if err := ensureFrameFS(framePath, meta); err != nil {
		return err
	}
	if err := copyTsBinary(framePath); err != nil {
		log.Printf("Warning: failed to copy ts binary to %s: %v", framePath, err)
	}
	return nil
}

// handleServersJSONControl handles GET /ts/servers.json on the control socket
// This allows ts inside containers to access the mesh peer list
var globalMeshState *meshState

func handleServersJSONControl(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}

	if globalMeshState == nil {
		// Mesh not enabled, return empty list
		writeJSON(w, http.StatusOK, []meshPeer{})
		return
	}

	peers := globalMeshState.getPeersIncludingSelf()
	writeJSON(w, http.StatusOK, peers)
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
	if !requireMethod(w, r, http.MethodPost) {
		return
	}

	var req WhoHasRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.SnapshotID == "" {
		writeJSON(w, http.StatusBadRequest, WhoHasResponse{
			Status: "error",
			Error:  "snapshot_id is required",
		})
		return
	}

	// Get mesh peers
	if globalMeshState == nil {
		writeJSON(w, http.StatusOK, WhoHasResponse{
			Status: "error",
			Error:  "mesh not enabled",
		})
		return
	}

	meshPeers := globalMeshState.getPeersIncludingSelf()
	if len(meshPeers) == 0 {
		writeJSON(w, http.StatusOK, WhoHasResponse{
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

	writeJSON(w, http.StatusOK, WhoHasResponse{
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
	if !requireMethod(w, r, http.MethodPost) {
		return
	}

	var req DownloadSnapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.SnapshotID == "" {
		writeJSON(w, http.StatusBadRequest, DownloadSnapResponse{
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
		writeJSON(w, http.StatusInternalServerError, DownloadSnapResponse{
			Status:  "error",
			Message: err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, DownloadSnapResponse{
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

	pw := &downloadProgressWriter{newProgressEmitter(w)}

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
	progressEmitter
}

func (pw *downloadProgressWriter) Write(p []byte) (n int, err error) {
	msg := strings.TrimSpace(string(p))
	if msg == "" {
		return len(p), nil
	}
	if err := pw.emit(DownloadSnapStreamEvent{Type: "progress", Message: msg}); err != nil {
		return 0, err
	}
	return len(p), nil
}

// doDownloadSnap performs the actual download operation using TSM/TSC format.
func doDownloadSnap(snapshotID string, progressWriter io.Writer, isTTY bool) (*tsm.DownloadResult, error) {
	// Check if snapshot already exists
	snapshotPath := filepath.Join(*flagSnapsDir, snapshotID)
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
		SnapsDir:       *flagSnapsDir,
		BaseURL:        baseURL,
		ProgressWriter: progressWriter,

		// Create the target directory as a btrfs subvolume.
		CreateTargetDir: func(path, parentStamp string) error {
			return createDownloadTargetDir(path, parentStamp, progressWriter)
		},

		// Clean up using btrfs subvolume delete since we created a subvolume
		CleanupTargetDir: func(path string) {
			btrfsutil.DeleteSubvol(path) // best effort
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
		return btrfsSnapshot(localAncestor, path, false)
	}

	// No local ancestor found, create a fresh subvolume
	return btrfsCreateSubvol(path)
}

// findLocalAncestor walks the parent chain starting from stampID and returns
// the path to the first snapshot that exists locally as a btrfs subvolume.
// Returns empty string if no local ancestor is found.
func findLocalAncestor(stampID string) string {
	// Limit the search depth to avoid infinite loops from circular references
	const maxDepth = 100

	currentID := stampID
	for i := 0; i < maxDepth && currentID != "" && currentID != "1"; i++ {
		snapPath := filepath.Join(*flagSnapsDir, currentID)

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
	basePath := filepath.Join(*flagSnapsDir, "1")
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

// createSnapshot creates a read-only snapshot of the given rootFS in snaps-dir.
// Returns the snapshot ID (based on the fidx checksum). If progressWriter is
// non-nil, progress updates are written to it. This is the whole-frame
// convenience form (no subdir); production handlers call createSnapshotSubdir
// directly, but it remains the entry point used by the e2e tests.
func createSnapshot(rootFS string, progressWriter io.Writer, isTTY bool) (string, error) {
	return createSnapshotSubdir(rootFS, "", progressWriter, isTTY)
}

// createSnapshotSubdir is createSnapshot with optional subdir support. When
// subdir is non-empty, only that subtree (re-rooted) is snapshotted and a
// single snapshot ID is returned; the frame's own stamp/metadata are left
// untouched, so this can be used to assemble a snap from a portion of a
// container's filesystem.
func createSnapshotSubdir(rootFS, subdir string, progressWriter io.Writer, isTTY bool) (string, error) {
	if subdir != "" {
		clean, err := snapsubdir.Validate(subdir)
		if err != nil {
			return "", err
		}
		// Inherit the frame's taints, but index the subtree in full (no
		// parent stamp, since the re-rooted paths don't match any parent).
		frameMeta, _ := readFrameSidecar(rootFS)
		var frameTaints []string
		if frameMeta != nil {
			frameTaints = frameMeta.Taints
		}
		return createSnapshotWithTaintsSubdir(rootFS, clean, "", frameTaints, progressWriter, isTTY)
	}

	// Check if this is a three-component frame (has nested home/work subvolumes)
	homePath := filepath.Join(rootFS, "home")
	workPath := filepath.Join(rootFS, "work")
	hasHomeSubvol := isSubvolume(homePath)
	hasWorkSubvol := isSubvolume(workPath)

	// Read the frame metadata for taints
	frameMeta, _ := readFrameSidecar(rootFS)
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
		frameMeta = &frames.Frame{}
	}
	frameMeta.Rootfs = rootfsID
	frameMeta.Home = homeID
	frameMeta.Work = workID
	if err := writeFrameSidecar(rootFS, frameMeta); err != nil {
		log.Printf("Warning: failed to update frame sidecar for %s: %v", rootFS, err)
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

// loadParentManifest loads the TSM and TSC manifests for the given parent
// snapshot ID from the snaps directory, for use as the incremental-indexing
// baseline. It returns (nil, nil) when the parent has no usable manifest
// (e.g. an empty/base stamp or a snapshot that predates manifests); callers
// then fall back to a full re-index.
func loadParentManifest(parentStampID string) (*tsm.TSMReader, *tsm.TSCReader) {
	if parentStampID == "" {
		return nil, nil
	}
	base := filepath.Join(*flagSnapsDir, parentStampID)
	parentTSM, err := tsm.ReadTSM(base + ".tsm")
	if err != nil {
		return nil, nil
	}
	parentTSC, err := tsm.ReadTSC(base + ".tsc")
	if err != nil {
		return nil, nil
	}
	return parentTSM, parentTSC
}

// createSnapshotWithTaints creates a read-only snapshot of source with the
// given explicit taints. If taints is nil, taints are inherited from the parent
// snap.
func createSnapshotWithTaints(source, parentStampID string, taints []string, progressWriter io.Writer, isTTY bool) (string, error) {
	return createSnapshotWithTaintsSubdir(source, "", parentStampID, taints, progressWriter, isTTY)
}

// createSnapshotWithTaintsSubdir creates a read-only snapshot in snaps-dir and
// generates fidx and tsm files for it. The snapshot is named after the SHA-256
// of its TSM manifest, so if a snapshot with the same SHA-256 already exists it
// returns the existing ID and discards the new snapshot, performing taint
// intersection on the metadata. The process is:
//  1. Create btrfs snapshot to a random tmp name
//  2. Create mfidx (with --ref to parent if exists)
//  3. Create TSM/TSC manifests
//  4. Load TSM to get its SHA-256, use that as the final snapshot ID
//  5. If snapshot already exists with that ID, perform taint intersection and discard new one
//  6. Otherwise rename all files to the SHA-256-based final names
//  7. Create fidx of the fidx and write snap.jsonc metadata
//
// When subdir is non-empty (a slash-relative path within the source subvolume),
// only that subtree is snapshotted: for atomicity the whole subvolume is still
// snapshotted first, then everything outside subdir is deleted and subdir's
// contents are promoted to the snapshot root before the subvolume is made
// read-only and indexed. The resulting snapshot ID is then the content hash of
// just that subtree, so it can be dropped into a frame on its own.
func createSnapshotWithTaintsSubdir(source, subdir, parentStampID string, taints []string, progressWriter io.Writer, isTTY bool) (string, error) {
	// Generate a random temporary ID for the work-in-progress snapshot
	tmpID, err := generateRandomID()
	if err != nil {
		return "", fmt.Errorf("generating temporary ID: %w", err)
	}

	tmpPath := filepath.Join(*flagSnapsDir, tmpID+".tmp")
	tmpTSMPath := tmpPath + ".tsm"
	tmpTSCPath := tmpPath + ".tsc"

	// Cleanup helper
	cleanupTmp := func() {
		btrfsutil.DeleteSubvol(tmpPath) // best effort
		os.Remove(tmpPath + ".stamp")
		os.Remove(tmpTSMPath)
		os.Remove(tmpTSCPath)
	}

	if subdir == "" {
		// Step 1: Create read-only btrfs snapshot to tmp path
		if err := btrfsSnapshot(source, tmpPath, true); err != nil {
			return "", err
		}
	} else {
		// Subdir snap: take a WRITABLE snapshot (so we can prune/promote),
		// then reduce it to just the requested subtree before making it
		// read-only.
		if err := snapsubdir.Snapshot(source, subdir, tmpPath); err != nil {
			cleanupTmp()
			return "", err
		}
	}

	// Write stamp file for the snapshot (in tmp location)
	if err := writeStampFile(tmpPath, parentStampID); err != nil {
		cleanupTmp()
		return "", fmt.Errorf("write stamp file: %w", err)
	}

	// Step 2: Create TSM/TSC manifests in tmp location.
	// Load the parent snapshot's manifest (if any) so the indexer can reuse
	// chunk hashes for files that are unchanged since the parent, instead of
	// re-reading and re-hashing every file. This makes a second consecutive
	// snap of an unchanged tree do essentially no file I/O.
	tsmOpts := tsm.IndexerOptions{
		ProgressWriter: progressWriter,
		IsTTY:          isTTY,
	}
	// Incremental reuse only applies to a full-root snap, where the parent
	// manifest's paths line up with this tree. A subdir snap re-roots the
	// tree, so the parent's paths no longer match; index it in full.
	if subdir == "" {
		if parentTSM, parentTSC := loadParentManifest(parentStampID); parentTSM != nil && parentTSC != nil {
			tsmOpts.ParentTSM = parentTSM
			tsmOpts.ParentTSC = parentTSC
		}
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

	finalPath := filepath.Join(*flagSnapsDir, snapshotID)
	finalTSMPath := finalPath + ".tsm"
	finalTSCPath := finalPath + ".tsc"

	// Determine taints for this snapshot
	if taints == nil {
		// Inherit from parent if available
		taints = getSnapTaints(*flagSnapsDir, parentStampID)
	}

	// Step 4: Check if a snapshot with this SHA-256 already exists
	if _, err := os.Stat(finalPath); err == nil {
		// Snapshot already exists! Perform taint intersection and discard the new one.
		log.Printf("Snapshot %s already exists, checking taints", snapshotID)

		existingMeta, _ := readSnapMeta(*flagSnapsDir, snapshotID)
		if existingMeta != nil && len(taints) > 0 {
			// Taint intersection: if we can produce the same content with fewer taints,
			// the removed taints are not inherent to the content.
			intersected := IntersectTaints(existingMeta.Taints, taints)
			if !taintsEqual(existingMeta.Taints, intersected) {
				existingMeta.Taints = intersected
				if err := writeSnapMeta(*flagSnapsDir, snapshotID, existingMeta); err != nil {
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
	if err := writeSnapMeta(*flagSnapsDir, snapshotID, snapMeta); err != nil {
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
	mu     sync.Mutex
	myURL  string
	myFQDN string
	peers  map[string]*meshPeer // keyed by hostname
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
	if !requireMethod(w, r, http.MethodPost) {
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

	writeJSON(w, http.StatusOK, resp)
}

// handleServersJSON handles GET /ts/servers.json - list known peers
func (m *meshState) handleServersJSON(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}

	peers := m.getPeers()
	writeJSON(w, http.StatusOK, peers)
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

// bupdateFileServer serves files from -snaps-dir with range request support
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

// btrfsMagic is the btrfs superblock magic returned by statfs(2). It is
// BTRFS_SUPER_MAGIC from <linux/magic.h>.
const btrfsMagic = 0x9123683E

// checkBtrfsFilesystems verifies that both directories exist, are on btrfs,
// and are on the same btrfs filesystem (required for subvolume snapshots).
func checkBtrfsFilesystems(fsDir, snapsDir string) error {
	// Ensure both directories exist
	if err := os.MkdirAll(fsDir, 0755); err != nil {
		return fmt.Errorf("creating fs-dir %s: %w", fsDir, err)
	}
	if err := os.MkdirAll(snapsDir, 0755); err != nil {
		return fmt.Errorf("creating snaps-dir %s: %w", snapsDir, err)
	}

	// Check that fs-dir is on btrfs
	var fsDirStatfs syscall.Statfs_t
	if err := syscall.Statfs(fsDir, &fsDirStatfs); err != nil {
		return fmt.Errorf("statfs on fs-dir %s: %w", fsDir, err)
	}
	if fsDirStatfs.Type != btrfsMagic {
		return fmt.Errorf("data-dir fs %s is not on a btrfs filesystem (type=0x%x, need btrfs=0x%x)", fsDir, fsDirStatfs.Type, btrfsMagic)
	}

	// Check that snaps-dir is on btrfs
	var snapsDirStatfs syscall.Statfs_t
	if err := syscall.Statfs(snapsDir, &snapsDirStatfs); err != nil {
		return fmt.Errorf("statfs on snaps-dir %s: %w", snapsDir, err)
	}
	if snapsDirStatfs.Type != btrfsMagic {
		return fmt.Errorf("data-dir snaps %s is not on a btrfs filesystem (type=0x%x, need btrfs=0x%x)", snapsDir, snapsDirStatfs.Type, btrfsMagic)
	}

	// Check that both are on the same filesystem by comparing device IDs
	var fsDirStat syscall.Stat_t
	if err := syscall.Stat(fsDir, &fsDirStat); err != nil {
		return fmt.Errorf("stat on fs-dir %s: %w", fsDir, err)
	}

	var snapsDirStat syscall.Stat_t
	if err := syscall.Stat(snapsDir, &snapsDirStat); err != nil {
		return fmt.Errorf("stat on snaps-dir %s: %w", snapsDir, err)
	}

	if fsDirStat.Dev != snapsDirStat.Dev {
		return fmt.Errorf("data-dir fs and snaps must be on the same btrfs filesystem for subvolume snapshots; fs device=%d, snaps device=%d", fsDirStat.Dev, snapsDirStat.Dev)
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
