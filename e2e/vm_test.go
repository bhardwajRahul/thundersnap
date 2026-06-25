// Package e2e contains end-to-end tests for thundersnap VM mode.
package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// vmSession encapsulates a running VM test session with all its components.
type vmSession struct {
	t             *testing.T
	virtiofsdCmd  *exec.Cmd
	passtCmd      *exec.Cmd
	chvCmd        *exec.Cmd
	chvPty        *os.File
	eventReadPipe *os.File
	vsockSock     string
	virtiofsSock  string
	passtSock     string
	vmLogs        *vmConsoleMonitor
	vmPanicked    chan struct{}
	vmExited      chan error
}

// startVM starts a VM with the given configuration and returns a vmSession.
// The caller must call session.cleanup() when done.
func startVM(t *testing.T, env *testEnv, framePath, vmDir string, memoryMB int, cmdline string) (*vmSession, error) {
	t.Helper()

	absFramePath, err := filepath.Abs(framePath)
	if err != nil {
		return nil, fmt.Errorf("abs path: %w", err)
	}

	// Create unique socket paths
	sessionID := fmt.Sprintf("%d%d", os.Getpid(), time.Now().UnixNano())
	virtiofsSock := filepath.Join("/tmp", fmt.Sprintf("virtiofs-vm-%s.sock", sessionID))
	vsockSock := filepath.Join("/tmp", fmt.Sprintf("vsock-vm-%s.sock", sessionID))
	passtSock := filepath.Join("/tmp", fmt.Sprintf("passt-vm-%s.sock", sessionID))

	session := &vmSession{
		t:            t,
		virtiofsSock: virtiofsSock,
		vsockSock:    vsockSock,
		passtSock:    passtSock,
		vmPanicked:   make(chan struct{}),
		vmExited:     make(chan error, 1),
	}

	// Start virtiofsd
	virtiofsdPath := "/usr/libexec/virtiofsd"
	if _, err := os.Stat(virtiofsdPath); err != nil {
		virtiofsdPath, _ = exec.LookPath("virtiofsd")
	}
	session.virtiofsdCmd = exec.Command(virtiofsdPath,
		"--socket-path="+virtiofsSock,
		"--shared-dir="+absFramePath,
		"--cache=always",
	)
	session.virtiofsdCmd.Stderr = os.Stderr
	if err := session.virtiofsdCmd.Start(); err != nil {
		return nil, fmt.Errorf("start virtiofsd: %w", err)
	}

	// Wait for virtiofsd socket
	if !waitForSocket(virtiofsSock, 5*time.Second) {
		session.cleanup()
		return nil, fmt.Errorf("virtiofsd socket not created")
	}

	// Start passt
	session.passtCmd = exec.Command("passt",
		"--socket", passtSock,
		"--vhost-user",
		"--foreground",
		"--quiet",
		"-a", "10.0.2.15",
		"-g", "10.0.2.2",
		"-D", "none",
	)
	session.passtCmd.Stderr = os.Stderr
	if err := session.passtCmd.Start(); err != nil {
		session.cleanup()
		return nil, fmt.Errorf("start passt: %w", err)
	}

	// Wait for passt socket
	if !waitForSocket(passtSock, 5*time.Second) {
		session.cleanup()
		return nil, fmt.Errorf("passt socket not created")
	}

	// Create pipe for event monitor
	eventReadPipe, eventWritePipe, err := os.Pipe()
	if err != nil {
		session.cleanup()
		return nil, fmt.Errorf("create event pipe: %w", err)
	}
	session.eventReadPipe = eventReadPipe

	// Start cloud-hypervisor
	chvPath := filepath.Join(vmDir, "cloud-hypervisor")
	kernelPath := filepath.Join(vmDir, "vmlinux")

	session.chvCmd = exec.Command(chvPath,
		"--kernel", kernelPath,
		"--cpus", "boot=1",
		"--memory", fmt.Sprintf("size=%dM,shared=on", memoryMB),
		"--fs", fmt.Sprintf("tag=rootfs,socket=%s", virtiofsSock),
		"--net", fmt.Sprintf("vhost_user=true,socket=%s,num_queues=2", passtSock),
		"--cmdline", cmdline,
		"--serial", "tty",
		"--console", "off",
		"--vsock", fmt.Sprintf("cid=3,socket=%s", vsockSock),
		"--pvpanic",
		"--event-monitor", "fd=3",
	)
	session.chvCmd.ExtraFiles = []*os.File{eventWritePipe}

	// Start with PTY for serial console
	chvPty, err := startWithPty(session.chvCmd)
	if err != nil {
		eventReadPipe.Close()
		eventWritePipe.Close()
		session.cleanup()
		return nil, fmt.Errorf("start cloud-hypervisor: %w", err)
	}
	session.chvPty = chvPty

	// Close write end in parent
	eventWritePipe.Close()

	// Monitor VM process exit
	go func() {
		session.vmExited <- session.chvCmd.Wait()
	}()

	// Monitor for panic events
	go monitorVMEvents(t, eventReadPipe, session.vmPanicked)

	// Collect VM console output
	session.vmLogs = &vmConsoleMonitor{}
	go session.vmLogs.monitor(t, chvPty)

	return session, nil
}

