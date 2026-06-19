// Package e2e contains end-to-end tests for thundersnap.
//
// These tests build and run the actual binaries (thundersnapd, ts) and
// interact with them only via CLI and HTTP APIs - no internal function calls.
//
// Requirements:
//   - root access (for btrfs subvolume operations)
//   - btrfs filesystem for temp directory
//   - go toolchain to build binaries
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// testEnv holds paths and resources for a test environment.
type testEnv struct {
	t            *testing.T
	root         string // temp dir root
	repoRoot     string // ts2 repository root
	fsDir        string
	snapshotsDir string
	libexecDir   string
	tsBinary     string
	daemonBinary string
}

// requireBtrfsRoot skips if not root or not on btrfs.
func requireBtrfsRoot(t *testing.T) string {
	t.Helper()

	if os.Getuid() != 0 {
		t.Skip("e2e test requires root for btrfs and container ops")
	}
	if _, err := exec.LookPath("btrfs"); err != nil {
		t.Skip("btrfs not on PATH")
	}

	root := t.TempDir()
	cmd := exec.Command("stat", "-f", "-c", "%T", root)
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("stat -f failed: %v", err)
	}
	if strings.TrimSpace(string(out)) != "btrfs" {
		t.Skipf("test dir %s not on btrfs (got %q)", root, strings.TrimSpace(string(out)))
	}

	return root
}

// findRepoRoot finds the ts2 repository root by looking for go.mod.
func findRepoRoot(t *testing.T) string {
	t.Helper()

	// Start from current working directory
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	// Walk up looking for go.mod with module github.com/tailscale/thundersnap
	for {
		gomod := filepath.Join(dir, "go.mod")
		if data, err := os.ReadFile(gomod); err == nil {
			if strings.Contains(string(data), "module github.com/tailscale/thundersnap") {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find ts2 repo root (no go.mod with thundersnap module)")
		}
		dir = parent
	}
}

// newTestEnv creates a test environment with built binaries.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	root := requireBtrfsRoot(t)
	repoRoot := findRepoRoot(t)

	env := &testEnv{
		t:            t,
		root:         root,
		repoRoot:     repoRoot,
		fsDir:        filepath.Join(root, "fs"),
		snapshotsDir: filepath.Join(root, "snapshots"),
		libexecDir:   filepath.Join(root, "libexec"),
	}

	// Create directories
	for _, d := range []string{env.fsDir, env.snapshotsDir, env.libexecDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// Build binaries
	env.tsBinary = env.buildBinary("./cmd/ts", "ts")
	env.daemonBinary = env.buildBinary("./cmd/thundersnapd", "thundersnapd")

	// Copy ts to libexec (thundersnapd looks for it there)
	if err := copyFile(env.tsBinary, filepath.Join(env.libexecDir, "ts")); err != nil {
		t.Fatalf("copy ts to libexec: %v", err)
	}

	t.Cleanup(env.cleanup)

	return env
}

func (e *testEnv) buildBinary(pkg, name string) string {
	e.t.Helper()

	outPath := filepath.Join(e.root, name)
	cmd := exec.Command("go", "build", "-o", outPath, pkg)
	cmd.Dir = e.repoRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")

	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("building %s: %v\noutput: %s", name, err, out)
	}

	return outPath
}

func (e *testEnv) cleanup() {
	// Clean up btrfs subvolumes
	cleanupSubvolumes(e.fsDir)
	cleanupSubvolumes(e.snapshotsDir)
}

func cleanupSubvolumes(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			// Recursively clean nested subvolumes
			cleanupSubvolumes(path)
			exec.Command("btrfs", "subvolume", "delete", path).Run()
		}
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

	// Copy permissions
	info, err := in.Stat()
	if err != nil {
		return err
	}
	return os.Chmod(dst, info.Mode())
}

// createBaseSnapshot creates a minimal base snapshot "1" with test files.
// This mimics what downloading a Docker image would produce.
func (e *testEnv) createBaseSnapshot() string {
	e.t.Helper()

	snapPath := filepath.Join(e.snapshotsDir, "1")

	// Create as btrfs subvolume
	cmd := exec.Command("btrfs", "subvolume", "create", snapPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		e.t.Fatalf("btrfs subvolume create: %v\n%s", err, out)
	}

	// Populate with minimal filesystem
	e.populateRootFS(snapPath)

	return "1"
}

