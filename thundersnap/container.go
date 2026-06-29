// Container namespace management for shared PID namespaces.
//
// This file provides ContainerNsManager which manages shared PID/mount/UTS namespaces
// for container sessions. Each rootFS gets a single "init" process that creates and
// anchors the namespaces. All sessions join these existing namespaces rather than
// creating their own. This allows processes from different sessions to see each
// other via /proc.
package thundersnap

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

// ContainerNsManager manages shared PID/mount/UTS namespaces for container sessions.
// Each rootFS gets a single "init" process that creates and anchors the namespaces.
// All sessions join these existing namespaces rather than creating their own.
//
// TODO(#4): this type duplicates the daemon's own namespace manager
// (cmd/thundersnapd/main.go containerNsEntry + GetOrCreateContainerNs). It
// survives only as the e2e harness (setupSharedNsFrame) to spawn a
// container-init and hand back its PID. Eliminate this copy when the
// session/namespace layer is unified: extract the daemon's manager into an
// importable package and point both the daemon and the e2e harness at it.
type ContainerNsManager struct {
	mu      sync.Mutex
	entries map[string]*containerNsEntry // key: rootFS path
}

type containerNsEntry struct {
	initPid   int            // host PID of the container-init process
	initStdin io.WriteCloser // write end of pipe - close to signal shutdown
	initCmd   *exec.Cmd      // the container-init command (for Wait)
	refCount  int
}

// NewContainerNsManager creates a new container namespace manager.
func NewContainerNsManager() *ContainerNsManager {
	return &ContainerNsManager{
		entries: make(map[string]*containerNsEntry),
	}
}

// GetOrCreateContainerNs returns an existing namespace entry or creates a new one
// by spawning "ts container-init". Returns the init process PID that sessions
// should use to join namespaces via /proc/<pid>/ns/*.
func (m *ContainerNsManager) GetOrCreateContainerNs(rootFS, hostname, domainname string) (initPid int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check for existing entry
	if entry, ok := m.entries[rootFS]; ok {
		// Verify init is still alive. Signal 0 performs only the existence
		// /permission check without delivering a signal.
		//
		// CAVEAT (PID reuse): if the original init exited and the kernel
		// recycled its PID for an unrelated process, this probe succeeds and
		// we would wrongly reuse a foreign PID. We do not verify start-time or
		// cmdline here; this is acceptable in practice because init lives for
		// the lifetime of the refcounted namespace and is shut down explicitly
		// in ReleaseContainerNs, but it is a known hazard.
		if err := syscall.Kill(entry.initPid, 0); err == nil {
			entry.refCount++
			log.Printf("Reusing container namespace for %s (initPid=%d, refCount=%d)",
				rootFS, entry.initPid, entry.refCount)
			return entry.initPid, nil
		}
		// Init died - clean up stale entry
		log.Printf("Container init for %s died (pid %d), cleaning up", rootFS, entry.initPid)
		entry.initStdin.Close()
		entry.initCmd.Wait()
		delete(m.entries, rootFS)
	}

	// Create new container-init process
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

	// Create pipe for stdin - closing it signals shutdown
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return 0, fmt.Errorf("create stdin pipe: %w", err)
	}

	// Create pipe for stdout to read READY signal
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		stdinPipe.Close()
		return 0, fmt.Errorf("create stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr

	// Start in new PID, mount, and UTS namespaces
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS,
	}

	if err := cmd.Start(); err != nil {
		stdinPipe.Close()
		return 0, fmt.Errorf("start container-init: %w", err)
	}

	// Wait for READY signal
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

	entry := &containerNsEntry{
		initPid:   cmd.Process.Pid,
		initStdin: stdinPipe,
		initCmd:   cmd,
		refCount:  1,
	}
	m.entries[rootFS] = entry
	log.Printf("Created container namespace for %s (initPid=%d)", rootFS, entry.initPid)

	return entry.initPid, nil
}

// ReleaseContainerNs decrements the reference count and shuts down init if zero.
func (m *ContainerNsManager) ReleaseContainerNs(rootFS string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.entries[rootFS]
	if !ok {
		return
	}

	entry.refCount--
	log.Printf("Released container namespace for %s (initPid=%d, refCount=%d)",
		rootFS, entry.initPid, entry.refCount)

	if entry.refCount <= 0 {
		// Shut down the container-init process
		log.Printf("Shutting down container namespace for %s (initPid=%d)",
			rootFS, entry.initPid)
		entry.initStdin.Close()
		entry.initCmd.Wait()
		delete(m.entries, rootFS)
	}
}
