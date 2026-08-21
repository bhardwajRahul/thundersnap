package infiniblock

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestServerClientE2E(t *testing.T) {
	// Create temp dir for backing file
	tmpDir := t.TempDir()
	backingPath := filepath.Join(tmpDir, "backing.sparse")

	// Create sparse file backend
	backend, err := OpenSparseFile(backingPath)
	if err != nil {
		t.Fatalf("OpenSparseFile: %v", err)
	}
	defer backend.Close()

	// Create server on random port
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	server := NewServer(ServerConfig{
		Backend:    backend,
		ExportSize: DefaultExportSize,
	})

	// Start server in background
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Serve(ln)
	}()
	defer server.Close()

	// Give server a moment to start
	time.Sleep(10 * time.Millisecond)

	// Connect client
	client, err := Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	// Test 1: Verify reported size is 1 EiB
	t.Run("size", func(t *testing.T) {
		if client.Size() != DefaultExportSize {
			t.Errorf("Size = %d, want %d", client.Size(), DefaultExportSize)
		}
	})

	// Test 2: Verify TRIM support is advertised
	t.Run("trim_support", func(t *testing.T) {
		if !client.SupportsTrim() {
			t.Error("Server should advertise TRIM support")
		}
	})

	// Test 3: Verify FLUSH support is advertised
	t.Run("flush_support", func(t *testing.T) {
		if !client.SupportsFlush() {
			t.Error("Server should advertise FLUSH support")
		}
	})

	// Test 4: Write and read back data
	t.Run("write_read", func(t *testing.T) {
		data := make([]byte, 4096)
		rand.Read(data)
		offset := uint64(1024 * 1024) // 1 MiB offset

		if err := client.Write(data, offset); err != nil {
			t.Fatalf("Write: %v", err)
		}

		readBuf := make([]byte, 4096)
		if err := client.Read(readBuf, offset); err != nil {
			t.Fatalf("Read: %v", err)
		}

		if !bytes.Equal(data, readBuf) {
			t.Error("Read data doesn't match written data")
		}
	})

	// Test 5: Reading unwritten area returns zeros
	t.Run("read_zeros", func(t *testing.T) {
		readBuf := make([]byte, 4096)
		offset := uint64(500 * 1024 * 1024) // 500 MiB - never written

		if err := client.Read(readBuf, offset); err != nil {
			t.Fatalf("Read: %v", err)
		}

		zeros := make([]byte, 4096)
		if !bytes.Equal(readBuf, zeros) {
			t.Error("Unwritten area should return zeros")
		}
	})

	// Test 6: Write at large offset (sparse extension)
	t.Run("sparse_write", func(t *testing.T) {
		data := make([]byte, 4096)
		rand.Read(data)
		// Write at 1 GiB offset - should create sparse hole
		offset := uint64(1024 * 1024 * 1024)

		if err := client.Write(data, offset); err != nil {
			t.Fatalf("Write at large offset: %v", err)
		}

		// Verify we can read it back
		readBuf := make([]byte, 4096)
		if err := client.Read(readBuf, offset); err != nil {
			t.Fatalf("Read: %v", err)
		}

		if !bytes.Equal(data, readBuf) {
			t.Error("Read data doesn't match written data at large offset")
		}

		// Check that the backing file size extended
		size, err := backend.Size()
		if err != nil {
			t.Fatalf("Size: %v", err)
		}
		expectedMinSize := int64(offset) + int64(len(data))
		if size < expectedMinSize {
			t.Errorf("Backing file size = %d, want >= %d", size, expectedMinSize)
		}

		// Check that allocated size is much smaller than logical size (sparse)
		allocated, err := backend.AllocatedSize()
		if err != nil {
			t.Fatalf("AllocatedSize: %v", err)
		}
		// Allocated should be way less than 1 GiB (just a few blocks)
		if allocated > 100*1024*1024 {
			t.Errorf("Allocated size = %d, should be much smaller (sparse file)", allocated)
		}
		t.Logf("Logical size: %d, Allocated: %d (%.2f%%)", size, allocated, 100*float64(allocated)/float64(size))
	})

	// Test 7: TRIM punches holes and reduces allocated size
	t.Run("trim", func(t *testing.T) {
		// Write a block of data
		data := make([]byte, 64*1024) // 64 KiB
		rand.Read(data)
		offset := uint64(2 * 1024 * 1024) // 2 MiB

		if err := client.Write(data, offset); err != nil {
			t.Fatalf("Write: %v", err)
		}

		// Flush to ensure data is on disk
		if err := client.Flush(); err != nil {
			t.Fatalf("Flush: %v", err)
		}

		// Get allocated size before trim
		allocBefore, err := backend.AllocatedSize()
		if err != nil {
			t.Fatalf("AllocatedSize: %v", err)
		}

		// Trim the block
		if err := client.Trim(offset, uint64(len(data))); err != nil {
			t.Fatalf("Trim: %v", err)
		}

		// Sync to ensure hole is punched
		if err := backend.Sync(); err != nil {
			t.Fatalf("Sync: %v", err)
		}

		// Get allocated size after trim
		allocAfter, err := backend.AllocatedSize()
		if err != nil {
			t.Fatalf("AllocatedSize: %v", err)
		}

		t.Logf("Allocated before trim: %d, after: %d", allocBefore, allocAfter)

		// Allocated size should decrease (or stay same if it was already minimal)
		if allocAfter > allocBefore {
			t.Errorf("Allocated size increased after trim: %d -> %d", allocBefore, allocAfter)
		}

		// Reading trimmed area should return zeros
		readBuf := make([]byte, 64*1024)
		if err := client.Read(readBuf, offset); err != nil {
			t.Fatalf("Read after trim: %v", err)
		}

		zeros := make([]byte, 64*1024)
		if !bytes.Equal(readBuf, zeros) {
			t.Error("Trimmed area should return zeros")
		}
	})

	// Test 8: Flush works
	t.Run("flush", func(t *testing.T) {
		if err := client.Flush(); err != nil {
			t.Errorf("Flush: %v", err)
		}
	})

	// Test 9: Graceful disconnect
	t.Run("disconnect", func(t *testing.T) {
		if err := client.Disconnect(); err != nil {
			t.Errorf("Disconnect: %v", err)
		}
	})
}

