// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
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
	var chrootFd int = -1
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
		} else if strings.HasPrefix(args[i], "--chroot-fd=") {
			fd, err := strconv.Atoi(strings.TrimPrefix(args[i], "--chroot-fd="))
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: invalid --chroot-fd: %v\n", err)
				os.Exit(1)
			}
			chrootFd = fd
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

	// Defense-in-depth: this function performs destructive mount operations
	// (MS_PRIVATE on /, bind mounts, pivot_root, setupDev) that must only run
	// inside a fresh mount namespace created by the caller via
	// Cloneflags:CLONE_NEWPID|CLONE_NEWNS. If invoked without Cloneflags (e.g.
	// directly from a shell), those operations would land on the host's mount
	// namespace and destroy it. PID 1 is the reliable signal that the caller
	// created a new PID namespace (and, by convention, a new mount namespace):
	// container-init, autorun, and the VM guest init are all PID 1.
	if os.Getpid() != 1 {
		fmt.Fprintf(os.Stderr, "error: drop-caps-and-run must run as PID 1 (got pid %d); the caller must use Cloneflags:CLONE_NEWPID|CLONE_NEWNS, or use 'ts join-and-run' to join an existing namespace\n", os.Getpid())
		os.Exit(1)
	}

	// Make all mounts private so mounts inside the container don't propagate
	// to the host. This must be done BEFORE chroot while "/" is still a real
	// mount point. After CLONE_NEWNS, we have our own copy of the mount table
	// but it still has "shared" propagation. Making it private here only
	// affects our namespace, not the parent.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to make mounts private: %v\n", err)
		os.Exit(1)
	}

	// Change the root filesystem to the container's root using pivot_root, which
	// properly changes the root without the "chrooted" flag that prevents user
	// namespace creation.
	//
	// In nested containers on btrfs, the pivot path may traverse btrfs subvolume
	// boundaries that are inaccessible after setns due to a stale root dentry.
	// When --chroot-fd is provided (opened before setns), use fchdir to reach
	// the target through the fd's dentry.
	if chrootFd >= 0 || chrootPath != "" {
		// Use pivot_root with a bind mount.
		// The sequence must match container-init:
		// The sequence must match container-init:
		// 1. Bind mount the target path to itself FIRST (before chdir)
		// 2. chdir to the target
		// 3. Create old_root directory
		// 4. pivot_root(".", ".old_root")
		// 5. chdir("/")
		// 6. Unmount old_root

		// Get the target path
		targetPath := chrootPath
		if chrootFd >= 0 {
			// For fd case, we need to resolve the path from the fd
			// by reading /proc/self/fd/<fd>
			link := fmt.Sprintf("/proc/self/fd/%d", chrootFd)
			resolved, err := os.Readlink(link)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: failed to resolve chroot fd path: %v\n", err)
				os.Exit(1)
			}
			targetPath = resolved
		}

		// Bind mount the target to itself BEFORE chdir
		if err := unix.Mount(targetPath, targetPath, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to bind-mount %s: %v\n", targetPath, err)
			os.Exit(1)
		}

		// Now chdir to the target
		if chrootFd >= 0 {
			if err := unix.Fchdir(chrootFd); err != nil {
				fmt.Fprintf(os.Stderr, "error: failed to fchdir to chroot fd: %v\n", err)
				os.Exit(1)
			}
		} else {
			if err := unix.Chdir(chrootPath); err != nil {
				fmt.Fprintf(os.Stderr, "error: failed to chdir to %s: %v\n", chrootPath, err)
				os.Exit(1)
			}
		}

		oldRoot := ".old_root"
		if err := os.MkdirAll(oldRoot, 0700); err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to create old root dir: %v\n", err)
			os.Exit(1)
		}

		if err := unix.PivotRoot(".", oldRoot); err != nil {
			fmt.Fprintf(os.Stderr, "error: pivot_root failed: %v\n", err)
			os.Exit(1)
		}

		if err := unix.Chdir("/"); err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to chdir to / after pivot_root: %v\n", err)
			os.Exit(1)
		}

		if err := unix.Unmount("/.old_root", unix.MNT_DETACH); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to unmount old root: %v\n", err)
		}
		os.Remove("/.old_root")
	}
	// Close the chroot fd if we used it — it was opened by ts nsenter before
	// setns and passed without O_CLOEXEC so it survived exec.
	if chrootFd >= 0 {
		unix.Close(chrootFd)
	}

	// Ensure mount points exist (blank containers may not have them).
	os.MkdirAll("/proc", 0555)
	os.MkdirAll("/sys", 0555)
	if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil && err != unix.EBUSY {
		fmt.Fprintf(os.Stderr, "error: failed to mount proc: %v\n", err)
		os.Exit(1)
	}
	if err := unix.Mount("sysfs", "/sys", "sysfs", 0, ""); err != nil && err != unix.EBUSY {
		fmt.Fprintf(os.Stderr, "error: failed to mount sysfs: %v\n", err)
		os.Exit(1)
	}
	if err := ensureCgroup2Mount(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
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

	dropCapsAndExec(cmdArgs, keepDevCaps, usePty)
}

