// Package thundersnap provides session management for container and VM environments.
package thundersnap

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/creack/pty"
)

// VMConfig holds configuration for starting a VM session.
type VMConfig struct {
	// RootFS is the path to the root filesystem to share via virtiofs.
	RootFS string
	// VMDir is the path to the directory containing cloud-hypervisor and vmlinux.
	VMDir string
	// Stdin is the input stream for the VM console.
	Stdin io.Reader
	// Stdout is the output stream for the VM console.
	Stdout io.Writer
	// UsePTY indicates whether to allocate a PTY for cloud-hypervisor.
	// This is needed when Stdin is not a real TTY (e.g., SSH sessions).
	UsePTY bool
}

// VMSession represents a running VM session.
type VMSession struct {
	virtiofsdCmd *exec.Cmd
	chvCmd       *exec.Cmd
	virtiofsSock string
	pty          *os.File // only set if UsePTY is true
	stdinPipe    io.WriteCloser // only set if UsePTY is false
	done         chan struct{}
	stdinClosed  chan struct{}
}

// StartVM starts a new VM session with the given configuration.
func StartVM(cfg VMConfig) (*VMSession, error) {
	// Create unique socket path for this session
	sessionID := fmt.Sprintf("%d%d", os.Getpid(), time.Now().UnixNano())
	virtiofsSock := filepath.Join("/tmp", fmt.Sprintf("virtiofs-%s.sock", sessionID))

	// Start virtiofsd
	log.Printf("Starting virtiofsd with shared-dir=%s", cfg.RootFS)
	virtiofsdCmd := exec.Command("/usr/libexec/virtiofsd",
		"--socket-path="+virtiofsSock,
		"--shared-dir="+cfg.RootFS,
		"--cache=always",
	)
	virtiofsdCmd.Stderr = os.Stderr
	if err := virtiofsdCmd.Start(); err != nil {
		return nil, fmt.Errorf("start virtiofsd: %w", err)
	}

	// Wait for virtiofsd socket to be created
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(virtiofsSock); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(virtiofsSock); err != nil {
		virtiofsdCmd.Process.Kill()
		virtiofsdCmd.Wait()
		os.Remove(virtiofsSock)
		return nil, fmt.Errorf("virtiofsd socket not created: %w", err)
	}
	log.Printf("virtiofsd socket ready at %s", virtiofsSock)

	// Paths to cloud-hypervisor and kernel
	chvPath := filepath.Join(cfg.VMDir, "cloud-hypervisor")
	kernelPath := filepath.Join(cfg.VMDir, "vmlinux")

	// Build kernel command line
	// We wrap the shell in a tiny script that powers off the VM when the shell exits.
	// This avoids the kernel panic from init exiting, and cloud-hypervisor exits
	// cleanly on ACPI shutdown. We use busybox poweroff -f which doesn't need /proc.
	cmdline := `console=ttyS0 rootfstype=virtiofs root=rootfs rw init=/bin/sh -- -c "/bin/sh; /bin/busybox poweroff -f"`

	// Start cloud-hypervisor
	// --pvpanic enables the pvpanic device which allows the guest to signal panic to the host
	log.Printf("Starting cloud-hypervisor")
	chvCmd := exec.Command(chvPath,
		"--kernel", kernelPath,
		"--cpus", "boot=1",
		"--memory", "size=512M,shared=on",
		"--fs", fmt.Sprintf("tag=rootfs,socket=%s", virtiofsSock),
		"--cmdline", cmdline,
		"--serial", "tty",
		"--console", "off",
		"--pvpanic",
	)

	session := &VMSession{
		virtiofsdCmd: virtiofsdCmd,
		chvCmd:       chvCmd,
		virtiofsSock: virtiofsSock,
		done:         make(chan struct{}),
		stdinClosed:  make(chan struct{}),
	}

	if cfg.UsePTY {
		// Use PTY for cloud-hypervisor - needed when stdin is not a real TTY
		// (e.g., SSH sessions) because cloud-hypervisor's --serial tty expects one
		ptmx, err := pty.Start(chvCmd)
		if err != nil {
			virtiofsdCmd.Process.Kill()
			virtiofsdCmd.Wait()
			os.Remove(virtiofsSock)
			return nil, fmt.Errorf("start cloud-hypervisor with pty: %w", err)
		}
		session.pty = ptmx
		log.Printf("cloud-hypervisor started with PID %d (using PTY)", chvCmd.Process.Pid)

		// Monitor cloud-hypervisor in background
		go func() {
			chvCmd.Wait()
			log.Printf("cloud-hypervisor exited")
			close(session.done)
		}()

		// Copy stdout from PTY to cfg.Stdout
		go func() {
			io.Copy(cfg.Stdout, ptmx)
		}()

		// Copy stdin from cfg.Stdin to PTY and detect when it closes
		go func() {
			io.Copy(ptmx, cfg.Stdin)
			log.Printf("stdin closed")
			close(session.stdinClosed)
		}()
	} else {
		// Direct pipe mode - works when stdin is already a TTY
		stdinPipe, err := chvCmd.StdinPipe()
		if err != nil {
			virtiofsdCmd.Process.Kill()
			virtiofsdCmd.Wait()
			os.Remove(virtiofsSock)
			return nil, fmt.Errorf("create stdin pipe: %w", err)
		}
		session.stdinPipe = stdinPipe

		chvCmd.Stdout = cfg.Stdout
		chvCmd.Stderr = os.Stderr

		if err := chvCmd.Start(); err != nil {
			virtiofsdCmd.Process.Kill()
			virtiofsdCmd.Wait()
			os.Remove(virtiofsSock)
			return nil, fmt.Errorf("start cloud-hypervisor: %w", err)
		}
		log.Printf("cloud-hypervisor started with PID %d", chvCmd.Process.Pid)

		// Monitor cloud-hypervisor in background
		go func() {
			chvCmd.Wait()
			log.Printf("cloud-hypervisor exited")
			close(session.done)
		}()

		// Copy stdin to cloud-hypervisor and detect when it closes
		go func() {
			io.Copy(stdinPipe, cfg.Stdin)
			log.Printf("stdin closed")
			close(session.stdinClosed)
		}()
	}

	return session, nil
}

