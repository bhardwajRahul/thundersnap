// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// autorun.go provides process management for autorun commands configured on refs.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/tailscale/thundersnap/frameid"
	"github.com/tailscale/thundersnap/refs"
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
type autorunProcess struct {
	key       autorunKey
	frameUUID frameid.ID
	argv      []string
	cmd       *exec.Cmd
	cancel    context.CancelFunc
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
	store := refs.NewUserStore(m.dataDir, user)
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
			// Kill the current process if running
			m.mu.Lock()
			if proc.cmd != nil && proc.cmd.Process != nil {
				proc.cmd.Process.Signal(syscall.SIGTERM)
				// Give it a moment to exit gracefully, then force kill
				go func(cmd *exec.Cmd) {
					time.Sleep(2 * time.Second)
					if cmd.Process != nil {
						cmd.Process.Kill()
					}
				}(proc.cmd)
			}
			m.mu.Unlock()
			return

		default:
		}

		// Run the process
		err := m.runOnce(ctx, proc)
		if err != nil {
			if ctx.Err() != nil {
				// Context cancelled, exit cleanly
				return
			}
			log.Printf("autorun: process %s/%s exited: %v, restarting in %v",
				proc.key.user, proc.key.refName, err, backoff)
		} else {
			log.Printf("autorun: process %s/%s exited with status 0, restarting in %v",
				proc.key.user, proc.key.refName, backoff)
		}

		// Wait before restarting
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		// Increase backoff for next restart (exponential with cap)
		backoff = backoff * 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// runOnce runs the autorun command once and waits for it to exit.
func (m *autorunManager) runOnce(ctx context.Context, proc *autorunProcess) error {
	// Build the frame path: fs/<user>/<uuid>
	framePath := filepath.Join(m.dataDir, "fs", proc.key.user, proc.frameUUID.String())

	// Build the command to run via ts drop-caps-and-run
	// This runs the command in the container environment with proper namespaces.
	tsBinary := filepath.Join(framePath, "bin", "ts")
	if _, err := os.Stat(tsBinary); err != nil {
		return fmt.Errorf("ts binary not found in frame: %v", err)
	}

	// The command is: ts drop-caps-and-run --chroot=<framePath> -- <argv...>
	// TODO: --keep-dev-caps is currently always passed to allow running
	// thundersnap recursively (for development). See vshd's buildSessionCmd
	// for the full rationale; this should be made configurable.
	args := []string{"drop-caps-and-run", "--chroot=" + framePath, "--keep-dev-caps", "--"}
	args = append(args, proc.argv...)

	cmd := exec.CommandContext(ctx, tsBinary, args...)
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS,
	}

	// Redirect stdout/stderr to logs
	cmd.Stdout = &autorunLogWriter{user: proc.key.user, refName: proc.key.refName, stream: "stdout"}
	cmd.Stderr = &autorunLogWriter{user: proc.key.user, refName: proc.key.refName, stream: "stderr"}

	m.mu.Lock()
	proc.cmd = cmd
	m.mu.Unlock()

	err := cmd.Run()

	m.mu.Lock()
	proc.cmd = nil
	m.mu.Unlock()

	return err
}

// stop stops the autorun process.
func (p *autorunProcess) stop() {
	if p.cancel != nil {
		p.cancel()
	}
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
