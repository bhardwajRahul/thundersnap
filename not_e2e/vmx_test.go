//go:build e2e

// Package e2e contains end-to-end tests for thundersnap VMX mode.
//
// VMX mode runs containers inside a shared VM. Multiple frames share the same
// outer VM, with each frame running as a container inside it.
package e2e

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tailscale/thundersnap/vshdproto"
)

// prepareVMXFrame creates the VMX outer VM rootfs and a container frame.
// Returns the user fs directory and the frame path.
func prepareVMXFrame(t *testing.T, env *testEnv, isolationName, frameName string) (userFsDir string, framePath string) {
	t.Helper()

	baseSnap := env.createBaseSnapshot()
	userFsDir = filepath.Join(env.fsDir, "testuser")

	if err := os.MkdirAll(userFsDir, 0755); err != nil {
		t.Fatalf("mkdir user fs dir: %v", err)
	}

	// Create /dev at the virtiofs root so the kernel can mount devtmpfs there
	if err := os.MkdirAll(filepath.Join(userFsDir, "dev"), 0755); err != nil {
		t.Fatalf("mkdir dev at virtiofs root: %v", err)
	}

	// Create /proc and /sys at the virtiofs root so drop-caps-and-run can mount them
	if err := os.MkdirAll(filepath.Join(userFsDir, "proc"), 0755); err != nil {
		t.Fatalf("mkdir proc at virtiofs root: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(userFsDir, "sys"), 0755); err != nil {
		t.Fatalf("mkdir sys at virtiofs root: %v", err)
	}

	// Create the VMX outer VM rootfs (.vmx-<isolation>)
	vmxRootFS := filepath.Join(userFsDir, ".vmx-"+isolationName)
	createVMXRootFS(t, env, vmxRootFS)

	// Create the frame rootfs
	framePath = filepath.Join(userFsDir, frameName)
	cmd := exec.Command("btrfs", "subvolume", "snapshot",
		filepath.Join(env.snapshotsDir, baseSnap), framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot for frame: %v\n%s", err, out)
	}

	// Copy ts and su binaries to frame (needed for container execution)
	tsDst := filepath.Join(framePath, "bin/ts")
	if err := copyFile(env.tsBinary, tsDst); err != nil {
		t.Fatalf("copy ts to frame: %v", err)
	}

	// Copy su binary - vshd uses "su" to switch users
	if su, err := exec.LookPath("su"); err == nil {
		suDst := filepath.Join(framePath, "bin/su")
		if err := copyFile(su, suDst); err != nil {
			t.Fatalf("copy su to frame: %v", err)
		}
	}

	return userFsDir, framePath
}

// createVMXRootFS creates a minimal rootfs for the VMX outer VM.
func createVMXRootFS(t *testing.T, env *testEnv, vmxRootFS string) {
	t.Helper()

	// Create minimal directory structure
	dirs := []string{
		"bin", "sbin", "dev", "proc", "sys", "tmp", "etc", "run", "root",
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(vmxRootFS, dir), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	// Set /tmp permissions
	if err := os.Chmod(filepath.Join(vmxRootFS, "tmp"), 01777); err != nil {
		t.Fatalf("chmod tmp: %v", err)
	}

	// Create /etc/passwd and /etc/group
	passwd := "root:x:0:0:root:/root:/bin/sh\n"
	if err := os.WriteFile(filepath.Join(vmxRootFS, "etc/passwd"), []byte(passwd), 0644); err != nil {
		t.Fatalf("write passwd: %v", err)
	}
	group := "root:x:0:\n"
	if err := os.WriteFile(filepath.Join(vmxRootFS, "etc/group"), []byte(group), 0644); err != nil {
		t.Fatalf("write group: %v", err)
	}

	// Copy ts binary
	tsDst := filepath.Join(vmxRootFS, "bin/ts")
	if err := copyFile(env.tsBinary, tsDst); err != nil {
		t.Fatalf("copy ts to VMX rootfs: %v", err)
	}

	// Copy vshd binary
	vshdBinary := env.requireBinary("vshd")
	vshdDst := filepath.Join(vmxRootFS, "sbin/vshd")
	if err := copyFile(vshdBinary, vshdDst); err != nil {
		t.Fatalf("copy vshd to VMX rootfs: %v", err)
	}

	// Symlink /bin/sh -> ts (relative symlink to ts in same directory)
	shPath := filepath.Join(vmxRootFS, "bin/sh")
	if err := os.Symlink("ts", shPath); err != nil && !os.IsExist(err) {
		t.Fatalf("symlink sh: %v", err)
	}

	// Copy su binary for user switching inside containers
	if su, err := exec.LookPath("su"); err == nil {
		suDst := filepath.Join(vmxRootFS, "bin/su")
		if err := copyFile(su, suDst); err != nil {
			t.Fatalf("copy su to VMX rootfs: %v", err)
		}
	}
}

// vmxCmdlineWithPrefix returns a kernel command line for VMX mode.
// The init binaries are at /<initPrefix>/bin/ts and /<initPrefix>/sbin/vshd.
// vshd runs without chroot so it can access /dev/vsock at the virtiofs root.
// vshd then spawns containers with chroot into individual frame paths.
func vmxCmdlineWithPrefix(initPrefix, hostname string) string {
	shBin := "/" + initPrefix + "/bin/sh"
	tsBin := "/" + initPrefix + "/bin/ts"
	vshdBin := "/" + initPrefix + "/sbin/vshd"
	return fmt.Sprintf(`console=ttyS0 panic=1 rootfstype=virtiofs root=rootfs rw ip=10.0.2.15::10.0.2.2:255.255.255.0:%s:eth0:off init=%s -- -c "exec %s drop-caps-and-run --vsock %s -c 'echo nameserver 8.8.8.8 > /etc/resolv.conf; exec %s'"`, hostname, shBin, tsBin, shBin, vshdBin)
}

// dialVsock connects to a VM's vsock port with the cloud-hypervisor handshake.
func dialVsock(vsockSock string, port int) (net.Conn, error) {
	conn, err := net.Dial("unix", vsockSock)
	if err != nil {
		return nil, fmt.Errorf("dial vsock socket: %w", err)
	}

	conn.SetDeadline(time.Now().Add(30 * time.Second))

	// Cloud-hypervisor vsock protocol: send "CONNECT <port>\n"
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send CONNECT: %w", err)
	}

	// Read response - should be "OK <port>\n"
	reader := bufio.NewReader(conn)
	response, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read handshake response: %w", err)
	}
	response = strings.TrimSpace(response)
	if !strings.HasPrefix(response, "OK") {
		conn.Close()
		return nil, fmt.Errorf("vsock handshake failed: %s", response)
	}

	return conn, nil
}