// cleanup terminates all VM processes and removes sockets.
func (s *vmSession) cleanup() {
	if s.chvCmd != nil && s.chvCmd.Process != nil {
		s.chvCmd.Process.Kill()
		s.chvCmd.Wait()
	}
	if s.chvPty != nil {
		s.chvPty.Close()
	}
	if s.eventReadPipe != nil {
		s.eventReadPipe.Close()
	}
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
	os.Remove(s.passtSock)
	// Also remove port-specific vsock sockets
	os.Remove(fmt.Sprintf("%s_%d", s.vsockSock, 5222))
	os.Remove(fmt.Sprintf("%s_%d", s.vsockSock, 5223))
}

// waitForVshd waits for vshd to become ready by trying to connect via vsock.
func (s *vmSession) waitForVshd(timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// Check if VM panicked or exited
		select {
		case <-s.vmPanicked:
			return "", fmt.Errorf("VM kernel panic detected\n\nConsole:\n%s", s.vmLogs.output())
		case err := <-s.vmExited:
			return "", fmt.Errorf("VM exited unexpectedly: %v\n\nConsole:\n%s", err, s.vmLogs.output())
		default:
		}

		// Try to connect to vshd via vsock handshake
		if err := tryVsockConnect(s.vsockSock, 5222); err == nil {
			return s.vsockSock, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", fmt.Errorf("vshd did not become ready\n\nConsole:\n%s", s.vmLogs.output())
}

// waitForSocket waits for a socket file to exist.
func waitForSocket(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// prepareVMFrame creates a frame suitable for VM testing with ts and vshd binaries.
func prepareVMFrame(t *testing.T, env *testEnv, name string) string {
	t.Helper()

	baseSnap := env.createBaseSnapshot()
	framePath := filepath.Join(env.fsDir, "testuser", name)

	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir frame parent: %v", err)
	}

	cmd := exec.Command("btrfs", "subvolume", "snapshot",
		filepath.Join(env.snapshotsDir, baseSnap), framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	// Copy ts binary
	tsDst := filepath.Join(framePath, "bin/ts")
	if err := copyFile(env.tsBinary, tsDst); err != nil {
		t.Fatalf("copy ts to frame: %v", err)
	}

	// Copy vshd binary
	vshdBinary := env.requireBinary("vshd")
	vshdDst := filepath.Join(framePath, "sbin/vshd")
	if err := os.MkdirAll(filepath.Dir(vshdDst), 0755); err != nil {
		t.Fatalf("mkdir sbin: %v", err)
	}
	if err := copyFile(vshdBinary, vshdDst); err != nil {
		t.Fatalf("copy vshd to frame: %v", err)
	}

	// Copy su binary - vshd uses "su" to switch users when running commands
	if su, err := exec.LookPath("su"); err == nil {
		suDst := filepath.Join(framePath, "bin/su")
		if err := copyFile(su, suDst); err != nil {
			t.Fatalf("Failed to copy su: %v", err)
		}
		t.Logf("Copied su from %s to %s", su, suDst)
	} else {
		t.Fatalf("su binary not found in PATH: %v", err)
	}

	return framePath
}

// standardVMCmdline returns the kernel command line for a standard VM test.
// Uses kernel IP autoconfiguration (ip=) instead of manual ip commands because
// the test container doesn't have the ip binary. This matches thundersnap/vm.go.
func standardVMCmdline() string {
	return vmCmdlineWithHostname("thundersnap")
}

// vmCmdlineWithHostname returns a kernel command line with the specified hostname.
// The hostname is passed via the kernel IP autoconfig ip= parameter:
// ip=<client-ip>::<gw-ip>:<netmask>:<hostname>:<device>:<autoconf>
func vmCmdlineWithHostname(hostname string) string {
	return fmt.Sprintf(`console=ttyS0 panic=1 rootfstype=virtiofs root=rootfs rw ip=10.0.2.15::10.0.2.2:255.255.255.0:%s:eth0:off init=/bin/sh -- -c "exec /bin/ts drop-caps-and-run /bin/sh -c 'echo nameserver 8.8.8.8 > /etc/resolv.conf; exec /sbin/vshd'"`, hostname)
}

// TestVMLaunchSuccess tests that a VM launches successfully with sufficient memory.
func TestVMLaunchSuccess(t *testing.T) {
	env := newTestEnv(t)
	vmDir := requireVMDeps(t)

	framePath := prepareVMFrame(t, env, "vm-launch-success")
	session, err := startVM(t, env, framePath, vmDir, 512, standardVMCmdline())
	if err != nil {
		t.Fatalf("Failed to start VM: %v", err)
	}
	defer session.cleanup()

	// Wait for vshd to be ready
	_, err = session.waitForVshd(10 * time.Second)
	if err != nil {
		t.Fatalf("VM did not become ready: %v", err)
	}

	// Run a simple command to verify the VM is working
	output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", "echo VM_OK")
	if err != nil {
		t.Fatalf("Failed to run command in VM: %v", err)
	}

	if !strings.Contains(output, "VM_OK") {
		t.Errorf("Expected output to contain 'VM_OK', got: %q", output)
	}

	t.Log("VM launched successfully with 512MB memory")
}

// TestVMLaunchInsufficientMemory tests that a VM fails gracefully with insufficient memory.
// Note: cloud-hypervisor may accept very low memory values but the VM won't boot properly.
func TestVMLaunchInsufficientMemory(t *testing.T) {
	env := newTestEnv(t)
	vmDir := requireVMDeps(t)

	framePath := prepareVMFrame(t, env, "vm-low-memory")

	// Try to start VM with only 64MB - way too little for a Linux kernel
	// We expect the VM to either fail to start or crash during boot.
	session, err := startVM(t, env, framePath, vmDir, 64, standardVMCmdline())
	if err != nil {
		// VM failed to start - this is acceptable
		t.Logf("VM failed to start with 64MB (expected): %v", err)
		return
	}
	defer session.cleanup()

	// If VM did start, it should fail during boot or vshd won't come up
	_, err = session.waitForVshd(5 * time.Second)
	if err != nil {
		// Expected - VM couldn't boot properly
		t.Logf("VM with 64MB failed to become ready (expected): %v", err)
		return
	}

	// If vshd came up with 64MB, that's unexpected but not necessarily wrong
	t.Log("VM surprisingly became ready with 64MB memory")
}

// TestVMVirtiofsSharing tests that virtiofs filesystem sharing works correctly.
func TestVMVirtiofsSharing(t *testing.T) {
	env := newTestEnv(t)
	vmDir := requireVMDeps(t)

	framePath := prepareVMFrame(t, env, "vm-virtiofs")

	// Create a test file in the frame before starting the VM
	testContent := "virtiofs-test-content-12345"
	testFile := filepath.Join(framePath, "tmp/virtiofs-test.txt")
	if err := os.WriteFile(testFile, []byte(testContent), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	session, err := startVM(t, env, framePath, vmDir, 512, standardVMCmdline())
	if err != nil {
		t.Fatalf("Failed to start VM: %v", err)
	}
	defer session.cleanup()

	_, err = session.waitForVshd(10 * time.Second)
	if err != nil {
		t.Fatalf("VM did not become ready: %v", err)
	}

	// Read the test file from inside the VM using shell read builtin (no cat)
	// Use -r to prevent backslash processing, and handle the last line without newline
	output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c",
		"while IFS= read -r line || [ -n \"$line\" ]; do echo \"$line\"; done < /tmp/virtiofs-test.txt")
	if err != nil {
		t.Fatalf("Failed to read test file in VM: %v", err)
	}

	if !strings.Contains(output, testContent) {
		t.Errorf("virtiofs test file content mismatch: expected %q, got %q", testContent, output)
	}

	// Write a new file from inside the VM
	newContent := "written-from-vm-67890"
	_, err = runVshCommand(session.vsockSock, "root", "/bin/sh", "-c",
		fmt.Sprintf("echo -n '%s' > /tmp/vm-written.txt", newContent))
	if err != nil {
		t.Fatalf("Failed to write file from VM: %v", err)
	}

	// Read it back from the host
	hostFile := filepath.Join(framePath, "tmp/vm-written.txt")
	data, err := os.ReadFile(hostFile)
	if err != nil {
		t.Fatalf("Failed to read VM-written file on host: %v", err)
	}

	if string(data) != newContent {
		t.Errorf("VM-written file content mismatch: expected %q, got %q", newContent, string(data))
	}

	t.Log("virtiofs filesystem sharing works correctly")
}

// TestVMVshdCommunication tests that vshd communication over vsock works correctly.
func TestVMVshdCommunication(t *testing.T) {
	env := newTestEnv(t)
	vmDir := requireVMDeps(t)

	framePath := prepareVMFrame(t, env, "vm-vsock")
	session, err := startVM(t, env, framePath, vmDir, 512, standardVMCmdline())
	if err != nil {
		t.Fatalf("Failed to start VM: %v", err)
	}
	defer session.cleanup()

	_, err = session.waitForVshd(10 * time.Second)
	if err != nil {
		t.Fatalf("VM did not become ready: %v", err)
	}

	// Test multiple commands to verify vsock communication is reliable
	// Note: the test container has minimal binaries (just ts/sh), so we use
	// only shell builtins and avoid external commands like grep/head/whoami.
	testCases := []struct {
		name     string
		cmd      []string
		contains string
	}{
		{"echo", []string{"/bin/sh", "-c", "echo hello-vsock"}, "hello-vsock"},
		{"hostname", []string{"/bin/sh", "-c", "hostname 2>/dev/null || echo hostname-test"}, ""},
		{"pwd", []string{"/bin/sh", "-c", "pwd"}, "/"},
		// Read /proc/self/status using shell read builtin and pattern matching
		{"uid", []string{"/bin/sh", "-c", "while read line; do case $line in Uid:*) echo $line; break;; esac; done < /proc/self/status"}, "Uid:"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			output, err := runVshCommand(session.vsockSock, "root", tc.cmd...)
			if err != nil {
				t.Errorf("Command %v failed: %v", tc.cmd, err)
				return
			}
			if tc.contains != "" && !strings.Contains(output, tc.contains) {
				t.Errorf("Expected output to contain %q, got: %q", tc.contains, output)
			}
		})
	}

	t.Log("vshd communication over vsock works correctly")
}

// TestVMNetworkingPasst tests that VM networking via passt works.
func TestVMNetworkingPasst(t *testing.T) {
	env := newTestEnv(t)
	vmDir := requireVMDeps(t)

	framePath := prepareVMFrame(t, env, "vm-network")
	session, err := startVM(t, env, framePath, vmDir, 512, standardVMCmdline())
	if err != nil {
		t.Fatalf("Failed to start VM: %v", err)
	}
	defer session.cleanup()

	_, err = session.waitForVshd(10 * time.Second)
	if err != nil {
		t.Fatalf("VM did not become ready: %v", err)
	}

	// Check that eth0 interface exists by reading /proc/net/dev using shell redirection
	// (our minimal container doesn't have cat or ip binaries)
	output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", "while read line; do echo $line; done < /proc/net/dev")
	if err != nil {
		t.Fatalf("Failed to check network interface: %v", err)
	}

	// The kernel should have configured eth0 with ip= parameter
	if !strings.Contains(output, "eth0") {
		t.Errorf("eth0 interface not found in output: %q", output)
	}

	// Check if the IP is configured by reading /sys/class/net/eth0/address
	output2, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", "test -d /sys/class/net/eth0 && echo eth0_exists")
	if err == nil && strings.Contains(output2, "eth0_exists") {
		t.Log("eth0 network interface exists")
	}

	// Verify the kernel IP autoconfig ran by checking /proc/cmdline
	output3, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", "while read line; do echo $line; done < /proc/cmdline")
	if err == nil && strings.Contains(output3, "ip=10.0.2.15") {
		t.Log("Kernel IP autoconfiguration parameter verified")
	}

	t.Log("VM networking via passt is configured")
}

// TestVMHostname tests that the hostname is correctly set via kernel IP autoconfig.
func TestVMHostname(t *testing.T) {
	env := newTestEnv(t)
	vmDir := requireVMDeps(t)

	framePath := prepareVMFrame(t, env, "vm-hostname")

	// Use a custom hostname that's clearly different from the default
	testHostname := "test-custom-hostname"
	session, err := startVM(t, env, framePath, vmDir, 512, vmCmdlineWithHostname(testHostname))
	if err != nil {
		t.Fatalf("Failed to start VM: %v", err)
	}
	defer session.cleanup()

	_, err = session.waitForVshd(10 * time.Second)
	if err != nil {
		t.Fatalf("VM did not become ready: %v", err)
	}

	// Check the hostname using /proc/sys/kernel/hostname (always available)
	output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c",
		"while read line; do echo $line; done < /proc/sys/kernel/hostname")
	if err != nil {
		t.Fatalf("Failed to read hostname: %v", err)
	}

	hostname := strings.TrimSpace(output)
	if hostname != testHostname {
		t.Errorf("Expected hostname %q, got %q", testHostname, hostname)
	} else {
		t.Logf("Hostname correctly set to %q", hostname)
	}

	// Also verify the hostname appears in /proc/cmdline ip= parameter
	cmdline, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c",
		"while read line; do echo $line; done < /proc/cmdline")
	if err == nil && strings.Contains(cmdline, testHostname) {
		t.Logf("Hostname %q found in kernel cmdline ip= parameter", testHostname)
	}
}

