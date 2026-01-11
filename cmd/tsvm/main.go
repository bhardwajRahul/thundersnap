// tsvm is a test tool for spinning up a VM with a thundersnap filesystem.
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/tailscale/thundersnap/thundersnap"
)

func main() {
	vmDir := flag.String("vm-dir", "", "Directory containing cloud-hypervisor and vmlinux (required)")
	rootFS := flag.String("root", "", "Path to root filesystem (required)")
	flag.Parse()

	if *vmDir == "" {
		log.Fatal("-vm-dir is required")
	}
	if *rootFS == "" {
		log.Fatal("-root is required")
	}

	// Handle signals for clean shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Start the VM - always use PTY since cloud-hypervisor's --serial tty needs it
	session, err := thundersnap.StartVM(thundersnap.VMConfig{
		RootFS: *rootFS,
		VMDir:  *vmDir,
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		UsePTY: true,
	})
	if err != nil {
		log.Fatalf("Failed to start VM: %v", err)
	}

	log.Printf("VM started, press Ctrl-C or Ctrl-D to exit")

	// Wait for either VM to exit, signal, or stdin to close
	select {
	case <-session.Done():
		log.Printf("VM exited on its own")
	case <-sigCh:
		log.Printf("Received signal, shutting down VM")
		session.Close()
	case <-session.StdinClosed():
		log.Printf("Stdin closed, shutting down VM")
		session.Close()
	}

	log.Printf("tsvm exiting")
}