func TestBackingSparseFile(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "test.sparse")

	sf, err := OpenSparseFile(path)
	if err != nil {
		t.Fatalf("OpenSparseFile: %v", err)
	}
	defer sf.Close()

	// Write at offset - should create sparse hole before it
	data := []byte("hello world")
	offset := int64(1024 * 1024) // 1 MiB

	n, err := sf.WriteAt(data, offset)
	if err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if n != len(data) {
		t.Errorf("WriteAt wrote %d bytes, want %d", n, len(data))
	}

	// Check sizes
	size, err := sf.Size()
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if size != offset+int64(len(data)) {
		t.Errorf("Size = %d, want %d", size, offset+int64(len(data)))
	}

	allocated, err := sf.AllocatedSize()
	if err != nil {
		t.Fatalf("AllocatedSize: %v", err)
	}
	// Allocated should be much smaller than 1 MiB
	if allocated > 100*1024 {
		t.Errorf("AllocatedSize = %d, want < 100KB for sparse file", allocated)
	}

	// Read it back
	readBuf := make([]byte, len(data))
	n, err = sf.ReadAt(readBuf, offset)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(readBuf, data) {
		t.Errorf("ReadAt = %q, want %q", readBuf, data)
	}

	// Read from hole - should be zeros
	holeBuf := make([]byte, 100)
	n, err = sf.ReadAt(holeBuf, 0)
	if err != nil {
		t.Fatalf("ReadAt hole: %v", err)
	}
	for i, b := range holeBuf {
		if b != 0 {
			t.Errorf("Hole byte %d = %d, want 0", i, b)
			break
		}
	}

	// Trim the data
	if err := sf.Trim(offset, int64(len(data))); err != nil {
		t.Fatalf("Trim: %v", err)
	}

	// Read should now be zeros
	n, err = sf.ReadAt(readBuf, offset)
	if err != nil {
		t.Fatalf("ReadAt after trim: %v", err)
	}
	for i, b := range readBuf {
		if b != 0 {
			t.Errorf("Trimmed byte %d = %d, want 0", i, b)
			break
		}
	}
}