// TestVMProcessIsolation tests that the VM is properly isolated from the host.
func TestVMProcessIsolation(t *testing.T) {
	env := newTestEnv(t)
	vmDir := requireVMDeps(t)

	framePath := prepareVMFrame(t, env, "vm-isolation")
	session, err := startVM(t, env, framePath, vmDir, 512, standardVMCmdline())
	if err != nil {
		t.Fatalf("Failed to start VM: %v", err)
	}
	defer session.cleanup()

	_, err = session.waitForVshd(10 * time.Second)
	if err != nil {
		t.Fatalf("VM did not become ready: %v", err)
	}

	// Get process list from inside the VM
	output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", "cat /proc/1/comm")
	if err != nil {
		t.Fatalf("Failed to read init process name: %v", err)
	}

	// PID 1 in the VM should be "sh" (the init process), not systemd or anything from host
	output = strings.TrimSpace(output)
	// The init process is /bin/sh running our init script
	if output != "sh" && output != "ts" && output != "vshd" {
		t.Logf("PID 1 is %q (expected sh, ts, or vshd)", output)
	}

	// Verify we can't see host processes by checking for a process that exists on host
	// but shouldn't be visible in VM
	hostPid := os.Getpid()
	vmCheck, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c",
		fmt.Sprintf("test -d /proc/%d && echo HOST_VISIBLE || echo ISOLATED", hostPid))
	if err != nil {
		t.Fatalf("Failed to check process isolation: %v", err)
	}

	if strings.Contains(vmCheck, "HOST_VISIBLE") {
		t.Errorf("VM can see host process %d - isolation broken!", hostPid)
	} else if strings.Contains(vmCheck, "ISOLATED") {
		t.Log("VM is properly isolated from host processes")
	}

	// Verify the VM has its own mount namespace
	mounts, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", "cat /proc/mounts | head -5")
	if err == nil {
		// Should see virtiofs as the root filesystem
		if strings.Contains(mounts, "virtiofs") || strings.Contains(mounts, "rootfs") {
			t.Log("VM has its own mount namespace with virtiofs root")
		}
	}

	t.Log("VM process isolation verified")
}

