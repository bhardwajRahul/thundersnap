// Package e2e contains end-to-end tests for thundersnap.
//
// These tests run the actual binaries (thundersnapd, ts) and interact with
// them only via CLI and HTTP APIs - no internal function calls.
//
// Requirements:
//   - root access (for btrfs subvolume operations)
//   - btrfs filesystem for temp directory
//   - pre-built binaries specified via environment variables:
//     - TS_BINARY: path to pre-built ts binary
//     - VSHD_BINARY: path to pre-built vshd binary
//     - THUNDERSNAPD_BINARY: path to pre-built thundersnapd binary
//
// Use "make e2e" to build binaries and run tests with the correct environment.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// testEnv holds paths and resources for a test environment.
type testEnv struct {
	t            *testing.T
	root         string // temp dir root
	repoRoot     string // ts2 repository root
	fsDir        string
	snapshotsDir string
	libexecDir   string
	tsBinary     string
	daemonBinary string
}

// requireBtrfsRoot skips if not root or not on btrfs.
func requireBtrfsRoot(t *testing.T) string {
	t.Helper()

	if os.Getuid() != 0 {
		t.Skip("e2e test requires root for btrfs and container ops")
	}
	if _, err := exec.LookPath("btrfs"); err != nil {
		t.Skip("btrfs not on PATH")
	}

	root := t.TempDir()
	cmd := exec.Command("stat", "-f", "-c", "%T", root)
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("stat -f failed: %v", err)
	}
	if strings.TrimSpace(string(out)) != "btrfs" {
		t.Skipf("test dir %s not on btrfs (got %q)", root, strings.TrimSpace(string(out)))
	}

	return root
}

// findRepoRoot finds the ts2 repository root by looking for go.mod.
func findRepoRoot(t *testing.T) string {
	t.Helper()

	// Start from current working directory
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	// Walk up looking for go.mod with module github.com/tailscale/thundersnap
	for {
		gomod := filepath.Join(dir, "go.mod")
		if data, err := os.ReadFile(gomod); err == nil {
			if strings.Contains(string(data), "module github.com/tailscale/thundersnap") {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find ts2 repo root (no go.mod with thundersnap module)")
		}
		dir = parent
	}
}

// newTestEnv creates a test environment with built binaries.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	root := requireBtrfsRoot(t)
	repoRoot := findRepoRoot(t)

	env := &testEnv{
		t:            t,
		root:         root,
		repoRoot:     repoRoot,
		fsDir:        filepath.Join(root, "fs"),
		snapshotsDir: filepath.Join(root, "snapshots"),
		libexecDir:   filepath.Join(root, "libexec"),
	}

	// Create directories
	for _, d := range []string{env.fsDir, env.snapshotsDir, env.libexecDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// Get pre-built binaries from environment
	env.tsBinary = env.requireBinary("ts")
	env.daemonBinary = env.requireBinary("thundersnapd")

	// Copy ts to libexec (thundersnapd looks for it there)
	if err := copyFile(env.tsBinary, filepath.Join(env.libexecDir, "ts")); err != nil {
		t.Fatalf("copy ts to libexec: %v", err)
	}

	t.Cleanup(env.cleanup)

	return env
}

func (e *testEnv) requireBinary(name string) string {
	e.t.Helper()

	envVar := strings.ToUpper(name) + "_BINARY"
	path := os.Getenv(envVar)
	if path == "" {
		e.t.Fatalf("%s not set; use 'make e2e' to run e2e tests", envVar)
	}
	if _, err := os.Stat(path); err != nil {
		e.t.Fatalf("%s=%s but file not found: %v", envVar, path, err)
	}
	e.t.Logf("using %s from %s", name, path)
	return path
}

func (e *testEnv) cleanup() {
	// Clean up btrfs subvolumes
	cleanupSubvolumes(e.fsDir)
	cleanupSubvolumes(e.snapshotsDir)
}

func cleanupSubvolumes(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			// Recursively clean nested subvolumes
			cleanupSubvolumes(path)
			exec.Command("btrfs", "subvolume", "delete", path).Run()
		}
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

	// Copy permissions
	info, err := in.Stat()
	if err != nil {
		return err
	}
	return os.Chmod(dst, info.Mode())
}

