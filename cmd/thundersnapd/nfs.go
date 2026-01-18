// NFS server implementation for thundersnapd.
// Provides NFSv3 server with maximal caching for high-speed file access.
package main

import (
	"context"
	"fmt"
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

	return &nfsServer{
		listener: listener,
		handler:  cachingHandler,
	}, nil
}
