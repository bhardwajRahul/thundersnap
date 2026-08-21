// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// autorun.go provides process management for autorun commands configured on refs.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tailscale/thundersnap/frameid"
	"github.com/tailscale/thundersnap/refs"
	"github.com/tailscale/thundersnap/vshdproto"
)

// autorunManager manages autorun processes for refs. It ensures that refs with
// autorun configurations have their processes running, and handles starting,
// stopping, and restarting processes as needed.
type autorunManager struct {
	mu        sync.Mutex
	processes map[autorunKey]*autorunProcess
	dataDir   string // root data directory
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// autorunKey identifies an autorun process by user and ref name.
type autorunKey struct {
	user    string
	refName string
}

// autorunProcess tracks a running autorun process.
//
// The process runs inside a vshd container session (the same path SSH container
// sessions take), so it is tracked by the vshd session connection rather than a
// host exec.Cmd. Closing conn tears the session down and SIGHUPs the child (see
// vshdsession.servePipe), so stop() closes conn rather than signalling a PID.
type autorunProcess struct {
	key       autorunKey
	frameUUID frameid.ID
	argv      []string
	cancel    context.CancelFunc
	mu        sync.Mutex    // guards conn
	conn      net.Conn      // current vshd session conn; nil between runs / when stopped
	done      chan struct{} // closed when the supervisor goroutine exits
}

// globalAutorun is the singleton autorun manager.
var globalAutorun *autorunManager

// initAutorunManager creates and starts the global autorun manager.
// It scans all existing refs for autorun configurations and starts processes.
func initAutorunManager(dataDir string) {
	ctx, cancel := context.WithCancel(context.Background())
	globalAutorun = &autorunManager{
		processes: make(map[autorunKey]*autorunProcess),
		dataDir:   dataDir,
		ctx:       ctx,
		cancel:    cancel,
	}

	// Scan existing refs and start autorun processes
	globalAutorun.scanAndStartAll()
}

// shutdownAutorunManager stops all autorun processes and shuts down the manager.
func shutdownAutorunManager() {
	if globalAutorun == nil {
		return
	}

	globalAutorun.cancel()

	globalAutorun.mu.Lock()
	for _, proc := range globalAutorun.processes {
		proc.stop()
	}
	globalAutorun.mu.Unlock()

	// Wait for all supervisor goroutines to exit
	globalAutorun.wg.Wait()
}

// scanAndStartAll scans all users' refs directories and starts autorun processes.
func (m *autorunManager) scanAndStartAll() {
	refsRoot := filepath.Join(m.dataDir, "refs")
	userDirs, err := os.ReadDir(refsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return // No refs yet
		}
		log.Printf("autorun: failed to scan refs dir: %v", err)
		return
	}

	for _, userDir := range userDirs {
		if !userDir.IsDir() {
			continue
		}
		user := userDir.Name()
		m.scanUserRefs(user)
	}
}

// scanUserRefs scans a user's refs and starts autorun processes for any with autorun configured.
func (m *autorunManager) scanUserRefs(user string) {
	store := refs.NewNamespaceStore(m.dataDir, user)
	names, err := store.List()
	if err != nil {
		log.Printf("autorun: failed to list refs for user %s: %v", user, err)
		return
	}

	for _, name := range names {
		ref, err := store.Get(name)
		if err != nil {
			log.Printf("autorun: failed to get ref %s/%s: %v", user, name, err)
			continue
		}

		if len(ref.Autorun) > 0 {
			m.startProcess(user, name, ref.UUID, ref.Autorun)
		}
	}
}

