// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// cmdNsenter joins the PID/mount/UTS namespaces of a target init process and
// execs a command inside them. It is a CGO-free, in-binary replacement for the
// external nsenter(1): because `ts` ships in every container/VM rootfs, vshd can
// invoke `ts nsenter` to enter a shared container namespace identically on the
// host and inside a VM, with no util-linux dependency.
//
// Usage (a subset of nsenter(1), enough for our session path):
//
//	ts nsenter -t <pid> -p -m -u -- <cmd> [args...]
//
// where -p/-m/-u select the PID/mount/UTS namespaces of <pid>. We never pass -F
// (--no-fork): a Go program cannot start in a freshly-joined PID namespace
// without the fork that places it there (the runtime fails to create threads).
//
// Joining a mount namespace via setns(CLONE_NEWNS) is rejected with EINVAL on a
// multithreaded process, and the Go runtime is always multithreaded. We work
// around this with a two-stage reexec:
//
//   - Stage 1 (this function) joins the UTS and PID namespaces in-process
//     (setns for those is allowed multithreaded; the PID join takes effect for
//     children, which is exactly the stage-2 child we fork next). It then
//     reexecs `/proc/self/exe nsenter --stage2 ...`, passing the mount-ns fd as
//     an extra fd. The forked child lands in the joined PID+UTS namespaces.
//   - Stage 2 (cmdNsenterStage2) locks its OS thread, unshares CLONE_FS so the
//     thread no longer shares its filesystem context with the rest of the
//     runtime, then setns(mnt, CLONE_NEWNS) succeeds on that single thread, and
//     it immediately execs the target (collapsing to a single-threaded image in
//     the joined mount namespace).
func cmdNsenter(args []string) {
	var targetPid int = -1
	var wantPID, wantMnt, wantUTS, wantCgroup bool
	var cmdArgs []string

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-t" && i+1 < len(args):
			pid, err := strconv.Atoi(args[i+1])
			if err != nil {
				fatalNsenter("invalid -t pid %q: %v", args[i+1], err)
			}
			targetPid = pid
			i++
		case args[i] == "-p":
			wantPID = true
		case args[i] == "-m":
			wantMnt = true
		case args[i] == "-u":
			wantUTS = true
		case args[i] == "-C":
			wantCgroup = true
		case args[i] == "--":
			cmdArgs = args[i+1:]
			i = len(args)
		default:
			cmdArgs = args[i:]
			i = len(args)
		}
	}

	if targetPid < 0 {
		fatalNsenter("nsenter requires -t <pid>")
	}
	if len(cmdArgs) == 0 {
		fatalNsenter("nsenter requires a command after --")
	}

	// Join UTS first (cosmetic; lets the child see the container hostname).
	if wantUTS {
		if err := setnsPath(fmt.Sprintf("/proc/%d/ns/uts", targetPid), unix.CLONE_NEWUTS); err != nil {
			fatalNsenter("setns uts: %v", err)
		}
	}

	// Join the cgroup namespace before forking the stage-2 child. This makes
	// /proc/self/cgroup and a cgroup2 mount resolve relative to the container's
	// delegated cgroup root.
	if wantCgroup {
		if err := setnsPath(fmt.Sprintf("/proc/%d/ns/cgroup", targetPid), unix.CLONE_NEWCGROUP); err != nil {
			fatalNsenter("setns cgroup: %v", err)
		}
	}

	// Join the PID namespace. This affects only future children, so the
	// stage-2 child we fork below is what actually runs inside it.
	if wantPID {
		if err := setnsPath(fmt.Sprintf("/proc/%d/ns/pid", targetPid), unix.CLONE_NEWPID); err != nil {
			fatalNsenter("setns pid: %v", err)
		}
	}

	// Build the stage-2 reexec. We always reexec (even without -m) so the PID
	// join takes effect via the fork. The mount-ns fd, when requested, is passed
	// as fd 3 for stage 2 to consume.
	stage2Args := []string{"nsenter", "--stage2"}
	if wantMnt {
		stage2Args = append(stage2Args, "--mnt-fd=3")
		// Pass the target PID so stage2 can use /proc/<pid>/root as a fallback
		// for path resolution if setns(CLONE_NEWNS) gives a stale root dentry
		// (happens in nested containers on btrfs — see comment in stage2).
		stage2Args = append(stage2Args, "--target-pid="+strconv.Itoa(targetPid))
	}
	stage2Args = append(stage2Args, "--")
	stage2Args = append(stage2Args, cmdArgs...)

	self, err := os.Executable()
	if err != nil {
		self = "/proc/self/exe"
	}

	var attr syscall.ProcAttr
	attr.Env = os.Environ()
	attr.Files = []uintptr{os.Stdin.Fd(), os.Stdout.Fd(), os.Stderr.Fd()}

	if wantMnt {
		mntFd, err := unix.Open(fmt.Sprintf("/proc/%d/ns/mnt", targetPid), unix.O_RDONLY, 0)
		if err != nil {
			fatalNsenter("open mnt ns: %v", err)
		}
		attr.Files = append(attr.Files, uintptr(mntFd))
	}

	argv := append([]string{self}, stage2Args...)
	pid, err := syscall.ForkExec(self, argv, &attr)
	if err != nil {
		fatalNsenter("fork stage2: %v", err)
	}

	// Reap the stage-2 child and mirror its exit status so callers (vshd) see
	// the real exit code.
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
		fatalNsenter("wait stage2: %v", err)
	}
	if ws.Signaled() {
		os.Exit(128 + int(ws.Signal()))
	}
	os.Exit(ws.ExitStatus())
}

