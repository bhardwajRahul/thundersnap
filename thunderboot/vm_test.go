package thunderboot

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// requireThunderbootDeps skips the test if required dependencies are missing.
func requireThunderbootDeps(t *testing.T) {
	t.Helper()

	// Check for root (needed for bcache operations in guest, and virtiofsd)
	if os.Getuid() != 0 {
		t.Skip("thunderboot tests require root")
	}

	// Note: make-bcache is no longer required - we have native Go implementation

	// Check for cloud-hypervisor
	vmDir := getVMDir()
	if _, err := os.Stat(filepath.Join(vmDir, "cloud-hypervisor")); err != nil {
		t.Skipf("cloud-hypervisor not found in %s", vmDir)
	}
	if _, err := os.Stat(filepath.Join(vmDir, "vmlinux")); err != nil {
		t.Skipf("vmlinux not found in %s", vmDir)
	}

	// Check for virtiofsd
	if _, err := exec.LookPath("virtiofsd"); err != nil {
		if _, err := os.Stat("/usr/libexec/virtiofsd"); err != nil {
			t.Skip("virtiofsd not found")
		}
	}

	// Check for passt
	if _, err := exec.LookPath("passt"); err != nil {
		t.Skip("passt not found")
	}
}

// createMinimalRootFS creates a minimal root filesystem for testing.
// It copies essential binaries and creates required directories.
func createMinimalRootFS(t *testing.T, dir string) {
	t.Helper()

	// Create essential directories
	dirs := []string{
		"bin", "sbin", "lib", "lib64", "usr/bin", "usr/lib",
		"dev", "proc", "sys", "tmp", "etc", "root", "home/user",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(dir, d), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// Copy busybox (required for init and shell)
	busyboxSrc := "/bin/busybox"
	if _, err := os.Stat(busyboxSrc); err != nil {
		t.Skipf("busybox not found at %s", busyboxSrc)
	}
	busyboxDst := filepath.Join(dir, "bin/busybox")
	if err := copyFile(busyboxSrc, busyboxDst); err != nil {
		t.Fatalf("copy busybox: %v", err)
	}
	os.Chmod(busyboxDst, 0755)

	// Create busybox symlinks for essential commands
	busyboxLinks := []string{
		"bin/sh", "bin/cat", "bin/ls", "bin/mount", "bin/umount",
		"bin/mkdir", "bin/echo", "bin/true", "bin/false", "bin/su",
		"bin/sleep", "bin/grep",
		"sbin/poweroff", "sbin/ip", "sbin/mke2fs", "sbin/mkfs.ext4",
	}
	for _, link := range busyboxLinks {
		linkPath := filepath.Join(dir, link)
		os.Remove(linkPath) // remove if exists
		if err := os.Symlink("/bin/busybox", linkPath); err != nil {
			t.Fatalf("symlink %s: %v", link, err)
		}
	}

	// Copy vshd if available (for shell access)
	// Try multiple locations for vshd
	projectDir := "/home/user/src/ts4"
	vshd := filepath.Join(projectDir, "bin/vshd")
	if _, err := os.Stat(vshd); err != nil {
		// Try to build it
		t.Log("vshd not found, building...")
		cmd := exec.Command("go", "build", "-o", vshd, "./cmd/vshd")
		cmd.Dir = projectDir
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Logf("failed to build vshd: %v\n%s", err, output)
			t.Skip("vshd not available")
		}
	}
	vshdDst := filepath.Join(dir, "sbin/vshd")
	if err := copyFile(vshd, vshdDst); err != nil {
		t.Fatalf("copy vshd: %v", err)
	}
	os.Chmod(vshdDst, 0755)

	// Create minimal /etc files
	passwd := "root:x:0:0:root:/root:/bin/sh\nuser:x:1000:1000:user:/home/user:/bin/sh\n"
	if err := os.WriteFile(filepath.Join(dir, "etc/passwd"), []byte(passwd), 0644); err != nil {
		t.Fatalf("write passwd: %v", err)
	}
	group := "root:x:0:\nuser:x:1000:\n"
	if err := os.WriteFile(filepath.Join(dir, "etc/group"), []byte(group), 0644); err != nil {
		t.Fatalf("write group: %v", err)
	}

	// Create empty profile for login shell
	if err := os.WriteFile(filepath.Join(dir, "etc/profile"), []byte(""), 0644); err != nil {
		t.Fatalf("write profile: %v", err)
	}

	t.Logf("Created minimal rootfs at %s", dir)
}

// copyFile copies a file from src to dst.
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0755)
}

// getVMDir returns the VM directory path, checking multiple locations.
func getVMDir() string {
	if v := os.Getenv("THUNDERBOOT_VM_DIR"); v != "" {
		return v
	}
	// Try to find vm directory relative to the test file location
	// or in common locations
	candidates := []string{
		"/home/user/src/ts4/vm",
		filepath.Join(os.Getenv("HOME"), "src/ts4/vm"),
		"./vm",
		"../vm",
	}
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(c, "cloud-hypervisor")); err == nil {
			return c
		}
	}
	return filepath.Join(os.Getenv("HOME"), "src/ts4/vm")
}