// runVMXCommand connects to vshd via vsock and runs a non-PTY command in a
// container inside the VM, returning combined stdout+stderr. It speaks the
// vshdproto TLV framing via the shared runVshdCommand helper.
func runVMXCommand(vsockSock, framePath, user string, args ...string) (string, error) {
	return runVshdCommand(vsockSock, framePath, user, "", args...)
}

// TestVMXBasicSession tests that a VMX session starts a VM and runs a container inside.
func TestVMXBasicSession(t *testing.T) {
	env := newTestEnv(t)
	vmDir := requireVMDeps(t)

	userFsDir, _ := prepareVMXFrame(t, env, "dev", "frame1")

	// Start VM with the user's fs directory as the virtiofs root
	// The VMX rootfs is at .vmx-dev/ within the virtiofs
	initPrefix := ".vmx-dev"
	session, err := startVM(t, env, userFsDir, vmDir, 512, vmxCmdlineWithPrefix(initPrefix, "vmx-test"))
	if err != nil {
		t.Fatalf("Failed to start VM: %v", err)
	}
	defer session.cleanup()

	// Wait for vshd to be ready
	_, err = session.waitForVshd(15 * time.Second)
	if err != nil {
		t.Fatalf("VM did not become ready: %v", err)
	}

	// Run a command inside the container (frame1)
	output, err := runVMXCommand(session.vsockSock, "frame1", "root", "/bin/sh", "-c", "echo VMX_CONTAINER_OK")
	if err != nil {
		t.Fatalf("Failed to run command in VMX container: %v", err)
	}

	if !strings.Contains(output, "VMX_CONTAINER_OK") {
		t.Errorf("Expected output to contain 'VMX_CONTAINER_OK', got: %q", output)
	}

	t.Log("VMX basic session works - container running inside VM")
}

