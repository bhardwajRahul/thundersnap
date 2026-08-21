// Package thunderboot provides VM boot testing with bcache-backed storage.
//
// This package boots a minimal VM with:
//   - virtiofs for the root filesystem (shared from host)
//   - Two virtio-blk devices for bcache: fast cache + slow backing store
//   - bcache assembled in guest init, formatted, and mounted at /data
//
// It's designed to test the bcache + NBD storage architecture described
// in disk-store-plan.md before deploying to real hardware.
package thunderboot

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
	"strings"
	"time"

	"github.com/creack/pty"
)

// VMConfig holds configuration for starting a thunderboot VM.
type VMConfig struct {
	// Initramfs selects appliance mode when non-empty. In appliance mode the
	// initramfs supplies PID 1, DataDisk is /dev/vda, and ConfigDir is shared
	// read-only with the guest as the "bootconfig" virtiofs tag.
	Initramfs string
	// ApplianceCache and ApplianceDisk are kernel-style storage specs. Host
	// paths are attached in declaration order and rewritten to /dev/vd[a-z].
	ApplianceCache string
	ApplianceDisk  string
	// ApplianceDiskSize is the size used when creating missing host image paths.
	ApplianceDiskSize string
	TestOnly          bool
	CPUs              int
	// RootFS is the path to the root filesystem to share via virtiofs.
	RootFS string
	// VMDir is the path to the directory containing cloud-hypervisor and vmlinux.
	VMDir string
	// CacheFile is the path to the host file used as the fast bcache cache device.
	// This appears as /dev/vda in the guest.
	CacheFile string
	// BackingFile is the path to the host file used as the slow bcache backing device.
	// This appears as /dev/vdb in the guest. Mutually exclusive with BackingNBD.
	BackingFile string
	// CacheSizeMB is the size of the cache file in megabytes.
	CacheSizeMB int
	// BackingSizeMB is the size of the backing file in megabytes.
	// Only used when BackingNBD is empty.
	BackingSizeMB int
	// BackingNBD is the host:port of an NBD server to use as the backing device.
	// When set, the guest connects to this NBD server instead of using a local file.
	// Mutually exclusive with BackingFile/BackingSizeMB.
	BackingNBD string
	// MemoryMB is the VM memory size in megabytes. Default is 512.
	MemoryMB int
	// InitScript is an optional custom init script to run instead of the default.
	// If empty, uses the default bcache setup script.
	InitScript string
}

// VshPort is the vsock port used for vsh shell connections.
const VshPort = 5222

// VMSession represents a running thunderboot VM session.
type VMSession struct {
	virtiofsdCmd  *exec.Cmd
	passtCmd      *exec.Cmd
	chvCmd        *exec.Cmd
	virtiofsSock  string
	vsockSock     string
	cacheFile     string
	backingFile   string
	preserveDisks bool
	done          chan struct{}
	consoleOut    *bufio.Scanner // for reading console output
	consolePtmx   *os.File
}

// DefaultInitScript returns the default init script that sets up bcache.
// It expects /dev/vda as cache device and /dev/vdb as backing device.
// NOTE: Use semicolons to separate commands - kernel cmdline doesn't interpret \n.
func DefaultInitScript() string {
	return `export PATH=/bin:/sbin:/usr/bin:/usr/sbin; echo 'init: mounting essential filesystems'; mkdir -p /dev/pts /proc /sys; mount -t devpts devpts /dev/pts; mount -t proc proc /proc; mount -t sysfs sysfs /sys; echo 'init: configuring network'; ip link set eth0 up; ip addr add 10.0.2.15/24 dev eth0; ip route add default via 10.0.2.2; echo 'init: setting up bcache'; echo /dev/vdb > /sys/fs/bcache/register; sleep 1; echo /dev/vda > /sys/fs/bcache/register; sleep 1; ls -la /dev/bcache0 2>/dev/null && echo 'init: /dev/bcache0 exists' || echo 'init: /dev/bcache0 not found'; cat /sys/block/bcache0/bcache/state 2>/dev/null || true; echo 'init: starting vshd'; exec /sbin/vshd`
}

// NBDInitScript returns an init script that connects to an NBD server for the backing device.
// The cache device is still /dev/vda, but the backing device comes from NBD on /dev/nbd0.
func NBDInitScript(nbdHost, nbdPort string) string {
	return fmt.Sprintf(`export PATH=/bin:/sbin:/usr/bin:/usr/sbin; echo 'init: mounting essential filesystems'; mkdir -p /dev/pts /proc /sys; mount -t devpts devpts /dev/pts; mount -t proc proc /proc; mount -t sysfs sysfs /sys; echo 'init: configuring network'; ip link set eth0 up; ip addr add 10.0.2.15/24 dev eth0; ip route add default via 10.0.2.2; echo 'init: loading nbd module'; modprobe nbd; echo 'init: connecting to NBD server %s:%s'; nbd-client %s %s /dev/nbd0 -persist; echo 'init: setting up bcache with NBD backing'; echo 'init: starting vshd'; exec /sbin/vshd`, nbdHost, nbdPort, nbdHost, nbdPort)
}

