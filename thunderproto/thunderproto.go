// Package thunderproto holds the wire constants and handshake helpers shared by
// the thundersnap daemon (the control server) and the in-container `ts` client.
//
// The control protocol (snap, ping, ref/frame management, etc.) is plain HTTP/1,
// but it is reached over two transports: a real cloud-hypervisor vsock for VM/VMX
// frames, and a Unix socket for plain container frames. Cloud-hypervisor's vsock
// requires a "CONNECT <port>\n" / "OK <port>\n" text handshake before the byte
// stream begins, so the Unix-socket path emulates the same handshake. Keeping the
// port number and the handshake framing in one place stops the daemon and client
// copies from drifting apart.
package thunderproto

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Port is the vsock port used for the thunder control protocol. It is one past
// VshPort (5222) so the two protocols do not collide on the same vsock CID. Both
// the daemon's CONNECT-handshake check and the client's CONNECT request use this
// value, so they cannot disagree.
const Port = 5223

// WriteClientHandshake performs the client side of the emulated vsock handshake
// over a Unix socket: it sends "CONNECT <Port>\n" and reads the server's reply,
// which must begin with "OK". The provided reader must wrap the same connection
// so any data buffered past the handshake line is not lost; callers should keep
// reading subsequent bytes from r, not the raw connection.
func WriteClientHandshake(w io.Writer, r *bufio.Reader) error {
	if _, err := fmt.Fprintf(w, "CONNECT %d\n", Port); err != nil {
		return fmt.Errorf("send CONNECT: %w", err)
	}
	response, err := r.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read handshake response: %w", err)
	}
	response = strings.TrimSpace(response)
	if !strings.HasPrefix(response, "OK") {
		return fmt.Errorf("handshake failed: %s", response)
	}
	return nil
}

// ReadServerHandshake performs the server side of the emulated vsock handshake:
// it reads the "CONNECT <Port>\n" line from r, validates the port against Port,
// and on success writes "OK <Port>\n" back to w. On a malformed line or wrong
// port it writes an "ERROR ...\n" reply and returns an error so the caller can
// drop the connection. The line text is returned for logging on error.
func ReadServerHandshake(w io.Writer, r *bufio.Reader) error {
	line, err := r.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read handshake: %w", err)
	}

	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "CONNECT ") {
		fmt.Fprintf(w, "ERROR invalid handshake\n")
		return fmt.Errorf("invalid handshake: %s", line)
	}
	portStr := strings.TrimPrefix(line, "CONNECT ")
	port, err := strconv.Atoi(portStr)
	if err != nil || port != Port {
		fmt.Fprintf(w, "ERROR invalid port\n")
		return fmt.Errorf("invalid port: %s", portStr)
	}

	fmt.Fprintf(w, "OK %d\n", port)
	return nil
}
