package main

import (
	"io"
	"log"
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
	"github.com/mdlayher/vsock"
)

const vsockPort = 5222

func handleConnection(conn *vsock.Conn) {
	defer conn.Close()

	cmd := exec.Command("/bin/sh", "-l")
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		log.Printf("failed to start pty: %v", err)
		return
	}
	defer ptmx.Close()

	// Copy data between vsock and pty
	done := make(chan struct{}, 2)

	go func() {
		io.Copy(ptmx, conn)
		done <- struct{}{}
	}()

	go func() {
		io.Copy(conn, ptmx)
		done <- struct{}{}
	}()

	<-done
	cmd.Process.Signal(syscall.SIGHUP)
	cmd.Wait()
}

func main() {
	l, err := vsock.Listen(vsockPort, nil)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	defer l.Close()

	log.Printf("vshd listening on vsock port %d", vsockPort)

	for {
		conn, err := l.Accept()
		if err != nil {
			log.Printf("accept error: %v", err)
			continue
		}

		go handleConnection(conn.(*vsock.Conn))
	}
}