// StartVM starts a new thunderboot VM session with bcache-backed storage.
func StartVM(cfg VMConfig) (*VMSession, error) {
	if cfg.Initramfs != "" {
		return startApplianceVM(cfg)
	}
	if cfg.MemoryMB == 0 {
		cfg.MemoryMB = 512
	}
	if cfg.CacheSizeMB == 0 {
		cfg.CacheSizeMB = 256
	}
	if cfg.BackingSizeMB == 0 && cfg.BackingNBD == "" {
		cfg.BackingSizeMB = 512
	}

	useNBD := cfg.BackingNBD != ""

	// Create unique socket paths for this session
	sessionID := fmt.Sprintf("%d%d", os.Getpid(), time.Now().UnixNano())
	virtiofsSock := filepath.Join("/tmp", fmt.Sprintf("tb-virtiofs-%s.sock", sessionID))
	vsockSock := filepath.Join("/tmp", fmt.Sprintf("tb-vsock-%s.sock", sessionID))
	passtSock := filepath.Join("/tmp", fmt.Sprintf("tb-passt-%s.sock", sessionID))

	// Create cache file if path not provided
	cacheFile := cfg.CacheFile
	if cacheFile == "" {
		cacheFile = filepath.Join("/tmp", fmt.Sprintf("tb-cache-%s.img", sessionID))
	}

	// Create sparse file for cache device
	if err := createSparseFile(cacheFile, int64(cfg.CacheSizeMB)*1024*1024); err != nil {
		return nil, fmt.Errorf("create cache file: %w", err)
	}

	// Format cache file with bcache metadata
	if err := formatBcacheCache(cacheFile); err != nil {
		os.Remove(cacheFile)
		return nil, fmt.Errorf("format cache device: %w", err)
	}

	// Create backing file only if not using NBD
	var backingFile string
	if !useNBD {
		backingFile = cfg.BackingFile
		if backingFile == "" {
			backingFile = filepath.Join("/tmp", fmt.Sprintf("tb-backing-%s.img", sessionID))
		}
		if err := createSparseFile(backingFile, int64(cfg.BackingSizeMB)*1024*1024); err != nil {
			os.Remove(cacheFile)
			return nil, fmt.Errorf("create backing file: %w", err)
		}
		if err := formatBcacheBacking(backingFile); err != nil {
			os.Remove(cacheFile)
			os.Remove(backingFile)
			return nil, fmt.Errorf("format backing device: %w", err)
		}
	}

	// Helper to clean up files on error
	cleanupFiles := func() {
		os.Remove(cacheFile)
		if backingFile != "" {
			os.Remove(backingFile)
		}
	}

	// Start virtiofsd
	log.Printf("Starting virtiofsd with shared-dir=%s", cfg.RootFS)
	virtiofsdCmd := exec.Command("/usr/libexec/virtiofsd",
		"--socket-path="+virtiofsSock,
		"--shared-dir="+cfg.RootFS,
		"--cache=always",
		"--modcaps=-setfcap",
	)
	virtiofsdCmd.Stderr = os.Stderr
	if err := virtiofsdCmd.Start(); err != nil {
		cleanupFiles()
		return nil, fmt.Errorf("start virtiofsd: %w", err)
	}

	// Wait for virtiofsd socket
	if err := waitForSocket(virtiofsSock, 5*time.Second); err != nil {
		virtiofsdCmd.Process.Kill()
		virtiofsdCmd.Wait()
		cleanupFiles()
		return nil, fmt.Errorf("virtiofsd socket: %w", err)
	}
	log.Printf("virtiofsd socket ready at %s", virtiofsSock)

	// Start passt for networking
	log.Printf("Starting passt for user-space networking")
	passtCmd := exec.Command("passt",
		"--socket", passtSock,
		"--vhost-user",
		"--foreground",
		"--quiet",
		"-a", "10.0.2.15",
		"-g", "10.0.2.2",
		"-D", "none",
	)
	passtCmd.Stderr = os.Stderr
	if err := passtCmd.Start(); err != nil {
		virtiofsdCmd.Process.Kill()
		virtiofsdCmd.Wait()
		os.Remove(virtiofsSock)
		cleanupFiles()
		return nil, fmt.Errorf("start passt: %w", err)
	}

	if err := waitForSocket(passtSock, 5*time.Second); err != nil {
		passtCmd.Process.Kill()
		passtCmd.Wait()
		virtiofsdCmd.Process.Kill()
		virtiofsdCmd.Wait()
		os.Remove(virtiofsSock)
		cleanupFiles()
		return nil, fmt.Errorf("passt socket: %w", err)
	}
	log.Printf("passt socket ready at %s", passtSock)

	// Build init script
	initScript := cfg.InitScript
	if initScript == "" {
		if useNBD {
			// Parse host:port from BackingNBD
			nbdHost, nbdPort, _ := strings.Cut(cfg.BackingNBD, ":")
			if nbdPort == "" {
				nbdPort = "10809" // default NBD port
			}
			initScript = NBDInitScript(nbdHost, nbdPort)
		} else {
			initScript = DefaultInitScript()
		}
	}

	// Build kernel command line
	cmdline := fmt.Sprintf(
		`console=ttyS0 rootfstype=virtiofs root=rootfs rw init=/bin/sh -- -c %q`,
		initScript,
	)

	// Paths to cloud-hypervisor and kernel
	chvPath := filepath.Join(cfg.VMDir, "cloud-hypervisor")
	kernelPath := filepath.Join(cfg.VMDir, "vmlinux")

	// Start cloud-hypervisor with block devices
	// Note: cloud-hypervisor v49+ requires multiple disk specs as separate values
	// to a single --disk argument, not multiple --disk flags
	if useNBD {
		log.Printf("Starting cloud-hypervisor with NBD backing (%s)", cfg.BackingNBD)
	} else {
		log.Printf("Starting cloud-hypervisor with bcache devices")
	}
	chvArgs := []string{
		"--kernel", kernelPath,
		"--cpus", "boot=1",
		"--memory", fmt.Sprintf("size=%dM,shared=on", cfg.MemoryMB),
		"--fs", fmt.Sprintf("tag=rootfs,socket=%s", virtiofsSock),
		"--net", fmt.Sprintf("vhost_user=true,socket=%s,num_queues=2", passtSock),
		"--disk",
		fmt.Sprintf("path=%s", cacheFile), // /dev/vda - cache
	}
	if !useNBD {
		chvArgs = append(chvArgs, fmt.Sprintf("path=%s", backingFile)) // /dev/vdb - backing
	}
	chvArgs = append(chvArgs,
		"--cmdline", cmdline,
		"--serial", "tty",
		"--console", "off",
		"--pvpanic",
		"--vsock", fmt.Sprintf("cid=3,socket=%s", vsockSock),
	)
	chvCmd := exec.Command(chvPath, chvArgs...)

	session := &VMSession{
		virtiofsdCmd: virtiofsdCmd,
		passtCmd:     passtCmd,
		chvCmd:       chvCmd,
		virtiofsSock: virtiofsSock,
		vsockSock:    vsockSock,
		cacheFile:    cacheFile,
		backingFile:  backingFile,
		done:         make(chan struct{}),
	}

	// Run with PTY for serial console
	ptmx, err := pty.Start(chvCmd)
	if err != nil {
		session.cleanup()
		return nil, fmt.Errorf("start cloud-hypervisor: %w", err)
	}
	session.consolePtmx = ptmx
	session.consoleOut = bufio.NewScanner(ptmx)

	log.Printf("cloud-hypervisor started with PID %d", chvCmd.Process.Pid)

	// Monitor in background
	go func() {
		chvCmd.Wait()
		ptmx.Close()
		log.Printf("cloud-hypervisor exited")
		close(session.done)
	}()

	// Log console output
	go func() {
		scanner := bufio.NewScanner(ptmx)
		for scanner.Scan() {
			log.Printf("vm: %s", scanner.Text())
		}
	}()

	return session, nil
}

