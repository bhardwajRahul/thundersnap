// Package thundersnap provides session management for container and VM environments.
package thundersnap

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// VMConfig holds configuration for starting a VM session.
type VMConfig struct {
	// RootFS is the path to the root filesystem to share via virtiofs.
	RootFS string
	// VMDir is the path to the directory containing cloud-hypervisor and vmlinux.
	VMDir string
	// ControlHandler is the HTTP handler for serving the control socket protocol
	// over vsock. If nil, no vsock control socket is set up.
	ControlHandler http.Handler
	// Hostname is the hostname to set inside the VM via kernel IP autoconfig.
	// If empty, defaults to "thundersnap".
	Hostname string
	// InitPrefix is an optional path prefix where the init binaries are located
	// within the virtiofs mount. For VMX mode, this might be ".vmx-<isolation>"
	// so that /bin/ts is at /.vmx-<isolation>/bin/ts in the virtiofs.
	// If empty, binaries are expected at the root of the virtiofs mount.
	InitPrefix string
}

// VsockPort is the vsock port used for the thunder control socket.
const VsockPort = 5223

// VshPort is the vsock port used for vsh shell connections.
const VshPort = 5222

// VMSession represents a running VM session.
type VMSession struct {
	virtiofsdCmd   *exec.Cmd
	passtCmd       *exec.Cmd // passt process for user-space networking
	chvCmd         *exec.Cmd
	virtiofsSock   string
	vsockSock      string       // cloud-hypervisor vsock unix socket path
	vsockListener  net.Listener // listener for control vsock connections (guest-to-host)
	done           chan struct{}
	panicked       chan struct{} // closed when guest kernel panic is detected
	controlHandler http.Handler
}

// SetControlHandler updates the HTTP handler used for vsock control connections.
// This allows updating the handler for a running VM session.
func (s *VMSession) SetControlHandler(h http.Handler) {
	s.controlHandler = h
}

// waitForSocket polls for the named unix socket to appear, up to attempts
// times with delay between checks. It returns nil as soon as the socket exists
// and an error if it never appears. The helper (a process started by us creates
// the socket asynchronously) is shared by the virtiofsd and passt startup
// paths, which previously duplicated this loop verbatim.
func waitForSocket(path string, attempts int, delay time.Duration) error {
	for i := 0; i < attempts; i++ {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(delay)
	}
	if _, err := os.Stat(path); err != nil {
		return err
	}
	return nil
}

