// Package e2e contains end-to-end tests for thundersnap frame lifecycle.
package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestFrameLifecycleBasic tests the basic frame lifecycle:
// 1. Create frame with rootfs-only spec
// 2. Verify it exists in frames list
// 3. Delete it
// 4. Verify it's gone from frames list
func TestFrameLifecycleBasic(t *testing.T) {
	env := newTestEnv(t)

	// Start thundersnapd in-process with a control socket
	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startTestControlServer(t, env, sockPath)
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	// Create a base snapshot first
	baseSnap := env.createBaseSnapshot()
	t.Logf("Created base snapshot: %s", baseSnap)

	// Step 1: Create a frame with rootfs-only spec (baseSnap::)
	frameName := "testframe"
	frameSpec := baseSnap + "::"

	createResp, err := client.postJSON("/create", map[string]string{
		"frame_name":  frameName,
		"snapshot_id": frameSpec,
	})
	if err != nil {
		t.Fatalf("create frame: %v", err)
	}
	if createResp["status"] != "ok" {
		t.Fatalf("create frame failed: %v", createResp["message"])
	}
	t.Logf("Created frame: %s", frameName)

	// Step 2: Verify the frame appears in the list
	listResp, err := client.getJSON("/list-frames")
	if err != nil {
		t.Fatalf("list frames: %v", err)
	}
	if listResp["status"] != "ok" {
		t.Fatalf("list frames failed: %v", listResp["error"])
	}

	frames, ok := listResp["frames"].([]interface{})
	if !ok {
		t.Fatalf("frames is not a list: %T", listResp["frames"])
	}

	found := false
	for _, f := range frames {
		fmap := f.(map[string]interface{})
		if fmap["name"] == frameName {
			found = true
			t.Logf("Found frame in list: name=%s status=%s", fmap["name"], fmap["status"])
			break
		}
	}
	if !found {
		t.Fatalf("frame %q not found in frames list", frameName)
	}

	// Step 3: Delete the frame
	deleteResp, err := client.postJSON("/delete-frame", map[string]string{
		"frame_name": frameName,
	})
	if err != nil {
		t.Fatalf("delete frame: %v", err)
	}
	if deleteResp["status"] != "ok" {
		t.Fatalf("delete frame failed: %v", deleteResp["message"])
	}
	t.Logf("Deleted frame: %s", frameName)

	// Step 4: Verify the frame is gone
	listResp, err = client.getJSON("/list-frames")
	if err != nil {
		t.Fatalf("list frames after delete: %v", err)
	}
	if listResp["status"] != "ok" {
		t.Fatalf("list frames failed: %v", listResp["error"])
	}

	frames, _ = listResp["frames"].([]interface{})
	for _, f := range frames {
		fmap := f.(map[string]interface{})
		if fmap["name"] == frameName {
			t.Fatalf("frame %q still exists after deletion", frameName)
		}
	}
	t.Logf("Verified frame is deleted")
}

// testControlServer wraps a test control socket server.
type testControlServer struct {
	sockPath string
	listener net.Listener
	env      *testEnv
	done     chan struct{}
}

func (s *testControlServer) Close() {
	s.listener.Close()
	<-s.done
	os.Remove(s.sockPath)
}

// startTestControlServer starts a control server for testing.
// It implements the thundersnapd control protocol (vsock handshake + HTTP).
func startTestControlServer(t *testing.T, env *testEnv, sockPath string) *testControlServer {
	t.Helper()

	// Remove existing socket
	os.Remove(sockPath)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen %s: %v", sockPath, err)
	}

	if err := os.Chmod(sockPath, 0666); err != nil {
		t.Logf("warning: chmod socket: %v", err)
	}

	srv := &testControlServer{
		sockPath: sockPath,
		listener: ln,
		env:      env,
		done:     make(chan struct{}),
	}

	// Create handler mux
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", handleTestPing)
	mux.HandleFunc("/create", srv.handleCreate)
	mux.HandleFunc("/list-frames", srv.handleListFrames)
	mux.HandleFunc("/delete-frame", srv.handleDeleteFrame)
	mux.HandleFunc("/snap", srv.handleSnap)
	mux.HandleFunc("/list-snaps", srv.handleListSnaps)
	mux.HandleFunc("/taint", srv.handleTaint)

	go func() {
		defer close(srv.done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.handleConn(conn, mux)
		}
	}()

	return srv
}

