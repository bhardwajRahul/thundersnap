// Command thunderboot-logrelay mirrors thundersnapd's ordinary stdout/stderr to
// the host over virtio-vsock and to the serial console. It is intentionally a
// tiny transport helper: thundersnapd keeps its normal Go logging and does not
// know anything about Aperture's protocol.
//
// As of fix #1 (container-init-wedged-plan2.md) the relay is no longer the VM's
// PID 1. thunderboot-init stays PID 1, spawns thundersnapd and this relay, and
// reaps orphaned descendants itself (the root-cause fix for the
// permanent-wedge bug). The relay is now a fire-and-forget transport child:
// PID 1 pipes thundersnapd's stdout/stderr into the relay's stdin, and the relay
// mirrors each line to the host (vsock) and to its own stdout (the serial
// console). If the relay dies, thundersnapd's log writes get EPIPE (Go ignores
// SIGPIPE on fd 1/2) and the daemon keeps running; PID 1 reaps the dead relay
// and carries on. The relay owns no children and exits 0 when stdin EOFs.
package main

import (
	"bufio"
	"bytes"
	"io"
	"os"
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
	// stdin is the pipe that PID 1 (thunderboot-init) connected to thundersnapd's
	// stdout/stderr. Mirror every line to the host over vsock and to this
	// process's own stdout (the serial console, inherited from PID 1).
	s := &sender{}
	reader := bufio.NewReaderSize(os.Stdin, 64*1024)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) != 0 {
			_, _ = os.Stdout.Write(line)
			s.write(bytes.Clone(line))
		}
		if err != nil {
			break
		}
	}
}
