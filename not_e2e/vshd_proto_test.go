// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

package e2e

import (
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/tailscale/thundersnap/vshdproto"
)

// writeVshdHeader writes the null-delimited vshd request header. When framePath
// is non-empty it emits the VMX form; pty signals a terminal session. This
// mirrors the daemon's writeVshdRequest.
func writeVshdHeader(conn net.Conn, framePath, user string, pty bool, args []string) error {
	if framePath != "" {
		if _, err := fmt.Fprintf(conn, "VMX\x00%s\x00", framePath); err != nil {
			return err
		}
	}
	ptyFlag := "0"
	if pty {
		ptyFlag = "1"
	}
	if _, err := fmt.Fprintf(conn, "%s\x00%s\x00%d\x00", user, ptyFlag, len(args)); err != nil {
		return err
	}
	for _, arg := range args {
		if _, err := fmt.Fprintf(conn, "%s\x00", arg); err != nil {
			return err
		}
	}
	return nil
}

// vshdResult holds the collected output of a vshd session over the TLV stream.
type vshdResult struct {
	stdout   string
	stderr   string
	exitCode int32
	exited   bool
}

// sendVshdStdin frames stdin as a single FrameStdin (if non-empty).
func sendVshdStdin(conn net.Conn, stdin string) error {
	if stdin == "" {
		return nil
	}
	return vshdproto.WriteFrame(conn, vshdproto.FrameStdin, []byte(stdin))
}

// readVshdSession drains a vshd connection's TLV frames until EOF, accumulating
// stdout/stderr and the exit code. Unknown frame types are ignored.
func readVshdSession(conn net.Conn) (vshdResult, error) {
	var r vshdResult
	for {
		typ, payload, err := vshdproto.ReadFrame(conn)
		if err != nil {
			if err == io.EOF {
				return r, nil
			}
			return r, err
		}
		switch typ {
		case vshdproto.FrameStdout:
			r.stdout += string(payload)
		case vshdproto.FrameStderr:
			r.stderr += string(payload)
		case vshdproto.FrameExit:
			if code, derr := vshdproto.DecodeExit(payload); derr == nil {
				r.exitCode = code
				r.exited = true
			}
		}
	}
}

// runVshdCommand runs a non-PTY command over vshd and returns combined
// stdout+stderr (the historical behaviour of the old raw helpers, which merged
// the two). framePath selects VMX container mode when non-empty.
func runVshdCommand(vsockSock, framePath, user, stdin string, args ...string) (string, error) {
	conn, err := dialVsock(vsockSock, 5222)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	if err := writeVshdHeader(conn, framePath, user, false, args); err != nil {
		return "", err
	}
	if err := sendVshdStdin(conn, stdin); err != nil {
		return "", err
	}
	// NOTE: we deliberately do NOT half-close the write side here. Over the
	// cloud-hypervisor vsock proxy a CloseWrite tears down the whole
	// connection, which kills the guest command with SIGPIPE before it
	// finishes writing its output. The guest's stdin reader instead sees EOF
	// when the command exits and the connection is closed below.

	res, err := readVshdSession(conn)
	if err != nil {
		return "", err
	}
	return res.stdout + res.stderr, nil
}

// vshdPTYSession is an open PTY session over vshd. The host has signalled PTY
// mode with a leading FrameWinsize; callers feed stdin and winsize frames and
// read the accumulated guest output.
type vshdPTYSession struct {
	conn net.Conn
}

// startVshdPTY opens a PTY session: it writes the request header with pty=true,
// then sends the initial FrameWinsize that signals PTY mode to the guest. The
// command args run inside the allocated pty.
func startVshdPTY(vsockSock, framePath, user string, ws vshdproto.Winsize, args ...string) (*vshdPTYSession, error) {
	conn, err := dialVsock(vsockSock, 5222)
	if err != nil {
		return nil, err
	}
	if err := writeVshdHeader(conn, framePath, user, true, args); err != nil {
		conn.Close()
		return nil, err
	}
	// The leading FrameWinsize signals PTY mode and sizes the pty.
	if err := vshdproto.WriteFrame(conn, vshdproto.FrameWinsize, vshdproto.EncodeWinsize(ws)); err != nil {
		conn.Close()
		return nil, err
	}
	return &vshdPTYSession{conn: conn}, nil
}

// sendStdin frames s as host->guest terminal input.
func (p *vshdPTYSession) sendStdin(s string) error {
	return vshdproto.WriteFrame(p.conn, vshdproto.FrameStdin, []byte(s))
}

// resize sends a mid-session FrameWinsize.
func (p *vshdPTYSession) resize(ws vshdproto.Winsize) error {
	return vshdproto.WriteFrame(p.conn, vshdproto.FrameWinsize, vshdproto.EncodeWinsize(ws))
}

// readUntil reads guest output frames until the accumulated stdout contains
// marker or the deadline elapses. Returns the accumulated stdout.
func (p *vshdPTYSession) readUntil(marker string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var out string
	for {
		p.conn.SetReadDeadline(deadline)
		typ, payload, err := vshdproto.ReadFrame(p.conn)
		if err != nil {
			if err == io.EOF {
				return out, nil
			}
			return out, err
		}
		switch typ {
		case vshdproto.FrameStdout, vshdproto.FrameStderr:
			out += string(payload)
		}
		if marker != "" && strings.Contains(out, marker) {
			return out, nil
		}
		if time.Now().After(deadline) {
			return out, fmt.Errorf("timeout waiting for %q; got %q", marker, out)
		}
	}
}

// close shuts down the session connection.
func (p *vshdPTYSession) close() error {
	return p.conn.Close()
}