// dropCapsAndExec drops dangerous capabilities from the bounding set and execs
// the given command (or runs it under a PTY if usePty is true). This is the
// shared tail of cmdDropCapsAndRun (PID-1 namespace creator) and cmdJoinAndRun
// (namespace joiner): both paths end by restricting capabilities and exec'ing
// the target command.
//
// keepDevCaps retains CAP_MKNOD so nested thundersnap can mount devtmpfs and
// create device nodes for its own containers.
func dropCapsAndExec(cmdArgs []string, keepDevCaps, usePty bool) {
	// Capabilities to drop from the bounding set.
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

	executable, err := findExecutable(cmdArgs[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if usePty {
		runWithPty(executable, cmdArgs)
	} else {
		if err := syscall.Exec(executable, cmdArgs, os.Environ()); err != nil {
			fmt.Fprintf(os.Stderr, "error: exec %s: %v\n", cmdArgs[0], err)
			os.Exit(1)
		}
	}
}

// cmdJoinAndRun joins an existing container namespace (entered via ts nsenter
// or util-linux nsenter) and runs a command inside it. Unlike drop-caps-and-run
// which creates a fresh namespace and sets up mounts (MS_PRIVATE, pivot_root,
// setupDev), this command does ZERO mount operations: the container-init has
// already done all setup. It just navigates to the container root, drops caps,
// and execs the command.
//
// This is the replacement for 'drop-caps-and-run --skip-mount-setup'. Splitting
// the join path into its own command eliminates the ambiguous --skip-mount-setup
// flag and the dangerous soft-ignored pivot_root that accompanied it.
//
// Usage: ts join-and-run --chroot=<path> [--chroot-fd=<N>] [--keep-dev-caps]
//
//	[--pty] -- <command...>
func cmdJoinAndRun(args []string) {
	var chrootPath string
	var chrootFd int = -1
	var keepDevCaps bool
	var usePty bool
	var cmdArgs []string

	for i := 0; i < len(args); i++ {
		if args[i] == "--chroot" && i+1 < len(args) {
			chrootPath = args[i+1]
			i++
		} else if strings.HasPrefix(args[i], "--chroot=") {
			chrootPath = strings.TrimPrefix(args[i], "--chroot=")
		} else if args[i] == "--chroot-fd" && i+1 < len(args) {
			fd, err := strconv.Atoi(args[i+1])
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: invalid --chroot-fd: %v\n", err)
				os.Exit(1)
			}
			chrootFd = fd
			i++
		} else if strings.HasPrefix(args[i], "--chroot-fd=") {
			fd, err := strconv.Atoi(strings.TrimPrefix(args[i], "--chroot-fd="))
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: invalid --chroot-fd: %v\n", err)
				os.Exit(1)
			}
			chrootFd = fd
		} else if args[i] == "--keep-dev-caps" {
			keepDevCaps = true
		} else if args[i] == "--pty" {
			usePty = true
		} else if args[i] == "--" {
			cmdArgs = args[i+1:]
			break
		} else {
			cmdArgs = args[i:]
			break
		}
	}

	if len(cmdArgs) == 0 {
		fmt.Fprintln(os.Stderr, "error: join-and-run requires a command to execute")
		os.Exit(1)
	}

	// Navigate to the container root. After setns(CLONE_NEWNS) (done by ts
	// nsenter before exec'ing us), the mount table is the container's, but the
	// process's root dentry may be stale (a kernel VFS behavior on btrfs). The
	// --chroot-fd (opened by nsenter via /proc/<pid>/root before setns) has the
	// correct dentry; --chroot is a fallback for when the fd isn't available.
	if chrootFd >= 0 {
		if err := unix.Fchdir(chrootFd); err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to fchdir to chroot fd: %v\n", err)
			os.Exit(1)
		}
	} else if chrootPath != "" {
		if err := unix.Chdir(chrootPath); err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to chdir to %s: %v\n", chrootPath, err)
			os.Exit(1)
		}
	}

	// chdir to "/" to be at the root of the container's mount namespace.
	// The container-init has already pivot_root'd, so "/" IS the container
	// root. No pivot_root or chroot needed here — and crucially, no mount
	// operations at all. User namespaces work inside the container because
	// container-init used pivot_root (not chroot) to set the root.
	if err := unix.Chdir("/"); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to chdir to /: %v\n", err)
		os.Exit(1)
	}

	// Close the chroot fd if we used it — it was opened by ts nsenter before
	// setns and passed without O_CLOEXEC so it survived exec.
	if chrootFd >= 0 {
		unix.Close(chrootFd)
	}

	dropCapsAndExec(cmdArgs, keepDevCaps, usePty)
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

	// Propagate /dev/kvm into the container so nested VMs work. We create a
	// device node with the same major:minor as the host's /dev/kvm (captured by
	// cmdContainerInit before chrooting). The device node on the tmpfs
	// references the same kernel KVM device — no bind mount needed.
	//
	// TODO(container-isolation): When we lock down container isolation in the
	// future, /dev/kvm access should be gated behind a capability or frame
	// metadata flag. Giving every container direct KVM access is fine for the
	// development/nested-thundersnap use case, but a hardened deployment would
	// want to restrict which frames can spawn VMs. For now, this matches the
	// --keep-dev-caps philosophy: we retain capabilities that enable nesting.
	if kvmDeviceNumber != 0 {
		if err := unix.Mknod("/dev/kvm", unix.S_IFCHR|0666, int(kvmDeviceNumber)); err != nil {
			// Non-fatal: nested VMs won't work but the container is still usable.
			fmt.Fprintf(os.Stderr, "setupDev: warning: failed to create /dev/kvm: %v\n", err)
		}
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
// kvmDeviceNumber holds the device number of the host's /dev/kvm, if it
// exists. It is set by cmdContainerInit BEFORE chrooting (when /dev/kvm is
// still visible from the host) and read by setupDev AFTER chrooting (when
// /dev is a fresh tmpfs with no device nodes). A value of 0 means no /dev/kvm
// on the host — skip creating the device node.
var kvmDeviceNumber uint64

// btrfsFirstFreeObjectID is the inode number of every btrfs subvolume root
// directory. We use it to detect btrfs subvolume boundaries without requiring
// btrfs-specific ioctls.
const btrfsFirstFreeObjectID = 256

// bindMountSubvolumeAncestors walks the path from / to the given target and
// bind-mounts each ancestor directory that is a btrfs subvolume root (inode
// number 256) to itself. This creates explicit mount table entries for btrfs
// subvolume boundaries, which are visible to processes that join the mount
// namespace later via setns(CLONE_NEWNS). Without this, btrfs subvolume
// directories appear as regular files to joining processes because the VFS
// dentry cache is stale after setns.
//
// This is critical for nested thundersnap (frame inside a frame): the parent
// container's /work is a btrfs subvolume, and the inner container-init's
// nsenter path traverses /work to reach the frame rootfs.
//
// Failures are logged but not fatal: a failed bind mount just means the
// subvolume traversal might not work for joining processes, which is the
// pre-existing behavior.
func bindMountSubvolumeAncestors(target string) {
	// Clean the path and split into components. We walk from the root down to
	// the target, accumulating the path.
	target = filepath.Clean(target)
	if !strings.HasPrefix(target, "/") {
		return // relative path, nothing to do
	}

	// Walk from / down to the parent of target.
	// E.g., for /work/thundersnap/.tmp-e2e/rootfs, we check:
	// /, /work, /work/thundersnap, /work/thundersnap/.tmp-e2e
	components := strings.Split(strings.TrimPrefix(target, "/"), "/")
	path := ""
	for i := 0; i < len(components); i++ {
		path = path + "/" + components[i]
		// Skip the target itself — it gets its own bind mount below.
		if path == target {
			break
		}
		var st unix.Stat_t
		if err := unix.Stat(path, &st); err != nil {
			continue // path doesn't exist, skip
		}
		// Check if this directory is a btrfs subvolume root (inode 256).
		// On non-btrfs filesystems, directory inodes are rarely 256, so this
		// is a safe heuristic. If a non-btrfs directory happens to have inode
		// 256, bind-mounting it is still harmless.
		if st.Ino == btrfsFirstFreeObjectID && st.Mode&unix.S_IFMT == unix.S_IFDIR {
			if err := unix.Mount(path, path, "", unix.MS_BIND, ""); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to bind-mount subvolume %s: %v\n", path, err)
			} else {
				fmt.Fprintf(os.Stderr, "container-init: bind-mounted btrfs subvolume %s\n", path)
			}
		}
	}
}