func TestMultipleClients(t *testing.T) {
	tmpDir := t.TempDir()
	backingPath := filepath.Join(tmpDir, "backing.sparse")

	backend, err := OpenSparseFile(backingPath)
	if err != nil {
		t.Fatalf("OpenSparseFile: %v", err)
	}
	defer backend.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	server := NewServer(ServerConfig{Backend: backend})
	go server.Serve(ln)
	defer server.Close()

	time.Sleep(10 * time.Millisecond)

	// Connect two clients
	client1, err := Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial client1: %v", err)
	}
	defer client1.Close()

	client2, err := Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial client2: %v", err)
	}
	defer client2.Close()

	// Client 1 writes
	data1 := []byte("from client 1")
	if err := client1.Write(data1, 0); err != nil {
		t.Fatalf("client1 Write: %v", err)
	}

	// Client 2 reads what client 1 wrote
	readBuf := make([]byte, len(data1))
	if err := client2.Read(readBuf, 0); err != nil {
		t.Fatalf("client2 Read: %v", err)
	}
	if !bytes.Equal(readBuf, data1) {
		t.Errorf("client2 Read = %q, want %q", readBuf, data1)
	}
}

func TestHandshakeOptList(t *testing.T) {
	tmpDir := t.TempDir()
	backingPath := filepath.Join(tmpDir, "backing.sparse")

	backend, err := OpenSparseFile(backingPath)
	if err != nil {
		t.Fatalf("OpenSparseFile: %v", err)
	}
	defer backend.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	server := NewServer(ServerConfig{
		Backend:    backend,
		ExportName: "test-export",
	})
	go server.Serve(ln)
	defer server.Close()

	time.Sleep(10 * time.Millisecond)

	// Manual handshake to test NBD_OPT_LIST
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	// Read server greeting
	var buf [18]byte
	if _, err := conn.Read(buf[:]); err != nil {
		t.Fatalf("Read greeting: %v", err)
	}

	// Send client flags
	clientFlags := uint32(nbdFlagCFixedNewstyle | nbdFlagCNoZeroes)
	if err := writeUint32(conn, clientFlags); err != nil {
		t.Fatalf("Write client flags: %v", err)
	}

	// Send NBD_OPT_LIST
	if err := writeUint64(conn, ihaveoptMagic); err != nil {
		t.Fatal(err)
	}
	if err := writeUint32(conn, nbdOptList); err != nil {
		t.Fatal(err)
	}
	if err := writeUint32(conn, 0); err != nil { // no data
		t.Fatal(err)
	}

	// Read NBD_REP_SERVER reply
	magic, err := readUint64(conn)
	if err != nil {
		t.Fatalf("Read reply magic: %v", err)
	}
	if magic != optReplyMagic {
		t.Fatalf("Bad reply magic: %x", magic)
	}

	opt, err := readUint32(conn)
	if err != nil {
		t.Fatal(err)
	}
	if opt != nbdOptList {
		t.Fatalf("Bad option in reply: %d", opt)
	}

	replyType, err := readUint32(conn)
	if err != nil {
		t.Fatal(err)
	}
	if replyType != nbdRepServer {
		t.Fatalf("Expected NBD_REP_SERVER, got %d", replyType)
	}

	length, err := readUint32(conn)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, length)
	if _, err := conn.Read(data); err != nil {
		t.Fatal(err)
	}

	// Parse export name
	if length < 4 {
		t.Fatal("Reply too short")
	}
	nameLen := int(data[0])<<24 | int(data[1])<<16 | int(data[2])<<8 | int(data[3])
	if nameLen+4 > int(length) {
		t.Fatal("Name length mismatch")
	}
	exportName := string(data[4 : 4+nameLen])
	if exportName != "test-export" {
		t.Errorf("Export name = %q, want %q", exportName, "test-export")
	}

	t.Logf("Listed export: %s", exportName)
}

func writeUint32(conn net.Conn, v uint32) error {
	buf := []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
	_, err := conn.Write(buf)
	return err
}

