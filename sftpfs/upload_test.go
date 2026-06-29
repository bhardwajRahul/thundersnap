package sftpfs

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/pkg/sftp"
)

// startTestSFTP wires a real sftp.Client to a real sftp.RequestServer backed by
// the production Handler, over an in-memory pipe. This exercises the exact code
// path used for scp/sftp uploads (Filewrite/Filecmd), including the chown of
// newly-created files to the target user.
func startTestSFTP(t *testing.T, h *Handler) *sftp.Client {
	t.Helper()

	clientConn, serverConn := net.Pipe()

	server := sftp.NewRequestServer(serverConn, h.Handlers(),
		sftp.WithStartDirectory(h.HomeDir()))
	go func() {
		if err := server.Serve(); err != nil && err != io.EOF {
			t.Logf("sftp server: %v", err)
		}
		serverConn.Close()
	}()

	client, err := sftp.NewClientPipe(clientConn, clientConn)
	if err != nil {
		t.Fatalf("sftp client: %v", err)
	}
	t.Cleanup(func() {
		client.Close()
		clientConn.Close()
	})
	return client
}

// TestSFTPUploadOwnership verifies that files and directories created over SFTP
// (as scp does) are chowned to the target user, not left owned by root (the
// daemon process). This guards the "scp runs as root" security hole.
//
// Requires root because it asserts on file ownership after chown.
func TestSFTPUploadOwnership(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root to verify chown of uploaded files")
	}

	const wantUID, wantGID = 7575, 7575

	rootFS := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootFS, "home", "user"), 0755); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(rootFS, "/home/user", wantUID, wantGID)
	client := startTestSFTP(t, h)

	// Upload a file (the scp case).
	f, err := client.Create("/home/user/uploaded.txt")
	if err != nil {
		t.Fatalf("create remote file: %v", err)
	}
	if _, err := f.Write([]byte("hello from scp\n")); err != nil {
		t.Fatalf("write remote file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close remote file: %v", err)
	}

	// Create a directory (scp -r / sftp mkdir case).
	if err := client.Mkdir("/home/user/uploaded-dir"); err != nil {
		t.Fatalf("mkdir remote: %v", err)
	}

	checkOwner := func(rel string) {
		t.Helper()
		fi, err := os.Lstat(filepath.Join(rootFS, rel))
		if err != nil {
			t.Fatalf("stat %s: %v", rel, err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		if int(st.Uid) != wantUID || int(st.Gid) != wantGID {
			t.Errorf("%s owned by %d:%d, want %d:%d", rel, st.Uid, st.Gid, wantUID, wantGID)
		}
	}
	checkOwner("home/user/uploaded.txt")
	checkOwner("home/user/uploaded-dir")
}
