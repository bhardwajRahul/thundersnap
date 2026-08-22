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
	stopping   bool
	stopped    chan struct{}
}

// initShutdownTimeout bounds how long a container-init may take to exit after
// its lifecycle pipe is closed. A wedged init must not wedge vshd forever.
// It is a variable so tests can exercise the escalation path quickly.
var initShutdownTimeout = 10 * time.Second

// stopEntryDelay is a test-only knob that inserts a synchronous delay at the
// start of stopEntry (after marking the entry stopping, before reaping it).
// It simulates the slow init/cgroup/btrfs teardown observed on a slower arm64
// VM, widening the window in which a concurrent GetOrCreate for the same
// rootfs observes the entry mid-teardown and blocks on e.stopped. Real callers
// never experience this delay unless they opt in via TS_TEST_STOP_ENTRY_DELAY
// (parsed in an init() below); production is unchanged.
var stopEntryDelay time.Duration

func init() {
	if v := os.Getenv("TS_TEST_STOP_ENTRY_DELAY"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			stopEntryDelay = d
		}
	}
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

	// Check for existing entry.
	if e, ok := m.entries[rootFS]; ok {
		// A final Release is shutting this entry down without holding m.mu.
		// Wait for that specific rootfs, then retry. Other rootfs entries remain
		// usable while a slow or wedged init is being reaped.
		if e.stopping {
			stopped := e.stopped
			m.mu.Unlock()
			<-stopped
			return m.GetOrCreate(rootFS, hostname, domainname)
		}
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
			m.mu.Unlock()
			return e.initPid, nil
		}
		// Init died (or its PID was recycled). Reaping can block in Wait, so do
		// it without the manager mutex; otherwise one bad frame freezes every
		// subsequent vshd session, even for unrelated frames.
		log.Printf("Container init for %s died (pid %d), cleaning up", rootFS, e.initPid)
		e.stopping = true
		e.stopped = make(chan struct{})
		m.mu.Unlock()
		m.stopEntry(rootFS, e)
		return m.GetOrCreate(rootFS, hostname, domainname)
	}

	// Keep creation serialized. It includes waiting for READY, but a second
	// creator must not start another init for the same rootfs.
	defer m.mu.Unlock()

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

	e, ok := m.entries[rootFS]
	if !ok || e.stopping {
		m.mu.Unlock()
		return
	}

	e.refCount--
	log.Printf("Released container namespace for %s (initPid=%d, refCount=%d)",
		rootFS, e.initPid, e.refCount)

	if e.refCount > 0 {
		m.mu.Unlock()
		return
	}

	// Process shutdown and Wait can block. Mark this rootfs as stopping, then
	// reap it without m.mu so an unhealthy init cannot freeze all containers.
	e.stopping = true
	e.stopped = make(chan struct{})
	m.mu.Unlock()
	m.stopEntry(rootFS, e)
}

// stopEntry terminates and reaps e, then removes it if it is still the current
// entry for rootFS. The caller must have set e.stopping while holding m.mu.
func (m *Manager) stopEntry(rootFS string, e *entry) {
	log.Printf("Shutting down container namespace for %s (initPid=%d)", rootFS, e.initPid)
	if stopEntryDelay > 0 {
		log.Printf("TEST: injecting %s stopEntry delay for %s", stopEntryDelay, rootFS)
		time.Sleep(stopEntryDelay)
	}
	_ = e.initStdin.Close()

	waited := make(chan error, 1)
	go func() { waited <- e.initCmd.Wait() }()
	select {
	case <-waited:
	case <-time.After(initShutdownTimeout):
		log.Printf("Container init for %s (pid %d) did not exit after %s; killing it",
			rootFS, e.initPid, initShutdownTimeout)
		_ = e.initCmd.Process.Kill()
		<-waited
	}

	if m.cgroupMgr != nil {
		// Kill any detached leftover that escaped container-init's PID-1 shutdown
		// (setsid/nohup jobs, nested-thundersnap children) before removing the
		// cgroup. Otherwise such a survivor keeps the container cgroup non-empty,
		// RemoveContainer's rmdir returns EBUSY, the cgroup dir persists with stale
		// limits/procs, and the next GetOrCreate (which reuses the same
		// cgroupName derived from rootFS) inherits a poisoned cgroup that can wedge
		// the fresh container-init's startup. Best-effort: log and proceed.
		if err := m.cgroupMgr.KillContainer(e.cgroupName); err != nil {
			log.Printf("warning: failed to kill leftover processes in cgroup for %s: %v", rootFS, err)
		}
		if err := m.cgroupMgr.RemoveContainer(e.cgroupName); err != nil {
			log.Printf("warning: failed to remove cgroup for %s: %v", rootFS, err)
		}
	}

	m.mu.Lock()
	if m.entries[rootFS] == e {
		delete(m.entries, rootFS)
	}
	close(e.stopped)
	m.mu.Unlock()
}
