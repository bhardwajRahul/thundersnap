// NFS server implementation for thundersnapd.
// Provides NFSv3 server with maximal caching for high-speed file access.
package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	nfs "github.com/willscott/go-nfs"
	nfshelper "github.com/willscott/go-nfs/helpers"
)

// billyHandler implements nfs.Handler using go-billy filesystem.
type billyHandler struct {
	fs billy.Filesystem
}

// Mount returns the root billy.Filesystem for the given connection.
func (h *billyHandler) Mount(ctx context.Context, conn net.Conn, req nfs.MountRequest) (nfs.MountStatus, billy.Filesystem, []nfs.AuthFlavor) {
	return nfs.MountStatusOk, h.fs, []nfs.AuthFlavor{nfs.AuthFlavorNull}
}

// Change returns the billy.Change interface for the filesystem.
// This enables write operations (chmod, chown, chtimes).
func (h *billyHandler) Change(fs billy.Filesystem) billy.Change {
	if c, ok := fs.(billy.Change); ok {
		return c
	}
	return nil
}

// FSStat returns filesystem statistics.
func (h *billyHandler) FSStat(ctx context.Context, fs billy.Filesystem, req *nfs.FSStat) error {
	// Return large values to indicate no limits
	req.TotalSize = 1 << 60   // ~1 exabyte
	req.FreeSize = 1 << 60
	req.AvailableSize = 1 << 60
	req.TotalFiles = 1 << 30  // ~1 billion files
	req.FreeFiles = 1 << 30
	req.AvailableFiles = 1 << 30
	req.CacheHint = 0 // No cache policy hint
	return nil
}

// ToHandle converts a filesystem path to an opaque file handle.
func (h *billyHandler) ToHandle(fs billy.Filesystem, path []string) []byte {
	// Use CachingHandler for this
	return nil
}

// FromHandle converts an opaque file handle back to a filesystem path.
func (h *billyHandler) FromHandle(fh []byte) (billy.Filesystem, []string, error) {
	// Use CachingHandler for this
	return nil, nil, fmt.Errorf("not implemented")
}

// InvalidateHandle invalidates a cached handle.
func (h *billyHandler) InvalidateHandle(fs billy.Filesystem, fh []byte) error {
	return nil
}

// HandleLimit returns the maximum number of handles to cache.
func (h *billyHandler) HandleLimit() int {
	// Large number for maximal caching
	return 1000000
}

// nfsServer wraps the NFS server listener for control.
type nfsServer struct {
	listener net.Listener
	handler  nfs.Handler
}

// Serve starts serving NFS requests.
func (s *nfsServer) Serve() error {
	return nfs.Serve(s.listener, s.handler)
}

// nfsVersionFilterListener wraps a listener to reject NFSv4 requests.
// This makes Linux clients fall back to NFSv3 gracefully instead of failing with EIO.
type nfsVersionFilterListener struct {
	net.Listener
}

func (l *nfsVersionFilterListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &nfsVersionFilterConn{Conn: conn}, nil
}

// nfsVersionFilterConn wraps a connection to intercept and reject NFSv4 RPC calls.
type nfsVersionFilterConn struct {
	net.Conn
	buf []byte // buffer for peeked data that needs to be re-read
}

