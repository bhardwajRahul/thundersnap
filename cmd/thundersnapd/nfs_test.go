package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"github.com/go-git/go-billy/v5/osfs"
	nfs "github.com/willscott/go-nfs"
	nfshelper "github.com/willscott/go-nfs/helpers"
	nfsc "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
)

// findFreePort finds an available TCP port.
func findFreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port, nil
}

// testNFSSetup creates a test NFS server and client.
type testNFSSetup struct {
	tmpDir   string
	port     int
	listener net.Listener
	client   *rpc.Client
	target   *nfsc.Target
}

func (s *testNFSSetup) Close() {
	if s.target != nil {
		s.target.Close()
	}
	if s.client != nil {
		s.client.Close()
	}
	if s.listener != nil {
		s.listener.Close()
	}
	if s.tmpDir != "" {
		os.RemoveAll(s.tmpDir)
	}
}

func setupNFSTest(t *testing.T) *testNFSSetup {
	t.Helper()
	setup := &testNFSSetup{}

	// Create a temporary directory as the NFS root
	var err error
	setup.tmpDir, err = os.MkdirTemp("", "nfs-test")
	if err != nil {
		t.Fatalf("creating temp dir: %v", err)
	}

	// Find a free port
	setup.port, err = findFreePort()
	if err != nil {
		setup.Close()
		t.Fatalf("finding free port: %v", err)
	}

	// Start the NFS server
	fs := osfs.New(setup.tmpDir)
	handler := &billyHandler{fs: fs}
	cachingHandler := nfshelper.NewCachingHandler(handler, 100000)

	addr := fmt.Sprintf(":%d", setup.port)
	setup.listener, err = net.Listen("tcp", addr)
	if err != nil {
		setup.Close()
		t.Fatalf("creating listener: %v", err)
	}

	// Run server in background
	go func() {
		nfs.Serve(setup.listener, cachingHandler)
	}()

	// Give the server a moment to start
	time.Sleep(100 * time.Millisecond)

	// Connect directly to the port (go-nfs multiplexes MOUNT and NFS)
	setup.client, err = nfsc.DialServiceAtPort("127.0.0.1", setup.port)
	if err != nil {
		setup.Close()
		t.Fatalf("dialing service: %v", err)
	}

	// Perform mount RPC to get root file handle
	auth := rpc.NewAuthUnix("test", 0, 0)

	type mountReq struct {
		rpc.Header
		Dirpath string
	}

	res, err := setup.client.Call(&mountReq{
		Header: rpc.Header{
			Rpcvers: 2,
			Prog:    nfsc.MountProg,
			Vers:    3, // MOUNT v3
			Proc:    1, // MNT
			Cred:    auth.Auth(),
			Verf:    rpc.AuthNull,
		},
		Dirpath: "/",
	})
	if err != nil {
		setup.Close()
		t.Fatalf("mount RPC: %v", err)
	}

	// Parse mount response
	var mountStatus uint32
	if _, err := res.Read((*[4]byte)(unsafe.Pointer(&mountStatus))[:4]); err != nil {
		setup.Close()
		t.Fatalf("reading mount status: %v", err)
	}
	if mountStatus != 0 {
		setup.Close()
		t.Fatalf("mount failed with status %d", mountStatus)
	}

	// Read file handle length and data
	var fhLen uint32
	if _, err := res.Read((*[4]byte)(unsafe.Pointer(&fhLen))[:4]); err != nil {
		setup.Close()
		t.Fatalf("reading fh length: %v", err)
	}
	// Convert from network byte order
	fhLen = (fhLen>>24) | ((fhLen>>8)&0xff00) | ((fhLen<<8)&0xff0000) | (fhLen<<24)

	fh := make([]byte, fhLen)
	if _, err := io.ReadFull(res, fh); err != nil {
		setup.Close()
		t.Fatalf("reading file handle: %v", err)
	}

	// Create Target with the file handle
	setup.target, err = nfsc.NewTargetWithClient(setup.client, auth.Auth(), fh, "/", time.Second*5)
	if err != nil {
		setup.Close()
		t.Fatalf("creating target: %v", err)
	}

	t.Logf("NFS test setup complete: port=%d, tmpDir=%s", setup.port, setup.tmpDir)
	return setup
}