// createBaseSnapshot creates a minimal base snapshot "1" with test files.
// This mimics what downloading a Docker image would produce.
// It uses the programmatic test fixture generator for reproducible results.
func (e *testEnv) createBaseSnapshot() string {
	e.t.Helper()

	snapPath := filepath.Join(e.snapshotsDir, "1")

	// Create as btrfs subvolume
	cmd := exec.Command("btrfs", "subvolume", "create", snapPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		e.t.Fatalf("btrfs subvolume create: %v\n%s", err, out)
	}

	// Use the fixture generator to create a comprehensive test filesystem
	CreateTestContainer(e.t, snapPath, e.tsBinary)

	return "1"
}

// httpClient is an HTTP client that talks to a Unix socket.
type httpClient struct {
	sockPath string
	client   *http.Client
}

func newHTTPClient(sockPath string) *httpClient {
	return &httpClient{
		sockPath: sockPath,
		client: &http.Client{
			Transport: &http.Transport{
				DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", sockPath)
				},
			},
			Timeout: 60 * time.Second,
		},
	}
}

// doRequest performs an HTTP request with the vsock handshake.
func (c *httpClient) doRequest(method, path string, body io.Reader) (*http.Response, error) {
	// Connect to socket
	conn, err := net.Dial("unix", c.sockPath)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	// Send vsock handshake
	if _, err := fmt.Fprintf(conn, "CONNECT 5223\n"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("handshake write: %w", err)
	}

	// Read response
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("handshake read: %w", err)
	}
	resp := string(buf[:n])
	if !strings.HasPrefix(resp, "OK ") {
		conn.Close()
		return nil, fmt.Errorf("handshake failed: %s", resp)
	}

	// Now send HTTP request over the same connection
	var bodyBytes []byte
	if body != nil {
		bodyBytes, _ = io.ReadAll(body)
	}

	reqLine := fmt.Sprintf("%s %s HTTP/1.1\r\n", method, path)
	headers := "Host: localhost\r\nConnection: close\r\n"
	if len(bodyBytes) > 0 {
		headers += fmt.Sprintf("Content-Length: %d\r\n", len(bodyBytes))
		headers += "Content-Type: application/json\r\n"
	}
	headers += "\r\n"

	if _, err := conn.Write([]byte(reqLine + headers)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("write request: %w", err)
	}
	if len(bodyBytes) > 0 {
		if _, err := conn.Write(bodyBytes); err != nil {
			conn.Close()
			return nil, fmt.Errorf("write body: %w", err)
		}
	}

	// Read response
	return http.ReadResponse(bufio.NewReader(conn), nil)
}

func (c *httpClient) post(path string, body interface{}) (map[string]interface{}, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(data)
	}

	resp, err := c.doRequest("POST", path, bodyReader)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return result, nil
}

// TestE2EBasicSnapshot tests creating a base snapshot and snapping it.
func TestE2EBasicSnapshot(t *testing.T) {
	env := newTestEnv(t)

	// Create a base snapshot
	baseSnap := env.createBaseSnapshot()
	t.Logf("Created base snapshot: %s", baseSnap)

	// Verify the snapshot exists
	snapPath := filepath.Join(env.snapshotsDir, baseSnap)
	if _, err := os.Stat(snapPath); err != nil {
		t.Fatalf("snapshot not found: %v", err)
	}

	// Verify it has expected structure
	for _, path := range []string{"etc/passwd", "etc/group", "bin/ts"} {
		full := filepath.Join(snapPath, path)
		if _, err := os.Stat(full); err != nil {
			t.Errorf("expected file %s not found: %v", path, err)
		}
	}
}

