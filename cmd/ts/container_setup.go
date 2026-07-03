// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// cmdDropCapsAndRun sets up container isolation and then execs the command
// specified in the remaining arguments. This is used by thundersnapd to
// initialize and restrict container processes.
//
// Setup performed:
//   - Makes all mounts private (prevents mount propagation to host)
//   - Mounts /proc filesystem
//   - Sets hostname and domainname (if --hostname/--domainname provided)
//   - Drops dangerous capabilities from the bounding set
//
// Capabilities dropped:
//   - CAP_NET_ADMIN: prevents iptables, routing, interface config changes
//   - CAP_SYS_MODULE: prevents loading kernel modules
//   - CAP_SYS_BOOT: prevents reboot
//   - CAP_SYS_TIME: prevents changing system clock
//   - CAP_MKNOD: prevents creating device nodes (unless --keep-dev-caps)
//   - CAP_AUDIT_WRITE: prevents writing to audit log
//   - CAP_SETFCAP: prevents setting file capabilities
func cmdDropCapsAndRun(args []string) {
	// Parse our flags manually since we need to pass remaining args to exec
	var hostname, domainname, chrootPath string
	var skipMountSetup bool
	var usePty bool
	var mountVsock bool
	var keepDevCaps bool
	var cmdArgs []string

	for i := 0; i < len(args); i++ {
		if args[i] == "--hostname" && i+1 < len(args) {
			hostname = args[i+1]
			i++
		} else if strings.HasPrefix(args[i], "--hostname=") {
			hostname = strings.TrimPrefix(args[i], "--hostname=")
		} else if args[i] == "--domainname" && i+1 < len(args) {
			domainname = args[i+1]
			i++
		} else if strings.HasPrefix(args[i], "--domainname=") {
			domainname = strings.TrimPrefix(args[i], "--domainname=")
		} else if args[i] == "--chroot" && i+1 < len(args) {
			chrootPath = args[i+1]
			i++
		} else if strings.HasPrefix(args[i], "--chroot=") {
			chrootPath = strings.TrimPrefix(args[i], "--chroot=")
		} else if args[i] == "--skip-mount-setup" {
			// Used when nsenter has already joined existing namespaces where
			// container-init has set up mounts. We just need to chroot and drop caps.
			skipMountSetup = true
		} else if args[i] == "--pty" {
			usePty = true
		} else if args[i] == "--vsock" {
			// Set by the VM init cmdline: the vshd that runs as init needs
			// /dev/vsock to listen on AF_VSOCK. Containers never pass this.
			mountVsock = true
		} else if args[i] == "--keep-dev-caps" {
			// Keep CAP_MKNOD so nested thundersnap can mount devtmpfs and create
			// device nodes. Used when developing thundersnap inside thundersnap.
			keepDevCaps = true
		} else if args[i] == "--" {
			cmdArgs = args[i+1:]
			break
		} else {
			// First non-flag argument starts the command
			cmdArgs = args[i:]
			break
		}
	}

	if len(cmdArgs) == 0 {
		fmt.Fprintln(os.Stderr, "error: drop-caps-and-run requires a command to execute")
		os.Exit(1)
	}

	if !skipMountSetup {
		// Make all mounts private so mounts inside the container don't propagate
		// to the host. This must be done BEFORE chroot while "/" is still a real
		// mount point. After CLONE_NEWNS, we have our own copy of the mount table
		// but it still has "shared" propagation. Making it private here only
		// affects our namespace, not the parent.
		//
		// In VM mode (running as init), this may fail because the root filesystem
		// (virtiofs) doesn't support propagation changes. That's fine - VMs don't
		// have mount propagation concerns anyway.
		if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
			// Only log, don't exit - this is expected to fail in VM mode
			fmt.Fprintf(os.Stderr, "warning: failed to make mounts private: %v (ok in VM mode)\n", err)
		}
	}

	// Chroot into the container rootfs if specified.
	// This is needed both when creating new namespaces and when joining existing ones,
	// because even after setns(CLONE_NEWNS), our root is still the host's "/".
	if chrootPath != "" {
		if err := unix.Chroot(chrootPath); err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to chroot to %s: %v\n", chrootPath, err)
			os.Exit(1)
		}
		if err := unix.Chdir("/"); err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to chdir to /: %v\n", err)
			os.Exit(1)
		}
	}

	if !skipMountSetup {
		// Ensure mount points exist (blank containers may not have them)
		os.MkdirAll("/proc", 0555)
		os.MkdirAll("/sys", 0555)

		// Mount /proc filesystem
		if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
			// Ignore errors - /proc might already be mounted
			_ = err
		}

		// Mount /sys filesystem
		if err := unix.Mount("sysfs", "/sys", "sysfs", 0, ""); err != nil {
			// Ignore errors - /sys might already be mounted
			_ = err
		}

		// Set up /dev like Docker/containerd do:
		// - tmpfs at /dev
		// - Essential device nodes (null, zero, full, random, urandom, tty)
		// - Symlinks for stdin/stdout/stderr and /dev/fd
		// - /dev/pts for pseudoterminals
		// - /dev/shm for shared memory
		setupDev(mountVsock)

		// Set hostname if provided (only when creating namespace, not joining)
		if hostname != "" {
			if err := unix.Sethostname([]byte(hostname)); err != nil {
				fmt.Fprintf(os.Stderr, "error: failed to set hostname: %v\n", err)
				os.Exit(1)
			}
		}

		// Set domainname if provided (only when creating namespace, not joining)
		if domainname != "" {
			if err := unix.Setdomainname([]byte(domainname)); err != nil {
				fmt.Fprintf(os.Stderr, "error: failed to set domainname: %v\n", err)
				os.Exit(1)
			}
		}
	}

	// Capabilities to drop from the bounding set. When keepDevCaps is true,
	// we retain CAP_MKNOD so nested thundersnap can mount devtmpfs and create
	// device nodes for its own containers.
	capsToDrop := []uintptr{
		unix.CAP_NET_ADMIN,
		unix.CAP_SYS_MODULE,
		unix.CAP_SYS_BOOT,
		unix.CAP_SYS_TIME,
		unix.CAP_AUDIT_WRITE,
		unix.CAP_SETFCAP,
	}
	if !keepDevCaps {
		capsToDrop = append(capsToDrop, unix.CAP_MKNOD)
	}

	// Drop each capability from the bounding set
	for _, cap := range capsToDrop {
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, cap, 0, 0, 0); err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to drop capability %d: %v\n", cap, err)
			os.Exit(1)
		}
	}

	// Ensure PATH is set - the kernel doesn't set it when starting init,
	// and child processes (like vshd calling "su") need it.
	if os.Getenv("PATH") == "" {
		os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}

	// Find the executable in PATH
	executable, err := findExecutable(cmdArgs[0])
	if err != nil {
		// If "su" isn't found, fall back to /bin/sh for root user.
		// This allows minimal containers without su to still work.
		// The fallback preserves the same semantics:
		//   su - root      -> /bin/sh -l
		//   su root -c CMD -> /bin/sh -c CMD
		if cmdArgs[0] == "su" {
			if sh, shErr := findExecutable("/bin/sh"); shErr == nil {
				// Check if this is "su - root" (login shell) or "su root -c CMD"
				if len(cmdArgs) >= 3 && cmdArgs[1] == "-" && cmdArgs[2] == "root" {
					// su - root -> /bin/sh -l
					executable = sh
					cmdArgs = []string{"/bin/sh", "-l"}
				} else if len(cmdArgs) >= 4 && cmdArgs[1] == "root" && cmdArgs[2] == "-c" {
					// su root -c CMD -> /bin/sh -c CMD
					executable = sh
					cmdArgs = append([]string{"/bin/sh", "-c"}, cmdArgs[3:]...)
				} else {
					fmt.Fprintf(os.Stderr, "error: %v\n", err)
					os.Exit(1)
				}
			} else {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
		} else {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}

	if usePty {
		// Allocate a PTY inside the container (after devpts is mounted)
		// and run the command with it, proxying I/O to our stdin/stdout.
		runWithPty(executable, cmdArgs)
	} else {
		// Exec the command, replacing this process
		if err := syscall.Exec(executable, cmdArgs, os.Environ()); err != nil {
			fmt.Fprintf(os.Stderr, "error: exec %s: %v\n", cmdArgs[0], err)
			os.Exit(1)
		}
	}
}

// runWithPty allocates a PTY inside the container and runs the command with it.
// It proxies I/O between the PTY master and our stdin/stdout. This is used when
// --pty is specified, ensuring the PTY is allocated AFTER devpts is mounted.
//
// Window resize handling: the parent (thundersnapd) writes "WIDTH HEIGHT\n" to
// /tmp/.pty-winsize and sends SIGWINCH. We read that file and apply the size.
func runWithPty(executable string, cmdArgs []string) {
	// Open the PTY master
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open /dev/ptmx: %v\n", err)
		os.Exit(1)
	}
	defer ptmx.Close()

	// Get the PTY slave name and unlock it
	ptsName, err := ptsname(ptmx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: ptsname: %v\n", err)
		os.Exit(1)
	}
	if err := unlockpt(ptmx); err != nil {
		fmt.Fprintf(os.Stderr, "error: unlockpt: %v\n", err)
		os.Exit(1)
	}

	// Open the PTY slave
	pts, err := os.OpenFile(ptsName, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open pty slave %s: %v\n", ptsName, err)
		os.Exit(1)
	}

	// Set initial window size if available
	applyWinsize(ptmx)

	// Set up SIGWINCH handler to resize PTY when notified
	sigwinch := make(chan os.Signal, 1)
	signal.Notify(sigwinch, syscall.SIGWINCH)
	go func() {
		for range sigwinch {
			applyWinsize(ptmx)
		}
	}()

	// Fork and exec the command with the PTY slave as stdin/stdout/stderr
	pid, err := syscall.ForkExec(executable, cmdArgs, &syscall.ProcAttr{
		Dir:   "/",
		Env:   os.Environ(),
		Files: []uintptr{pts.Fd(), pts.Fd(), pts.Fd()},
		Sys: &syscall.SysProcAttr{
			Setsid:  true,
			Setctty: true,
			Ctty:    0, // The first fd (stdin) is the controlling terminal
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: fork/exec %s: %v\n", cmdArgs[0], err)
		os.Exit(1)
	}

	// Close the slave in the parent - the child has it
	pts.Close()

	// Proxy I/O between stdin/stdout and the PTY master
	done := make(chan struct{}, 2)

	// stdin -> ptmx
	go func() {
		io.Copy(ptmx, os.Stdin)
		done <- struct{}{}
	}()

	// ptmx -> stdout
	go func() {
		io.Copy(os.Stdout, ptmx)
		done <- struct{}{}
	}()

	// Wait for the child to exit
	var status syscall.WaitStatus
	for {
		wpid, err := syscall.Wait4(pid, &status, 0, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: wait: %v\n", err)
			os.Exit(1)
		}
		if wpid == pid {
			break
		}
	}

	signal.Stop(sigwinch)

	// Exit with the child's exit code
	if status.Exited() {
		os.Exit(status.ExitStatus())
	}
	if status.Signaled() {
		os.Exit(128 + int(status.Signal()))
	}
	os.Exit(1)
}

// ptsname returns the name of the PTY slave device for the given PTY master.
func ptsname(f *os.File) (string, error) {
	var ptyno uint32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&ptyno)))
	if errno != 0 {
		return "", errno
	}
	return fmt.Sprintf("/dev/pts/%d", ptyno), nil
}