// VshSocketPath returns the Unix socket path for connecting to vshd in the guest.
func (s *VMSession) VshSocketPath() string {
	return s.vsockSock
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

// Close terminates the VM session and cleans up resources. It escalates
// SIGKILL -> SIGTERM-on-deadline so a wedged cloud-hypervisor (e.g. stuck in
// vhost teardown) cannot hang shutdown indefinitely, and always tears down
// passt/virtiofsd even if CHV is already gone.
func (s *VMSession) Close() error {
	log.Printf("Closing thunderboot VM session")

	if s.chvCmd != nil && s.chvCmd.Process != nil {
		log.Printf("Killing cloud-hypervisor PID %d", s.chvCmd.Process.Pid)
		_ = s.chvCmd.Process.Kill()
		select {
		case <-s.done:
		case <-time.After(10 * time.Second):
			log.Printf("cloud-hypervisor did not exit within 10s; forcing cleanup")
		}
	}

	s.cleanup()
	return nil
}

func (s *VMSession) cleanup() {
	if s.virtiofsdCmd != nil && s.virtiofsdCmd.Process != nil {
		s.virtiofsdCmd.Process.Kill()
		s.virtiofsdCmd.Wait()
	}
	if s.passtCmd != nil && s.passtCmd.Process != nil {
		s.passtCmd.Process.Kill()
		s.passtCmd.Wait()
	}

	os.Remove(s.virtiofsSock)
	os.Remove(s.vsockSock)
	os.Remove(fmt.Sprintf("%s_%d", s.vsockSock, VshPort))
	if !s.preserveDisks {
		os.Remove(s.cacheFile)
		os.Remove(s.backingFile)
	}
	log.Printf("Cleaned up thunderboot resources")
}

// createSparseFile creates a sparse file of the given size.
func createSparseFile(path string, size int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(size)
}

// waitForSocket waits for a Unix socket to exist.
func waitForSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s", path)
}

