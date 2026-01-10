package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/term"
)

const vsockPort = 5222

func main() {
	if len(os.Args) < 2 {
		log.Fatalf("usage: %s <socket-path>", os.Args[0])
	}
	socketPath := os.Args[1]

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		log.Fatalf("failed to connect to %s: %v", socketPath, err)
	}
	defer conn.Close()

	// Cloud Hypervisor vsock protocol: send "CONNECT <port>\n"
	_, err = fmt.Fprintf(conn, "CONNECT %d\n", vsockPort)
	if err != nil {
		log.Fatalf("failed to send CONNECT: %v", err)
	}

	// Read response - should be "OK <port>\n"
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		log.Fatalf("failed to read response: %v", err)
	}
	response := strings.TrimSpace(string(buf[:n]))
	if !strings.HasPrefix(response, "OK") {
		log.Fatalf("vsock connection failed: %s", response)
	}

	// Put terminal in raw mode if stdin is a tty
	var oldState *term.State
	if term.IsTerminal(int(os.Stdin.Fd())) {
		oldState, err = term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			log.Fatalf("failed to set raw mode: %v", err)
		}
		defer term.Restore(int(os.Stdin.Fd()), oldState)

		// Handle window size changes
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGWINCH)
		go func() {
			for range sigCh {
				// Could send window size to guest here
			}
		}()
	}

	done := make(chan struct{})

	// stdin -> vsock
	go func() {
		io.Copy(conn, os.Stdin)
	}()

	// vsock -> stdout
	go func() {
		io.Copy(os.Stdout, conn)
		close(done)
	}()

	<-done
}