// unlockpt unlocks the PTY slave device for the given PTY master.
func unlockpt(f *os.File) error {
	var unlock int32 = 0
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock)))
	if errno != 0 {
		return errno
	}
	return nil
}

// winsizeFile is where thundersnapd writes "WIDTH HEIGHT\n" for window resizes.
const winsizeFile = "/tmp/.pty-winsize"

// applyWinsize reads the window size from winsizeFile and applies it to the PTY.
// Silently does nothing if the file doesn't exist or is malformed.
func applyWinsize(ptmx *os.File) {
	data, err := os.ReadFile(winsizeFile)
	if err != nil {
		return
	}
	parts := strings.Fields(strings.TrimSpace(string(data)))
	if len(parts) != 2 {
		return
	}
	width, err1 := strconv.Atoi(parts[0])
	height, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || width <= 0 || height <= 0 {
		return
	}
	setWinsize(ptmx, width, height)
}

// setWinsize sets the window size of the given PTY.
func setWinsize(f *os.File, w, h int) {
	ws := struct{ row, col, xpixel, ypixel uint16 }{uint16(h), uint16(w), 0, 0}
	syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws)))
}

// setupDev builds a minimal, controlled /dev for a container or VM: a tmpfs at
// /dev with a fixed set of device nodes, the std{in,out,err}/fd symlinks, and
// devpts/shm/mqueue. It deliberately does NOT expose the kernel's full devtmpfs
// (no disks, kmsg, console, etc.).
//
// mountVsock is set only by the VM init process: the vshd that runs as init in
// a cloud-hypervisor guest listens on AF_VSOCK and so needs /dev/vsock, a misc
// device that only works when backed by devtmpfs. Containers never need vsock
// (sessions reach vshd via the shared namespace / /thunder.sock, not vsock), so
// they pass false and get no vsock node at all.
func setupDev(mountVsock bool) {
	// Ensure /dev exists (blank containers may not have it)
	os.MkdirAll("/dev", 0755)

	// Mount tmpfs at /dev
	if err := unix.Mount("tmpfs", "/dev", "tmpfs", unix.MS_NOSUID|unix.MS_STRICTATIME, "mode=755,size=65536k"); err != nil {
		// We might not have permissions
		return
	}

	// Create essential device nodes
	// Format: name, mode, major, minor
	// Note: vsock is NOT included here - it's a misc device that only works via
	// devtmpfs, so it is bind-mounted separately below when mountVsock is set.
	devices := []struct {
		name  string
		mode  uint32
		major uint32
		minor uint32
	}{
		{"null", unix.S_IFCHR | 0666, 1, 3},
		{"zero", unix.S_IFCHR | 0666, 1, 5},
		{"full", unix.S_IFCHR | 0666, 1, 7},
		{"random", unix.S_IFCHR | 0666, 1, 8},
		{"urandom", unix.S_IFCHR | 0666, 1, 9},
		{"tty", unix.S_IFCHR | 0666, 5, 0},
	}

	for _, dev := range devices {
		path := "/dev/" + dev.name
		devNum := unix.Mkdev(dev.major, dev.minor)
		// Ignore errors - we're best-effort here
		if err := unix.Mknod(path, dev.mode, int(devNum)); err == nil {
			// Mknod doesn't respect mode bits for permissions (affected by umask),
			// so explicitly set the permissions after creating the device.
			unix.Chmod(path, dev.mode&0777)
		}
	}

	// Create symlinks for stdin/stdout/stderr
	os.Symlink("/proc/self/fd/0", "/dev/stdin")
	os.Symlink("/proc/self/fd/1", "/dev/stdout")
	os.Symlink("/proc/self/fd/2", "/dev/stderr")

	// Create /dev/fd -> /proc/self/fd
	os.Symlink("/proc/self/fd", "/dev/fd")

	// Create /dev/pts directory and mount devpts
	os.MkdirAll("/dev/pts", 0755)
	unix.Mount("devpts", "/dev/pts", "devpts", unix.MS_NOSUID|unix.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=620")

	// Create /dev/ptmx symlink to /dev/pts/ptmx for the newinstance mount
	os.Symlink("pts/ptmx", "/dev/ptmx")

	// Create /dev/shm for shared memory. 0o1777 = sticky + world-writable, the
	// standard mode for shared scratch space (the decimal literal 1777 would be
	// octal 03561, a wrong mode); the immediately following tmpfs mount with
	// mode=1777 is what users actually see, but keep the mkdir mode correct too.
	os.MkdirAll("/dev/shm", 0o1777)
	unix.Mount("tmpfs", "/dev/shm", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=1777,size=65536k")

	// Create /dev/mqueue for POSIX message queues (optional but some programs expect it)
	os.MkdirAll("/dev/mqueue", 0755)
	unix.Mount("mqueue", "/dev/mqueue", "mqueue", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "")

	// Expose /dev/vsock for the VM init's vshd. vsock is a misc device that only
	// works when backed by devtmpfs, so we can't just mknod it onto our tmpfs.
	// Mount a throwaway devtmpfs at a scratch dir inside /dev, bind its vsock
	// node onto /dev/vsock, then drop the scratch mount so the rest of /dev
	// stays the controlled tmpfs (no disks/kmsg/console leaking in).
	//
	// We don't condition this on /dev/vsock pre-existing: when vshd runs as the
	// guest's init the kernel does not auto-mount devtmpfs at /dev, so /dev/vsock
	// is absent here even though the host exposed the device. The VM always has a
	// vsock, so mountVsock==true unconditionally surfaces it.
	if mountVsock {
		scratch := "/dev/.devtmpfs"
		os.MkdirAll(scratch, 0755)
		if err := unix.Mount("devtmpfs", scratch, "devtmpfs", 0, ""); err == nil {
			if f, err := os.OpenFile("/dev/vsock", os.O_CREATE|os.O_WRONLY, 0666); err == nil {
				f.Close()
				unix.Mount(scratch+"/vsock", "/dev/vsock", "", unix.MS_BIND, "")
			}
			unix.Unmount(scratch, 0)
		}
		os.Remove(scratch)
	}
}

// cmdContainerInit is a minimal init process for container PID namespaces.
// It performs namespace setup (mounts, /dev, etc.) and then sits idle, acting
// as PID 1 to anchor the namespace. All actual sessions join this namespace
// via setns() and run their own processes.
//
// Usage: ts container-init --chroot=/path/to/rootfs [--hostname=X] [--domainname=Y]
//
// The process:
// 1. Sets up mount namespace (private propagation, /proc, /sys, /dev)
// 2. Chroots into the container rootfs
// 3. Writes "READY\n" to stdout to signal setup is complete
// 4. Sits idle, waiting for stdin to close (which signals shutdown)
// 5. As PID 1, reaps any orphaned zombie processes
func cmdContainerInit(args []string) {
	var hostname, domainname, chrootPath string

	for i := 0; i < len(args); i++ {
		if args[i] == "--hostname" && i+1 < len(args) {
			hostname = args[i+1]
			i++
		} else if strings.HasPrefix(args[i], "--hostname=") {
			hostname = strings.TrimPrefix(args[i], "--hostname=")
		} else if args[i] == "--domainname" && i+1 < len(args) {
			domainname = args[i+1]
			i++
		} else if strings.HasPrefix(args[i], "--domainname=") {
			domainname = strings.TrimPrefix(args[i], "--domainname=")
		} else if args[i] == "--chroot" && i+1 < len(args) {
			chrootPath = args[i+1]
			i++
		} else if strings.HasPrefix(args[i], "--chroot=") {
			chrootPath = strings.TrimPrefix(args[i], "--chroot=")
		}
	}

	if chrootPath == "" {
		fmt.Fprintln(os.Stderr, "error: container-init requires --chroot")
		os.Exit(1)
	}

	// Make all mounts private so mounts inside the container don't propagate
	// to the host. This must be done BEFORE chroot while "/" is still a real
	// mount point.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		// Only log, don't exit - this is expected to fail in VM mode
		fmt.Fprintf(os.Stderr, "warning: failed to make mounts private: %v (ok in VM mode)\n", err)
	}

	// Bind-mount chrootPath to itself to ensure it's explicitly in this mount
	// namespace's mount table. This is needed for nested containers: when running
	// inside a container, the outer /work bind mount isn't automatically copied
	// to our new mount namespace. By explicitly bind-mounting chrootPath, we
	// ensure processes that later join via setns(CLONE_NEWNS) can see it.
	if err := unix.Mount(chrootPath, chrootPath, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to bind-mount %s: %v\n", chrootPath, err)
		os.Exit(1)
	}

	// Chroot into the container rootfs
	if err := unix.Chroot(chrootPath); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to chroot to %s: %v\n", chrootPath, err)
		os.Exit(1)
	}
	if err := unix.Chdir("/"); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to chdir to /: %v\n", err)
		os.Exit(1)
	}

	// Ensure mount points exist (blank containers may not have them)
	os.MkdirAll("/proc", 0555)
	os.MkdirAll("/sys", 0555)

	// Mount /proc filesystem
	if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		_ = err // Ignore - /proc might already be mounted
	}

	// Mount /sys filesystem
	if err := unix.Mount("sysfs", "/sys", "sysfs", 0, ""); err != nil {
		_ = err // Ignore - /sys might already be mounted
	}

	// Set up /dev (tmpfs with device nodes, devpts, etc.). Containers never need
	// vsock, so pass false.
	setupDev(false)

	// Set hostname if provided
	if hostname != "" {
		if err := unix.Sethostname([]byte(hostname)); err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to set hostname: %v\n", err)
			os.Exit(1)
		}
	}

	// Set domainname if provided
	if domainname != "" {
		if err := unix.Setdomainname([]byte(domainname)); err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to set domainname: %v\n", err)
			os.Exit(1)
		}
	}

	// Signal that setup is complete
	fmt.Println("READY")

	// Set up SIGCHLD handler to reap zombies. As PID 1, orphaned processes
	// get reparented to us, and we need to wait() on them or they become zombies.
	sigchld := make(chan os.Signal, 16)
	signal.Notify(sigchld, syscall.SIGCHLD)

	// Also handle SIGTERM for graceful shutdown
	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGTERM)

	// Wait for stdin to close (shutdown signal) while reaping zombies
	stdinClosed := make(chan struct{})
	go func() {
		// Read until EOF
		buf := make([]byte, 1)
		for {
			_, err := os.Stdin.Read(buf)
			if err != nil {
				break
			}
		}
		close(stdinClosed)
	}()

	for {
		select {
		case <-sigchld:
			// Reap all zombies
			for {
				var status syscall.WaitStatus
				pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
				if err != nil || pid <= 0 {
					break
				}
			}
		case <-sigterm:
			// Graceful shutdown requested
			os.Exit(0)
		case <-stdinClosed:
			// Parent closed our stdin - time to exit
			os.Exit(0)
		}
	}
}

