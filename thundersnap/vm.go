// Package thundersnap provides session management for container and VM environments.
package thundersnap

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/creack/pty"
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
}

// VsockPort is the vsock port used for the thunder control socket.
const VsockPort = 5223

// VshPort is the vsock port used for vsh shell connections.
const VshPort = 5222

// VMSession represents a running VM session.
type VMSession struct {
	virtiofsdCmd   *exec.Cmd
	chvCmd         *exec.Cmd
	virtiofsSock   string
	vsockSock      string       // cloud-hypervisor vsock unix socket path
	vsockListener  net.Listener // listener for control vsock connections (guest-to-host)
	done           chan struct{}
	controlHandler http.Handler
}

// StartVM starts a new VM session with the given configuration.
func StartVM(cfg VMConfig) (*VMSession, error) {
	// Create unique socket paths for this session
	sessionID := fmt.Sprintf("%d%d", os.Getpid(), time.Now().UnixNano())
	virtiofsSock := filepath.Join("/tmp", fmt.Sprintf("virtiofs-%s.sock", sessionID))
	vsockSock := filepath.Join("/tmp", fmt.Sprintf("vsock-%s.sock", sessionID))

	// Start virtiofsd
	log.Printf("Starting virtiofsd with shared-dir=%s", cfg.RootFS)
	virtiofsdCmd := exec.Command("/usr/libexec/virtiofsd",
		"--socket-path="+virtiofsSock,
		"--shared-dir="+cfg.RootFS,
		"--cache=always",
	)
	virtiofsdCmd.Stderr = os.Stderr
	if err := virtiofsdCmd.Start(); err != nil {
		return nil, fmt.Errorf("start virtiofsd: %w", err)
	}

	// Wait for virtiofsd socket to be created
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(virtiofsSock); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(virtiofsSock); err != nil {
		virtiofsdCmd.Process.Kill()
		virtiofsdCmd.Wait()
		os.Remove(virtiofsSock)
		return nil, fmt.Errorf("virtiofsd socket not created: %w", err)
	}
	log.Printf("virtiofsd socket ready at %s", virtiofsSock)

	// Paths to cloud-hypervisor and kernel
	chvPath := filepath.Join(cfg.VMDir, "cloud-hypervisor")
	kernelPath := filepath.Join(cfg.VMDir, "vmlinux")

	// Build kernel command line
	// The VM runs vshd in the foreground as init. When vshd exits (or is killed),
	// we power off the VM cleanly using busybox poweroff.
	// We must mount devpts for PTY support (required by vshd).
	// The ts command inside the VM detects vsock via /dev/vsock and connects directly.
	// We echo status messages to help debug boot issues.
	cmdline := `console=ttyS0 rootfstype=virtiofs root=rootfs rw init=/bin/sh -- -c "echo 'init: mounting devpts'; mkdir -p /dev/pts; mount -t devpts devpts /dev/pts; echo 'init: starting vshd'; /sbin/vshd; echo 'init: vshd exited, powering off'; /bin/busybox poweroff -f"`

	// Start cloud-hypervisor
	// --pvpanic enables the pvpanic device which allows the guest to signal panic to the host
	log.Printf("Starting cloud-hypervisor")
	chvArgs := []string{
		"--kernel", kernelPath,
		"--cpus", "boot=1",
		"--memory", "size=512M,shared=on",
		"--fs", fmt.Sprintf("tag=rootfs,socket=%s", virtiofsSock),
		"--cmdline", cmdline,
		"--serial", "tty",
		"--console", "off",
		"--pvpanic",
	}
	// Add vsock if we have a control handler
	if cfg.ControlHandler != nil {
		chvArgs = append(chvArgs, "--vsock", fmt.Sprintf("cid=3,socket=%s", vsockSock))
	}
	chvCmd := exec.Command(chvPath, chvArgs...)

	session := &VMSession{
		virtiofsdCmd:   virtiofsdCmd,
		chvCmd:         chvCmd,
		virtiofsSock:   virtiofsSock,
		vsockSock:      vsockSock,
		done:           make(chan struct{}),
		controlHandler: cfg.ControlHandler,
	}

	// Run cloud-hypervisor in headless mode with a PTY (required for --serial tty)
	// Console output goes to our log system via a goroutine
	ptmx, err := pty.Start(chvCmd)
	if err != nil {
		virtiofsdCmd.Process.Kill()
		virtiofsdCmd.Wait()
		os.Remove(virtiofsSock)
		return nil, fmt.Errorf("start cloud-hypervisor with pty: %w", err)
	}
	log.Printf("cloud-hypervisor started with PID %d", chvCmd.Process.Pid)

	// Monitor cloud-hypervisor in background
	go func() {
		chvCmd.Wait()
		ptmx.Close()
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

// Wait blocks until the VM exits.
func (s *VMSession) Wait() error {
	<-s.done
	return nil
}

// Done returns a channel that is closed when the VM exits.
func (s *VMSession) Done() <-chan struct{} {
	return s.done
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
// The guest opens a raw TCP-like connection, and we serve HTTP over it.
func (s *VMSession) handleVsockConnection(conn net.Conn) {
	defer conn.Close()

	// Read the HTTP request line and headers
	reader := bufio.NewReader(conn)
	for {
		// Parse HTTP request
		req, err := http.ReadRequest(reader)
		if err != nil {
			if err != io.EOF {
				log.Printf("vsock: failed to read request: %v", err)
			}
			return
		}

		// Create a response writer that writes to the connection
		rw := &vsockResponseWriter{
			conn:    conn,
			headers: make(http.Header),
		}

		// Serve the request
		s.controlHandler.ServeHTTP(rw, req)

		// Flush the response
		if err := rw.finish(); err != nil {
			log.Printf("vsock: failed to write response: %v", err)
			return
		}

		// Close after handling one request (HTTP/1.0 style)
		return
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

	// Write body
	if _, err := w.conn.Write(w.body); err != nil {
		return err
	}

	return nil
}

// VshSocketPath returns the Unix socket path for connecting to the VM's vsh daemon.
// Callers can dial this socket and use the cloud-hypervisor vsock protocol
// (send "CONNECT <port>\n", wait for "OK <port>\n") to connect to vshd in the guest.
func (s *VMSession) VshSocketPath() string {
	return s.vsockSock
}