// TestNFSServerClient tests the NFS server and client wired together.
func TestNFSServerClient(t *testing.T) {
	setup := setupNFSTest(t)
	defer setup.Close()

	// Create some test files
	testFile := filepath.Join(setup.tmpDir, "testfile.txt")
	testContent := []byte("Hello, NFS World!")
	if err := os.WriteFile(testFile, testContent, 0644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	// Create a subdirectory
	subDir := filepath.Join(setup.tmpDir, "subdir")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("creating subdir: %v", err)
	}

	// Test: Read file
	t.Run("read file", func(t *testing.T) {
		f, err := setup.target.Open("/testfile.txt")
		if err != nil {
			t.Fatalf("opening file: %v", err)
		}
		defer f.Close()

		data, err := io.ReadAll(f)
		if err != nil {
			t.Fatalf("reading file: %v", err)
		}
		if !bytes.Equal(data, testContent) {
			t.Errorf("content mismatch: got %q, want %q", string(data), string(testContent))
		}
	})

	// Test: List directory
	t.Run("list directory", func(t *testing.T) {
		entries, err := setup.target.ReadDirPlus("/")
		if err != nil {
			t.Fatalf("listing directory: %v", err)
		}

		foundFile := false
		foundDir := false
		for _, e := range entries {
			if e.FileName == "testfile.txt" {
				foundFile = true
			}
			if e.FileName == "subdir" {
				foundDir = true
			}
		}
		if !foundFile {
			t.Error("did not find testfile.txt in listing")
		}
		if !foundDir {
			t.Error("did not find subdir in listing")
		}
	})

	// Test: Get file info
	t.Run("get file info", func(t *testing.T) {
		attr, err := setup.target.Getattr("/testfile.txt")
		if err != nil {
			t.Fatalf("getting file info: %v", err)
		}
		if attr.Filesize != uint64(len(testContent)) {
			t.Errorf("size mismatch: got %d, want %d", attr.Filesize, len(testContent))
		}
	})

	// Test: Write file
	t.Run("write file", func(t *testing.T) {
		newContent := []byte("New content written via NFS!")

		// Create and write file
		f, err := setup.target.OpenFile("/newfile.txt", 0644)
		if err != nil {
			t.Fatalf("creating file: %v", err)
		}
		n, err := f.Write(newContent)
		if err != nil {
			f.Close()
			t.Fatalf("writing file: %v", err)
		}
		if n != len(newContent) {
			f.Close()
			t.Errorf("wrote %d bytes, expected %d", n, len(newContent))
		}
		f.Close()

		// Verify by reading back
		f, err = setup.target.Open("/newfile.txt")
		if err != nil {
			t.Fatalf("opening file for read: %v", err)
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			t.Fatalf("reading back file: %v", err)
		}
		if !bytes.Equal(data, newContent) {
			t.Errorf("content mismatch after write: got %q, want %q", string(data), string(newContent))
		}

		// Also verify on the local filesystem
		localContent, err := os.ReadFile(filepath.Join(setup.tmpDir, "newfile.txt"))
		if err != nil {
			t.Fatalf("reading local file: %v", err)
		}
		if !bytes.Equal(localContent, newContent) {
			t.Errorf("local content mismatch: got %q, want %q", string(localContent), string(newContent))
		}
	})

	// Test: Create directory
	t.Run("create directory", func(t *testing.T) {
		_, err := setup.target.Mkdir("/newdir", 0755)
		if err != nil {
			t.Fatalf("creating directory: %v", err)
		}

		// Verify it exists
		attr, err := setup.target.Getattr("/newdir")
		if err != nil {
			t.Fatalf("getting new directory info: %v", err)
		}
		if attr.Type != 2 { // 2 = directory in NFS
			t.Errorf("expected directory type 2, got %d", attr.Type)
		}

		// Verify on local filesystem
		localPath := filepath.Join(setup.tmpDir, "newdir")
		stat, err := os.Stat(localPath)
		if err != nil {
			t.Fatalf("stat local directory: %v", err)
		}
		if !stat.IsDir() {
			t.Error("local path is not a directory")
		}
	})

	// Test: Delete file
	t.Run("delete file", func(t *testing.T) {
		// First create a file to delete
		deleteFile := filepath.Join(setup.tmpDir, "todelete.txt")
		if err := os.WriteFile(deleteFile, []byte("delete me"), 0644); err != nil {
			t.Fatalf("creating file to delete: %v", err)
		}

		// Delete it
		if err := setup.target.Remove("/todelete.txt"); err != nil {
			t.Fatalf("deleting file: %v", err)
		}

		// Verify it's gone
		_, err := setup.target.Getattr("/todelete.txt")
		if err == nil {
			t.Error("expected error for deleted file, got none")
		}
	})

	// Test: Large file transfer
	t.Run("large file", func(t *testing.T) {
		// Create a 1MB file locally
		largeContent := make([]byte, 1024*1024)
		for i := range largeContent {
			largeContent[i] = byte(i % 256)
		}
		largePath := filepath.Join(setup.tmpDir, "largefile.bin")
		if err := os.WriteFile(largePath, largeContent, 0644); err != nil {
			t.Fatalf("writing large file: %v", err)
		}

		// Read it back via NFS
		f, err := setup.target.Open("/largefile.bin")
		if err != nil {
			t.Fatalf("opening large file: %v", err)
		}
		defer f.Close()

		data, err := io.ReadAll(f)
		if err != nil {
			t.Fatalf("reading large file: %v", err)
		}
		if len(data) != len(largeContent) {
			t.Errorf("read %d bytes, expected %d", len(data), len(largeContent))
		}
		if !bytes.Equal(data, largeContent) {
			t.Error("large file content mismatch")
		}
	})
}