// cmdCheckDev outputs the state of /dev for e2e testing.
// Output format is one item per line:
//
//	DEV:<name>:<exists|missing>:<perms>
//	LINK:<name>:<exists|missing>:<target>
//	DIR:<name>:<exists|missing>
//	DONE
func cmdCheckDev() {
	// Check device nodes (vsock is optional - only works in VMs with vsock support)
	devices := []string{"null", "zero", "full", "random", "urandom", "tty", "vsock"}
	for _, dev := range devices {
		path := "/dev/" + dev
		info, err := os.Lstat(path)
		if err != nil {
			fmt.Printf("DEV:%s:missing:0\n", dev)
			continue
		}
		if info.Mode()&os.ModeCharDevice == 0 {
			fmt.Printf("DEV:%s:not-chardev:%o\n", dev, info.Mode().Perm())
			continue
		}
		fmt.Printf("DEV:%s:exists:%o\n", dev, info.Mode().Perm())
	}

	// Check symlinks
	links := []string{"stdin", "stdout", "stderr", "fd"}
	for _, link := range links {
		path := "/dev/" + link
		target, err := os.Readlink(path)
		if err != nil {
			fmt.Printf("LINK:%s:missing:\n", link)
			continue
		}
		fmt.Printf("LINK:%s:exists:%s\n", link, target)
	}

	// Check directories
	dirs := []string{"pts", "shm", "mqueue"}
	for _, dir := range dirs {
		path := "/dev/" + dir
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			fmt.Printf("DIR:%s:missing\n", dir)
			continue
		}
		fmt.Printf("DIR:%s:exists\n", dir)
	}

	// List all entries in /dev for completeness checking
	// This allows tests to verify that unwanted devtmpfs entries are not present
	entries, err := os.ReadDir("/dev")
	if err == nil {
		for _, entry := range entries {
			fmt.Printf("ENTRY:%s\n", entry.Name())
		}
	}

	fmt.Println("DONE")
}

