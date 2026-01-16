package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"syscall"

	"github.com/creack/pty"
	"github.com/mdlayher/vsock"
)

const vsockPort = 5222

var connectionID uint64

func handleConnection(conn *vsock.Conn) {
	id := atomic.AddUint64(&connectionID, 1)
	log.Printf("[conn %d] new connection from %v", id, conn.RemoteAddr())
	defer func() {
		conn.Close()
		log.Printf("[conn %d] connection closed", id)
	}()

	// Read the command protocol:
	// First: argument count terminated by \0 (0 = interactive shell)
	// Then: each argument terminated by \0
	reader := bufio.NewReader(conn)
	countStr, err := reader.ReadString('\x00')
	if err != nil {
		log.Printf("[conn %d] failed to read arg count: %v", id, err)
		return
	}
	argCount, err := strconv.Atoi(countStr[:len(countStr)-1])
	if err != nil {
		log.Printf("[conn %d] invalid arg count %q: %v", id, countStr, err)
		return
	}

	var cmdArgs []string
	for i := 0; i < argCount; i++ {
		arg, err := reader.ReadString('\x00')
		if err != nil {
			log.Printf("[conn %d] failed to read arg %d: %v", id, i, err)
			return
		}
		cmdArgs = append(cmdArgs, arg[:len(arg)-1])
	}

	if argCount > 0 {
		log.Printf("[conn %d] running command: %v", id, cmdArgs)
		runCommand(id, conn, reader, cmdArgs)
	} else {
		log.Printf("[conn %d] starting interactive shell", id)
		runInteractiveShell(id, conn, reader)
	}
}

// runInteractiveShell spawns an interactive shell with PTY
func runInteractiveShell(id uint64, conn *vsock.Conn, reader *bufio.Reader) {
	cmd := exec.Command("/bin/sh", "-l")
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		log.Printf("[conn %d] failed to start pty: %v", id, err)
		return
	}
	defer ptmx.Close()
	log.Printf("[conn %d] shell started with PID %d", id, cmd.Process.Pid)

	// Copy data between vsock and pty
	done := make(chan struct{}, 2)

	go func() {
		n, err := io.Copy(ptmx, reader)
		log.Printf("[conn %d] stdin copy ended: %d bytes, err=%v", id, n, err)
		done <- struct{}{}
	}()

	go func() {
		n, err := io.Copy(conn, ptmx)
		log.Printf("[conn %d] stdout copy ended: %d bytes, err=%v", id, n, err)
		done <- struct{}{}
	}()

	<-done
	log.Printf("[conn %d] signaling shell to exit", id)
	cmd.Process.Signal(syscall.SIGHUP)
	cmd.Wait()
	log.Printf("[conn %d] shell exited", id)
}

// runCommand executes a command without PTY and exits when done
func runCommand(id uint64, conn *vsock.Conn, reader *bufio.Reader, cmdArgs []string) {
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Env = os.Environ()

	// Set up pipes for stdin/stdout/stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("[conn %d] failed to create stdin pipe: %v", id, err)
		fmt.Fprintf(conn, "vshd: failed to create stdin pipe: %v\n", err)
		return
	}
	cmd.Stdout = conn
	cmd.Stderr = conn

	if err := cmd.Start(); err != nil {
		log.Printf("[conn %d] failed to start command: %v", id, err)
		fmt.Fprintf(conn, "vshd: %v\n", err)
		return
	}
	log.Printf("[conn %d] command started with PID %d", id, cmd.Process.Pid)

	// Copy stdin in background
	go func() {
		io.Copy(stdin, reader)
		stdin.Close()
	}()

	// Wait for command to complete
	err = cmd.Wait()
	if err != nil {
		log.Printf("[conn %d] command exited with error: %v", id, err)
	} else {
		log.Printf("[conn %d] command exited successfully", id)
	}
}

func main() {
	log.Printf("vshd starting up")

	l, err := vsock.Listen(vsockPort, nil)
	if err != nil {
		log.Fatalf("failed to listen on vsock port %d: %v", vsockPort, err)
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
