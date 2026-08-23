// Command thunderboot boots a VM with bcache-backed storage and provides shell access.
//
// Usage:
//
//	thunderboot [flags]
//
// Flags:
//
//	-rootfs string    Root filesystem to share via virtiofs (required, or use -auto-rootfs)
//	-auto-rootfs      Create a minimal rootfs automatically in /tmp
//	-vmdir string     Directory containing cloud-hypervisor and vmlinux
//	-cache-mb int     Size of bcache cache device in MB (default 256)
//	-backing-mb int   Size of bcache backing device in MB (default 512)
//	-memory-mb int    VM memory size in MB (default 512)
//	-no-bcache        Skip bcache setup, just boot with virtiofs
//
// The command boots a VM with two virtio-blk devices configured for bcache
// (fast cache + slow backing store) and drops you into an interactive shell.
// Press Ctrl-C to exit and terminate the VM.
//
// Note: virtiofsd cannot share "/" directly. You must either provide a path
// to an existing rootfs directory, or use -auto-rootfs to create one.
package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pborman/getopt/v2"
	"github.com/tailscale/thundersnap/thunderboot"
	"github.com/tailscale/thundersnap/version"
	"golang.org/x/term"
)

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	rootFS := getopt.StringLong("rootfs", 'r', "", "Root filesystem to share via virtiofs")
	autoRootFS := getopt.BoolLong("auto-rootfs", 'a', "Create a minimal rootfs automatically")
	vmDir := getopt.StringLong("vmdir", 'v', "", "Directory containing cloud-hypervisor and vmlinux")
	cacheMB := getopt.IntLong("cache-mb", 'c', 256, "Size of bcache cache device in MB")
	backingMB := getopt.IntLong("backing-mb", 'b', 0, "Size of bcache backing device in MB (mutually exclusive with --backing-nbd)")
	backingNBD := getopt.StringLong("backing-nbd", 0, "", "NBD server host:port for backing device (mutually exclusive with --backing-mb)")
	memoryMB := getopt.IntLong("memory-mb", 'm', 512, "VM memory size in MB")
	noBcache := getopt.BoolLong("no-bcache", 'n', "Skip bcache setup, just boot with virtiofs")
	appliance := getopt.BoolLong("appliance", 0, "Boot the thundersnap appliance initramfs")
	initramfs := getopt.StringLong("initramfs", 0, "thunderboot-out/initramfs.cpio", "Appliance initramfs path")
	applianceCache := getopt.StringLong("cache", 0, "", "Appliance cache spec (device list or raid0/raid1;devices)")
	applianceDisk := getopt.StringLong("disk", 0, "thunderboot-out/data.img", "Appliance disk spec (devices, RAID, or nbd:// URL)")
	diskSize := getopt.StringLong("disk-size", 0, "2T", "Size for newly created host disk images")
	testOnly := getopt.BoolLong("testonly", 0, "Set up and mount storage, then power off")
	cpus := getopt.IntLong("cpus", 0, 1, "Appliance virtual CPU count")
	help := getopt.BoolLong("help", 'h', "Show help")
	showVersion := getopt.BoolLong("version", 0, "Print version and exit")

	getopt.Parse()

	if *showVersion {
		fmt.Printf("thunderboot %s\n", version.String())
		os.Exit(0)
	}
	if *help {
		getopt.Usage()
		os.Exit(0)
	}

	// Find vmdir
	if *vmDir == "" {
		// Try common locations
		candidates := []string{
			filepath.Join(os.Getenv("HOME"), "src/ts4/vm"),
			"./vm",
			"/usr/share/thunderboot/vm",
		}
		for _, c := range candidates {
			if _, err := os.Stat(filepath.Join(c, "cloud-hypervisor")); err == nil {
				*vmDir = c
				break
			}
		}
		if *vmDir == "" {
			log.Fatal("Could not find vmdir. Specify with -vmdir flag.")
		}
	}

	// Verify vmdir contents
	if _, err := os.Stat(filepath.Join(*vmDir, "cloud-hypervisor")); err != nil {
		log.Fatalf("cloud-hypervisor not found in %s", *vmDir)
	}
	if _, err := os.Stat(filepath.Join(*vmDir, "vmlinux")); err != nil {
		log.Fatalf("vmlinux not found in %s", *vmDir)
	}

	// Appliance mode supplies its root through an initramfs.
	if !*appliance && *rootFS == "" && !*autoRootFS {
		log.Fatal("Must specify -rootfs or -auto-rootfs. virtiofsd cannot share / directly.")
	}
	if *rootFS == "/" {
		log.Fatal("Cannot use / as rootfs. virtiofsd cannot pivot_root when sharing /. Use -auto-rootfs or specify a different directory.")
	}

	// Validate backing device options
	if *backingMB > 0 && *backingNBD != "" {
		log.Fatal("Cannot specify both --backing-mb and --backing-nbd")
	}
	if *backingMB == 0 && *backingNBD == "" && !*noBcache {
		*backingMB = 512 // default when neither specified
	}

	// Create auto rootfs if requested
	var tempRootFS string
	if *autoRootFS && !*appliance {
		var err error
		tempRootFS, err = createAutoRootFS(*vmDir)
		if err != nil {
			log.Fatalf("Failed to create auto rootfs: %v", err)
		}
		*rootFS = tempRootFS
		defer os.RemoveAll(tempRootFS)
		log.Printf("Created temporary rootfs at %s", tempRootFS)
	}

	// Build config
	cfg := thunderboot.VMConfig{
		RootFS:        *rootFS,
		VMDir:         *vmDir,
		CacheSizeMB:   *cacheMB,
		BackingSizeMB: *backingMB,
		BackingNBD:    *backingNBD,
		MemoryMB:      *memoryMB,
	}
	if *appliance {
		cfg.Initramfs = *initramfs
		cfg.ApplianceCache = *applianceCache
		cfg.ApplianceDisk = *applianceDisk
		cfg.ApplianceDiskSize = *diskSize
		cfg.TestOnly = *testOnly
		cfg.CPUs = *cpus
	}

	// Use simple init script if no bcache
	if *noBcache && !*appliance {
		// Keep this on one line: the kernel command-line parser does not
		// preserve the newlines escaped by strconv.Quote/%q for the shell.
		cfg.InitScript = `echo 'init: mounting essential filesystems'; mkdir -p /dev/pts /proc /sys; mount -t devpts devpts /dev/pts; mount -t proc proc /proc; mount -t sysfs sysfs /sys; echo 'init: configuring network'; ip link set eth0 up 2>/dev/null || true; ip addr add 10.0.2.15/24 dev eth0 2>/dev/null || true; ip route add default via 10.0.2.2 2>/dev/null || true; echo 'init: starting vshd'; exec /sbin/vshd`
	}

	log.Printf("Starting thunderboot VM...")
	if *appliance {
		log.Printf("  Initramfs: %s", cfg.Initramfs)
		log.Printf("  Cache: %s", cfg.ApplianceCache)
		log.Printf("  Disk: %s (%s for new host images)", cfg.ApplianceDisk, cfg.ApplianceDiskSize)
		log.Printf("  CPUs: %d", cfg.CPUs)
	} else {
		log.Printf("  RootFS: %s", cfg.RootFS)
	}
	log.Printf("  VMDir: %s", cfg.VMDir)
	log.Printf("  Cache: %d MB", cfg.CacheSizeMB)
	if cfg.BackingNBD != "" {
		log.Printf("  Backing: NBD %s", cfg.BackingNBD)
	} else {
		log.Printf("  Backing: %d MB (local file)", cfg.BackingSizeMB)
	}
	log.Printf("  Memory: %d MB", cfg.MemoryMB)
	if *noBcache {
		log.Printf("  bcache: disabled")
	}

	session, err := thunderboot.StartVM(cfg)
	if err != nil {
		log.Fatalf("Failed to start VM: %v", err)
	}

	// Handle Ctrl-C to cleanly shut down
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Printf("\nReceived signal, shutting down...")
		session.Close()
		os.Exit(0)
	}()

	if *appliance {
		log.Printf("Appliance VM started; serial console follows")
		if err := session.Wait(); err != nil {
			log.Fatal(err)
		}
		// Reap passt and remove its socket. This is particularly important when
		// stdout/stderr are pipes: a surviving helper keeps the pipe open and
		// makes callers wait forever after Cloud Hypervisor exits.
		if err := session.Close(); err != nil {
			log.Printf("cleanup: %v", err)
		}
		return
	}

	log.Printf("VM started, waiting for vshd...")

	// Wait for vshd to become available
	var connected bool
	for i := 0; i < 30; i++ {
		select {
		case <-session.Done():
			log.Fatal("VM exited before vshd became available")
		case <-time.After(1 * time.Second):
		}

		// Try to connect
		_, err := session.RunCommand("root", "/bin/busybox", "true")
		if err == nil {
			connected = true
			break
		}
	}

	if !connected {
		log.Fatal("Timeout waiting for vshd")
	}

	log.Printf("vshd ready! Connecting to shell...")
	log.Printf("(Press Ctrl-C to exit)")
	fmt.Println()

	// Connect to interactive shell via vshd
	if err := runInteractiveShell(session); err != nil {
		log.Printf("Shell exited: %v", err)
	}

	log.Printf("Shutting down VM...")
	session.Close()
}