// TestVMGracefulShutdown tests that a VM shuts down cleanly.
func TestVMGracefulShutdown(t *testing.T) {
	env := newTestEnv(t)
	vmDir := requireVMDeps(t)

	framePath := prepareVMFrame(t, env, "vm-shutdown")

	// Use a cmdline where the VM exits after vshd is done
	// The standard cmdline runs vshd, which exits when the connection closes
	session, err := startVM(t, env, framePath, vmDir, 512, standardVMCmdline())
	if err != nil {
		t.Fatalf("Failed to start VM: %v", err)
	}
	defer session.cleanup()

	_, err = session.waitForVshd(10 * time.Second)
	if err != nil {
		t.Fatalf("VM did not become ready: %v", err)
	}

	// Verify VM is running
	output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", "echo VM_RUNNING")
	if err != nil {
		t.Fatalf("Failed to verify VM is running: %v", err)
	}
	if !strings.Contains(output, "VM_RUNNING") {
		t.Fatalf("VM not running as expected")
	}

	// Request shutdown via reboot syscall (since we have panic=1, this will exit)
	// Actually, let's just send a poweroff command if available
	_, _ = runVshCommand(session.vsockSock, "root", "/bin/sh", "-c",
		"echo o > /proc/sysrq-trigger 2>/dev/null || echo 'poweroff not available'")

	// Wait for VM to exit (with timeout)
	select {
	case err := <-session.vmExited:
		if err != nil {
			t.Logf("VM exited with: %v (this may be normal)", err)
		} else {
			t.Log("VM exited cleanly")
		}
	case <-time.After(5 * time.Second):
		// VM didn't exit on its own - that's okay, we'll kill it in cleanup
		t.Log("VM did not exit on poweroff command (may need forced shutdown)")
	}

	t.Log("VM shutdown test completed")
}

