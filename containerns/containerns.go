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
	"strings"
	"sync"
	"syscall"
	"time"
)

// Manager manages shared PID/mount/UTS namespaces keyed by rootFS path.
type Manager struct {
	mu      sync.Mutex
	entries map[string]*entry // key: rootFS path
}

type entry struct {
	initPid   int            // host PID of the container-init process
	initStdin io.WriteCloser // write end of pipe - close to signal shutdown
	initCmd   *exec.Cmd      // the container-init command (for Wait)
	refCount  int
}

// New creates a new namespace manager.
func New() *Manager {
	return &Manager{
		entries: make(map[string]*entry),
	}
}

// GetOrCreate returns an existing namespace entry or creates a new one by
// spawning "ts container-init". It returns the init process PID that sessions
// should use to join namespaces via /proc/<pid>/ns/*.
func (m *Manager) GetOrCreate(rootFS, hostname, domainname string) (initPid int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check for existing entry.
	if e, ok := m.entries[rootFS]; ok {
		// Verify init is still alive. Signal 0 performs only the existence/
		// permission check without delivering a signal.
		//
		// CAVEAT (PID reuse): if the original init exited and the kernel
		// recycled its PID for an unrelated process, this probe succeeds and
		// we would wrongly reuse a foreign PID. We do not verify start-time or
		// cmdline here; this is acceptable in practice because init lives for
		// the lifetime of the refcounted namespace and is shut down explicitly
		// in Release, but it is a known hazard.
		if err := syscall.Kill(e.initPid, 0); err == nil {
			e.refCount++
			log.Printf("Reusing container namespace for %s (initPid=%d, refCount=%d)",
				rootFS, e.initPid, e.refCount)
			return e.initPid, nil
		}
		// Init died - clean up stale entry.
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

	// Start in new PID, mount, and UTS namespaces.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS,
	}

	if err := cmd.Start(); err != nil {
		stdinPipe.Close()
		return 0, fmt.Errorf("start container-init: %w", err)
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
		initPid:   cmd.Process.Pid,
		initStdin: stdinPipe,
		initCmd:   cmd,
		refCount:  1,
	}
	m.entries[rootFS] = e
	log.Printf("Created container namespace for %s (initPid=%d)", rootFS, e.initPid)

	return e.initPid, nil
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
		delete(m.entries, rootFS)
	}
}