// TestVMBootWithBcache tests that a VM boots with bcache devices and can run busybox.
func TestVMBootWithBcache(t *testing.T) {
	requireThunderbootDeps(t)

	// Use the project's vm directory
	vmDir := getVMDir()

	// Create a minimal rootfs for testing
	// virtiofsd cannot share "/" directly (pivot_root fails)
	rootFS := filepath.Join(t.TempDir(), "rootfs")
	if err := os.MkdirAll(rootFS, 0755); err != nil {
		t.Fatal(err)
	}
	createMinimalRootFS(t, rootFS)

	// Create VM config - kernel bcache needs min 1024 buckets
	// With 128KB buckets: 1024 * 128KB = 128MB minimum cache
	cfg := VMConfig{
		RootFS:        rootFS,
		VMDir:         vmDir,
		CacheSizeMB:   128, // Min 128MB for 1024 x 128KB buckets
		BackingSizeMB: 256, // Backing device
		MemoryMB:      512,
	}

	t.Log("Starting thunderboot VM...")
	session, err := StartVM(cfg)
	if err != nil {
		t.Fatalf("StartVM failed: %v", err)
	}
	defer session.Close()

	// Wait for VM to boot and vshd to start
	// We'll poll for vshd availability
	t.Log("Waiting for vshd to become available...")
	var lastErr error
	var vshReady bool
	for i := 0; i < 30; i++ {
		time.Sleep(1 * time.Second)

		// Try to run a simple command
		output, err := session.RunCommand("root", "/bin/busybox", "echo", "hello")
		if err != nil {
			lastErr = err
			continue
		}

		if strings.TrimSpace(string(output)) == "hello" {
			t.Log("vshd is responding!")
			vshReady = true
			break
		}
		lastErr = fmt.Errorf("unexpected output: %q", output)
	}
	if !vshReady {
		t.Fatalf("vshd never became available: %v", lastErr)
	}

	// Test 1: Run busybox and verify output
	t.Log("Test 1: Running busybox echo...")
	output, err := session.RunCommand("root", "/bin/busybox", "echo", "thunderboot test passed")
	if err != nil {
		t.Fatalf("RunCommand failed: %v", err)
	}
	if !strings.Contains(string(output), "thunderboot test passed") {
		t.Errorf("unexpected output: %s", output)
	}
	t.Logf("busybox echo output: %s", strings.TrimSpace(string(output)))

	// Test 2: Check bcache device exists
	t.Log("Test 2: Checking /dev/bcache0 exists...")
	output, err = session.RunCommand("root", "/bin/busybox", "ls", "-la", "/dev/bcache0")
	if err != nil {
		t.Fatalf("ls /dev/bcache0 failed: %v", err)
	}
	t.Logf("/dev/bcache0: %s", strings.TrimSpace(string(output)))

	// Test 3: Check bcache state
	t.Log("Test 3: Checking bcache state...")
	output, err = session.RunCommand("root", "/bin/busybox", "cat", "/sys/block/bcache0/bcache/state")
	if err != nil {
		t.Logf("bcache state check failed: %v", err)
	} else {
		t.Logf("bcache state: %s", strings.TrimSpace(string(output)))
	}

	t.Log("All thunderboot tests completed")
}

// TestVMBootBasic tests basic VM boot without bcache (for faster iteration).
func TestVMBootBasic(t *testing.T) {
	requireThunderbootDeps(t)

	vmDir := getVMDir()

	// Create a minimal rootfs for testing
	rootFS := filepath.Join(t.TempDir(), "rootfs")
	if err := os.MkdirAll(rootFS, 0755); err != nil {
		t.Fatal(err)
	}
	createMinimalRootFS(t, rootFS)

	// Simple init script that just starts vshd without bcache setup
	// NOTE: Use semicolons - kernel cmdline doesn't interpret \n
	simpleInit := `echo 'init: mounting essential filesystems'; mkdir -p /dev/pts /proc /sys; mount -t devpts devpts /dev/pts; mount -t proc proc /proc; mount -t sysfs sysfs /sys; echo 'init: configuring network'; ip link set eth0 up; ip addr add 10.0.2.15/24 dev eth0; ip route add default via 10.0.2.2; echo 'init: starting vshd'; exec /sbin/vshd`

	cfg := VMConfig{
		RootFS:        rootFS,
		VMDir:         vmDir,
		CacheSizeMB:   32,
		BackingSizeMB: 32,
		MemoryMB:      256,
		InitScript:    simpleInit,
	}

	t.Log("Starting basic VM...")
	session, err := StartVM(cfg)
	if err != nil {
		t.Fatalf("StartVM failed: %v", err)
	}
	defer session.Close()

	// Wait for vshd
	t.Log("Waiting for vshd...")
	var lastErr error
	for i := 0; i < 20; i++ {
		time.Sleep(500 * time.Millisecond)
		_, err := session.RunCommand("root", "/bin/busybox", "true")
		if err == nil {
			t.Log("vshd ready")
			lastErr = nil
			break
		}
		lastErr = err
	}
	if lastErr != nil {
		t.Fatalf("vshd not available: %v", lastErr)
	}

	// Run busybox
	output, err := session.RunCommand("root", "/bin/busybox", "uname", "-a")
	if err != nil {
		t.Fatalf("uname failed: %v", err)
	}
	t.Logf("uname: %s", strings.TrimSpace(string(output)))
}