// startProcess starts an autorun process for a ref. If a process is already
// running for this ref, it does nothing.
func (m *autorunManager) startProcess(user, refName string, frameUUID frameid.ID, argv []string) {
	if len(argv) == 0 {
		return
	}

	key := autorunKey{user: user, refName: refName}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if already running
	if existing, ok := m.processes[key]; ok {
		// If running in the same frame with the same command, do nothing
		if existing.frameUUID == frameUUID && argsEqual(existing.argv, argv) {
			return
		}
		// Different frame or command - stop the old one
		existing.stop()
		delete(m.processes, key)
	}

	// Create new process
	ctx, cancel := context.WithCancel(m.ctx)
	proc := &autorunProcess{
		key:       key,
		frameUUID: frameUUID,
		argv:      argv,
		cancel:    cancel,
		done:      make(chan struct{}),
	}
	m.processes[key] = proc

	// Start the supervisor goroutine
	m.wg.Add(1)
	go m.supervise(ctx, proc)

	log.Printf("autorun: started process for %s/%s in frame %s: %v", user, refName, frameUUID, argv)
}

// stopProcess stops an autorun process for a ref.
func (m *autorunManager) stopProcess(user, refName string) {
	key := autorunKey{user: user, refName: refName}

	m.mu.Lock()
	proc, ok := m.processes[key]
	if ok {
		delete(m.processes, key)
	}
	m.mu.Unlock()

	if ok {
		proc.stop()
		// Wait for supervisor to exit
		<-proc.done
		log.Printf("autorun: stopped process for %s/%s", user, refName)
	}
}

// restartProcess stops the process in the old frame and starts it in the new frame.
// This is used when a ref is moved.
func (m *autorunManager) restartProcess(user, refName string, newUUID frameid.ID, argv []string) {
	if len(argv) == 0 {
		return
	}

	key := autorunKey{user: user, refName: refName}

	m.mu.Lock()
	proc, ok := m.processes[key]
	if ok {
		delete(m.processes, key)
	}
	m.mu.Unlock()

	if ok {
		proc.stop()
		<-proc.done
		log.Printf("autorun: stopped process for %s/%s (for move)", user, refName)
	}

	m.startProcess(user, refName, newUUID, argv)
}

// supervise monitors a process and restarts it if it dies unexpectedly.
// It runs until the process's context is cancelled.
func (m *autorunManager) supervise(ctx context.Context, proc *autorunProcess) {
	defer m.wg.Done()
	defer close(proc.done)

	backoff := 100 * time.Millisecond
	maxBackoff := 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			// stop() (or shutdownAutorunManager) already cancelled ctx and
			// closed proc.conn, which tears down the in-container session and
			// SIGHUPs the child. Close conn defensively in case ctx was
			// cancelled by another path; a nil/double close is harmless.
			proc.mu.Lock()
			if proc.conn != nil {
				_ = proc.conn.Close()
				proc.conn = nil
			}
			proc.mu.Unlock()
			return

		default:
		}

		// Run one attempt. On failure the in-container autorun-run wrapper holds
		// this same session open for backoff, visibly as `ts retry-on-fail`.
		err := m.runOnce(ctx, proc, backoff)
		if err != nil {
			if ctx.Err() != nil {
				// Context cancelled, exit cleanly
				return
			}
			log.Printf("autorun: process %s/%s exited: %v, restarting",
				proc.key.user, proc.key.refName, err)
		} else {
			log.Printf("autorun: process %s/%s exited with status 0, restarting in %v",
				proc.key.user, proc.key.refName, backoff)
		}

		// Failed attempts already spent the delay inside the container as the
		// visible retry-on-fail process. Successful commands are also restarted,
		// but retain the daemon-side delay to avoid a tight loop.
		if err == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
		}

		// Increase backoff for next restart (exponential with cap)
		backoff = backoff * 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// runOnce runs the autorun command once by entering a container session via