func writeUint64(conn net.Conn, v uint64) error {
	buf := []byte{
		byte(v >> 56), byte(v >> 48), byte(v >> 40), byte(v >> 32),
		byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v),
	}
	_, err := conn.Write(buf)
	return err
}

func readUint32(conn net.Conn) (uint32, error) {
	var buf [4]byte
	if _, err := conn.Read(buf[:]); err != nil {
		return 0, err
	}
	return uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3]), nil
}

func readUint64(conn net.Conn) (uint64, error) {
	var buf [8]byte
	if _, err := conn.Read(buf[:]); err != nil {
		return 0, err
	}
	return uint64(buf[0])<<56 | uint64(buf[1])<<48 | uint64(buf[2])<<40 | uint64(buf[3])<<32 |
		uint64(buf[4])<<24 | uint64(buf[5])<<16 | uint64(buf[6])<<8 | uint64(buf[7]), nil
}

// TestBusyboxStyleClient simulates busybox nbd-client which sends 0 for client flags
// (doesn't set NBD_FLAG_C_FIXED_NEWSTYLE) and uses NBD_OPT_EXPORT_NAME.
func TestBusyboxStyleClient(t *testing.T) {
	tmpDir := t.TempDir()
	backingPath := filepath.Join(tmpDir, "backing.sparse")

	backend, err := OpenSparseFile(backingPath)
	if err != nil {
		t.Fatalf("OpenSparseFile: %v", err)
	}
	defer backend.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	server := NewServer(ServerConfig{Backend: backend})
	go server.Serve(ln)
	defer server.Close()

	time.Sleep(10 * time.Millisecond)

	// Manual handshake like busybox nbd-client
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	// Read server greeting (8 + 8 + 2 = 18 bytes)
	var buf [18]byte
	if _, err := io.ReadFull(conn, buf[:]); err != nil {
		t.Fatalf("Read greeting: %v", err)
	}

	// Verify magic
	magic := uint64(buf[0])<<56 | uint64(buf[1])<<48 | uint64(buf[2])<<40 | uint64(buf[3])<<32 |
		uint64(buf[4])<<24 | uint64(buf[5])<<16 | uint64(buf[6])<<8 | uint64(buf[7])
	if magic != nbdMagic {
		t.Fatalf("Bad magic: %x", magic)
	}

	// Send client flags = 0 (like busybox)
	if err := writeUint32(conn, 0); err != nil {
		t.Fatalf("Write client flags: %v", err)
	}

	// Send NBD_OPT_EXPORT_NAME with empty name
	if err := writeUint64(conn, ihaveoptMagic); err != nil {
		t.Fatal(err)
	}
	if err := writeUint32(conn, nbdOptExportName); err != nil {
		t.Fatal(err)
	}
	if err := writeUint32(conn, 0); err != nil { // empty export name
		t.Fatal(err)
	}

	// Read response: 8 bytes size + 2 bytes flags + 124 bytes zeros
	var resp [134]byte
	if _, err := io.ReadFull(conn, resp[:]); err != nil {
		t.Fatalf("Read export info: %v", err)
	}

	// Parse size
	size := uint64(resp[0])<<56 | uint64(resp[1])<<48 | uint64(resp[2])<<40 | uint64(resp[3])<<32 |
		uint64(resp[4])<<24 | uint64(resp[5])<<16 | uint64(resp[6])<<8 | uint64(resp[7])
	if size != DefaultExportSize {
		t.Errorf("Size = %d, want %d", size, DefaultExportSize)
	}

	// Parse flags
	flags := uint16(resp[8])<<8 | uint16(resp[9])
	if flags&nbdFlagSendTrim == 0 {
		t.Error("Expected TRIM flag")
	}

	// Verify 124 zero bytes
	for i := 10; i < 134; i++ {
		if resp[i] != 0 {
			t.Errorf("Expected zero at offset %d, got %d", i, resp[i])
			break
		}
	}

	t.Logf("Busybox-style handshake succeeded: size=%d, flags=%04x", size, flags)

	// Now we're in transmission phase - try a simple read
	handle := uint64(1)
	req := Request{
		Magic:  requestMagic,
		Flags:  0,
		Type:   nbdCmdRead,
		Handle: handle,
		Offset: 0,
		Length: 512,
	}
	if err := binary.Write(conn, binary.BigEndian, &req); err != nil {
		t.Fatalf("Write request: %v", err)
	}

	// Read reply header (4 + 4 + 8 = 16 bytes)
	var replyBuf [16]byte
	if _, err := io.ReadFull(conn, replyBuf[:]); err != nil {
		t.Fatalf("Read reply: %v", err)
	}

	replyMagic := uint32(replyBuf[0])<<24 | uint32(replyBuf[1])<<16 | uint32(replyBuf[2])<<8 | uint32(replyBuf[3])
	if replyMagic != simpleReplyMagic {
		t.Fatalf("Bad reply magic: %x", replyMagic)
	}

	replyErr := uint32(replyBuf[4])<<24 | uint32(replyBuf[5])<<16 | uint32(replyBuf[6])<<8 | uint32(replyBuf[7])
	if replyErr != 0 {
		t.Fatalf("Reply error: %d", replyErr)
	}

	// Read data
	data := make([]byte, 512)
	if _, err := io.ReadFull(conn, data); err != nil {
		t.Fatalf("Read data: %v", err)
	}

	t.Log("Busybox-style read succeeded")
}