func ensureCgroup2Mount() error {
	if err := os.MkdirAll("/sys/fs/cgroup", 0755); err != nil {
		return fmt.Errorf("create cgroup2 mountpoint: %w", err)
	}
	if err := unix.Mount("cgroup2", "/sys/fs/cgroup", "cgroup2", 0, "nsdelegate"); err != nil && err != unix.EBUSY {
		return fmt.Errorf("mount cgroup2: %w", err)
	}
	controllers, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers")
	if err != nil {
		return fmt.Errorf("read cgroup2 controllers: %w", err)
	}
	have := make(map[string]bool)
	for _, controller := range strings.Fields(string(controllers)) {
		have[controller] = true
	}
	for _, required := range []string{"memory", "pids", "cpu"} {
		if !have[required] {
			return fmt.Errorf("required cgroup2 controller %q unavailable", required)
		}
	}
	return nil
}

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

	// Defense-in-depth: container-init performs destructive mount operations
	// (MS_PRIVATE on /, bind mounts, pivot_root, setupDev) that must only run
	// inside a fresh mount namespace created by the caller via
	// Cloneflags:CLONE_NEWPID|CLONE_NEWNS. If invoked without Cloneflags (e.g.
	// directly from a shell), those operations would land on the host's mount
	// namespace and destroy it. PID 1 is the reliable signal that the caller
	// created a new PID namespace.
	if os.Getpid() != 1 {
		fmt.Fprintf(os.Stderr, "error: container-init must run as PID 1 (got pid %d); the caller must use Cloneflags:CLONE_NEWPID|CLONE_NEWNS\n", os.Getpid())
		os.Exit(1)
	}

	// Make all mounts private so mounts inside the container don't propagate
	// to the host. This must be done BEFORE chroot while "/" is still a real
	// mount point.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to make mounts private: %v\n", err)
		os.Exit(1)
	}

	// Bind-mount btrfs subvolume boundaries in the path from / to chrootPath.
	//
	// When thundersnap runs nested (a frame inside a frame), the parent's /work
	// is a btrfs subvolume. After CLONE_NEWNS + MS_REC|MS_PRIVATE, processes
	// created in the new mount namespace can traverse btrfs subvolume boundaries
	// normally. But processes that JOIN the namespace later via
	// setns(CLONE_NEWNS) — like `ts nsenter` stage2 — cannot: the btrfs
	// subvolume directory appears as a regular file (ENOTDIR) because the VFS
	// dentry cache from the old namespace is stale and setns doesn't invalidate
	// it. This breaks the nsenter path: findExecutable can't stat
	// /work/.../bin/ts because /work isn't a directory.
	//
	// The fix: for each ancestor of chrootPath that is a btrfs subvolume, create
	// an explicit bind mount to itself. A bind mount creates a real mount table
	// entry, which IS visible after setns — the kernel resolves it through the
	// mount table, not the btrfs subvolume dentry cache. This makes the path
	// traversable for joining processes.
	//
	// We detect btrfs subvolumes by checking if the directory's inode number is
	// BTRFS_FIRST_FREE_OBJECTID (256), which is the inode number of every
	// btrfs subvolume root directory. This is a lightweight stat check, not a
	// btrfs ioctl, so it works even on non-btrfs filesystems (where no
	// directory will have inode 256, or if one does, bind-mounting it is still
	// harmless).
	bindMountSubvolumeAncestors(chrootPath)

	// Bind-mount chrootPath to itself to ensure it's explicitly in this mount
	// namespace's mount table. This is needed for nested containers: when running
	// inside a container, the outer /work bind mount isn't automatically copied
	// to our new mount namespace. By explicitly bind-mounting chrootPath, we
	// ensure processes that later join via setns(CLONE_NEWNS) can see it.
	if err := unix.Mount(chrootPath, chrootPath, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to bind-mount %s: %v\n", chrootPath, err)
		os.Exit(1)
	}

	// Stat /dev/kvm BEFORE pivot_root (while the host's /dev is still visible).
	// We store the device number so setupDev can mknod it after pivot_root (when
	// /dev is a fresh tmpfs with no device nodes). This propagates KVM into
	// containers so nested VMs work.
	if st, err := os.Stat("/dev/kvm"); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok && sys.Mode&unix.S_IFCHR != 0 {
			kvmDeviceNumber = sys.Rdev
		}
	}

	// Use pivot_root instead of chroot. pivot_root properly changes the root
	// filesystem in a way that allows user namespaces to be created inside the
	// container. With chroot, unshare(CLONE_NEWUSER) fails with EPERM because
	// the kernel considers the process to be in a "chroot jail" and denies
	// user namespace creation as a security measure.
	//
	// The pivot_root dance:
	// 1. chdir to the new root (already bind-mounted above)
	// 2. Create a directory to mount the old root
	// 3. pivot_root(".", "old_root") - atomically swaps / with the current dir
	// 4. chdir("/") to be in the new root
	// 5. Unmount and remove the old root

	// chdir to the new root before pivot_root
	if err := unix.Chdir(chrootPath); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to chdir to %s: %v\n", chrootPath, err)
		os.Exit(1)
	}

	// Create a directory for the old root. We'll unmount and remove it after pivot.
	oldRoot := ".old_root"
	if err := os.MkdirAll(oldRoot, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to create old root dir: %v\n", err)
		os.Exit(1)
	}

	// pivot_root(".", ".old_root") - the new root is "." (current dir), and the
	// old root will be mounted at ".old_root" (relative to new root, so /.old_root)
	if err := unix.PivotRoot(".", oldRoot); err != nil {
		fmt.Fprintf(os.Stderr, "error: pivot_root failed: %v\n", err)
		os.Exit(1)
	}

	// Now we're in the new root. chdir to "/" to be at the new root.
	if err := unix.Chdir("/"); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to chdir to / after pivot_root: %v\n", err)
		os.Exit(1)
	}

	// Unmount the old root. MNT_DETACH allows lazy unmount even if something
	// is still using it (shouldn't be, but be safe).
	if err := unix.Unmount("/.old_root", unix.MNT_DETACH); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to unmount old root: %v\n", err)
		// Non-fatal: we can continue, the old root is just still visible
	}

	// Remove the old root directory
	if err := os.Remove("/.old_root"); err != nil {
		// Non-fatal: directory might not be empty if unmount failed
		fmt.Fprintf(os.Stderr, "warning: failed to remove old root dir: %v\n", err)
	}

	// Ensure mount points exist (blank containers may not have them).
	os.MkdirAll("/proc", 0555)
	os.MkdirAll("/sys/fs/cgroup", 0755)

	if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to mount proc: %v\n", err)
		os.Exit(1)
	}
	if err := unix.Mount("sysfs", "/sys", "sysfs", 0, ""); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to mount sysfs: %v\n", err)
		os.Exit(1)
	}
	// Every container gets a private cgroup namespace and fresh cgroup2 mount.
	// Its namespace root is the delegated container cgroup prepared by vshd.
	if err := ensureCgroup2Mount(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
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

	log.Printf("container-init ready, PID 1 reaper active")

	// --- Robust PID 1 reaper + bounded shutdown ---
	//
	// As PID 1 of the container PID namespace, orphaned descendants (e.g. detached
	// grandchildren of an MCP job's shell) reparent to us; if we don't wait() on
	// them they stay zombies. A field failure on a Linux-VM-on-macOS host showed
	// zombies accumulating and the frame eventually wedging. The root cause is
	// the textbook-fragile Go pattern `signal.Notify(SIGCHLD) + Wait4(-1,
	// WNOHANG)` sharing the main loop: standard signals coalesce, and Go drops a
	// SIGCHLD when the (16-slot) channel is full and no goroutine is blocked
	// receiving, so under bursty MCP load a reaped batch can be followed by a
	// lost wakeup and stranded zombies.
	//
	// This implementation follows what real container inits (tini, containerd)
	// do:
	//   - a *dedicated* reaper goroutine decoupled from shutdown;
	//   - a *periodic safety-net reap* so a dropped/lost SIGCHLD can never strand
	//     a zombie (1s worst-case reap latency even if every signal is lost);
	//   - a generously buffered channel to ride bursty exits;
	//   - EINTR/ECHILD treated as "nothing to reap right now", never fatal.
	//
	// The shutdown select only handles SIGTERM and stdin-EOF, so a (now
	// impossible) stall in the reap path can never delay process exit.
	sigDone := make(chan struct{})
	startReaper(sigDone)

	// SIGTERM: clean exit so orchestrators see a normal shutdown signal.
	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGTERM)

	// Parent (containerns.Manager.stopEntry) closes our stdin to request exit.
	// vshd's spliceContainerSession does NOT close stdin, so EOF reliably means
	// "the manager wants us gone".
	stdinClosed := make(chan struct{})
	go func() {
		buf := make([]byte, 1)
		for {
			if _, err := os.Stdin.Read(buf); err != nil {
				break
			}
		}
		close(stdinClosed)
	}()

	select {
	case <-sigterm:
		log.Printf("container-init: SIGTERM, exiting")
		os.Exit(0)
	case <-stdinClosed:
		log.Printf("container-init: stdin closed, exiting")
		os.Exit(0)
	}
}