// formatBcacheCache formats a file as a bcache cache device.
// Uses make-bcache if available, otherwise falls back to native Go implementation.
func formatBcacheCache(path string) error {
	// Try make-bcache first (more reliable)
	// Use 128KB buckets to allow smaller test devices
	// Use full path since /usr/sbin may not be in PATH
	makeBcache := "/usr/sbin/make-bcache"
	if _, err := os.Stat(makeBcache); err != nil {
		makeBcache = "make-bcache" // fallback to PATH lookup
	}
	cmd := exec.Command(makeBcache, "-C", "--bucket", "128k", path)
	if output, err := cmd.CombinedOutput(); err == nil {
		return nil
	} else {
		log.Printf("make-bcache -C failed (%v: %s), trying native implementation", err, output)
	}
	return FormatBcacheCache(path, 128) // 128KB buckets
}

// formatBcacheBacking formats a file as a bcache backing device.
// Uses make-bcache if available, otherwise falls back to native Go implementation.
func formatBcacheBacking(path string) error {
	// Try make-bcache first (more reliable)
	// Use full path since /usr/sbin may not be in PATH
	makeBcache := "/usr/sbin/make-bcache"
	if _, err := os.Stat(makeBcache); err != nil {
		makeBcache = "make-bcache" // fallback to PATH lookup
	}
	cmd := exec.Command(makeBcache, "-B", path)
	if output, err := cmd.CombinedOutput(); err == nil {
		return nil
	} else {
		log.Printf("make-bcache -B failed (%v: %s), trying native implementation", err, output)
	}
	return FormatBcacheBacking(path)
}

// ConnectVsh connects to vshd in the guest and returns a connection.
// The caller should send the vsh protocol header before using the connection.
func (s *VMSession) ConnectVsh() (net.Conn, error) {
	conn, err := net.Dial("unix", s.vsockSock)
	if err != nil {
		return nil, fmt.Errorf("dial vsock: %w", err)
	}

	// Cloud Hypervisor vsock protocol: send "CONNECT <port>\n"
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", VshPort); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send CONNECT: %w", err)
	}

	// Read response - should be "OK <port>\n"
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read response: %w", err)
	}
	response := string(buf[:n])
	if len(response) < 2 || response[:2] != "OK" {
		conn.Close()
		return nil, fmt.Errorf("vsock connection failed: %s", response)
	}

	return conn, nil
}

// RunCommand runs a command in the guest via vshd and returns the output.
func (s *VMSession) RunCommand(user string, args ...string) ([]byte, error) {
	conn, err := s.ConnectVsh()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// Send vshd protocol: user\0, argcount\0, args...\0
	if _, err := fmt.Fprintf(conn, "%s\x00%d\x00", user, len(args)); err != nil {
		return nil, fmt.Errorf("send header: %w", err)
	}
	for _, arg := range args {
		if _, err := fmt.Fprintf(conn, "%s\x00", arg); err != nil {
			return nil, fmt.Errorf("send arg: %w", err)
		}
	}

	// Read all output
	output, err := io.ReadAll(conn)
	if err != nil {
		return nil, fmt.Errorf("read output: %w", err)
	}
	return output, nil
}

// vsockResponseWriter implements http.ResponseWriter for vsock connections.
// (Kept for potential future use with control handlers)
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

	statusText := http.StatusText(w.statusCode)
	if _, err := fmt.Fprintf(w.conn, "HTTP/1.0 %d %s\r\n", w.statusCode, statusText); err != nil {
		return err
	}

	w.headers.Set("Content-Length", strconv.Itoa(len(w.body)))

	for key, values := range w.headers {
		for _, value := range values {
			if _, err := fmt.Fprintf(w.conn, "%s: %s\r\n", key, value); err != nil {
				return err
			}
		}
	}

	if _, err := w.conn.Write([]byte("\r\n")); err != nil {
		return err
	}

	if _, err := w.conn.Write(w.body); err != nil {
		return err
	}

	return nil
}