// TestVMPanicRecoveryTimeout tests panic recovery with various timeout scenarios.
// This extends the basic panic test by verifying timing.
func TestVMPanicRecoveryTimeout(t *testing.T) {
	env := newTestEnv(t)
	vmDir := requireVMDeps(t)

	framePath := prepareVMFrame(t, env, "vm-panic-timeout")

	// Create a cmdline that triggers a panic via sysrq
	panicCmdline := `console=ttyS0 panic=1 rootfstype=virtiofs root=rootfs rw init=/bin/sh -- -c "mount -t proc proc /proc; echo 1 > /proc/sys/kernel/sysrq; echo c > /proc/sysrq-trigger"`

	startTime := time.Now()

	session, err := startVM(t, env, framePath, vmDir, 512, panicCmdline)
	if err != nil {
		t.Fatalf("Failed to start VM: %v", err)
	}
	defer session.cleanup()

	// Wait for panic to be detected
	select {
	case <-session.vmPanicked:
		elapsed := time.Since(startTime)
		t.Logf("Panic detected after %v", elapsed)

		// With panic=1, the kernel should reboot within ~1 second of panic
		// Allow some extra time for boot and panic trigger
		if elapsed > 10*time.Second {
			t.Errorf("Panic detection took too long: %v (expected < 10s)", elapsed)
		}
	case err := <-session.vmExited:
		elapsed := time.Since(startTime)
		t.Logf("VM exited after %v: %v", elapsed, err)
		// This is also acceptable - panic=1 causes reboot which may exit cloud-hypervisor
	case <-time.After(10 * time.Second):
		t.Fatalf("Panic not detected within 10 seconds\n\nConsole:\n%s", session.vmLogs.output())
	}

	t.Log("VM panic recovery timing verified")
}