// cmdNsenterStage2 is the reexec'd child of cmdNsenter. It runs inside the
// already-joined PID/UTS namespaces; its only remaining job is to join the
// mount namespace (when requested) on a single locked thread and exec the
// target command. See cmdNsenter for the full rationale.
func cmdNsenterStage2(args []string) {
	var mntFd int = -1
	var targetPid int = -1
	var cmdArgs []string

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--stage2":
			// marker, ignore
		case len(args[i]) > len("--mnt-fd=") && args[i][:len("--mnt-fd=")] == "--mnt-fd=":
			fd, err := strconv.Atoi(args[i][len("--mnt-fd="):])
			if err != nil {
				fatalNsenter("stage2: invalid --mnt-fd: %v", err)
			}
			mntFd = fd
		case len(args[i]) > len("--target-pid=") && args[i][:len("--target-pid=")] == "--target-pid=":
			pid, err := strconv.Atoi(args[i][len("--target-pid="):])
			if err != nil {
				fatalNsenter("stage2: invalid --target-pid: %v", err)
			}
			targetPid = pid
		case args[i] == "--":
			cmdArgs = args[i+1:]
			i = len(args)
		default:
			cmdArgs = args[i:]
			i = len(args)
		}
	}

	if len(cmdArgs) == 0 {
		fatalNsenter("stage2: no command")
	}

	// Open the executable BEFORE setns(CLONE_NEWNS). In nested containers on
	// btrfs, setns gives us a stale root dentry from the old mount namespace,
	// so path resolution through "/" hits the wrong filesystem. By opening the
	// executable file now (while we're still in the parent's mount namespace
	// where the path resolves correctly), we get a file descriptor that remains
	// valid after setns. We then exec via /proc/self/fd/<fd>.
	//
	// This is the CGO-free equivalent of `fexecve(3)`: open the file, then
	// exec via /proc/self/fd/N, which works because the kernel's exec path
	// follows the fd to the inode, not the path.
	//
	// WHY THIS IS NEEDED (the "stale root dentry" problem):
	//
	// After setns(CLONE_NEWNS), the process's ns/mnt link points to the correct
	// mount namespace (verified by comparing /proc/self/ns/mnt before and after).
	// However, the process's ROOT DENTRY — the in-memory VFS dentry for "/" — is
	// NOT updated. It still points to the root of the OLD mount namespace. Path
	// resolution starts from this stale root, so any path that traverses a
	// different filesystem (e.g. a btrfs subvolume boundary at /work in a nested
	// thundersnap container) fails: the btrfs subvolume directory appears as a
	// regular file (ENOTDIR) because the stale dentry doesn't know it's a mount
	// point / subvolume root.
	//
	// The fd opened here bypasses this: the fd references the inode directly,
	// and /proc/self/fd/<fd> is a procfs magic link that the kernel resolves
	// through the fd table, not through the root dentry.
	execFd, execPath := openExecutableForExec(cmdArgs[0])

	// Similarly, find and open the --chroot= path before setns, so
	// drop-caps-and-run can fchdir+chroot(".") via the fd instead of resolving
	// the path through the stale root dentry. We inject --chroot-fd=N into the
	// command args so drop-caps-and-run picks it up.
	chrootFd := openChrootFd(cmdArgs, targetPid)

	if mntFd >= 0 {
		// Pin to one OS thread and break its CLONE_FS sharing with the rest of
		// the Go runtime so setns(CLONE_NEWNS) is permitted on this thread, then
		// exec immediately (which collapses to a single-threaded image in the
		// joined mount namespace). We never unlock the thread.
		runtime.LockOSThread()
		if err := unix.Unshare(unix.CLONE_FS); err != nil {
			fatalNsenter("stage2: unshare fs: %v", err)
		}
		if err := unix.Setns(mntFd, unix.CLONE_NEWNS); err != nil {
			fatalNsenter("stage2: setns mnt: %v", err)
		}
	}

	// Exec the target. If we opened a file descriptor before setns (the
	// nested-btrfs case), exec via /proc/self/fd/<fd> to use the pre-setns
	// inode rather than re-resolving the path through the (possibly stale)
	// mount namespace root.
	// Inject --chroot-fd=N into cmdArgs if we opened a chroot fd.
	if chrootFd >= 0 {
		cmdArgs = injectChrootFd(cmdArgs, chrootFd)
	}
	if execFd >= 0 {
		defer unix.Close(execFd)
		execPath := fmt.Sprintf("/proc/self/fd/%d", execFd)
		if err := syscall.Exec(execPath, cmdArgs, os.Environ()); err != nil {
			fatalNsenter("stage2: exec %s (via fd %d): %v", cmdArgs[0], execFd, err)
		}
	}
	// Normal case: exec by path
	if err := syscall.Exec(execPath, cmdArgs, os.Environ()); err != nil {
		fatalNsenter("stage2: exec %s: %v", cmdArgs[0], err)
	}
}