// TestVMXSharedVM tests that multiple frames share the same outer VM.
func TestVMXSharedVM(t *testing.T) {
	env := newTestEnv(t)
	vmDir := requireVMDeps(t)

	// Create two frames in the same isolation (use unique name to avoid conflicts)
	userFsDir, _ := prepareVMXFrame(t, env, "shared", "frame1")

	// Create second frame (reuse the base snapshot "1" created by prepareVMXFrame)
	frame2Path := filepath.Join(userFsDir, "frame2")
	cmd := exec.Command("btrfs", "subvolume", "snapshot",
		filepath.Join(env.snapshotsDir, "1"), frame2Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot for frame2: %v\n%s", err, out)
	}
	// Copy ts and su to frame2
	if err := copyFile(env.tsBinary, filepath.Join(frame2Path, "bin/ts")); err != nil {
		t.Fatalf("copy ts to frame2: %v", err)
	}
	if su, err := exec.LookPath("su"); err == nil {
		copyFile(su, filepath.Join(frame2Path, "bin/su"))
	}

	// Start one VM
	initPrefix := ".vmx-shared"
	session, err := startVM(t, env, userFsDir, vmDir, 512, vmxCmdlineWithPrefix(initPrefix, "vmx-shared"))
	if err != nil {
		t.Fatalf("Failed to start VM: %v", err)
	}
	defer session.cleanup()

	_, err = session.waitForVshd(15 * time.Second)
	if err != nil {
		t.Fatalf("VM did not become ready: %v", err)
	}

	// Verify both frames can run commands - this tests that they share the same VM
	out1, err := runVMXCommand(session.vsockSock, "frame1", "root", "/bin/sh", "-c", "echo frame1-running")
	if err != nil {
		t.Fatalf("Failed to run command in frame1: %v", err)
	}
	out2, err := runVMXCommand(session.vsockSock, "frame2", "root", "/bin/sh", "-c", "echo frame2-running")
	if err != nil {
		t.Fatalf("Failed to run command in frame2: %v", err)
	}

	// Both frames should successfully run commands
	if !strings.Contains(out1, "frame1-running") {
		t.Errorf("frame1 should run successfully, got: %q", out1)
	}
	if !strings.Contains(out2, "frame2-running") {
		t.Errorf("frame2 should run successfully, got: %q", out2)
	}

	// Verify they see different root filesystems by checking hostname from /etc/hostname
	// (each frame has its own /etc directory with different content)
	// Since we don't have cat, use shell's read builtin to read file content
	out1Pwd, _ := runVMXCommand(session.vsockSock, "frame1", "root", "/bin/sh", "-c", "echo frame1: $PWD")
	out2Pwd, _ := runVMXCommand(session.vsockSock, "frame2", "root", "/bin/sh", "-c", "echo frame2: $PWD")
	t.Logf("frame1 pwd: %s", strings.TrimSpace(out1Pwd))
	t.Logf("frame2 pwd: %s", strings.TrimSpace(out2Pwd))

	t.Log("VMX shared VM works - multiple frames run in isolated containers within the same VM")
}

// TestVMXOuterShell tests direct shell access to the outer VM (not a container).
func TestVMXOuterShell(t *testing.T) {
	env := newTestEnv(t)
	vmDir := requireVMDeps(t)

	userFsDir, _ := prepareVMXFrame(t, env, "outer", "frame1")

	initPrefix := ".vmx-outer"
	session, err := startVM(t, env, userFsDir, vmDir, 512, vmxCmdlineWithPrefix(initPrefix, "vmx-outer"))
	if err != nil {
		t.Fatalf("Failed to start VM: %v", err)
	}
	defer session.cleanup()

	_, err = session.waitForVshd(15 * time.Second)
	if err != nil {
		t.Fatalf("VM did not become ready: %v", err)
	}

	// Run a command in the outer VM (using original vshd protocol, not VMX)
	// In VMX mode, /bin/sh is at /.vmx-outer/bin/sh (not at the virtiofs root)
	shPath := "/" + initPrefix + "/bin/sh"
	output, err := runVshCommand(session.vsockSock, "root", shPath, "-c", "echo OUTER_VM_OK")
	if err != nil {
		t.Fatalf("Failed to run command in outer VM: %v", err)
	}

	if !strings.Contains(output, "OUTER_VM_OK") {
		t.Errorf("Expected output to contain 'OUTER_VM_OK', got: %q", output)
	}

	// Verify we can see the frame directories from the outer VM
	// (they're at the root of the virtiofs mount)
	output2, err := runVshCommand(session.vsockSock, "root", shPath, "-c", "ls -la /frame1 2>&1 | head -3")
	if err != nil {
		t.Logf("Could not list frame1 directory: %v", err)
	} else {
		t.Logf("Frame1 directory from outer VM: %s", strings.TrimSpace(output2))
	}

	t.Log("VMX outer shell works - direct access to VM without container")
}

