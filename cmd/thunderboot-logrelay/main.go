// Command thunderboot-logrelay runs the appliance daemon and mirrors its
// ordinary stdout/stderr to the host over virtio-vsock. It is intentionally a
// tiny transport helper: thundersnapd keeps its normal Go logging and does not
// know anything about Aperture's protocol.
package main

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/mdlayher/vsock"
)

const (
	hostCID     = 2
	controlPort = 5230
)

type sender struct {
	mu   sync.Mutex
	conn io.ReadWriteCloser
}

func (s *sender) write(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		conn, err := vsock.Dial(hostCID, controlPort, nil)
		if err != nil {
			return // The serial console remains the authoritative fallback.
		}
		s.conn = conn
	}
	if _, err := s.conn.Write(data); err != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
}

func main() {
	if len(os.Args) < 2 {
		_, _ = os.Stderr.WriteString("usage: thunderboot-logrelay COMMAND [ARGS...]\n")
		os.Exit(2)
	}

	cmd := exec.Command(os.Args[1], os.Args[2:]...)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		panic(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		panic(err)
	}

	s := &sender{}
	reader := bufio.NewReaderSize(pipe, 64*1024)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) != 0 {
			_, _ = os.Stdout.Write(line)
			s.write(bytes.Clone(line))
		}
		if readErr != nil {
			break
		}
	}
	if err := cmd.Wait(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			os.Exit(exit.ExitCode())
		}
		panic(err)
	}
}