// TestNFSConcurrentAccess tests concurrent read/write operations.
func TestNFSConcurrentAccess(t *testing.T) {
	setup := setupNFSTest(t)
	defer setup.Close()

	// Create multiple clients
	const numClients = 3
	const numOpsPerClient = 5

	errChan := make(chan error, numClients*numOpsPerClient*2)

	for i := 0; i < numClients; i++ {
		go func(clientID int) {
			// Each goroutine creates its own client connection
			client, err := nfsc.DialServiceAtPort("127.0.0.1", setup.port)
			if err != nil {
				errChan <- fmt.Errorf("client %d dial: %w", clientID, err)
				return
			}
			defer client.Close()

			// Perform mount to get root handle
			auth := rpc.NewAuthUnix(fmt.Sprintf("client%d", clientID), uint32(clientID), 0)

			type mountReq struct {
				rpc.Header
				Dirpath string
			}

			res, err := client.Call(&mountReq{
				Header: rpc.Header{
					Rpcvers: 2,
					Prog:    nfsc.MountProg,
					Vers:    3,
					Proc:    1,
					Cred:    auth.Auth(),
					Verf:    rpc.AuthNull,
				},
				Dirpath: "/",
			})
			if err != nil {
				errChan <- fmt.Errorf("client %d mount: %w", clientID, err)
				return
			}

			// Parse response (skip status, read fh)
			var buf [8]byte
			res.Read(buf[:4]) // status
			res.Read(buf[:4]) // fh len
			fhLen := (uint32(buf[0])<<24) | (uint32(buf[1])<<16) | (uint32(buf[2])<<8) | uint32(buf[3])
			fh := make([]byte, fhLen)
			io.ReadFull(res, fh)

			target, err := nfsc.NewTargetWithClient(client, auth.Auth(), fh, "/", time.Second*5)
			if err != nil {
				errChan <- fmt.Errorf("client %d target: %w", clientID, err)
				return
			}
			defer target.Close()

			for j := 0; j < numOpsPerClient; j++ {
				filename := fmt.Sprintf("/client%d_file%d.txt", clientID, j)
				content := []byte(fmt.Sprintf("Content from client %d, file %d", clientID, j))

				// Write
				f, err := target.OpenFile(filename, 0644)
				if err != nil {
					errChan <- fmt.Errorf("client %d create %s: %w", clientID, filename, err)
					continue
				}
				_, err = f.Write(content)
				f.Close()
				if err != nil {
					errChan <- fmt.Errorf("client %d write %s: %w", clientID, filename, err)
					continue
				}

				// Read back
				f, err = target.Open(filename)
				if err != nil {
					errChan <- fmt.Errorf("client %d open %s: %w", clientID, filename, err)
					continue
				}
				data, err := io.ReadAll(f)
				f.Close()
				if err != nil {
					errChan <- fmt.Errorf("client %d read %s: %w", clientID, filename, err)
					continue
				}

				if !bytes.Equal(data, content) {
					errChan <- fmt.Errorf("client %d content mismatch for %s", clientID, filename)
				}
			}
		}(i)
	}

	// Wait for all clients to finish (with timeout)
	timeout := time.After(30 * time.Second)
	expectedFiles := numClients * numOpsPerClient
	for {
		select {
		case err := <-errChan:
			t.Errorf("concurrent error: %v", err)
		case <-timeout:
			t.Fatal("test timed out")
			return
		default:
			// Check if files exist to know when we're done
			files, _ := os.ReadDir(setup.tmpDir)
			count := 0
			for _, f := range files {
				if !f.IsDir() {
					count++
				}
			}
			if count >= expectedFiles {
				// Give a bit more time for any pending errors
				time.Sleep(100 * time.Millisecond)
				// Drain any remaining errors
				for len(errChan) > 0 {
					err := <-errChan
					t.Errorf("concurrent error: %v", err)
				}
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// BenchmarkNFSRead benchmarks NFS read performance.
func BenchmarkNFSRead(b *testing.B) {
	// Create a temporary directory
	tmpDir, err := os.MkdirTemp("", "nfs-bench")
	if err != nil {
		b.Fatalf("creating temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create a test file
	testFile := filepath.Join(tmpDir, "benchmark.bin")
	content := make([]byte, 1024*1024) // 1MB
	for i := range content {
		content[i] = byte(i % 256)
	}
	if err := os.WriteFile(testFile, content, 0644); err != nil {
		b.Fatalf("writing test file: %v", err)
	}

	// Start NFS server
	port, _ := findFreePort()
	fs := osfs.New(tmpDir)
	handler := &billyHandler{fs: fs}
	cachingHandler := nfshelper.NewCachingHandler(handler, 100000)

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		b.Fatalf("creating listener: %v", err)
	}
	defer listener.Close()

	go nfs.Serve(listener, cachingHandler)
	time.Sleep(100 * time.Millisecond)

	// Connect client
	client, err := nfsc.DialServiceAtPort("127.0.0.1", port)
	if err != nil {
		b.Fatalf("dialing service: %v", err)
	}
	defer client.Close()

	// Mount
	auth := rpc.NewAuthUnix("bench", 0, 0)
	type mountReq struct {
		rpc.Header
		Dirpath string
	}
	res, err := client.Call(&mountReq{
		Header: rpc.Header{
			Rpcvers: 2,
			Prog:    nfsc.MountProg,
			Vers:    3,
			Proc:    1,
			Cred:    auth.Auth(),
			Verf:    rpc.AuthNull,
		},
		Dirpath: "/",
	})
	if err != nil {
		b.Fatalf("mount: %v", err)
	}
	var buf [8]byte
	res.Read(buf[:4])
	res.Read(buf[:4])
	fhLen := (uint32(buf[0])<<24) | (uint32(buf[1])<<16) | (uint32(buf[2])<<8) | uint32(buf[3])
	fh := make([]byte, fhLen)
	io.ReadFull(res, fh)

	target, err := nfsc.NewTargetWithClient(client, auth.Auth(), fh, "/", time.Second*5)
	if err != nil {
		b.Fatalf("target: %v", err)
	}
	defer target.Close()

	b.ResetTimer()
	b.SetBytes(int64(len(content)))

	for i := 0; i < b.N; i++ {
		f, err := target.Open("/benchmark.bin")
		if err != nil {
			b.Fatalf("open failed: %v", err)
		}
		_, err = io.ReadAll(f)
		f.Close()
		if err != nil {
			b.Fatalf("read failed: %v", err)
		}
	}
}

// BenchmarkNFSWrite benchmarks NFS write performance.
func BenchmarkNFSWrite(b *testing.B) {
	// Create a temporary directory
	tmpDir, err := os.MkdirTemp("", "nfs-bench")
	if err != nil {
		b.Fatalf("creating temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Start NFS server
	port, _ := findFreePort()
	fs := osfs.New(tmpDir)
	handler := &billyHandler{fs: fs}
	cachingHandler := nfshelper.NewCachingHandler(handler, 100000)

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		b.Fatalf("creating listener: %v", err)
	}
	defer listener.Close()

	go nfs.Serve(listener, cachingHandler)
	time.Sleep(100 * time.Millisecond)

	// Connect client
	client, err := nfsc.DialServiceAtPort("127.0.0.1", port)
	if err != nil {
		b.Fatalf("dialing service: %v", err)
	}
	defer client.Close()

	// Mount
	auth := rpc.NewAuthUnix("bench", 0, 0)
	type mountReq struct {
		rpc.Header
		Dirpath string
	}
	res, err := client.Call(&mountReq{
		Header: rpc.Header{
			Rpcvers: 2,
			Prog:    nfsc.MountProg,
			Vers:    3,
			Proc:    1,
			Cred:    auth.Auth(),
			Verf:    rpc.AuthNull,
		},
		Dirpath: "/",
	})
	if err != nil {
		b.Fatalf("mount: %v", err)
	}
	var buf [8]byte
	res.Read(buf[:4])
	res.Read(buf[:4])
	fhLen := (uint32(buf[0])<<24) | (uint32(buf[1])<<16) | (uint32(buf[2])<<8) | uint32(buf[3])
	fh := make([]byte, fhLen)
	io.ReadFull(res, fh)

	target, err := nfsc.NewTargetWithClient(client, auth.Auth(), fh, "/", time.Second*5)
	if err != nil {
		b.Fatalf("target: %v", err)
	}
	defer target.Close()

	content := make([]byte, 1024*1024) // 1MB
	for i := range content {
		content[i] = byte(i % 256)
	}

	b.ResetTimer()
	b.SetBytes(int64(len(content)))

	for i := 0; i < b.N; i++ {
		filename := fmt.Sprintf("/bench%d.bin", i)
		f, err := target.OpenFile(filename, 0644)
		if err != nil {
			b.Fatalf("create failed: %v", err)
		}
		_, err = f.Write(content)
		f.Close()
		if err != nil {
			b.Fatalf("write failed: %v", err)
		}
	}
}