// reapInterval bounds how long a zombie can survive if its SIGCHLD was lost or
// coalesced. A dedicated reaper goroutine wakes on SIGCHLD *and* on this
// ticker, so missed signals can never strand zombies. 1s is far below any
// user-visible latency and trivially cheap (one WNOHANG sweep).
const reapInterval = 1 * time.Second

// startReaper runs a dedicated goroutine that reaps all zombie descendants.
// It drains every available zombie on each SIGCHLD and on a periodic ticker
// (the safety net). SIGCHLD is a standard signal and coalesces; Go's
// signal.Notify also drops a signal when the channel is full and nothing is
// blocked receiving, so the ticker is what makes reaping robust under bursty
// load. The goroutine exits when sigDone is closed (process is shutting down).
//
// container-init spawns no os/exec children of its own, so Wait4(-1,...) here
// cannot steal a child that os/exec.Cmd.Wait is tracking (the classic
// PID-1-in-Go conflict); it only reaps namespace orphans reparented to PID 1.
func startReaper(sigDone chan struct{}) {
	sigchld := make(chan os.Signal, 256)
	signal.Notify(sigchld, syscall.SIGCHLD)
	go func() {
		ticker := time.NewTicker(reapInterval)
		defer ticker.Stop()
		for {
			select {
			case <-sigchld:
				reapAll()
			case <-ticker.C:
				reapAll()
			case <-sigDone:
				return
			}
		}
	}()
}

// reapAll drains every currently-zombieable child with WNOHANG. It loops until
// no child is available (0) or there are no children at all (ECHILD, treated as
// "nothing to do" since PID 1 may legitimately have no descendants between
// sessions). EINTR is retried within the call; any other error aborts the sweep
// (it will be retried on the next signal/tick). A WNOHANG sweep never blocks.
func reapAll() {
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		switch {
		case err == nil && pid > 0:
			// Reaped one; keep draining.
			continue
		case err == syscall.EINTR:
			continue
		default:
			// pid == 0 (no zombie ready) or ECHILD (no children): nothing to reap.
			return
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