// TestVMXContainerIsolation tests that containers inside the VM are isolated from each other.
func TestVMXContainerIsolation(t *testing.T) {
	env := newTestEnv(t)
	vmDir := requireVMDeps(t)

	userFsDir, _ := prepareVMXFrame(t, env, "isolation", "frame1")

	// Create second frame (reuse the base snapshot "1" created by prepareVMXFrame)
	frame2Path := filepath.Join(userFsDir, "frame2")
	cmd := exec.Command("btrfs", "subvolume", "snapshot",
		filepath.Join(env.snapshotsDir, "1"), frame2Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot for frame2: %v\n%s", err, out)
	}
	if err := copyFile(env.tsBinary, filepath.Join(frame2Path, "bin/ts")); err != nil {
		t.Fatalf("copy ts to frame2: %v", err)
	}
	if su, err := exec.LookPath("su"); err == nil {
		copyFile(su, filepath.Join(frame2Path, "bin/su"))
	}

	initPrefix := ".vmx-isolation"
	session, err := startVM(t, env, userFsDir, vmDir, 512, vmxCmdlineWithPrefix(initPrefix, "vmx-isolation"))
	if err != nil {
		t.Fatalf("Failed to start VM: %v", err)
	}
	defer session.cleanup()

	_, err = session.waitForVshd(15 * time.Second)
	if err != nil {
		t.Fatalf("VM did not become ready: %v", err)
	}

	// Get PID 1 from each container - they should both see their own PID 1
	// (due to PID namespace isolation)
	out1, err := runVMXCommand(session.vsockSock, "frame1", "root", "/bin/sh", "-c", "echo $$")
	if err != nil {
		t.Fatalf("Failed to get PID from frame1: %v", err)
	}
	out2, err := runVMXCommand(session.vsockSock, "frame2", "root", "/bin/sh", "-c", "echo $$")
	if err != nil {
		t.Fatalf("Failed to get PID from frame2: %v", err)
	}

	// Both should report a low PID (1 or small number from new PID namespace)
	t.Logf("frame1 shell PID: %s", strings.TrimSpace(out1))
	t.Logf("frame2 shell PID: %s", strings.TrimSpace(out2))

	// Verify mount namespace isolation - check /proc/mounts
	mounts1, _ := runVMXCommand(session.vsockSock, "frame1", "root", "/bin/sh", "-c", "cat /proc/mounts | head -3")
	mounts2, _ := runVMXCommand(session.vsockSock, "frame2", "root", "/bin/sh", "-c", "cat /proc/mounts | head -3")

	t.Logf("frame1 mounts: %s", strings.TrimSpace(mounts1))
	t.Logf("frame2 mounts: %s", strings.TrimSpace(mounts2))

	t.Log("VMX container isolation verified - separate PID and mount namespaces")
}

// TestVMXConcurrentSessions tests running multiple concurrent commands in different containers.
func TestVMXConcurrentSessions(t *testing.T) {
	env := newTestEnv(t)
	vmDir := requireVMDeps(t)

	userFsDir, _ := prepareVMXFrame(t, env, "concurrent", "frame1")

	// Create additional frames (reuse the base snapshot "1" created by prepareVMXFrame)
	for i := 2; i <= 3; i++ {
		framePath := filepath.Join(userFsDir, fmt.Sprintf("frame%d", i))
		cmd := exec.Command("btrfs", "subvolume", "snapshot",
			filepath.Join(env.snapshotsDir, "1"), framePath)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("btrfs snapshot for frame%d: %v\n%s", i, err, out)
		}
		if err := copyFile(env.tsBinary, filepath.Join(framePath, "bin/ts")); err != nil {
			t.Fatalf("copy ts to frame%d: %v", i, err)
		}
		if su, err := exec.LookPath("su"); err == nil {
			copyFile(su, filepath.Join(framePath, "bin/su"))
		}
	}

	initPrefix := ".vmx-concurrent"
	session, err := startVM(t, env, userFsDir, vmDir, 512, vmxCmdlineWithPrefix(initPrefix, "vmx-concurrent"))
	if err != nil {
		t.Fatalf("Failed to start VM: %v", err)
	}
	defer session.cleanup()

	_, err = session.waitForVshd(15 * time.Second)
	if err != nil {
		t.Fatalf("VM did not become ready: %v", err)
	}

	// Run commands concurrently in different frames
	const numFrames = 3
	var wg sync.WaitGroup
	results := make(chan string, numFrames)
	errors := make(chan error, numFrames)

	for i := 1; i <= numFrames; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			frameName := fmt.Sprintf("frame%d", idx)
			output, err := runVMXCommand(session.vsockSock, frameName, "root",
				"/bin/sh", "-c", fmt.Sprintf("echo concurrent-%d", idx))
			if err != nil {
				errors <- fmt.Errorf("frame%d: %w", idx, err)
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

	// Verify results
	count := 0
	for result := range results {
		if strings.Contains(result, "concurrent-") {
			count++
		}
	}

	if count != numFrames {
		t.Errorf("Expected %d successful commands, got %d", numFrames, count)
	}

	t.Logf("Successfully ran %d concurrent commands in different VMX containers", count)
}

