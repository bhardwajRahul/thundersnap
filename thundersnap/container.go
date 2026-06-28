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

// buildNsenterCmd builds the `nsenter ... ts drop-caps-and-run` command that
// joins the shared namespaces of initPid and runs args inside the chroot. It is
// the single source of truth shared by RunInContainerNs and StartInContainerNs
// so the two cannot drift. If tsBinary is empty it defaults to <absRootFS>/bin/ts.
func buildNsenterCmd(absRootFS, tsBinary string, initPid int, args ...string) *exec.Cmd {
	if tsBinary == "" {
		tsBinary = filepath.Join(absRootFS, "bin", "ts")
	}

	// Build the command: ts drop-caps-and-run --chroot=<rootFS> -- <args>
	tsArgs := []string{
		tsBinary, "drop-caps-and-run",
		"--chroot=" + absRootFS,
		"--",
	}
	tsArgs = append(tsArgs, args...)

	// nsenter joins the PID, mount, and UTS namespaces of the init process.
	// IMPORTANT: We do NOT use -F (--no-fork) because Go programs fail to start
	// in a joined PID namespace without the fork that properly places them in
	// the namespace. Without the fork, Go's runtime fails with EINVAL when
	// trying to create OS threads.
	nsenterArgs := []string{
		"-t", fmt.Sprintf("%d", initPid),
		"-p", "-m", "-u",
		"--",
	}
	nsenterArgs = append(nsenterArgs, tsArgs...)

	cmd := exec.Command("nsenter", nsenterArgs...)
	cmd.Dir = "/"
	return cmd
}

// RunInContainerNs runs a command in the container namespace for rootFS.
// It handles:
// 1. Getting or creating the shared namespace for rootFS
// 2. Using nsenter to join the namespace
// 3. Running the command via ts drop-caps-and-run
//
// The command output is returned. If tsBinary is empty, it defaults to
// <rootFS>/bin/ts.
func (m *ContainerNsManager) RunInContainerNs(rootFS, hostname, domainname, tsBinary string, args ...string) ([]byte, error) {
	initPid, err := m.GetOrCreateContainerNs(rootFS, hostname, domainname)
	if err != nil {
		return nil, fmt.Errorf("get container namespace: %w", err)
	}
	defer m.ReleaseContainerNs(rootFS)

	absRootFS, err := filepath.Abs(rootFS)
	if err != nil {
		return nil, fmt.Errorf("abs path: %w", err)
	}

	return buildNsenterCmd(absRootFS, tsBinary, initPid, args...).CombinedOutput()
}

// StartInContainerNs starts a command in the container namespace for rootFS
// without waiting for it to complete. This is useful for long-running processes.
// The caller is responsible for calling ReleaseContainerNs after the process exits.
//
// Returns the started command and the init PID.
func (m *ContainerNsManager) StartInContainerNs(rootFS, hostname, domainname, tsBinary string, args ...string) (*exec.Cmd, int, error) {
	initPid, err := m.GetOrCreateContainerNs(rootFS, hostname, domainname)
	if err != nil {
		return nil, 0, fmt.Errorf("get container namespace: %w", err)
	}
	// Note: caller must call ReleaseContainerNs when done

	absRootFS, err := filepath.Abs(rootFS)
	if err != nil {
		m.ReleaseContainerNs(rootFS)
		return nil, 0, fmt.Errorf("abs path: %w", err)
	}

	cmd := buildNsenterCmd(absRootFS, tsBinary, initPid, args...)
	if err := cmd.Start(); err != nil {
		m.ReleaseContainerNs(rootFS)
		return nil, 0, fmt.Errorf("start command: %w", err)
	}

	return cmd, initPid, nil
}