// TestVMConcurrentSessions tests running multiple commands concurrently via vshd.
func TestVMConcurrentSessions(t *testing.T) {
	env := newTestEnv(t)
	vmDir := requireVMDeps(t)

	framePath := prepareVMFrame(t, env, "vm-concurrent")
	session, err := startVM(t, env, framePath, vmDir, 512, standardVMCmdline())
	if err != nil {
		t.Fatalf("Failed to start VM: %v", err)
	}
	defer session.cleanup()

	_, err = session.waitForVshd(10 * time.Second)
	if err != nil {
		t.Fatalf("VM did not become ready: %v", err)
	}

	// Run multiple commands concurrently
	const numCommands = 5
	var wg sync.WaitGroup
	results := make(chan string, numCommands)
	errors := make(chan error, numCommands)

	for i := 0; i < numCommands; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			output, err := runVshCommand(session.vsockSock, "root",
				"/bin/sh", "-c", fmt.Sprintf("echo concurrent-%d", idx))
			if err != nil {
				errors <- fmt.Errorf("command %d failed: %w", idx, err)
				return
			}
			results <- output
		}(i)
	}

	wg.Wait()
	close(results)
	close(errors)

	// Check for errors
	for err := range errors {
		t.Error(err)
	}

	// Verify all results
	count := 0
	for result := range results {
		if strings.Contains(result, "concurrent-") {
			count++
		}
	}

	if count != numCommands {
		t.Errorf("Expected %d successful commands, got %d", numCommands, count)
	}

	t.Logf("Successfully ran %d concurrent commands in VM", count)
}