// runInteractiveShell connects to vshd and runs an interactive shell.
func runInteractiveShell(session *thunderboot.VMSession) error {
	conn, err := session.ConnectVsh()
	if err != nil {
		return fmt.Errorf("connect to vshd: %w", err)
	}
	defer conn.Close()

	// Send vshd protocol for interactive shell: user\0, 0\0 (zero args = interactive)
	if _, err := fmt.Fprintf(conn, "root\x000\x00"); err != nil {
		return fmt.Errorf("send shell request: %w", err)
	}

	// Put terminal in raw mode if stdin is a tty
	var oldState *term.State
	if term.IsTerminal(int(os.Stdin.Fd())) {
		oldState, err = term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			return fmt.Errorf("set raw mode: %w", err)
		}
		defer term.Restore(int(os.Stdin.Fd()), oldState)

		// Handle window size changes
		winchCh := make(chan os.Signal, 1)
		signal.Notify(winchCh, syscall.SIGWINCH)
		go func() {
			for range winchCh {
				// Could send window size to guest here
			}
		}()
	}

	done := make(chan struct{})

	// stdin -> vsock
	go func() {
		io.Copy(conn, os.Stdin)
	}()

	// vsock -> stdout
	go func() {
		io.Copy(os.Stdout, conn)
		close(done)
	}()

	// Wait for shell to exit or VM to terminate
	select {
	case <-done:
	case <-session.Done():
	}

	// Restore terminal before returning
	if oldState != nil {
		term.Restore(int(os.Stdin.Fd()), oldState)
	}

	// Print newline to clean up terminal
	fmt.Println()

	return nil
}