func (c *nfsVersionFilterConn) Read(b []byte) (int, error) {
	// If we have buffered data from a previous peek, return that first
	if len(c.buf) > 0 {
		n := copy(b, c.buf)
		c.buf = c.buf[n:]
		return n, nil
	}

	// Read the RPC record marker (4 bytes)
	var fragHeader [4]byte
	if _, err := io.ReadFull(c.Conn, fragHeader[:]); err != nil {
		return 0, err
	}
	fragLen := binary.BigEndian.Uint32(fragHeader[:]) & 0x7FFFFFFF

	// Read enough of the message to check the NFS version
	// RPC header: xid(4) + msgType(4) + rpcVers(4) + prog(4) + vers(4) = 20 bytes minimum
	if fragLen < 20 {
		// Too short to be a valid RPC call, pass through
		c.buf = append(fragHeader[:], make([]byte, fragLen)...)
		if _, err := io.ReadFull(c.Conn, c.buf[4:]); err != nil {
			return 0, err
		}
		n := copy(b, c.buf)
		c.buf = c.buf[n:]
		return n, nil
	}

	// Read the full fragment
	msg := make([]byte, fragLen)
	if _, err := io.ReadFull(c.Conn, msg); err != nil {
		return 0, err
	}

	// Parse RPC header to check for NFSv4
	xid := binary.BigEndian.Uint32(msg[0:4])
	msgType := binary.BigEndian.Uint32(msg[4:8])
	prog := binary.BigEndian.Uint32(msg[12:16])
	vers := binary.BigEndian.Uint32(msg[16:20])

	// Check if this is an NFSv4 CALL (program 100003, version 4)
	if msgType == 0 && prog == 100003 && vers == 4 {
		log.Printf("NFS: rejecting NFSv4 request (xid=%d) with PROG_MISMATCH, supported versions 3-3", xid)
		// Send PROG_MISMATCH response to trigger client fallback to NFSv3
		resp := makeProgMismatch(xid, 3, 3) // low=3, high=3 (only NFSv3 supported)
		c.Conn.Write(resp)
		// Continue reading the next request
		return c.Read(b)
	}

	// Check if this is an NFS_ACL CALL (program 100227) - not supported
	if msgType == 0 && prog == 100227 {
		log.Printf("NFS: rejecting NFS_ACL request (xid=%d) with PROG_UNAVAIL", xid)
		resp := makeProgUnavail(xid)
		c.Conn.Write(resp)
		// Continue reading the next request
		return c.Read(b)
	}

	// Supported request, pass through normally
	c.buf = append(fragHeader[:], msg...)
	n := copy(b, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

// makeProgMismatch creates an RPC PROG_MISMATCH response with version range.
// This tells the client which versions are supported and triggers version fallback.
func makeProgMismatch(xid uint32, lowVers, highVers uint32) []byte {
	// RPC reply format for PROG_MISMATCH:
	// record marker (4) + xid (4) + REPLY=1 (4) + MSG_ACCEPTED=0 (4) +
	// verf_flavor=0 (4) + verf_len=0 (4) + accept_stat=PROG_MISMATCH=2 (4) +
	// mismatch_info: low (4) + high (4)
	resp := make([]byte, 4+32)

	// Record marker with last-fragment bit set
	binary.BigEndian.PutUint32(resp[0:4], 32|0x80000000)

	binary.BigEndian.PutUint32(resp[4:8], xid)
	binary.BigEndian.PutUint32(resp[8:12], 1)      // REPLY
	binary.BigEndian.PutUint32(resp[12:16], 0)     // MSG_ACCEPTED
	binary.BigEndian.PutUint32(resp[16:20], 0)     // AUTH_NULL
	binary.BigEndian.PutUint32(resp[20:24], 0)     // verf length 0
	binary.BigEndian.PutUint32(resp[24:28], 2)     // PROG_MISMATCH
	binary.BigEndian.PutUint32(resp[28:32], lowVers)
	binary.BigEndian.PutUint32(resp[32:36], highVers)

	return resp
}

// makeProgUnavail creates an RPC PROG_UNAVAIL response.
// This tells the client that the requested program is not available.
func makeProgUnavail(xid uint32) []byte {
	// RPC reply format for PROG_UNAVAIL:
	// record marker (4) + xid (4) + REPLY=1 (4) + MSG_ACCEPTED=0 (4) +
	// verf_flavor=0 (4) + verf_len=0 (4) + accept_stat=PROG_UNAVAIL=1 (4)
	resp := make([]byte, 4+24)

	// Record marker with last-fragment bit set
	binary.BigEndian.PutUint32(resp[0:4], 24|0x80000000)

	binary.BigEndian.PutUint32(resp[4:8], xid)
	binary.BigEndian.PutUint32(resp[8:12], 1)  // REPLY
	binary.BigEndian.PutUint32(resp[12:16], 0) // MSG_ACCEPTED
	binary.BigEndian.PutUint32(resp[16:20], 0) // AUTH_NULL
	binary.BigEndian.PutUint32(resp[20:24], 0) // verf length 0
	binary.BigEndian.PutUint32(resp[24:28], 1) // PROG_UNAVAIL

	return resp
}

// Close stops the NFS server.
func (s *nfsServer) Close() error {
	return s.listener.Close()
}

// startNFSServer starts the NFSv3 server on the provided listener.
func startNFSServer(root string, listener net.Listener) (*nfsServer, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolving root path: %w", err)
	}

	// Ensure root exists
	if _, err := os.Stat(absRoot); err != nil {
		return nil, fmt.Errorf("root directory: %w", err)
	}

	// Create go-billy filesystem for the root directory
	fs := osfs.New(absRoot)

	// Create handler with caching wrapper for optimal performance
	handler := &billyHandler{fs: fs}
	cachingHandler := nfshelper.NewCachingHandler(handler, handler.HandleLimit())

	log.Printf("NFSv3 server exporting %s", absRoot)

	// Wrap listener to reject NFSv4 requests with PROG_MISMATCH,
	// which triggers Linux clients to fall back to NFSv3 gracefully.
	filteredListener := &nfsVersionFilterListener{Listener: listener}

	return &nfsServer{
		listener: filteredListener,
		handler:  cachingHandler,
	}, nil
}
