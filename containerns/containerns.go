// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// Package containerns manages shared PID/mount/UTS namespaces for container
// sessions. Each rootFS gets a single "init" process ("ts container-init") that
// creates and anchors the namespaces; all sessions join these existing
// namespaces rather than creating their own, so processes from different
// sessions see each other via /proc.
//
// This is the single source of truth used by both the daemon and the e2e
// harness (it replaces the daemon's former in-file manager and the duplicate
// thundersnap.ContainerNsManager).
package containerns

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tailscale/thundersnap/cgroup"
	"golang.org/x/sys/unix"
)

// Manager manages shared PID/mount/UTS namespaces keyed by rootFS path.
type Manager struct {
	mu        sync.Mutex
	entries   map[string]*entry // key: rootFS path
	cgroupMgr *cgroup.Manager
}

type entry struct {
	initPid   int    // host PID of the container-init process
	initStart uint64 // /proc/<initPid>/stat starttime (clock ticks),
	// pins the init identity against PID reuse: a recycled PID has a
	// different starttime, so a stale entry whose init died is not
	// mistaken for a live one.
	initStdin  io.WriteCloser // write end of pipe - close to signal shutdown
	initCmd    *exec.Cmd      // the container-init command (for Wait)
	cgroupName string
	refCount   int
}

// New creates a namespace manager. cgroupMgr is required: every container is
// placed in a delegated cgroup and a fresh cgroup namespace before it starts.
func New(cgroupMgr *cgroup.Manager) *Manager {
	return &Manager{entries: make(map[string]*entry), cgroupMgr: cgroupMgr}
}

// GetOrCreate returns an existing namespace entry or creates a new one by
// spawning "ts container-init". It returns the init process PID that sessions
// should use to join namespaces via /proc/<pid>/ns/*.
func (m *Manager) GetOrCreate(rootFS, hostname, domainname string) (initPid int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check for existing entry.
	if e, ok := m.entries[rootFS]; ok {
		// Verify init is still alive AND is the same process we started
		// (defend against PID reuse). Signal 0 performs only the existence/
		// permission check without delivering a signal; combined with the
		// recorded /proc/<pid>/stat starttime, a recycled PID (different
		// starttime) is detected and treated as a dead init rather than
		// reused, so we never nsenter into a foreign process's namespaces.
		alive := false
		if err := syscall.Kill(e.initPid, 0); err == nil {
			if e.initStart == 0 {
				// No starttime recorded (older entry / read failed at
				// creation): fall back to the kill(0) probe alone.
				alive = true
			} else if st, err := processStartTime(e.initPid); err == nil && st == e.initStart {
				alive = true
			}
		}
		if alive {
			e.refCount++
			log.Printf("Reusing container namespace for %s (initPid=%d, refCount=%d)",
				rootFS, e.initPid, e.refCount)
			return e.initPid, nil
		}
		// Init died (or its PID was recycled) - clean up stale entry.
		log.Printf("Container init for %s died (pid %d), cleaning up", rootFS, e.initPid)
		e.initStdin.Close()
		e.initCmd.Wait()
		delete(m.entries, rootFS)
	}

	// Create new container-init process.
	absRootFS, err := filepath.Abs(rootFS)
	if err != nil {
		return 0, fmt.Errorf("abs path: %w", err)
	}

	tsBinary := filepath.Join(absRootFS, "bin", "ts")
	args := []string{"container-init", "--chroot=" + absRootFS}
	if hostname != "" {
		args = append(args, "--hostname="+hostname)
	}
	if domainname != "" {
		args = append(args, "--domainname="+domainname)
	}

	if m.cgroupMgr == nil {
		return 0, fmt.Errorf("container cgroup manager is required")
	}
	cgroupName := m.cgroupMgr.ContainerName(absRootFS)
	cgroupDir, err := m.cgroupMgr.PrepareContainer(cgroupName)
	if err != nil {
		return 0, fmt.Errorf("prepare container cgroup: %w", err)
	}
	defer cgroupDir.Close()

	cmd := exec.Command(tsBinary, args...)
	cmd.Dir = "/"

	// Create pipe for stdin - closing it signals shutdown.
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return 0, fmt.Errorf("create stdin pipe: %w", err)
	}

	// Create pipe for stdout to read READY signal.
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		stdinPipe.Close()
		return 0, fmt.Errorf("create stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr

	// Prefer clone3's atomic CLONE_INTO_CGROUP path. A process whose cgroup
	// namespace exposes a relative ".." path cannot use that kernel API: the
	// cgroup fd is unreachable from its namespace and exec returns ENOENT. Such
	// a process is already confined by an outer cgroup, so start it normally.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS,
	}
	cloneInto := cgroup.CanCloneInto()
	if cloneInto {
		cmd.SysProcAttr.Cloneflags |= unix.CLONE_NEWCGROUP
		cmd.SysProcAttr.UseCgroupFD = true
		cmd.SysProcAttr.CgroupFD = int(cgroupDir.Fd())
	}

	if err := cmd.Start(); err != nil {
		stdinPipe.Close()
		return 0, fmt.Errorf("start container-init in cgroup %s: %w", cgroupName, err)
	}
	if !cloneInto {
		// This environment already supplies outer cgroup confinement but cannot
		// address host-relative nested cgroup paths from this namespace.
		// Continue without nested delegation rather than treating the kernel's
		// namespace-relative ENOENT as a missing executable.
		log.Printf("cgroup path is namespace-relative; using inherited outer confinement for %s", rootFS)
	} else if err := m.cgroupMgr.DelegateContainer(cmd.Process.Pid, cgroupName); err != nil {
		stdinPipe.Close()
		cmd.Process.Kill()
		cmd.Wait()
		return 0, fmt.Errorf("delegate container cgroup: %w", err)
	}

	// Wait for READY signal.
	readyCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 64)
		n, err := stdoutPipe.Read(buf)
		if err != nil {
			readyCh <- fmt.Errorf("read ready: %w", err)
			return
		}
		if !strings.HasPrefix(string(buf[:n]), "READY") {
			readyCh <- fmt.Errorf("unexpected init response: %q", string(buf[:n]))
			return
		}
		readyCh <- nil
	}()

	select {
	case err := <-readyCh:
		if err != nil {
			stdinPipe.Close()
			cmd.Process.Kill()
			cmd.Wait()
			return 0, err
		}
	case <-time.After(10 * time.Second):
		stdinPipe.Close()
		cmd.Process.Kill()
		cmd.Wait()
		return 0, fmt.Errorf("container-init timeout")
	}

	e := &entry{
		initPid:    cmd.Process.Pid,
		initStart:  startTimeOrZero(cmd.Process.Pid),
		initStdin:  stdinPipe,
		initCmd:    cmd,
		cgroupName: cgroupName,
		refCount:   1,
	}
	m.entries[rootFS] = e
	log.Printf("Created container namespace for %s (initPid=%d)", rootFS, e.initPid)

	return e.initPid, nil
}