// TestE2EOwnership verifies file ownership is preserved correctly.
func TestE2EOwnership(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()
	snapPath := filepath.Join(env.snapshotsDir, baseSnap)

	// Check ownership of home/user
	homePath := filepath.Join(snapPath, "home/user")
	info, err := os.Stat(homePath)
	if err != nil {
		t.Fatalf("stat home/user: %v", err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if stat.Uid != 1000 || stat.Gid != 1000 {
		t.Errorf("home/user ownership: got %d:%d, want 1000:1000", stat.Uid, stat.Gid)
	}

	// Check ownership of .profile
	profilePath := filepath.Join(snapPath, "home/user/.profile")
	info, err = os.Stat(profilePath)
	if err != nil {
		t.Fatalf("stat .profile: %v", err)
	}
	stat = info.Sys().(*syscall.Stat_t)
	if stat.Uid != 1000 || stat.Gid != 1000 {
		t.Errorf(".profile ownership: got %d:%d, want 1000:1000", stat.Uid, stat.Gid)
	}
}

// devCheckResult holds parsed output from "ts check-dev".
type devCheckResult struct {
	devicePerms map[string]string // device name -> octal perms
	linkTargets map[string]string // symlink name -> target
	dirs        map[string]bool   // directory name -> exists
	allEntries  []string          // all entries in /dev
}

// parseDevCheckOutput parses the output of "ts check-dev".
func parseDevCheckOutput(output string) devCheckResult {
	result := devCheckResult{
		devicePerms: make(map[string]string),
		linkTargets: make(map[string]string),
		dirs:        make(map[string]bool),
	}

	for _, line := range strings.Split(output, "\n") {
		parts := strings.Split(line, ":")
		if len(parts) < 2 {
			continue
		}
		switch parts[0] {
		case "DEV":
			if len(parts) >= 4 && parts[2] == "exists" {
				result.devicePerms[parts[1]] = parts[3]
			}
		case "LINK":
			if len(parts) >= 4 && parts[2] == "exists" {
				result.linkTargets[parts[1]] = parts[3]
			}
		case "DIR":
			if len(parts) >= 3 && parts[2] == "exists" {
				result.dirs[parts[1]] = true
			}
		case "ENTRY":
			result.allEntries = append(result.allEntries, parts[1])
		}
	}

	return result
}

// verifyDevSetup checks that /dev was set up correctly.
func verifyDevSetup(t *testing.T, result devCheckResult) {
	t.Helper()

	// Verify device permissions are 666 (the fix ensures this via Chmod after Mknod)
	expectedDevices := []string{"null", "zero", "full", "random", "urandom", "tty"}
	for _, dev := range expectedDevices {
		perm, ok := result.devicePerms[dev]
		if !ok {
			t.Errorf("/dev/%s: not found", dev)
			continue
		}
		if perm != "666" {
			t.Errorf("/dev/%s: permissions = %s, want 666", dev, perm)
		}
	}

	// Verify symlinks
	expectedLinks := map[string]string{
		"stdin":  "/proc/self/fd/0",
		"stdout": "/proc/self/fd/1",
		"stderr": "/proc/self/fd/2",
		"fd":     "/proc/self/fd",
	}
	for link, expectedTarget := range expectedLinks {
		target, ok := result.linkTargets[link]
		if !ok {
			t.Errorf("/dev/%s: symlink not found", link)
			continue
		}
		if target != expectedTarget {
			t.Errorf("/dev/%s: target = %q, want %q", link, target, expectedTarget)
		}
	}

	// Verify directories
	expectedDirs := []string{"pts", "shm", "mqueue"}
	for _, dir := range expectedDirs {
		if !result.dirs[dir] {
			t.Errorf("/dev/%s: directory not found", dir)
		}
	}

	// Verify that devtmpfs-specific entries are NOT present
	// These would indicate we're using the kernel's devtmpfs instead of our controlled tmpfs
	devtmpfsEntries := []string{
		"loop0", "loop1", "loop2", // loop devices
		"sda", "sdb", "vda",       // disk devices
		"dri",                     // GPU
		"kvm",                     // KVM
		"btrfs-control",          // btrfs
		"autofs",                 // autofs
		"console",                // console (we don't create this)
		"kmsg",                   // kernel messages
		"mem",                    // memory device
	}

	// Build set of allowed entries
	allowedEntries := map[string]bool{
		"null": true, "zero": true, "full": true,
		"random": true, "urandom": true, "tty": true, "vsock": true,
		"stdin": true, "stdout": true, "stderr": true, "fd": true,
		"pts": true, "shm": true, "mqueue": true, "ptmx": true,
	}

	for _, entry := range devtmpfsEntries {
		for _, actual := range result.allEntries {
			if actual == entry {
				t.Errorf("/dev/%s: found devtmpfs-specific entry that should not exist in controlled /dev", entry)
			}
		}
	}

	// Also warn about any unexpected entries (not necessarily an error, but useful for debugging)
	for _, entry := range result.allEntries {
		if !allowedEntries[entry] {
			t.Logf("Note: unexpected /dev entry: %s", entry)
		}
	}
}

// TestE2EDevSetup verifies that /dev is created correctly in both container and VM modes.
// This tests the fix for device permission issues where Mknod wasn't respecting
// mode bits due to umask - we now call Chmod after Mknod to ensure 0666 perms.
func TestE2EDevSetup(t *testing.T) {
	t.Run("container", testDevSetupContainer)
	t.Run("vm", testDevSetupVM)
}

// TestE2EVMPanicRecovery verifies that a kernel panic causes the VM to exit quickly.
// This tests the panic=1 kernel parameter which reboots after 1 second.
func TestE2EVMPanicRecovery(t *testing.T) {
	testVMPanicRecovery(t)
}

// testDevSetupContainer tests /dev setup in container mode (chroot-based).
func testDevSetupContainer(t *testing.T) {
	env := newTestEnv(t)

	// Create a base snapshot and frame
	baseSnap := env.createBaseSnapshot()
	framePath := filepath.Join(env.fsDir, "testuser", "devtest")

	// Create the frame directory structure
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir frame parent: %v", err)
	}

	// Clone snapshot to frame
	cmd := exec.Command("btrfs", "subvolume", "snapshot",
		filepath.Join(env.snapshotsDir, baseSnap), framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	// Copy ts binary into the frame
	tsDst := filepath.Join(framePath, "bin/ts")
	if err := copyFile(env.tsBinary, tsDst); err != nil {
		t.Fatalf("copy ts to frame: %v", err)
	}

	// Get absolute path for the frame (required for ts --chroot)
	absFramePath, err := filepath.Abs(framePath)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}

	// Run ts drop-caps-and-run the same way thundersnapd does:
	// The ts binary is run with CLONE_NEWPID, CLONE_NEWNS, CLONE_NEWUTS set
	// via SysProcAttr.Cloneflags. ts then:
	// 1. Makes all mounts private
	// 2. Chroots into the container
	// 3. Mounts /proc, /sys
	// 4. Calls setupDev() to create /dev with device nodes
	// 5. Execs the specified command
	//
	// We use the ts "check-dev" command which outputs /dev state in a parseable
	// format, avoiding the need for a shell.
	tsBinary := filepath.Join(absFramePath, "bin", "ts")
	cmd = exec.Command(tsBinary, "drop-caps-and-run",
		"--chroot="+absFramePath,
		"--", "/bin/ts", "check-dev")
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS,
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("ts drop-caps-and-run output: %s", output)
		t.Fatalf("ts drop-caps-and-run error: %v", err)
	}

	result := parseDevCheckOutput(string(output))
	verifyDevSetup(t, result)
}

