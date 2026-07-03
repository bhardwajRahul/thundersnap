// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// vsh connects to a VM's vsh daemon over a cloud-hypervisor vsock unix socket
// and bridges the local terminal to the guest shell. It dials the socket given
// as argv[1], performs the cloud-hypervisor "CONNECT <port>" handshake, puts the
// local terminal into raw mode (when stdin is a TTY), and then copies bytes in
// both directions until the guest side closes.
package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"

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

	// Read the "OK <port>\n" handshake reply. We assume the whole line arrives
	// in a single read (true in practice: cloud-hypervisor writes it as one
	// short message before any shell output) and that no shell bytes are
	// consumed here prematurely.
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		log.Fatalf("failed to read response: %v", err)
	}
	response := strings.TrimSpace(string(buf[:n]))
	if !strings.HasPrefix(response, "OK") {
		log.Fatalf("vsock connection failed: %s", response)
	}

	// Put terminal in raw mode if stdin is a tty.
	if term.IsTerminal(int(os.Stdin.Fd())) {
		oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			log.Fatalf("failed to set raw mode: %v", err)
		}
		defer term.Restore(int(os.Stdin.Fd()), oldState)
	}

	done := make(chan struct{})

	// stdin -> vsock. This goroutine is intentionally not awaited: when the
	// guest side closes (the vsock->stdout copy below returns), we exit
	// immediately and any stdin still buffered here is discarded.
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