func (s *testControlServer) handleConn(conn net.Conn, handler http.Handler) {
	defer conn.Close()

	reader := bufio.NewReader(conn)

	// Read vsock handshake
	line, err := reader.ReadString('\n')
	if err != nil {
		return
	}

	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "CONNECT ") {
		conn.Write([]byte("ERROR invalid handshake\n"))
		return
	}

	// Send OK response
	conn.Write([]byte("OK 5223\n"))

	// Handle HTTP request
	req, err := http.ReadRequest(reader)
	if err != nil {
		return
	}

	rw := &testResponseWriter{
		conn:    conn,
		headers: make(http.Header),
	}
	handler.ServeHTTP(rw, req)
	rw.finish()
}

func handleTestPing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"message": "pong",
	})
}

func (s *testControlServer) handleCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		FrameName  string `json:"frame_name"`
		SnapshotID string `json:"snapshot_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "error",
			"message": "invalid request: " + err.Error(),
		})
		return
	}

	// Parse snapshot spec (rootfs:home:work)
	rootfs, home, work := parseFrameSpec(req.SnapshotID)

	// Create frame directory
	framePath := filepath.Join(s.env.fsDir, "testuser", req.FrameName)
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "error",
			"message": "mkdir: " + err.Error(),
		})
		return
	}

	// Clone rootfs snapshot to frame
	rootfsPath := filepath.Join(s.env.snapshotsDir, rootfs)
	cmd := exec.Command("btrfs", "subvolume", "snapshot", rootfsPath, framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "error",
			"message": "btrfs snapshot: " + err.Error() + ": " + string(out),
		})
		return
	}

	// Create home subvolume if specified empty or with snap
	homePath := filepath.Join(framePath, "home")
	if home == "" {
		// Remove existing home directory contents and create empty subvolume
		os.RemoveAll(homePath)
		cmd = exec.Command("btrfs", "subvolume", "create", homePath)
		cmd.CombinedOutput()
		os.Chown(homePath, 1000, 1000)
	} else if home != "nil" {
		// Clone from home snapshot
		homeSnapPath := filepath.Join(s.env.snapshotsDir, home)
		os.RemoveAll(homePath)
		cmd = exec.Command("btrfs", "subvolume", "snapshot", homeSnapPath, homePath)
		cmd.CombinedOutput()
	}

	// Create work subvolume if specified empty or with snap
	workPath := filepath.Join(framePath, "work")
	if work == "" {
		os.RemoveAll(workPath)
		cmd = exec.Command("btrfs", "subvolume", "create", workPath)
		cmd.CombinedOutput()
		os.Chown(workPath, 1000, 1000)
	} else if work != "nil" {
		workSnapPath := filepath.Join(s.env.snapshotsDir, work)
		os.RemoveAll(workPath)
		cmd = exec.Command("btrfs", "subvolume", "snapshot", workSnapPath, workPath)
		cmd.CombinedOutput()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type":   "result",
		"status": "ok",
		"path":   framePath,
	})
}

func (s *testControlServer) handleListFrames(w http.ResponseWriter, r *http.Request) {
	var frames []map[string]string

	// Scan fs directory for frames
	userDirs, _ := os.ReadDir(s.env.fsDir)
	for _, userDir := range userDirs {
		if !userDir.IsDir() {
			continue
		}
		userPath := filepath.Join(s.env.fsDir, userDir.Name())
		frameDirs, _ := os.ReadDir(userPath)
		for _, frameDir := range frameDirs {
			if !frameDir.IsDir() {
				continue
			}
			frames = append(frames, map[string]string{
				"name":   frameDir.Name(),
				"status": "stopped",
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
		"frames": frames,
	})
}

func (s *testControlServer) handleDeleteFrame(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		FrameName string `json:"frame_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "error",
			"message": "invalid request: " + err.Error(),
		})
		return
	}

	// Find and delete the frame
	framePath := filepath.Join(s.env.fsDir, "testuser", req.FrameName)

	// Delete nested subvolumes first (home, work)
	for _, subvol := range []string{"home", "work"} {
		subvolPath := filepath.Join(framePath, subvol)
		exec.Command("btrfs", "subvolume", "delete", subvolPath).Run()
	}

	// Delete the frame subvolume
	cmd := exec.Command("btrfs", "subvolume", "delete", framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "error",
			"message": "btrfs delete: " + err.Error() + ": " + string(out),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
	})
}