// Wait blocks until the VM exits.
func (s *VMSession) Wait() error {
	<-s.done
	return nil
}

// Done returns a channel that is closed when the VM exits.
func (s *VMSession) Done() <-chan struct{} {
	return s.done
}

// StdinClosed returns a channel that is closed when stdin reaches EOF.
func (s *VMSession) StdinClosed() <-chan struct{} {
	return s.stdinClosed
}

// Close terminates the VM session and cleans up resources.
func (s *VMSession) Close() error {
	log.Printf("Closing VM session, killing cloud-hypervisor PID %d", s.chvCmd.Process.Pid)

	// Close PTY if we have one (this will help unblock io.Copy goroutines)
	if s.pty != nil {
		s.pty.Close()
	}

	// Kill cloud-hypervisor
	if err := s.chvCmd.Process.Kill(); err != nil {
		log.Printf("Warning: failed to kill cloud-hypervisor: %v", err)
	}

	// Wait for it to actually exit
	<-s.done
	log.Printf("cloud-hypervisor has exited")

	// Kill virtiofsd (it may have already exited when cloud-hypervisor disconnected)
	log.Printf("Killing virtiofsd PID %d", s.virtiofsdCmd.Process.Pid)
	s.virtiofsdCmd.Process.Kill()
	s.virtiofsdCmd.Wait()
	log.Printf("virtiofsd has exited")

	// Clean up socket
	os.Remove(s.virtiofsSock)
	log.Printf("Cleaned up socket %s", s.virtiofsSock)

	return nil
}