// processStartTime returns the starttime field (field 22, in clock ticks) of
// /proc/<pid>/stat. The comm field may contain spaces and parentheses, so the
// parse anchors on the last ')' before splitting the remaining fields. It is
// used to detect PID reuse: a recycled PID reports a different starttime than
// the process we recorded.
func processStartTime(pid int) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	idx := strings.LastIndexByte(string(data), ')')
	if idx < 0 {
		return 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	fields := strings.Fields(string(data[idx+1:]))
	// After ')', the list begins at field 3 (state). starttime is field 22, i.e.
	// fields[22-3] = fields[19].
	const starttimeIdx = 19
	if len(fields) <= starttimeIdx {
		return 0, fmt.Errorf("short /proc/%d/stat", pid)
	}
	return strconv.ParseUint(fields[starttimeIdx], 10, 64)
}

// startTimeOrZero returns the process starttime, or 0 if it cannot be read
// (the caller falls back to a kill(pid,0)-only liveness probe in that case).
func startTimeOrZero(pid int) uint64 {
	st, err := processStartTime(pid)
	if err != nil {
		log.Printf("warning: could not read starttime for container-init pid %d: %v", pid, err)
		return 0
	}
	return st
}

// CgroupName returns the delegated cgroup for rootFS.
func (m *Manager) CgroupName(rootFS string) string {
	return m.cgroupMgr.ContainerName(rootFS)
}

// Release decrements the reference count for rootFS and shuts down its init
// process when the count reaches zero. Releasing an unknown rootFS is a no-op.
func (m *Manager) Release(rootFS string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.entries[rootFS]
	if !ok {
		return
	}

	e.refCount--
	log.Printf("Released container namespace for %s (initPid=%d, refCount=%d)",
		rootFS, e.initPid, e.refCount)

	if e.refCount <= 0 {
		// Close stdin to signal init to exit, then wait for it.
		log.Printf("Shutting down container namespace for %s (initPid=%d)",
			rootFS, e.initPid)
		e.initStdin.Close()
		e.initCmd.Wait()
		if err := m.cgroupMgr.RemoveContainer(e.cgroupName); err != nil {
			log.Printf("warning: failed to remove cgroup for %s: %v", rootFS, err)
		}
		delete(m.entries, rootFS)
	}
}