// setnsPath opens the namespace file at path and joins it with the given type.
func setnsPath(path string, nsType int) error {
	fd, err := unix.Open(path, unix.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer unix.Close(fd)
	return unix.Setns(fd, nsType)
}

func fatalNsenter(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "error: nsenter: "+format+"\n", a...)
	os.Exit(1)
}

// openExecutableForExec opens the executable file and returns (fd, path).
// The fd can be used with /proc/self/fd/<fd> to exec the file even after
// setns changes the mount namespace (the kernel follows the fd to the inode,
// not the path). Returns (-1, path) if the file can't be opened but the path
// exists (fall back to normal path-based exec). Returns (-1, "") if the
// executable doesn't exist at all (fatal error).
func openExecutableForExec(path string) (fd int, execPath string) {
	resolved, err := findExecutable(path)
	if err != nil {
		fatalNsenter("stage2: %v", err)
	}
	// Open the file with O_PATH | O_CLOEXEC. O_PATH gives us a fd that can be
	// used for execvia /proc/self/fd/<fd> but doesn't require read permission
	// on the file itself (though exec does require exec permission). O_CLOEXEC
	// ensures the fd doesn't leak to the exec'd process (though exec via
	// /proc/self/fd closes it anyway).
	f, err := os.OpenFile(resolved, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		// Can't open the fd — fall back to path-based exec. This works in
		// the non-nested case where setns gives the correct mount table.
		return -1, resolved
	}
	return int(f.Fd()), resolved
}

// openChrootFd opens a directory fd for the chroot path, using
// /proc/<targetPid>/root to reference the container-init's root dentry.
//
// This is critical: container-init bind-mounts chrootPath to itself before
// chrooting and mounting /proc, /sys, /dev. Those mounts are on the bind
// mount's dentry tree, not the original dentry tree. If we open the --chroot=
// path directly, we get the original dentry tree (no proc/sys/dev). By opening
// /proc/<targetPid>/root, we get the container-init's root dentry, which IS
// the bind mount — so proc/sys/dev are visible after chroot.
//
// Must be called BEFORE setns(CLONE_NEWNS), while /proc is still accessible
// from the outer mount namespace. The fd is opened without O_CLOEXEC so it
// survives exec into drop-caps-and-run.
// Returns -1 if the fd can't be opened (fall back to path-based chroot).
func openChrootFd(cmdArgs []string, targetPid int) int {
	if targetPid < 0 {
		return -1
	}
	rootPath := fmt.Sprintf("/proc/%d/root", targetPid)
	fd, err := unix.Open(rootPath, unix.O_PATH|unix.O_DIRECTORY, 0)
	if err != nil {
		return -1 // fall back to path-based chroot
	}
	return fd
}

// injectChrootFd inserts --chroot-fd=<fd> into cmdArgs, right after the
// --chroot=<path> flag if present, or at the beginning otherwise.
func injectChrootFd(cmdArgs []string, fd int) []string {
	fdArg := fmt.Sprintf("--chroot-fd=%d", fd)
	for i, arg := range cmdArgs {
		if strings.HasPrefix(arg, "--chroot=") {
			result := make([]string, 0, len(cmdArgs)+1)
			result = append(result, cmdArgs[:i+1]...)
			result = append(result, fdArg)
			result = append(result, cmdArgs[i+1:]...)
			return result
		}
	}
	return append([]string{fdArg}, cmdArgs...)
}
