// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
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
	var wantPID, wantMnt, wantUTS bool
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

	executable, err := findExecutable(cmdArgs[0])
	if err != nil {
		fatalNsenter("stage2: %v", err)
	}
	if err := syscall.Exec(executable, cmdArgs, os.Environ()); err != nil {
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