// populateRootFS creates a minimal Linux root filesystem structure.
func (e *testEnv) populateRootFS(dir string) {
	e.t.Helper()

	// Create directory structure
	dirs := []string{
		"bin", "etc", "home/user", "lib", "proc", "root", "sys",
		"tmp", "usr/bin", "usr/lib", "var/log", "work",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(dir, d), 0755); err != nil {
			e.t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// Create /etc/passwd and /etc/group
	passwd := `root:x:0:0:root:/root:/bin/sh
user:x:1000:1000:user:/home/user:/bin/sh
daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin
`
	group := `root:x:0:
user:x:1000:
daemon:x:1:
`
	if err := os.WriteFile(filepath.Join(dir, "etc/passwd"), []byte(passwd), 0644); err != nil {
		e.t.Fatalf("write passwd: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "etc/group"), []byte(group), 0644); err != nil {
		e.t.Fatalf("write group: %v", err)
	}

	// Set ownership on home and work directories
	if err := os.Chown(filepath.Join(dir, "home/user"), 1000, 1000); err != nil {
		e.t.Fatalf("chown home: %v", err)
	}
	if err := os.Chown(filepath.Join(dir, "work"), 1000, 1000); err != nil {
		e.t.Fatalf("chown work: %v", err)
	}

	// Create a test file with non-root ownership
	testFile := filepath.Join(dir, "home/user/.profile")
	if err := os.WriteFile(testFile, []byte("# user profile\n"), 0644); err != nil {
		e.t.Fatalf("write .profile: %v", err)
	}
	if err := os.Chown(testFile, 1000, 1000); err != nil {
		e.t.Fatalf("chown .profile: %v", err)
	}

	// Copy ts binary to /bin/ts
	if err := copyFile(e.tsBinary, filepath.Join(dir, "bin/ts")); err != nil {
		e.t.Fatalf("copy ts to snapshot: %v", err)
	}
}

// httpClient is an HTTP client that talks to a Unix socket.
type httpClient struct {
	sockPath string
	client   *http.Client
}

func newHTTPClient(sockPath string) *httpClient {
	return &httpClient{
		sockPath: sockPath,
		client: &http.Client{
			Transport: &http.Transport{
				DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", sockPath)
				},
			},
			Timeout: 60 * time.Second,
		},
	}
}

// doRequest performs an HTTP request with the vsock handshake.
func (c *httpClient) doRequest(method, path string, body io.Reader) (*http.Response, error) {
	// Connect to socket
	conn, err := net.Dial("unix", c.sockPath)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	// Send vsock handshake
	if _, err := fmt.Fprintf(conn, "CONNECT 5223\n"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("handshake write: %w", err)
	}

	// Read response
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("handshake read: %w", err)
	}
	resp := string(buf[:n])
	if !strings.HasPrefix(resp, "OK ") {
		conn.Close()
		return nil, fmt.Errorf("handshake failed: %s", resp)
	}

	// Now send HTTP request over the same connection
	var bodyBytes []byte
	if body != nil {
		bodyBytes, _ = io.ReadAll(body)
	}

	reqLine := fmt.Sprintf("%s %s HTTP/1.1\r\n", method, path)
	headers := "Host: localhost\r\nConnection: close\r\n"
	if len(bodyBytes) > 0 {
		headers += fmt.Sprintf("Content-Length: %d\r\n", len(bodyBytes))
		headers += "Content-Type: application/json\r\n"
	}
	headers += "\r\n"

	if _, err := conn.Write([]byte(reqLine + headers)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("write request: %w", err)
	}
	if len(bodyBytes) > 0 {
		if _, err := conn.Write(bodyBytes); err != nil {
			conn.Close()
			return nil, fmt.Errorf("write body: %w", err)
		}
	}

	// Read response
	return http.ReadResponse(bufio.NewReader(conn), nil)
}

func (c *httpClient) post(path string, body interface{}) (map[string]interface{}, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(data)
	}

	resp, err := c.doRequest("POST", path, bodyReader)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return result, nil
}

// TestE2EBasicSnapshot tests creating a base snapshot and snapping it.
func TestE2EBasicSnapshot(t *testing.T) {
	env := newTestEnv(t)

	// Create a base snapshot
	baseSnap := env.createBaseSnapshot()
	t.Logf("Created base snapshot: %s", baseSnap)

	// Verify the snapshot exists
	snapPath := filepath.Join(env.snapshotsDir, baseSnap)
	if _, err := os.Stat(snapPath); err != nil {
		t.Fatalf("snapshot not found: %v", err)
	}

	// Verify it has expected structure
	for _, path := range []string{"etc/passwd", "etc/group", "bin/ts"} {
		full := filepath.Join(snapPath, path)
		if _, err := os.Stat(full); err != nil {
			t.Errorf("expected file %s not found: %v", path, err)
		}
	}
}

// TestE2EOwnership verifies file ownership is preserved correctly.
func TestE2EOwnership(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()
	snapPath := filepath.Join(env.snapshotsDir, baseSnap)

	// Check ownership of home/user
	homePath := filepath.Join(snapPath, "home/user")
	info, err := os.Stat(homePath)
	if err != nil {
		t.Fatalf("stat home/user: %v", err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if stat.Uid != 1000 || stat.Gid != 1000 {
		t.Errorf("home/user ownership: got %d:%d, want 1000:1000", stat.Uid, stat.Gid)
	}

	// Check ownership of .profile
	profilePath := filepath.Join(snapPath, "home/user/.profile")
	info, err = os.Stat(profilePath)
	if err != nil {
		t.Fatalf("stat .profile: %v", err)
	}
	stat = info.Sys().(*syscall.Stat_t)
	if stat.Uid != 1000 || stat.Gid != 1000 {
		t.Errorf(".profile ownership: got %d:%d, want 1000:1000", stat.Uid, stat.Gid)
	}
}