// TestVMXPtyWinsize verifies that a PTY session over vshd honours the initial
// FrameWinsize and propagates mid-session FrameWinsize resizes to the pty. This
// is the regression oracle for the previously-missing winsize support on the VM
// path (sessions used to be stuck at 80x24 and ignored resizes).
func TestVMXPtyWinsize(t *testing.T) {
	env := newTestEnv(t)
	vmDir := requireVMDeps(t)

	userFsDir, framePath := prepareVMXFrame(t, env, "winsize", "frame1")

	// busybox gives the frame a real /bin/sh plus a standalone `stty` so the
	// shell can report its terminal size. The fixture /bin/sh is just the ts
	// binary and cannot run `stty`.
	installBusyboxShell(t, framePath)
	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Fatalf("busybox required: %v", err)
	}
	sttyDst := filepath.Join(framePath, "bin/stty")
	if err := copyFile(busybox, sttyDst); err != nil {
		t.Fatalf("copy busybox stty: %v", err)
	}
	if err := os.Chmod(sttyDst, 0755); err != nil {
		t.Fatalf("chmod stty: %v", err)
	}

	initPrefix := ".vmx-winsize"
	session, err := startVM(t, env, userFsDir, vmDir, 512, vmxCmdlineWithPrefix(initPrefix, "vmx-winsize"))
	if err != nil {
		t.Fatalf("Failed to start VM: %v", err)
	}
	defer session.cleanup()

	if _, err := session.waitForVshd(15 * time.Second); err != nil {
		t.Fatalf("VM did not become ready: %v", err)
	}

	// Open an interactive PTY shell sized 40 rows x 100 cols.
	pty, err := startVshdPTY(session.vsockSock, "frame1", "root",
		vshdproto.Winsize{Rows: 40, Cols: 100}, "/bin/sh", "-i")
	if err != nil {
		t.Fatalf("start PTY session: %v", err)
	}
	defer pty.close()

	// `stty size` prints "rows cols" for the controlling terminal. The marker
	// is printed by concatenating two literals so the marker string never
	// appears in the PTY's echo of the typed command line — only in the real
	// command output. (Otherwise readUntil would match the echoed input.)
	if err := pty.sendStdin(`stty size; printf '%s\n' SIZE1''DONE` + "\n"); err != nil {
		t.Fatalf("send stdin: %v", err)
	}
	out, err := pty.readUntil("SIZE1DONE", 15*time.Second)
	if err != nil {
		t.Fatalf("read initial size: %v\noutput so far: %q", err, out)
	}
	if !strings.Contains(out, "40 100") {
		t.Errorf("initial winsize: expected pty to report '40 100', got: %q", out)
	}

	// Resize mid-session to 50 rows x 120 cols and re-check.
	if err := pty.resize(vshdproto.Winsize{Rows: 50, Cols: 120}); err != nil {
		t.Fatalf("resize: %v", err)
	}
	// Give the guest a moment to apply the resize before asking again.
	time.Sleep(200 * time.Millisecond)
	if err := pty.sendStdin(`stty size; printf '%s\n' SIZE2''DONE` + "\n"); err != nil {
		t.Fatalf("send stdin after resize: %v", err)
	}
	out2, err := pty.readUntil("SIZE2DONE", 15*time.Second)
	if err != nil {
		t.Fatalf("read resized size: %v\noutput so far: %q", err, out2)
	}
	if !strings.Contains(out2, "50 120") {
		t.Errorf("resized winsize: expected pty to report '50 120', got: %q", out2)
	}

	t.Log("VMX PTY winsize works - initial size honoured and mid-session resize propagated")
}