func TestLargeIO(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large I/O test in short mode")
	}

	tmpDir := t.TempDir()
	backingPath := filepath.Join(tmpDir, "backing.sparse")

	backend, err := OpenSparseFile(backingPath)
	if err != nil {
		t.Fatalf("OpenSparseFile: %v", err)
	}
	defer backend.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	server := NewServer(ServerConfig{Backend: backend})
	go server.Serve(ln)
	defer server.Close()

	time.Sleep(10 * time.Millisecond)

	client, err := Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	// Write 1 MiB of data
	data := make([]byte, 1024*1024)
	rand.Read(data)

	if err := client.Write(data, 0); err != nil {
		t.Fatalf("Write 1MiB: %v", err)
	}

	// Read it back
	readBuf := make([]byte, 1024*1024)
	if err := client.Read(readBuf, 0); err != nil {
		t.Fatalf("Read 1MiB: %v", err)
	}

	if !bytes.Equal(data, readBuf) {
		t.Error("1MiB read doesn't match write")
	}
}

func TestFilePersistence(t *testing.T) {
	tmpDir := t.TempDir()
	backingPath := filepath.Join(tmpDir, "backing.sparse")

	// Write data with first server/client
	func() {
		backend, err := OpenSparseFile(backingPath)
		if err != nil {
			t.Fatalf("OpenSparseFile: %v", err)
		}
		defer backend.Close()

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("Listen: %v", err)
		}

		server := NewServer(ServerConfig{Backend: backend})
		go server.Serve(ln)
		defer server.Close()

		time.Sleep(10 * time.Millisecond)

		client, err := Dial(ln.Addr().String())
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer client.Close()

		data := []byte("persistent data test")
		if err := client.Write(data, 12345); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := client.Flush(); err != nil {
			t.Fatalf("Flush: %v", err)
		}
	}()

	// Read it back with new server/client
	func() {
		backend, err := OpenSparseFile(backingPath)
		if err != nil {
			t.Fatalf("OpenSparseFile: %v", err)
		}
		defer backend.Close()

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("Listen: %v", err)
		}

		server := NewServer(ServerConfig{Backend: backend})
		go server.Serve(ln)
		defer server.Close()

		time.Sleep(10 * time.Millisecond)

		client, err := Dial(ln.Addr().String())
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer client.Close()

		readBuf := make([]byte, 20)
		if err := client.Read(readBuf, 12345); err != nil {
			t.Fatalf("Read: %v", err)
		}

		expected := []byte("persistent data test")
		if !bytes.Equal(readBuf, expected) {
			t.Errorf("Read = %q, want %q", readBuf, expected)
		}
	}()
}