// unused but kept for potential command execution mode
func runCommand(session *thunderboot.VMSession, args []string) error {
	output, err := session.RunCommand("root", args...)
	if err != nil {
		return err
	}
	fmt.Print(string(output))
	if !strings.HasSuffix(string(output), "\n") {
		fmt.Println()
	}
	return nil
}

// createAutoRootFS creates a minimal root filesystem for the VM.
func createAutoRootFS(vmDir string) (string, error) {
	dir, err := os.MkdirTemp("", "thunderboot-rootfs-")
	if err != nil {
		return "", err
	}

	// Create essential directories
	dirs := []string{
		"bin", "sbin", "lib", "lib64", "usr/bin", "usr/lib",
		"dev", "proc", "sys", "tmp", "etc", "root", "home/user", "data",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(dir, d), 0755); err != nil {
			os.RemoveAll(dir)
			return "", fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	// Copy busybox
	busyboxSrc := "/bin/busybox"
	if _, err := os.Stat(busyboxSrc); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("busybox not found at %s", busyboxSrc)
	}
	busyboxDst := filepath.Join(dir, "bin/busybox")
	if err := copyFile(busyboxSrc, busyboxDst); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("copy busybox: %w", err)
	}
	os.Chmod(busyboxDst, 0755)

	// Create busybox symlinks
	links := []string{
		"bin/sh", "bin/cat", "bin/ls", "bin/mount", "bin/umount",
		"bin/mkdir", "bin/echo", "bin/true", "bin/false", "bin/uname",
		"bin/su", "bin/sleep", "bin/grep",
		"sbin/poweroff", "sbin/ip", "sbin/mke2fs", "sbin/mkfs.ext4",
	}
	for _, link := range links {
		linkPath := filepath.Join(dir, link)
		os.Remove(linkPath)
		if err := os.Symlink("/bin/busybox", linkPath); err != nil {
			os.RemoveAll(dir)
			return "", fmt.Errorf("symlink %s: %w", link, err)
		}
	}

	// Copy or build vshd
	projectDir := filepath.Dir(filepath.Dir(vmDir)) // vmDir is usually project/vm
	vshd := filepath.Join(projectDir, "bin/vshd")
	if _, err := os.Stat(vshd); err != nil {
		// Try to build it
		log.Printf("vshd not found at %s, building...", vshd)
		cmd := exec.Command("go", "build", "-o", vshd, "./cmd/vshd")
		cmd.Dir = projectDir
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if output, err := cmd.CombinedOutput(); err != nil {
			os.RemoveAll(dir)
			return "", fmt.Errorf("build vshd: %w\n%s", err, output)
		}
	}
	vshdDst := filepath.Join(dir, "sbin/vshd")
	if err := copyFile(vshd, vshdDst); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("copy vshd: %w", err)
	}
	os.Chmod(vshdDst, 0755)

	// Create /etc files
	passwd := "root:x:0:0:root:/root:/bin/sh\nuser:x:1000:1000:user:/home/user:/bin/sh\n"
	if err := os.WriteFile(filepath.Join(dir, "etc/passwd"), []byte(passwd), 0644); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	group := "root:x:0:\nuser:x:1000:\n"
	if err := os.WriteFile(filepath.Join(dir, "etc/group"), []byte(group), 0644); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "etc/profile"), []byte("export PATH=/bin:/sbin:/usr/bin\n"), 0644); err != nil {
		os.RemoveAll(dir)
		return "", err
	}

	return dir, nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0755)
}