// vmDir returns the path to VM binaries if available, or empty string if not.
// Can be overridden with THUNDERSNAP_VM_DIR environment variable.
func vmDir() string {
	// Allow override via environment
	if dir := os.Getenv("THUNDERSNAP_VM_DIR"); dir != "" {
		chv := filepath.Join(dir, "cloud-hypervisor")
		kernel := filepath.Join(dir, "vmlinux")
		if _, err := os.Stat(chv); err == nil {
			if _, err := os.Stat(kernel); err == nil {
				return dir
			}
		}
	}

	// Check common locations for cloud-hypervisor
	candidates := []string{
		"/usr/local/lib/thundersnap",
		"/usr/lib/thundersnap",
		"/opt/thundersnap",
	}
	for _, dir := range candidates {
		chv := filepath.Join(dir, "cloud-hypervisor")
		kernel := filepath.Join(dir, "vmlinux")
		if _, err := os.Stat(chv); err == nil {
			if _, err := os.Stat(kernel); err == nil {
				return dir
			}
		}
	}
	return ""
}

// requireVMDeps skips the test if VM dependencies are not available.
func requireVMDeps(t *testing.T) string {
	t.Helper()

	dir := vmDir()
	if dir == "" {
		t.Skip("VM test requires cloud-hypervisor and vmlinux (not found in standard locations)")
	}

	// Also need virtiofsd and passt
	if _, err := exec.LookPath("virtiofsd"); err != nil {
		if _, err := os.Stat("/usr/libexec/virtiofsd"); err != nil {
			t.Skip("VM test requires virtiofsd")
		}
	}
	if _, err := exec.LookPath("passt"); err != nil {
		t.Skip("VM test requires passt")
	}

	return dir
}