// StartVM starts a new VM session with the given configuration.
func StartVM(cfg VMConfig) (*VMSession, error) {
	// Create unique socket paths for this session. The ID is the daemon PID
	// concatenated with the current time in nanoseconds, which is unique enough
	// to avoid collisions between concurrent VM sessions in the same process;
	// it is only ever used to name per-session /tmp sockets, not as a security
	// or correctness boundary.
	sessionID := fmt.Sprintf("%d%d", os.Getpid(), time.Now().UnixNano())
	virtiofsSock := filepath.Join("/tmp", fmt.Sprintf("virtiofs-%s.sock", sessionID))
	vsockSock := filepath.Join("/tmp", fmt.Sprintf("vsock-%s.sock", sessionID))
	passtSock := filepath.Join("/tmp", fmt.Sprintf("passt-%s.sock", sessionID))

	// Start virtiofsd
	log.Printf("Starting virtiofsd with shared-dir=%s", cfg.RootFS)
	virtiofsdCmd := exec.Command("/usr/libexec/virtiofsd",
		"--socket-path="+virtiofsSock,
		"--shared-dir="+cfg.RootFS,
		"--cache=always",
	)
	// Pdeathsig ensures virtiofsd exits when its parent (thundersnapd/test harness)
	// dies, preventing orphaned virtiofsd processes.
	virtiofsdCmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: unix.SIGTERM,
	}
	virtiofsdCmd.Stderr = os.Stderr
	if err := virtiofsdCmd.Start(); err != nil {
		return nil, fmt.Errorf("start virtiofsd: %w", err)
	}

	// cleanup tears down everything started so far, in reverse order. Each
	// startup failure path below appends its just-started resource and calls
	// cleanup(), replacing the five copy-pasted teardown ladders that this
	// function previously grew incrementally (a prime source of leaks).
	var passtCmd *exec.Cmd
	cleanup := func() {
		if passtCmd != nil {
			passtCmd.Process.Kill()
			passtCmd.Wait()
		}
		virtiofsdCmd.Process.Kill()
		virtiofsdCmd.Wait()
		os.Remove(virtiofsSock)
		os.Remove(passtSock)
	}

	// Wait for virtiofsd socket to be created
	if err := waitForSocket(virtiofsSock, 50, 100*time.Millisecond); err != nil {
		cleanup()
		return nil, fmt.Errorf("virtiofsd socket not created: %w", err)
	}
	log.Printf("virtiofsd socket ready at %s", virtiofsSock)

	// Start passt for user-space networking (provides outgoing network without iptables)
	// Use --vhost-user mode for cloud-hypervisor's virtio-net socket interface
	// Configure NAT-style addressing (like QEMU user networking) so DHCP clients
	// get predictable private addresses instead of the host's addresses.
	log.Printf("Starting passt for user-space networking")
	passtCmd = exec.Command("passt",
		"--socket", passtSock,
		"--vhost-user",    // vhost-user mode for cloud-hypervisor
		"--foreground",    // stay in foreground for process management
		"--quiet",         // reduce log noise
		"-a", "10.0.2.15", // guest address (QEMU-style NAT)
		"-g", "10.0.2.2", // gateway address
		"-D", "none", // don't intercept DNS
	)
	// Pdeathsig ensures passt exits when its parent (thundersnapd/test harness)
	// dies, preventing orphaned passt processes.
	passtCmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: unix.SIGTERM,
	}
	passtCmd.Stderr = os.Stderr
	if err := passtCmd.Start(); err != nil {
		passtCmd = nil // not started; don't let cleanup deref a nil Process
		cleanup()
		return nil, fmt.Errorf("start passt: %w", err)
	}

	// Wait for passt socket to be created
	if err := waitForSocket(passtSock, 50, 100*time.Millisecond); err != nil {
		cleanup()
		return nil, fmt.Errorf("passt socket not created: %w", err)
	}
	log.Printf("passt socket ready at %s", passtSock)

	// Paths to cloud-hypervisor and kernel
	chvPath := filepath.Join(cfg.VMDir, "cloud-hypervisor")
	kernelPath := filepath.Join(cfg.VMDir, "vmlinux")

	// Build kernel command line
	// The VM uses /bin/sh as init, which runs a script that:
	// 1. Calls "ts drop-caps-and-run" to set up /dev (consistent with container mode)
	// 2. Starts vshd in the foreground
	// 3. Powers off the VM when vshd exits
	//
	// Networking is configured via the kernel's IP autoconfiguration (ip=) rather
	// than running ip commands in userspace. Format: ip=<client-ip>::<gw-ip>:<netmask>:<hostname>:<device>:<autoconf>
	// This requires CONFIG_IP_PNP=y in the kernel config.
	//
	// We use sh as init because kernel cmdline argument parsing is limited -
	// it doesn't handle complex quoting well when passing args directly to init.
	// The shell script approach is more reliable.
	//
	// panic=1 tells the kernel to reboot 1 second after a panic. Since there's
	// no bootable device, cloud-hypervisor will exit when the VM reboots.
	hostname := cfg.Hostname
	if hostname == "" {
		hostname = "thundersnap"
	}
	// Build paths to binaries, accounting for optional InitPrefix
	tsBin := "/bin/ts"
	shBin := "/bin/sh" // shell used after drop-caps-and-run
	vshdBin := "/sbin/vshd"
	if cfg.InitPrefix != "" {
		// VMX mode: all binaries are in /<InitPrefix>/, vshd runs without chroot
		// (it needs access to /dev/vsock which is at the virtiofs root)
		tsBin = "/" + cfg.InitPrefix + "/bin/ts"
		shBin = "/" + cfg.InitPrefix + "/bin/sh"
		vshdBin = "/" + cfg.InitPrefix + "/sbin/vshd"
	}
	// Note: In VMX mode, we don't chroot the init process. vshd runs at the virtiofs
	// root so it can access /dev/vsock and spawn containers with chroot into frame paths.
	//
	// --vsock tells drop-caps-and-run to expose /dev/vsock in the controlled /dev
	// it builds: the vshd that runs as this VM's init listens on AF_VSOCK. (The
	// kernel does not auto-mount devtmpfs in the guest, so /dev/vsock would
	// otherwise be absent.) Containers never get this flag.
	cmdline := fmt.Sprintf(`console=ttyS0 panic=1 rootfstype=virtiofs root=rootfs rw ip=10.0.2.15::10.0.2.2:255.255.255.0:%s:eth0:off init=%s -- -c "exec %s drop-caps-and-run --vsock %s -c 'echo nameserver 8.8.8.8 > /etc/resolv.conf; exec %s'"`, hostname, shBin, tsBin, shBin, vshdBin)

	// Create pipe for event monitor - cloud-hypervisor writes events, we read them
	eventReadPipe, eventWritePipe, err := os.Pipe()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("create event monitor pipe: %w", err)
	}

	// Start cloud-hypervisor
	// --pvpanic enables the pvpanic device which allows the guest to signal panic to the host
	// --event-monitor fd=N tells cloud-hypervisor to write JSON events to the pipe
	// ExtraFiles[0] becomes fd 3 in the child process (after stdin=0, stdout=1, stderr=2)
	log.Printf("Starting cloud-hypervisor")
	const eventMonitorFd = 3
	chvArgs := []string{
		"--kernel", kernelPath,
		"--cpus", "boot=1",
		"--memory", "size=512M,shared=on",
		"--fs", fmt.Sprintf("tag=rootfs,socket=%s", virtiofsSock),
		"--net", fmt.Sprintf("vhost_user=true,socket=%s,num_queues=2", passtSock),
		"--cmdline", cmdline,
		"--serial", "tty",
		"--console", "off",
		"--pvpanic",
		"--event-monitor", fmt.Sprintf("fd=%d", eventMonitorFd),
	}
	// Add vsock if we have a control handler. cid=3 is the guest's context ID
	// (the host is always CID 2); the guest dials the host at CID 2 and
	// cloud-hypervisor maps that to the per-port unix socket we listen on below.
	if cfg.ControlHandler != nil {
		chvArgs = append(chvArgs, "--vsock", fmt.Sprintf("cid=3,socket=%s", vsockSock))
	}
	chvCmd := exec.Command(chvPath, chvArgs...)
	// Pass the event write pipe to cloud-hypervisor as fd 3. ExtraFiles[0] maps
	// to fd 3 in the child (after stdin/stdout/stderr = 0/1/2), which must match
	// eventMonitorFd above: keep this slice exactly one element long.
	chvCmd.ExtraFiles = []*os.File{eventWritePipe}
	// Pdeathsig ensures cloud-hypervisor exits when its parent (thundersnapd/test
	// harness) dies, preventing orphaned VMs.
	chvCmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: unix.SIGTERM,
	}

	session := &VMSession{
		virtiofsdCmd:   virtiofsdCmd,
		passtCmd:       passtCmd,
		chvCmd:         chvCmd,
		virtiofsSock:   virtiofsSock,
		vsockSock:      vsockSock,
		done:           make(chan struct{}),
		panicked:       make(chan struct{}),
		controlHandler: cfg.ControlHandler,
	}

	// Run cloud-hypervisor in headless mode with a PTY (required for --serial tty)
	// Console output goes to our log system via a goroutine
	ptmx, err := pty.Start(chvCmd)
	if err != nil {
		eventReadPipe.Close()
		eventWritePipe.Close()
		cleanup()
		return nil, fmt.Errorf("start cloud-hypervisor with pty: %w", err)
	}
	log.Printf("cloud-hypervisor started with PID %d", chvCmd.Process.Pid)

	// Close write end of event pipe in parent - cloud-hypervisor has its own copy
	eventWritePipe.Close()

	// Monitor cloud-hypervisor in background
	go func() {
		chvCmd.Wait()
		ptmx.Close()
		eventReadPipe.Close()
		log.Printf("cloud-hypervisor exited")
		close(session.done)
	}()

	// Log console output from VM (prefix each line with "vm:")
	go func() {
		scanner := bufio.NewScanner(ptmx)
		for scanner.Scan() {
			log.Printf("vm: %s", scanner.Text())
		}
	}()

	// Monitor event stream for panic events
	go session.monitorEvents(eventReadPipe)

	// Start vsock listener if we have a control handler
	// Cloud-hypervisor's vsock uses a naming convention: when guest connects to
	// CID 2 (host) on port N, it looks for a Unix socket at <vsock-socket>_N
	if cfg.ControlHandler != nil {
		vsockPortSock := fmt.Sprintf("%s_%d", vsockSock, VsockPort)
		log.Printf("Creating vsock listener at %s for port %d", vsockPortSock, VsockPort)

		// Listen on the port-specific Unix socket
		ln, err := net.Listen("unix", vsockPortSock)
		if err != nil {
			session.Close()
			return nil, fmt.Errorf("listen on vsock socket: %w", err)
		}
		session.vsockListener = ln

		// Handle vsock connections in background
		go session.serveVsock()
	}

	return session, nil
}