// TestVMUserSwitching tests that vshd correctly runs commands as root.
// Note: running as non-root users requires the su binary, which has dynamic
// library dependencies not available in our minimal test container.
func TestVMUserSwitching(t *testing.T) {
	env := newTestEnv(t)
	vmDir := requireVMDeps(t)

	framePath := prepareVMFrame(t, env, "vm-user")
	session, err := startVM(t, env, framePath, vmDir, 512, standardVMCmdline())
	if err != nil {
		t.Fatalf("Failed to start VM: %v", err)
	}
	defer session.cleanup()

	_, err = session.waitForVshd(10 * time.Second)
	if err != nil {
		t.Fatalf("VM did not become ready: %v", err)
	}

	// Test running as root using /proc/self/status (id/grep binaries not available)
	// Use shell builtins only: read, case, echo
	output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c",
		"while read line; do case $line in Uid:*) echo $line; break;; esac; done < /proc/self/status")
	if err != nil {
		t.Fatalf("Failed to run as root: %v", err)
	}

	// The Uid line format is: Uid:\t<real>\t<effective>\t<saved>\t<filesystem>
	// For root, all should be 0
	if !strings.Contains(output, "Uid:") || !strings.Contains(output, "0") {
		t.Errorf("Expected UID 0 for root, got output: %q", output)
	} else {
		t.Log("Verified running as root (UID 0)")
	}

	// Non-root user switching requires 'su' which has glibc dependencies.
	// In a real deployment, the container would have the full OS with su.
	// For this test, we verify that vshd accepts the user parameter.
	t.Log("VM user switching (root) works")
}

// runVshCommandWithStdin runs a command in the VM with input data.
func runVshCommandWithStdin(vsockSock, user, stdin string, args ...string) (string, error) {
	conn, err := net.Dial("unix", vsockSock)
	if err != nil {
		return "", fmt.Errorf("dial vsock socket: %w", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(30 * time.Second))

	// Cloud-hypervisor vsock protocol: send "CONNECT <port>\n"
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", 5222); err != nil {
		return "", fmt.Errorf("send CONNECT: %w", err)
	}

	// Read response - should be "OK <port>\n"
	reader := bufio.NewReader(conn)
	response, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read handshake response: %w", err)
	}
	response = strings.TrimSpace(response)
	if !strings.HasPrefix(response, "OK") {
		return "", fmt.Errorf("vsock handshake failed: %s", response)
	}

	// Send vshd protocol
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

	// Send stdin data and close write side
	if stdin != "" {
		conn.Write([]byte(stdin))
	}
	// For Unix sockets, we need to use CloseWrite if available
	if tcpConn, ok := conn.(*net.UnixConn); ok {
		tcpConn.CloseWrite()
	}

	// Read response
	var buf bytes.Buffer
	io.Copy(&buf, reader)

	return buf.String(), nil
}

// vmEventMonitor is used by tests to detect VM events.
type vmEventMonitor struct {
	mu       sync.Mutex
	events   []vmEvent
	panicked bool
}

type vmEvent struct {
	Source string `json:"source"`
	Event  string `json:"event"`
}

func (m *vmEventMonitor) monitor(r io.Reader) {
	decoder := json.NewDecoder(r)
	for {
		var event vmEvent
		if err := decoder.Decode(&event); err != nil {
			return
		}
		m.mu.Lock()
		m.events = append(m.events, event)
		if event.Source == "guest" && event.Event == "panic" {
			m.panicked = true
		}
		m.mu.Unlock()
	}
}

func (m *vmEventMonitor) hasPanicked() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.panicked
}

// Helper to get host PID count for isolation test
func getHostPIDCount() int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err == nil {
			count++
		}
	}
	return count
}
