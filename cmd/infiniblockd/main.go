// Command infiniblockd runs an NBD server backed by a sparse file.
//
// Usage:
//
//	infiniblockd --backing /path/to/backing.sparse [--addr :10809]
//
// The backing file is created if it doesn't exist. It's a sparse file
// that only allocates disk space for blocks that are actually written.
// The NBD export advertises 15 TiB of virtual space by default
// (see infiniblock.DefaultExportSize; busybox nbd-client compatibility).
//
// TRIM commands from the client punch holes in the backing file to
// reclaim disk space.
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/pborman/getopt/v2"
	"github.com/tailscale/thundersnap/infiniblock"
	"github.com/tailscale/thundersnap/version"
)

var (
	backing     = getopt.StringLong("backing", 'b', "", "path to backing sparse file (required)")
	addr        = getopt.StringLong("addr", 'a', ":10809", "address to listen on")
	exportSize  = getopt.Uint64Long("size", 's', infiniblock.DefaultExportSize, "export size in bytes (default 15 TiB)")
	help        = getopt.BoolLong("help", 'h', "show help")
	showVersion = getopt.BoolLong("version", 0, "print version and exit")
)

func main() {
	getopt.Parse()

	if *showVersion {
		fmt.Printf("infiniblockd %s\n", version.String())
		os.Exit(0)
	}

	if *help {
		getopt.Usage()
		os.Exit(0)
	}

	if *backing == "" {
		getopt.Usage()
		log.Fatal("--backing is required")
	}

	backend, err := infiniblock.OpenSparseFile(*backing)
	if err != nil {
		log.Fatalf("open backing file: %v", err)
	}
	defer backend.Close()

	server := infiniblock.NewServer(infiniblock.ServerConfig{
		Backend:    backend,
		ExportSize: *exportSize,
	})

	// Handle graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("shutting down...")
		server.Close()
	}()

	log.Printf("listening on %s, backing file: %s, export size: %d bytes", *addr, *backing, *exportSize)
	if err := server.ListenAndServe(*addr); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