// Wait blocks until the VM exits. It always returns a nil error; the return
// type is kept for API symmetry with other session types. Callers that only
// need to wait may prefer ranging over Done().
func (s *VMSession) Wait() error {
	<-s.done
	return nil
}

// Done returns a channel that is closed when the VM exits.
func (s *VMSession) Done() <-chan struct{} {
	return s.done
}

// Panicked returns a channel that is closed when a guest kernel panic is detected.
func (s *VMSession) Panicked() <-chan struct{} {
	return s.panicked
}

// chvEvent represents a cloud-hypervisor event from the event monitor stream.
type chvEvent struct {
	Source string `json:"source"`
	Event  string `json:"event"`
}

// monitorEvents reads the cloud-hypervisor event stream and detects panics.
// Cloud-hypervisor outputs pretty-printed JSON objects, so we use a JSON decoder
// which handles multi-line JSON correctly. It returns when the stream reaches
// EOF (VM exited) or a panic event is seen.
//
// NOTE (robustness): a single decode error currently stops panic monitoring for
// the rest of the VM's life. That is acceptable because the stream is produced
// by cloud-hypervisor itself (well-formed in practice) and a missed panic only
// loses the fast-path notification, not correctness; the VM still exits via done.
func (s *VMSession) monitorEvents(r io.Reader) {
	decoder := json.NewDecoder(r)
	for {
		var event chvEvent
		if err := decoder.Decode(&event); err != nil {
			if err == io.EOF {
				return
			}
			log.Printf("event-monitor: decode error: %v", err)
			return
		}
		log.Printf("event-monitor: source=%s event=%s", event.Source, event.Event)
		if event.Source == "guest" && event.Event == "panic" {
			log.Printf("event-monitor: guest kernel panic detected!")
			close(s.panicked)
			return
		}
	}
}