// TestAllocatedVsLogicalSize verifies the sparse file behavior in detail
func TestAllocatedVsLogicalSize(t *testing.T) {
	tmpDir := t.TempDir()
	backingPath := filepath.Join(tmpDir, "backing.sparse")

	backend, err := OpenSparseFile(backingPath)
	if err != nil {
		t.Fatalf("OpenSparseFile: %v", err)
	}
	defer backend.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	server := NewServer(ServerConfig{Backend: backend})
	go server.Serve(ln)
	defer server.Close()

	time.Sleep(10 * time.Millisecond)

	client, err := Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	// Initial state
	logicalSize, _ := backend.Size()
	allocatedSize, _ := backend.AllocatedSize()
	t.Logf("Initial: logical=%d, allocated=%d", logicalSize, allocatedSize)

	// Write 4KB at offset 0
	block := make([]byte, 4096)
	rand.Read(block)
	if err := client.Write(block, 0); err != nil {
		t.Fatal(err)
	}
	client.Flush()

	logicalSize, _ = backend.Size()
	allocatedSize, _ = backend.AllocatedSize()
	t.Logf("After 4KB at 0: logical=%d, allocated=%d", logicalSize, allocatedSize)

	// Write 4KB at 100MB offset (creates 100MB hole)
	if err := client.Write(block, 100*1024*1024); err != nil {
		t.Fatal(err)
	}
	client.Flush()

	logicalSize, _ = backend.Size()
	allocatedSize, _ = backend.AllocatedSize()
	t.Logf("After 4KB at 100MB: logical=%d, allocated=%d", logicalSize, allocatedSize)

	if logicalSize < 100*1024*1024 {
		t.Errorf("Logical size should be > 100MB, got %d", logicalSize)
	}
	// Allocated should be around 8KB (two 4KB blocks), not 100MB
	if allocatedSize > 1024*1024 {
		t.Errorf("Allocated size should be small, got %d", allocatedSize)
	}

	// Trim the block at offset 0
	if err := client.Trim(0, 4096); err != nil {
		t.Fatal(err)
	}
	backend.Sync()

	logicalSize2, _ := backend.Size()
	allocatedSize2, _ := backend.AllocatedSize()
	t.Logf("After trim at 0: logical=%d, allocated=%d", logicalSize2, allocatedSize2)

	// Logical size shouldn't change from trim
	if logicalSize2 != logicalSize {
		t.Errorf("Logical size changed after trim: %d -> %d", logicalSize, logicalSize2)
	}

	// Allocated should decrease
	if allocatedSize2 >= allocatedSize {
		t.Logf("Note: allocated didn't decrease after trim (may depend on filesystem)")
	}

	// Verify file exists and is readable
	fi, err := os.Stat(backingPath)
	if err != nil {
		t.Fatalf("Stat backing file: %v", err)
	}
	t.Logf("Backing file: size=%d", fi.Size())
}

// TestServerBoundsRejection verifies the server rejects NBD commands that
// fall outside the advertised export size, and that a rejected write does
// not extend the backing file past the export. Regression test for the
// missing bounds checks found in code review.
func TestServerBoundsRejection(t *testing.T) {
	tmpDir := t.TempDir()
	backingPath := filepath.Join(tmpDir, "backing.sparse")

	backend, err := OpenSparseFile(backingPath)
	if err != nil {
		t.Fatalf("OpenSparseFile: %v", err)
	}
	defer backend.Close()

	const exportSize = 1 << 20 // 1 MiB
	server := NewServer(ServerConfig{Backend: backend, ExportSize: exportSize})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = server.Serve(ln) }()
	defer server.Close()
	time.Sleep(10 * time.Millisecond)

	// A write whose tail crosses the export boundary must be rejected
	// (EINVAL, error code 22) and must NOT extend the backing file.
	client, err := Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	writeErr := client.Write(make([]byte, 4096), exportSize-512)
	if writeErr == nil {
		t.Fatal("write past export size succeeded; should have been rejected")
	}

	// Backing file must not have grown beyond the export size.
	fi, err := os.Stat(backingPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Size() > exportSize {
		t.Errorf("backing file grew to %d past export size %d after rejected write", fi.Size(), exportSize)
	}
}