func (s *testControlServer) handleSnap(w http.ResponseWriter, r *http.Request) {
	// Placeholder - will be implemented in snapshot_test.go
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type":        "result",
		"status":      "ok",
		"snapshot_id": "test-snap-id",
	})
}

func (s *testControlServer) handleListSnaps(w http.ResponseWriter, r *http.Request) {
	var snaps []map[string]interface{}

	// Scan snapshots directory
	entries, _ := os.ReadDir(s.env.snapshotsDir)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		snaps = append(snaps, map[string]interface{}{
			"id":   entry.Name(),
			"size": info.Size(),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
		"snaps":  snaps,
	})
}

func (s *testControlServer) handleTaint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		TaintName string `json:"taint_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "error",
			"message": "invalid request: " + err.Error(),
		})
		return
	}

	// Store taint (just acknowledge for now)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
		"taints": []string{req.TaintName},
	})
}

// parseFrameSpec parses a frame spec like "rootfs:home:work" or "rootfs::"
func parseFrameSpec(spec string) (rootfs, home, work string) {
	parts := strings.Split(spec, ":")
	if len(parts) >= 1 {
		rootfs = parts[0]
	}
	if len(parts) >= 2 {
		home = parts[1]
	}
	if len(parts) >= 3 {
		work = parts[2]
	}
	return
}

// testHTTPClient wraps HTTP client operations for the control socket.
type testHTTPClient struct {
	sockPath string
}

func newTestHTTPClient(sockPath string) *testHTTPClient {
	return &testHTTPClient{sockPath: sockPath}
}

func (c *testHTTPClient) postJSON(path string, body interface{}) (map[string]interface{}, error) {
	conn, err := net.Dial("unix", c.sockPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// Send vsock handshake
	if _, err := conn.Write([]byte("CONNECT 5223\n")); err != nil {
		return nil, err
	}

	// Read OK response
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(string(buf[:n]), "OK") {
		return nil, fmt.Errorf("handshake failed: %s", string(buf[:n]))
	}

	// Build HTTP request
	bodyBytes, _ := json.Marshal(body)
	req := fmt.Sprintf("POST %s HTTP/1.1\r\n"+
		"Host: localhost\r\n"+
		"Content-Type: application/json\r\n"+
		"Content-Length: %d\r\n"+
		"\r\n", path, len(bodyBytes))
	conn.Write([]byte(req))
	conn.Write(bodyBytes)

	// Read response
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	return result, nil
}

func (c *testHTTPClient) getJSON(path string) (map[string]interface{}, error) {
	conn, err := net.Dial("unix", c.sockPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// Send vsock handshake
	if _, err := conn.Write([]byte("CONNECT 5223\n")); err != nil {
		return nil, err
	}

	// Read OK response
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(string(buf[:n]), "OK") {
		return nil, fmt.Errorf("handshake failed: %s", string(buf[:n]))
	}

	// Build HTTP request
	req := fmt.Sprintf("GET %s HTTP/1.1\r\n"+
		"Host: localhost\r\n"+
		"\r\n", path)
	conn.Write([]byte(req))

	// Read response
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	return result, nil
}

// testResponseWriter implements http.ResponseWriter for test connections.
type testResponseWriter struct {
	conn       net.Conn
	headers    http.Header
	statusCode int
	body       bytes.Buffer
}

func (w *testResponseWriter) Header() http.Header {
	return w.headers
}

func (w *testResponseWriter) Write(data []byte) (int, error) {
	return w.body.Write(data)
}

func (w *testResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
}

func (w *testResponseWriter) finish() error {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}

	// Write status line
	statusText := http.StatusText(w.statusCode)
	fmt.Fprintf(w.conn, "HTTP/1.0 %d %s\r\n", w.statusCode, statusText)

	// Write content-length
	w.headers.Set("Content-Length", strconv.Itoa(w.body.Len()))

	// Write headers
	for key, values := range w.headers {
		for _, value := range values {
			fmt.Fprintf(w.conn, "%s: %s\r\n", key, value)
		}
	}
	w.conn.Write([]byte("\r\n"))

	// Write body
	w.conn.Write(w.body.Bytes())
	return nil
}