// Close terminates the VM session and cleans up resources.
func (s *VMSession) Close() error {
	log.Printf("Closing VM session, killing cloud-hypervisor PID %d", s.chvCmd.Process.Pid)

	// Kill cloud-hypervisor
	if err := s.chvCmd.Process.Kill(); err != nil {
		log.Printf("Warning: failed to kill cloud-hypervisor: %v", err)
	}

	// Wait for it to actually exit
	<-s.done
	log.Printf("cloud-hypervisor has exited")

	// Kill virtiofsd (it may have already exited when cloud-hypervisor disconnected)
	log.Printf("Killing virtiofsd PID %d", s.virtiofsdCmd.Process.Pid)
	s.virtiofsdCmd.Process.Kill()
	s.virtiofsdCmd.Wait()
	log.Printf("virtiofsd has exited")

	// Kill passt (it may have already exited when cloud-hypervisor disconnected)
	if s.passtCmd != nil {
		log.Printf("Killing passt PID %d", s.passtCmd.Process.Pid)
		s.passtCmd.Process.Kill()
		s.passtCmd.Wait()
		log.Printf("passt has exited")
	}

	// Close vsock listener if we have one
	if s.vsockListener != nil {
		s.vsockListener.Close()
	}

	// Clean up sockets
	os.Remove(s.virtiofsSock)
	// Also remove vsock socket and port-specific socket
	os.Remove(s.vsockSock)
	os.Remove(fmt.Sprintf("%s_%d", s.vsockSock, VsockPort))
	log.Printf("Cleaned up sockets")

	return nil
}

// serveVsock accepts connections on the vsock listener and serves the control protocol.
func (s *VMSession) serveVsock() {
	for {
		conn, err := s.vsockListener.Accept()
		if err != nil {
			// Listener was closed
			return
		}
		go s.handleVsockConnection(conn)
	}
}

// handleVsockConnection handles a single vsock connection from the guest.
// The guest opens a raw TCP-like connection, and we serve exactly one HTTP
// request over it (HTTP/1.0 style), then close — there is intentionally no
// keep-alive loop.
func (s *VMSession) handleVsockConnection(conn net.Conn) {
	defer conn.Close()

	// Parse the single HTTP request (request line + headers).
	reader := bufio.NewReader(conn)
	req, err := http.ReadRequest(reader)
	if err != nil {
		if err != io.EOF {
			log.Printf("vsock: failed to read request: %v", err)
		}
		return
	}

	// Create a response writer that buffers and writes to the connection.
	rw := &vsockResponseWriter{
		conn:    conn,
		headers: make(http.Header),
	}

	log.Printf("vsock: handling request %s %s", req.Method, req.URL.Path)

	s.controlHandler.ServeHTTP(rw, req)

	if err := rw.finish(); err != nil {
		log.Printf("vsock: failed to write response: %v", err)
	}
}

// vsockResponseWriter implements http.ResponseWriter for vsock connections.
type vsockResponseWriter struct {
	conn       net.Conn
	headers    http.Header
	statusCode int
	body       []byte
}

func (w *vsockResponseWriter) Header() http.Header {
	return w.headers
}

func (w *vsockResponseWriter) Write(data []byte) (int, error) {
	w.body = append(w.body, data...)
	return len(data), nil
}

func (w *vsockResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
}

func (w *vsockResponseWriter) finish() error {
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

	// Write body (skip a zero-length write, which is a no-op on the wire).
	if len(w.body) > 0 {
		if _, err := w.conn.Write(w.body); err != nil {
			return err
		}
	}

	return nil
}

// VshSocketPath returns the Unix socket path for connecting to the VM's vsh daemon.
// Callers can dial this socket and use the cloud-hypervisor vsock protocol
// (send "CONNECT <port>\n", wait for "OK <port>\n") to connect to vshd in the guest.
func (s *VMSession) VshSocketPath() string {
	return s.vsockSock
}