// the host vshd shim — the same path SSH container sessions take
// (runContainerSession) — and blocks until the command exits or the session is
// torn down. The command runs through the same unprivileged login path as
// `ssh user@<ref> -- <argv>`.
//
// Going through vshd (rather than spawning `ts drop-caps-and-run` with a bespoke
// CLONE_NEWPID) shares vshd's namespace anchoring and, critically, the
// disconnect cleanup in vshdsession.servePipe: when stop() closes conn (or the
// daemon exits and the kernel closes the conn's fd), the in-container
// session-serve SIGHUPs the child, so a non-stdin-reading autorun command (e.g.
// `while true; do :; done`) dies instead of being orphaned and spinning forever.
// The old bespoke CLONE_NEWPID spawn reparented to init on daemon exit and left
// the child running in its own PID namespace with nothing to kill it.
func (m *autorunManager) runOnce(ctx context.Context, proc *autorunProcess, retryDelay time.Duration) error {
	// Resolve the ref to its current frame rootfs so ref moves are reflected.
	rootFS, _, err := resolveFrameRootFS(proc.key.user, proc.key.refName)
	if err != nil {
		return fmt.Errorf("resolve frame %s/%s: %w", proc.key.user, proc.key.refName, err)
	}
	if err := prepareContainerRootFS(rootFS, ""); err != nil {
		return fmt.Errorf("prepare rootfs: %w", err)
	}
	if _, err := controlServers.getOrCreateControlServer(rootFS); err != nil {
		return fmt.Errorf("start control socket: %w", err)
	}
	defer controlServers.releaseControlServer(rootFS)

	absRootFS, err := filepath.Abs(rootFS)
	if err != nil {
		return fmt.Errorf("abs rootfs: %w", err)
	}
	framePathHdr := strings.TrimPrefix(absRootFS, "/")

	sockPath, err := hostVshd.ensure()
	if err != nil {
		return fmt.Errorf("start host vshd: %w", err)
	}
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return fmt.Errorf("dial host vshd: %w", err)
	}
	defer conn.Close()

	// Record the conn so stop() can close it to tear the session down. Cleared
	// (under proc.mu) when runOnce returns.
	proc.mu.Lock()
	proc.conn = conn
	proc.mu.Unlock()
	defer func() {
		proc.mu.Lock()
		proc.conn = nil
		proc.mu.Unlock()
	}()

	// Enter exactly like an `ssh user@<ref> -- ...` exec session. vshd wraps
	// this in `su - user -c`, setting HOME and the rest of the login identity
	// before starting the autorun wrapper. No stdin: an immediate EOF is sent.
	// The wrapper executes proc.argv without an additional shell and only
	// remains after failure, as the ps-visible retry-on-fail sleeper.
	argv := append([]string{"/bin/ts", "autorun-run", retryDelay.String()}, proc.argv...)
	writeVshdRequest(conn, framePathHdr, "user", false, argv, thundersnapSessionEnv(proc.key.user, proc.frameUUID))

	exitCode := proxyVshdSessionGeneric(
		strings.NewReader(""), // stdin: immediate EOF (no input)
		&autorunLogWriter{user: proc.key.user, refName: proc.key.refName, stream: "stdout"},
		&autorunLogWriter{user: proc.key.user, refName: proc.key.refName, stream: "stderr"},
		conn,
		false,               // isPty
		vshdproto.Winsize{}, // initialWinsize (n/a for non-PTY)
		nil,                 // winCh
		nil, nil, nil,       // clientClosed, done, panicked (n/a)
	)
	if exitCode != 0 {
		return fmt.Errorf("exit code %d", exitCode)
	}
	return nil
}

// stop stops the autorun process by cancelling its context and closing the
// vshd session conn. Closing conn tears down the in-container session and
// SIGHUPs the child (see runOnce / vshdsession.servePipe), so a non-stdin-
// reading command (e.g. `while true; do :; done`) exits promptly.
func (p *autorunProcess) stop() {
	if p.cancel != nil {
		p.cancel()
	}
	p.mu.Lock()
	if p.conn != nil {
		_ = p.conn.Close()
		p.conn = nil
	}
	p.mu.Unlock()
}

// autorunLogWriter logs output from autorun processes.
type autorunLogWriter struct {
	user    string
	refName string
	stream  string
}

func (w *autorunLogWriter) Write(p []byte) (int, error) {
	// Log each line
	log.Printf("autorun %s/%s %s: %s", w.user, w.refName, w.stream, string(p))
	return len(p), nil
}

// argsEqual compares two string slices for equality.
func argsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