// cmdCheckIsolation outputs the container isolation state for e2e testing.
// Output format is one item per line:
//
//	HOSTNAME:<hostname>
//	DOMAINNAME:<domainname>
//	PID1:<pid-is-1>
//	PROC:<mounted|not-mounted>
//	SYS:<mounted|not-mounted>
//	CAP:<name>:<has|dropped>
//	NS:<name>:<inode>
//	DONE
func cmdCheckIsolation() {
	// Check hostname
	hostname, _ := os.Hostname()
	fmt.Printf("HOSTNAME:%s\n", hostname)

	// Check domainname via syscall
	var uts unix.Utsname
	if err := unix.Uname(&uts); err == nil {
		domainname := string(uts.Domainname[:])
		if idx := strings.IndexByte(domainname, 0); idx >= 0 {
			domainname = domainname[:idx]
		}
		fmt.Printf("DOMAINNAME:%s\n", domainname)
	}

	// Check if we're PID 1 (indicates PID namespace isolation)
	if os.Getpid() == 1 {
		fmt.Println("PID1:yes")
	} else {
		fmt.Printf("PID1:no:%d\n", os.Getpid())
	}

	// Check /proc mount
	if _, err := os.Stat("/proc/self"); err == nil {
		fmt.Println("PROC:mounted")
	} else {
		fmt.Println("PROC:not-mounted")
	}

	// Check /sys mount
	if _, err := os.Stat("/sys/class"); err == nil {
		fmt.Println("SYS:mounted")
	} else {
		fmt.Println("SYS:not-mounted")
	}

	// Check capabilities in bounding set
	// These are the caps that cmdDropCapsAndRun drops
	capsToCheck := []struct {
		name string
		cap  uintptr
	}{
		{"NET_ADMIN", unix.CAP_NET_ADMIN},
		{"SYS_MODULE", unix.CAP_SYS_MODULE},
		{"SYS_BOOT", unix.CAP_SYS_BOOT},
		{"SYS_TIME", unix.CAP_SYS_TIME},
		{"MKNOD", unix.CAP_MKNOD},
		{"AUDIT_WRITE", unix.CAP_AUDIT_WRITE},
		{"SETFCAP", unix.CAP_SETFCAP},
	}

	for _, c := range capsToCheck {
		// Use prctl to check if capability is in bounding set
		ret, _, _ := unix.Syscall(unix.SYS_PRCTL, unix.PR_CAPBSET_READ, c.cap, 0)
		if ret == 1 {
			fmt.Printf("CAP:%s:has\n", c.name)
		} else {
			fmt.Printf("CAP:%s:dropped\n", c.name)
		}
	}

	// Check namespace inodes (to verify we're in new namespaces)
	namespaces := []string{"pid", "mnt", "uts", "net"}
	for _, ns := range namespaces {
		path := fmt.Sprintf("/proc/self/ns/%s", ns)
		info, err := os.Stat(path)
		if err != nil {
			fmt.Printf("NS:%s:error\n", ns)
			continue
		}
		stat := info.Sys().(*syscall.Stat_t)
		fmt.Printf("NS:%s:%d\n", ns, stat.Ino)
	}

	// Check mount propagation for root mount
	// Read /proc/self/mountinfo to determine propagation type
	mountinfo, err := os.ReadFile("/proc/self/mountinfo")
	foundRoot := false
	if err == nil {
		// Look for root mount (target = /) and check propagation flags
		// Format: id parent major:minor root target options opt:value - fstype source super-options
		for _, line := range strings.Split(string(mountinfo), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 5 {
				target := fields[4]
				if target == "/" {
					foundRoot = true
					// Options are in fields[5] onwards until "-"
					options := ""
					for i := 5; i < len(fields) && fields[i] != "-"; i++ {
						options += fields[i] + " "
					}
					// Propagation types: shared, private, slave, unbindable
					if strings.Contains(options, "shared:") {
						fmt.Println("MOUNT_PROPAGATION:shared")
					} else if strings.Contains(options, "master:") {
						fmt.Println("MOUNT_PROPAGATION:slave")
					} else if strings.Contains(options, "unbindable") {
						fmt.Println("MOUNT_PROPAGATION:unbindable")
					} else {
						// Default is private (no propagation marker)
						fmt.Println("MOUNT_PROPAGATION:private")
					}
					break
				}
			}
		}
		if !foundRoot {
			// In a container with a fresh mount namespace, there might not be a "/" entry
			// if the root is the pivot_root target. Default to private in this case.
			fmt.Println("MOUNT_PROPAGATION:private")
		}
	} else {
		fmt.Println("MOUNT_PROPAGATION:error")
	}

	fmt.Println("DONE")
}