// testDevSetupVM tests /dev setup in VM mode.
func testDevSetupVM(t *testing.T) {
	env := newTestEnv(t)
	vmDir := requireVMDeps(t)

	// Create a base snapshot and frame for the VM
	baseSnap := env.createBaseSnapshot()
	framePath := filepath.Join(env.fsDir, "testuser", "vmdevtest")

	// Create the frame directory structure
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir frame parent: %v", err)
	}

	// Clone snapshot to frame
	cmd := exec.Command("btrfs", "subvolume", "snapshot",
		filepath.Join(env.snapshotsDir, baseSnap), framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	// Copy ts binary into the frame
	tsDst := filepath.Join(framePath, "bin/ts")
	if err := copyFile(env.tsBinary, tsDst); err != nil {
		t.Fatalf("copy ts to frame: %v", err)
	}

	// Get vshd and copy it into the frame
	vshdBinary := env.requireBinary("vshd")
	vshdDst := filepath.Join(framePath, "sbin/vshd")
	if err := os.MkdirAll(filepath.Dir(vshdDst), 0755); err != nil {
		t.Fatalf("mkdir sbin: %v", err)
	}
	if err := copyFile(vshdBinary, vshdDst); err != nil {
		t.Fatalf("copy vshd to frame: %v", err)
	}

	// We also need busybox for the init script (poweroff)
	// For now, just create a minimal script that doesn't need busybox
	// Actually, let's copy /bin/busybox if it exists
	if busybox, err := exec.LookPath("busybox"); err == nil {
		busyboxDst := filepath.Join(framePath, "bin/busybox")
		if err := copyFile(busybox, busyboxDst); err != nil {
			t.Logf("Warning: couldn't copy busybox: %v", err)
		}
	}

	// Get absolute path for the frame
	absFramePath, err := filepath.Abs(framePath)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}

	// Start the VM
	t.Logf("Starting VM with rootfs=%s vmdir=%s", absFramePath, vmDir)

	// Import the thundersnap package to use StartVM
	// For e2e tests, we'll use exec to run cloud-hypervisor directly
	// to avoid import cycles. Actually, let's just import thundersnap.
	//
	// Since we can't easily import thundersnap from e2e without cycles,
	// we'll start the VM components manually similar to what StartVM does.

	// Create unique socket paths
	sessionID := fmt.Sprintf("%d%d", os.Getpid(), time.Now().UnixNano())
	virtiofsSock := filepath.Join("/tmp", fmt.Sprintf("virtiofs-test-%s.sock", sessionID))
	vsockSock := filepath.Join("/tmp", fmt.Sprintf("vsock-test-%s.sock", sessionID))
	passtSock := filepath.Join("/tmp", fmt.Sprintf("passt-test-%s.sock", sessionID))

	// Cleanup sockets on exit
	defer os.Remove(virtiofsSock)
	defer os.Remove(vsockSock)
	defer os.Remove(passtSock)

	// Start virtiofsd
	virtiofsdPath := "/usr/libexec/virtiofsd"
	if _, err := os.Stat(virtiofsdPath); err != nil {
		virtiofsdPath, _ = exec.LookPath("virtiofsd")
	}
	virtiofsdCmd := exec.Command(virtiofsdPath,
		"--socket-path="+virtiofsSock,
		"--shared-dir="+absFramePath,
		"--cache=always",
	)
	virtiofsdCmd.Stderr = os.Stderr
	if err := virtiofsdCmd.Start(); err != nil {
		t.Fatalf("start virtiofsd: %v", err)
	}
	defer virtiofsdCmd.Wait()
	defer virtiofsdCmd.Process.Kill()

	// Wait for virtiofsd socket
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(virtiofsSock); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Start passt
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
		t.Fatalf("start passt: %v", err)
	}
	defer passtCmd.Wait()
	defer passtCmd.Process.Kill()

	// Wait for passt socket
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(passtSock); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Start cloud-hypervisor
	chvPath := filepath.Join(vmDir, "cloud-hypervisor")
	kernelPath := filepath.Join(vmDir, "vmlinux")

	// Build kernel command line - use sh as init, then call ts drop-caps-and-run
	// for consistent /dev setup between container and VM modes.
	// panic=1 ensures the VM reboots (and thus terminates) on kernel panic.
	cmdline := `console=ttyS0 panic=1 rootfstype=virtiofs root=rootfs rw init=/bin/sh -- -c "exec /bin/ts drop-caps-and-run /bin/sh -c 'ip link set eth0 up; ip addr add 10.0.2.15/24 dev eth0; ip route add default via 10.0.2.2; echo nameserver 8.8.8.8 > /etc/resolv.conf; exec /sbin/vshd'"`

	// Create pipe for event monitor to detect panics
	eventReadPipe, eventWritePipe, err := os.Pipe()
	if err != nil {
		t.Fatalf("create event pipe: %v", err)
	}
	defer eventReadPipe.Close()

	chvCmd := exec.Command(chvPath,
		"--kernel", kernelPath,
		"--cpus", "boot=1",
		"--memory", "size=512M,shared=on",
		"--fs", fmt.Sprintf("tag=rootfs,socket=%s", virtiofsSock),
		"--net", fmt.Sprintf("vhost_user=true,socket=%s,num_queues=2", passtSock),
		"--cmdline", cmdline,
		"--serial", "tty",
		"--console", "off",
		"--vsock", fmt.Sprintf("cid=3,socket=%s", vsockSock),
		"--pvpanic",
		"--event-monitor", "fd=3",
	)
	chvCmd.ExtraFiles = []*os.File{eventWritePipe}

	// Start with PTY for serial console
	chvPty, err := startWithPty(chvCmd)
	if err != nil {
		t.Fatalf("start cloud-hypervisor: %v", err)
	}
	defer chvCmd.Wait()         // Wait runs second (after Kill)
	defer chvCmd.Process.Kill() // Kill runs first
	defer chvPty.Close()

	// Close write end in parent
	eventWritePipe.Close()

	// Monitor VM process exit
	vmExited := make(chan error, 1)
	go func() {
		vmExited <- chvCmd.Wait()
	}()

	// Monitor for panic events
	vmPanicked := make(chan struct{})
	go monitorVMEvents(t, eventReadPipe, vmPanicked)

	// Collect VM console output for debugging
	vmLogs := &vmConsoleMonitor{}
	go vmLogs.monitor(t, chvPty)

	// Wait for vshd to be ready (vsock port socket to exist)
	vshSockPath := fmt.Sprintf("%s_%d", vsockSock, 5222)
	t.Logf("Waiting for vshd at %s", vshSockPath)
	var vshReady bool
	for i := 0; i < 50; i++ { // 5 seconds max
		// Check if VM panicked
		select {
		case <-vmPanicked:
			t.Fatalf("VM kernel panic detected!\n\nVM console output:\n%s", vmLogs.output())
		case err := <-vmExited:
			t.Fatalf("VM exited unexpectedly: %v\n\nVM console output:\n%s", err, vmLogs.output())
		default:
		}

		if _, err := os.Stat(vshSockPath); err == nil {
			vshReady = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !vshReady {
		// Check one more time for panic/exit before reporting timeout
		select {
		case <-vmPanicked:
			t.Fatalf("VM kernel panic detected!\n\nVM console output:\n%s", vmLogs.output())
		case err := <-vmExited:
			t.Fatalf("VM exited unexpectedly: %v\n\nVM console output:\n%s", err, vmLogs.output())
		default:
		}
		t.Fatalf("vshd did not become ready (socket %s not created)\n\nVM console output:\n%s", vshSockPath, vmLogs.output())
	}
	t.Logf("vshd is ready")

	// Connect to vshd and run "ts check-dev"
	output, err := runVshCommand(vsockSock, "root", "/bin/ts", "check-dev")
	if err != nil {
		t.Fatalf("run ts check-dev in VM: %v", err)
	}
	t.Logf("VM /dev check output:\n%s", output)

	result := parseDevCheckOutput(output)
	verifyDevSetup(t, result)
}

// testVMPanicRecovery verifies that a kernel panic causes the VM to terminate quickly.
func testVMPanicRecovery(t *testing.T) {
	env := newTestEnv(t)
	vmDir := requireVMDeps(t)

	// Create a minimal frame - we just need ts binary
	baseSnap := env.createBaseSnapshot()
	framePath := filepath.Join(env.fsDir, "testuser", "panictest")

	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir frame parent: %v", err)
	}

	cmd := exec.Command("btrfs", "subvolume", "snapshot",
		filepath.Join(env.snapshotsDir, baseSnap), framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	tsDst := filepath.Join(framePath, "bin/ts")
	if err := copyFile(env.tsBinary, tsDst); err != nil {
		t.Fatalf("copy ts to frame: %v", err)
	}

	absFramePath, err := filepath.Abs(framePath)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}

	// Create sockets
	sessionID := fmt.Sprintf("%d%d", os.Getpid(), time.Now().UnixNano())
	virtiofsSock := filepath.Join("/tmp", fmt.Sprintf("virtiofs-panic-%s.sock", sessionID))
	vsockSock := filepath.Join("/tmp", fmt.Sprintf("vsock-panic-%s.sock", sessionID))
	passtSock := filepath.Join("/tmp", fmt.Sprintf("passt-panic-%s.sock", sessionID))

	defer os.Remove(virtiofsSock)
	defer os.Remove(vsockSock)
	defer os.Remove(passtSock)

	// Start virtiofsd
	virtiofsdPath := "/usr/libexec/virtiofsd"
	if _, err := os.Stat(virtiofsdPath); err != nil {
		virtiofsdPath, _ = exec.LookPath("virtiofsd")
	}
	virtiofsdCmd := exec.Command(virtiofsdPath,
		"--socket-path="+virtiofsSock,
		"--shared-dir="+absFramePath,
		"--cache=always",
	)
	virtiofsdCmd.Stderr = os.Stderr
	if err := virtiofsdCmd.Start(); err != nil {
		t.Fatalf("start virtiofsd: %v", err)
	}
	defer virtiofsdCmd.Wait()
	defer virtiofsdCmd.Process.Kill()

	for i := 0; i < 50; i++ {
		if _, err := os.Stat(virtiofsSock); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Start passt
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
		t.Fatalf("start passt: %v", err)
	}
	defer passtCmd.Wait()
	defer passtCmd.Process.Kill()

	for i := 0; i < 50; i++ {
		if _, err := os.Stat(passtSock); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Start VM with init that triggers a panic via sysrq
	// The init script mounts /proc, enables sysrq, then triggers panic with 'c'
	chvPath := filepath.Join(vmDir, "cloud-hypervisor")
	kernelPath := filepath.Join(vmDir, "vmlinux")

	// This init deliberately triggers a kernel panic
	cmdline := `console=ttyS0 panic=1 rootfstype=virtiofs root=rootfs rw init=/bin/sh -- -c "mount -t proc proc /proc; echo 1 > /proc/sys/kernel/sysrq; echo c > /proc/sysrq-trigger"`

	// Create pipe for event monitor to detect panics
	eventReadPipe, eventWritePipe, err := os.Pipe()
	if err != nil {
		t.Fatalf("create event pipe: %v", err)
	}
	defer eventReadPipe.Close()

	chvCmd := exec.Command(chvPath,
		"--kernel", kernelPath,
		"--cpus", "boot=1",
		"--memory", "size=512M,shared=on",
		"--fs", fmt.Sprintf("tag=rootfs,socket=%s", virtiofsSock),
		"--net", fmt.Sprintf("vhost_user=true,socket=%s,num_queues=2", passtSock),
		"--cmdline", cmdline,
		"--serial", "tty",
		"--console", "off",
		"--pvpanic",
		"--event-monitor", "fd=3",
	)
	chvCmd.ExtraFiles = []*os.File{eventWritePipe}

	chvPty, err := startWithPty(chvCmd)
	if err != nil {
		t.Fatalf("start cloud-hypervisor: %v", err)
	}
	defer chvCmd.Wait()         // Wait runs second (after Kill)
	defer chvCmd.Process.Kill() // Kill runs first
	defer chvPty.Close()

	// Close write end in parent
	eventWritePipe.Close()

	// Monitor for panic events
	vmPanicked := make(chan struct{})
	go monitorVMEvents(t, eventReadPipe, vmPanicked)

	// Collect console output
	vmLogs := &vmConsoleMonitor{}
	go vmLogs.monitor(t, chvPty)

	// Panic should be detected within 5 seconds via event monitor
	select {
	case <-vmPanicked:
		t.Logf("Panic detected via event monitor as expected")
		t.Logf("Console output:\n%s", vmLogs.output())
	case <-time.After(5 * time.Second):
		t.Fatalf("Panic not detected within 5 seconds\n\nConsole output:\n%s", vmLogs.output())
	}
}

// monitorVMEvents reads cloud-hypervisor event stream and closes panicked channel on panic.
// Cloud-hypervisor outputs pretty-printed JSON objects, so we use a JSON decoder
// which handles multi-line JSON correctly.
func monitorVMEvents(t *testing.T, r io.Reader, panicked chan struct{}) {
	type chvEvent struct {
		Source string `json:"source"`
		Event  string `json:"event"`
	}

	decoder := json.NewDecoder(r)
	for {
		var event chvEvent
		if err := decoder.Decode(&event); err != nil {
			if err == io.EOF {
				return
			}
			t.Logf("event-monitor: decode error: %v", err)
			return
		}
		t.Logf("event-monitor: source=%s event=%s", event.Source, event.Event)
		if event.Source == "guest" && event.Event == "panic" {
			t.Logf("event-monitor: guest kernel panic detected!")
			close(panicked)
			return
		}
	}
}

// vmConsoleMonitor captures VM console output for debugging.
type vmConsoleMonitor struct {
	mu    sync.Mutex
	lines []string
}

// monitor reads from the PTY and logs output.
func (m *vmConsoleMonitor) monitor(t *testing.T, r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		t.Logf("vm: %s", line)

		m.mu.Lock()
		m.lines = append(m.lines, line)
		m.mu.Unlock()
	}
}

