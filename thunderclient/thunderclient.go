// Package thunderclient is the client transport for talking to thundersnapd's
// control protocol. The protocol is plain HTTP/1 carried over either a real
// cloud-hypervisor vsock (inside a VM) or a Unix socket with an emulated vsock
// handshake (inside a container); see package thunderproto for the wire details.
//
// Callers obtain an *http.Client from NewHTTPClient and issue requests to
// "http://localhost/<path>" (the host part is ignored), or use the PostJSON
// helper for the common marshal/POST/decode round trip.
package thunderclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/mdlayher/vsock"
	"github.com/tailscale/thundersnap/thunderproto"
)

// hostCID is the vsock context ID of the host, used when dialing from inside a VM.
const hostCID = 2

// inVM reports whether we are running inside a VM with vsock support.
func inVM() bool {
	_, err := os.Stat("/dev/vsock")
	return err == nil
}

// Dial connects to thundersnapd and performs the vsock handshake. In VMs (when
// /dev/vsock exists) it connects directly via vsock to the host; in containers
// it connects to the Unix socket at sockPath and emulates the CONNECT/OK
// handshake over it.
func Dial(ctx context.Context, sockPath string) (net.Conn, error) {
	if inVM() {
		// In a VM: connect directly via vsock to the host. vsock connections do
		// not need the CONNECT handshake — they are already connected to the
		// right port, which the host receives as a direct connection on the
		// port-specific Unix socket.
		conn, err := vsock.Dial(hostCID, thunderproto.Port, nil)
		if err != nil {
			return nil, fmt.Errorf("vsock dial: %w", err)
		}
		return conn, nil
	}

	// In a container: connect to the Unix socket with the CONNECT handshake.
	return dialUnix(sockPath)
}

// dialUnix connects to the control Unix socket at sockPath and performs the
// emulated vsock CONNECT/OK handshake, returning a connection that reads through
// the handshake's buffered reader so no buffered bytes are lost.
func dialUnix(sockPath string) (net.Conn, error) {
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return nil, err
	}

	reader := bufio.NewReader(conn)
	if err := thunderproto.WriteClientHandshake(conn, reader); err != nil {
		conn.Close()
		return nil, err
	}

	return &bufferedConn{Conn: conn, reader: reader}, nil
}

// bufferedConn wraps a net.Conn with a buffered reader for the handshake.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

// NewHTTPClient returns an *http.Client whose transport dials thundersnapd's
// control socket via Dial (vsock in a VM, the unix socket otherwise). The host
// part of any request URL is ignored; use "http://localhost/<path>".
func NewHTTPClient(sockPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return Dial(ctx, sockPath)
			},
		},
	}
}

// PostJSON marshals req as JSON, POSTs it to path on the thunder control socket,
// and decodes the JSON response into a value of type Resp. path is the URL path
// only (e.g. "/delete-snap"); the localhost host is supplied here. Status/error
// fields are response-specific, so callers inspect the returned Resp themselves.
func PostJSON[Req any, Resp any](sockPath, path string, req Req) (Resp, error) {
	var result Resp

	body, err := json.Marshal(req)
	if err != nil {
		return result, fmt.Errorf("marshal request: %w", err)
	}

	client := NewHTTPClient(sockPath)
	resp, err := client.Post("http://localhost"+path, "application/json", bytes.NewReader(body))
	if err != nil {
		return result, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return result, fmt.Errorf("parse response: %w", err)
	}
	return result, nil
}