// output returns all captured console output as a string.
func (m *vmConsoleMonitor) output() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.lines, "\n")
}

// startWithPty starts a command with a PTY and returns the PTY master.
func startWithPty(cmd *exec.Cmd) (*os.File, error) {
	ptmx, tty, err := openPty()
	if err != nil {
		return nil, err
	}
	defer tty.Close()

	cmd.Stdin = tty
	cmd.Stdout = tty
	cmd.Stderr = tty
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
	}

	if err := cmd.Start(); err != nil {
		ptmx.Close()
		return nil, err
	}

	return ptmx, nil
}

// openPty opens a new PTY pair.
func openPty() (ptmx, tty *os.File, err error) {
	ptmx, err = os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}

	// Unlock the slave
	var n int
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, ptmx.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&n))); errno != 0 {
		ptmx.Close()
		return nil, nil, errno
	}

	// Get slave name
	var ptsNum int
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, ptmx.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&ptsNum))); errno != 0 {
		ptmx.Close()
		return nil, nil, errno
	}

	tty, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", ptsNum), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		ptmx.Close()
		return nil, nil, err
	}

	return ptmx, tty, nil
}

// runVshCommand connects to vshd via vsock and runs a command.
func runVshCommand(vsockSock, user string, args ...string) (string, error) {
	// Connect to cloud-hypervisor's vsock socket
	vshSockPath := fmt.Sprintf("%s_%d", vsockSock, 5222)
	conn, err := net.Dial("unix", vshSockPath)
	if err != nil {
		return "", fmt.Errorf("dial vsh socket: %w", err)
	}
	defer conn.Close()

	// Set deadline for the whole operation
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	// Send vshd protocol:
	// 1. target username (null-terminated)
	// 2. arg count (null-terminated)
	// 3. each arg (null-terminated)
	if _, err := conn.Write([]byte(user + "\x00")); err != nil {
		return "", fmt.Errorf("send user: %w", err)
	}
	if _, err := conn.Write([]byte(fmt.Sprintf("%d\x00", len(args)))); err != nil {
		return "", fmt.Errorf("send arg count: %w", err)
	}
	for _, arg := range args {
		if _, err := conn.Write([]byte(arg + "\x00")); err != nil {
			return "", fmt.Errorf("send arg: %w", err)
		}
	}

	// Read response
	var buf bytes.Buffer
	io.Copy(&buf, conn)

	return buf.String(), nil
}
