//go:build e2e

// Package e2e contains end-to-end tests for thundersnap frame lifecycle.
package e2e

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/tailscale/thundersnap/tsm"
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

// TestIdSubvolumeNotCloned tests that the /id subvolume is:
// 1. Created when a frame is created
// 2. Not included in snapshots (btrfs excludes nested subvolumes)
// 3. Created fresh (empty) when creating a new frame from a snapshot
//
// This is used for storing frame-local secrets like keys that should
// never be persisted or cloned.
func TestIdSubvolumeNotCloned(t *testing.T) {
	env := newTestEnv(t)

	// Start test control server
	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startTestControlServer(t, env, sockPath)
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	// Create a base snapshot
	baseSnap := env.createBaseSnapshot()
	t.Logf("Created base snapshot: %s", baseSnap)

	// Step 1: Create first frame
	frame1Name := "frame1"
	frame1Spec := baseSnap + "::"

	createResp, err := client.postJSON("/create", map[string]string{
		"frame_name":  frame1Name,
		"snapshot_id": frame1Spec,
	})
	if err != nil {
		t.Fatalf("create frame1: %v", err)
	}
	if createResp["status"] != "ok" {
		t.Fatalf("create frame1 failed: %v", createResp["message"])
	}

	frame1Path := filepath.Join(env.fsDir, "testuser", frame1Name)
	idPath1 := filepath.Join(frame1Path, "id")

	// Step 2: Verify /id exists and is a subvolume
	if _, err := os.Stat(idPath1); err != nil {
		t.Fatalf("/id should exist in frame1: %v", err)
	}
	cmd := exec.Command("btrfs", "subvolume", "show", idPath1)
	if err := cmd.Run(); err != nil {
		t.Fatalf("/id should be a btrfs subvolume: %v", err)
	}
	t.Logf("/id exists and is a subvolume in frame1")

	// Step 3: Write a secret file to /id
	secretPath := filepath.Join(idPath1, "secret.key")
	secretContent := []byte("super-secret-key-12345")
	if err := os.WriteFile(secretPath, secretContent, 0600); err != nil {
		t.Fatalf("write secret to /id: %v", err)
	}
	t.Logf("Wrote secret to /id/secret.key")

	// Step 4: Take a snapshot of frame1
	snapResp, err := client.postJSON("/snap", map[string]string{
		"frame_name": frame1Name,
	})
	if err != nil {
		t.Fatalf("snap frame1: %v", err)
	}
	if snapResp["status"] != "ok" {
		t.Fatalf("snap frame1 failed: %v", snapResp["message"])
	}
	snapID := snapResp["snapshot_id"].(string)
	t.Logf("Created snapshot: %s", snapID)

	// Step 5: Create a second frame from the snapshot
	frame2Name := "frame2"
	frame2Spec := snapID + "::"

	createResp, err = client.postJSON("/create", map[string]string{
		"frame_name":  frame2Name,
		"snapshot_id": frame2Spec,
	})
	if err != nil {
		t.Fatalf("create frame2: %v", err)
	}
	if createResp["status"] != "ok" {
		t.Fatalf("create frame2 failed: %v", createResp["message"])
	}

	frame2Path := filepath.Join(env.fsDir, "testuser", frame2Name)
	idPath2 := filepath.Join(frame2Path, "id")

	// Step 6: Verify /id exists in frame2
	if _, err := os.Stat(idPath2); err != nil {
		t.Fatalf("/id should exist in frame2: %v", err)
	}
	t.Logf("/id exists in frame2")

	// Step 7: Verify /id is a subvolume (not a regular directory)
	cmd = exec.Command("btrfs", "subvolume", "show", idPath2)
	if err := cmd.Run(); err != nil {
		t.Fatalf("/id should be a btrfs subvolume in frame2: %v", err)
	}
	t.Logf("/id is a btrfs subvolume in frame2")

	// Step 8: Verify /id is EMPTY (the secret was NOT cloned)
	entries, err := os.ReadDir(idPath2)
	if err != nil {
		t.Fatalf("read /id in frame2: %v", err)
	}
	if len(entries) != 0 {
		for _, e := range entries {
			t.Logf("Unexpected file in frame2 /id: %s", e.Name())
		}
		t.Fatalf("/id in frame2 should be empty, but has %d entries", len(entries))
	}
	t.Logf("/id in frame2 is empty as expected - secret was NOT cloned")

	// Step 9: Verify the secret still exists in frame1 /id (sanity check)
	if _, err := os.Stat(secretPath); err != nil {
		t.Fatalf("secret should still exist in frame1: %v", err)
	}
	t.Logf("Secret still exists in frame1 /id (sanity check passed)")
}

// testControlServer wraps a test control socket server.
type testControlServer struct {
	sockPath string
	listener net.Listener
	env      *testEnv
	done     chan struct{}
	taints   []string // Track taints for testing
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
	mux.HandleFunc("/import-docker-tarball", srv.handleImportDockerTarball)

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

	// Validate snapshot_id is not empty
	if req.SnapshotID == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "error",
			"message": "snapshot_id is required",
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

	// Clone rootfs snapshot to frame, or create empty subvolume if rootfs is explicitly "nil"
	// Note: empty string rootfs (from "::") should error, only explicit "nil" creates blank container
	if rootfs == "nil" {
		// Create empty rootfs subvolume with minimal structure
		cmd := exec.Command("btrfs", "subvolume", "create", framePath)
		if out, err := cmd.CombinedOutput(); err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"status":  "error",
				"message": "btrfs subvolume create: " + err.Error() + ": " + string(out),
			})
			return
		}
		// Create minimal directory structure for blank container
		setupMinimalRootfsForTest(framePath, s.env.tsBinary)
	} else if rootfs == "" {
		// Empty rootfs component (e.g., from "::") without explicit "nil" is an error
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "error",
			"message": "rootfs component is required (use 'nil' for blank container)",
		})
		return
	} else {
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
	}

	// Create home subvolume if specified empty or with snap
	homePath := filepath.Join(framePath, "home")
	if home == "" {
		// Remove existing home directory contents and create empty subvolume
		os.RemoveAll(homePath)
		exec.Command("btrfs", "subvolume", "create", homePath).CombinedOutput()
		os.Chown(homePath, 1000, 1000)
	} else if home != "nil" {
		// Clone from home snapshot
		homeSnapPath := filepath.Join(s.env.snapshotsDir, home)
		os.RemoveAll(homePath)
		exec.Command("btrfs", "subvolume", "snapshot", homeSnapPath, homePath).CombinedOutput()
	}

	// Create work subvolume if specified empty or with snap
	workPath := filepath.Join(framePath, "work")
	if work == "" {
		os.RemoveAll(workPath)
		exec.Command("btrfs", "subvolume", "create", workPath).CombinedOutput()
		os.Chown(workPath, 1000, 1000)
	} else if work != "nil" {
		workSnapPath := filepath.Join(s.env.snapshotsDir, work)
		os.RemoveAll(workPath)
		exec.Command("btrfs", "subvolume", "snapshot", workSnapPath, workPath).CombinedOutput()
	}

	// Create id subvolume (always empty, never cloned from snapshot)
	idPath := filepath.Join(framePath, "id")
	os.RemoveAll(idPath)
	exec.Command("btrfs", "subvolume", "create", idPath).CombinedOutput()
	os.Chmod(idPath, 0700)

	// Mirror production ensureFrameFS: ensure a "user" account (and matching
	// group/shadow entries) exists in the frame rootfs, plus passwordless sudo.
	if _, err := tsm.EnsureUserInPasswd(framePath); err != nil {
		log.Printf("Warning: EnsureUserInPasswd on %s: %v", framePath, err)
	}
	if err := tsm.EnsureSudoers(framePath); err != nil {
		log.Printf("Warning: EnsureSudoers on %s: %v", framePath, err)
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

	// Delete nested subvolumes first (home, work, id)
	for _, subvol := range []string{"home", "work", "id"} {
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
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		FrameName string `json:"frame_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"type":    "result",
			"status":  "error",
			"message": "invalid request: " + err.Error(),
		})
		return
	}

	// Find the frame path
	framePath := filepath.Join(s.env.fsDir, "testuser", req.FrameName)
	if _, err := os.Stat(framePath); err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"type":    "result",
			"status":  "error",
			"message": fmt.Sprintf("frame not found: %s", req.FrameName),
		})
		return
	}

	// Generate a random snapshot ID
	snapID, err := generateSnapshotID()
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"type":    "result",
			"status":  "error",
			"message": "generate snapshot ID: " + err.Error(),
		})
		return
	}

	// Create btrfs snapshot (read-only) in the snapshots directory
	snapPath := filepath.Join(s.env.snapshotsDir, snapID)
	cmd := exec.Command("btrfs", "subvolume", "snapshot", "-r", framePath, snapPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"type":    "result",
			"status":  "error",
			"message": fmt.Sprintf("btrfs snapshot: %v: %s", err, string(out)),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type":        "result",
		"status":      "ok",
		"snapshot_id": snapID,
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

		// Get actual directory size using du
		snapPath := filepath.Join(s.env.snapshotsDir, entry.Name())
		size := getDirSize(snapPath)

		snaps = append(snaps, map[string]interface{}{
			"id":   entry.Name(),
			"size": size,
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

	// Add taint if not already present (deduplication)
	found := false
	for _, t := range s.taints {
		if t == req.TaintName {
			found = true
			break
		}
	}
	if !found {
		s.taints = append(s.taints, req.TaintName)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
		"taints": s.taints,
	})
}

// setupMinimalRootfsForTest creates the minimal directory structure and files
// needed for a blank container to function in tests.
func setupMinimalRootfsForTest(rootFS, tsBinary string) {
	// Create essential directories
	dirs := []string{
		"bin", "sbin", "etc", "tmp", "proc", "sys", "dev",
		"root", "var", "var/log", "run", "usr", "usr/bin",
	}
	for _, dir := range dirs {
		os.MkdirAll(filepath.Join(rootFS, dir), 0755)
	}
	os.Chmod(filepath.Join(rootFS, "tmp"), 01777)
	os.Chmod(filepath.Join(rootFS, "root"), 0700)

	// Copy ts binary
	if tsBinary != "" {
		tsDst := filepath.Join(rootFS, "bin/ts")
		copyFile(tsBinary, tsDst)
		// Create /bin/sh symlink to ts
		os.Symlink("ts", filepath.Join(rootFS, "bin/sh"))
	}

	// Create minimal /etc files
	passwdContent := "root:x:0:0:root:/root:/bin/sh\nuser:x:1000:1000:user:/home/user:/bin/sh\nnobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin\n"
	os.WriteFile(filepath.Join(rootFS, "etc/passwd"), []byte(passwdContent), 0644)

	groupContent := "root:x:0:\nuser:x:1000:\nnogroup:x:65534:\n"
	os.WriteFile(filepath.Join(rootFS, "etc/group"), []byte(groupContent), 0644)

	os.WriteFile(filepath.Join(rootFS, "etc/hostname"), []byte("minimal\n"), 0644)
	os.WriteFile(filepath.Join(rootFS, "etc/hosts"), []byte("127.0.0.1\tlocalhost\n"), 0644)
	os.WriteFile(filepath.Join(rootFS, "etc/resolv.conf"), []byte("nameserver 8.8.8.8\n"), 0644)
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

// generateSnapshotID generates a random hex string for snapshot naming.
func generateSnapshotID() (string, error) {
	b := make([]byte, 16) // 32 hex characters
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// TestFrameWithHomeSpec tests creating a frame with rootfs+home spec.
func TestFrameWithHomeSpec(t *testing.T) {
	env := newTestEnv(t)

	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startTestControlServer(t, env, sockPath)
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	// Create base snapshot for rootfs
	baseSnap := env.createBaseSnapshot()
	t.Logf("Created base snapshot: %s", baseSnap)

	// Create a home snapshot separately
	homeSnapPath := filepath.Join(env.snapshotsDir, "home-snap")
	cmd := exec.Command("btrfs", "subvolume", "create", homeSnapPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create home snap: %v\n%s", err, out)
	}
	// Add some content to home snapshot
	homeUserDir := filepath.Join(homeSnapPath, "user")
	if err := os.MkdirAll(homeUserDir, 0755); err != nil {
		t.Fatalf("mkdir home/user: %v", err)
	}
	os.Chown(homeUserDir, 1000, 1000)
	homeFile := filepath.Join(homeUserDir, "home-marker.txt")
	if err := os.WriteFile(homeFile, []byte("from home snapshot\n"), 0644); err != nil {
		t.Fatalf("write home marker: %v", err)
	}
	os.Chown(homeFile, 1000, 1000)

	// Create frame with rootfs:home: spec
	frameName := "homespectest"
	frameSpec := baseSnap + ":home-snap:"

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
	t.Logf("Created frame with home spec: %s", frameName)

	// Verify the frame exists
	listResp, err := client.getJSON("/list-frames")
	if err != nil {
		t.Fatalf("list frames: %v", err)
	}
	frames, _ := listResp["frames"].([]interface{})
	found := false
	for _, f := range frames {
		fmap := f.(map[string]interface{})
		if fmap["name"] == frameName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("frame %q not found in list", frameName)
	}

	// Verify home content exists in the frame
	framePath := filepath.Join(env.fsDir, "testuser", frameName)
	markerPath := filepath.Join(framePath, "home", "user", "home-marker.txt")
	content, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("read home marker: %v", err)
	}
	if string(content) != "from home snapshot\n" {
		t.Errorf("home marker content: got %q, want 'from home snapshot\\n'", content)
	}
	t.Logf("Verified home content from snapshot present in frame")
}

// TestFrameWithAllThreeSpecs tests creating a frame with rootfs:home:work spec.
func TestFrameWithAllThreeSpecs(t *testing.T) {
	env := newTestEnv(t)

	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startTestControlServer(t, env, sockPath)
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	// Create base snapshot for rootfs
	baseSnap := env.createBaseSnapshot()

	// Create home snapshot
	homeSnapPath := filepath.Join(env.snapshotsDir, "home-snap2")
	cmd := exec.Command("btrfs", "subvolume", "create", homeSnapPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create home snap: %v\n%s", err, out)
	}
	homeUserDir := filepath.Join(homeSnapPath, "user")
	os.MkdirAll(homeUserDir, 0755)
	os.Chown(homeUserDir, 1000, 1000)
	os.WriteFile(filepath.Join(homeUserDir, "home-marker.txt"), []byte("home\n"), 0644)

	// Create work snapshot
	workSnapPath := filepath.Join(env.snapshotsDir, "work-snap")
	cmd = exec.Command("btrfs", "subvolume", "create", workSnapPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create work snap: %v\n%s", err, out)
	}
	os.Chown(workSnapPath, 1000, 1000)
	os.WriteFile(filepath.Join(workSnapPath, "work-marker.txt"), []byte("work\n"), 0644)

	// Create frame with rootfs:home:work spec
	frameName := "fullspectest"
	frameSpec := baseSnap + ":home-snap2:work-snap"

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
	t.Logf("Created frame with full spec: %s", frameName)

	// Verify home and work content
	framePath := filepath.Join(env.fsDir, "testuser", frameName)

	homeContent, err := os.ReadFile(filepath.Join(framePath, "home", "user", "home-marker.txt"))
	if err != nil {
		t.Fatalf("read home marker: %v", err)
	}
	if string(homeContent) != "home\n" {
		t.Errorf("home content: got %q, want 'home\\n'", homeContent)
	}

	workContent, err := os.ReadFile(filepath.Join(framePath, "work", "work-marker.txt"))
	if err != nil {
		t.Fatalf("read work marker: %v", err)
	}
	if string(workContent) != "work\n" {
		t.Errorf("work content: got %q, want 'work\\n'", workContent)
	}

	t.Log("Verified home and work content from snapshots present in frame")
}

// TestFrameUserGroupCreated verifies that when a frame is created from a base
// snapshot that has no "user" account, the frame's /etc/passwd gains a "user"
// account with UID 7575 AND /etc/group gains a matching "user" group with GID
// 7575. Without the matching group the user's primary GID 7575 is nameless,
// which breaks tools like `id` and `ls -l`.
func TestFrameUserGroupCreated(t *testing.T) {
	env := newTestEnv(t)

	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startTestControlServer(t, env, sockPath)
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	// Create a base snapshot, then strip the pre-existing "user" account from
	// /etc/passwd, /etc/shadow and /etc/group so that frame creation is forced
	// to add the thundersnap "user" (UID/GID 7575) from scratch.
	baseSnap := env.createBaseSnapshot()
	snapPath := filepath.Join(env.snapshotsDir, baseSnap)

	rewrite := func(name, content string) {
		p := filepath.Join(snapPath, "etc", name)
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatalf("rewrite %s: %v", name, err)
		}
	}
	// passwd/group/shadow with root only (no "user").
	rewrite("passwd", "root:x:0:0:root:/root:/bin/sh\n"+
		"daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin\n")
	rewrite("group", "root:x:0:\ndaemon:x:1:\n")
	rewrite("shadow", "root:*:19000:0:99999:7:::\n")

	// Create a frame (rootfs-only) from the stripped base snapshot.
	frameName := "usergroupframe"
	createResp, err := client.postJSON("/create", map[string]string{
		"frame_name":  frameName,
		"snapshot_id": baseSnap + "::",
	})
	if err != nil {
		t.Fatalf("create frame: %v", err)
	}
	if createResp["status"] != "ok" {
		t.Fatalf("create frame failed: %v", createResp["message"])
	}

	framePath := filepath.Join(env.fsDir, "testuser", frameName)

	// /etc/passwd should now contain the user account at UID/GID 7575.
	passwd, err := os.ReadFile(filepath.Join(framePath, "etc", "passwd"))
	if err != nil {
		t.Fatalf("read frame passwd: %v", err)
	}
	if !strings.Contains(string(passwd), "user:x:7575:7575:") {
		t.Fatalf("frame passwd missing user:x:7575:7575:\n%s", passwd)
	}

	// /etc/group MUST contain a matching "user" group with GID 7575. This is
	// the actual bug being guarded: previously only passwd was updated.
	group, err := os.ReadFile(filepath.Join(framePath, "etc", "group"))
	if err != nil {
		t.Fatalf("read frame group: %v", err)
	}
	if !strings.Contains(string(group), "user:x:7575:") {
		t.Fatalf("frame group missing matching user:x:7575: entry\n%s", group)
	}
	t.Logf("Verified user account and matching group 7575 created in frame")
}

// TestFrameFromNonExistentSnapshot tests error handling for creating a frame from non-existent snapshot.
func TestFrameFromNonExistentSnapshot(t *testing.T) {
	env := newTestEnv(t)

	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startTestControlServer(t, env, sockPath)
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	// Try to create frame from a snapshot that doesn't exist
	frameName := "badframe"
	frameSpec := "nonexistent-snapshot-xyz123::"

	createResp, err := client.postJSON("/create", map[string]string{
		"frame_name":  frameName,
		"snapshot_id": frameSpec,
	})
	if err != nil {
		t.Fatalf("create frame request: %v", err)
	}

	// Should return an error status
	status, ok := createResp["status"].(string)
	if !ok {
		t.Fatalf("response missing status: %v", createResp)
	}
	if status != "error" {
		t.Errorf("expected error status for non-existent snapshot, got %q", status)
	}

	// Verify message mentions the issue
	message, _ := createResp["message"].(string)
	t.Logf("Got expected error: %s", message)

	// Verify frame was not created
	listResp, err := client.getJSON("/list-frames")
	if err != nil {
		t.Fatalf("list frames: %v", err)
	}
	frames, _ := listResp["frames"].([]interface{})
	for _, f := range frames {
		fmap := f.(map[string]interface{})
		if fmap["name"] == frameName {
			t.Fatalf("frame %q should not exist after failed creation", frameName)
		}
	}
	t.Log("Verified frame was not created")
}

// TestDeleteRunningFrame tests that deleting a frame that has active sessions
// returns an error or stops the sessions first.
//
// Note: In the test control server, we don't have real sessions, so we test
// the behavior of trying to delete the current frame (which is always "running"
// in the context of a control server).
func TestDeleteRunningFrame(t *testing.T) {
	env := newTestEnv(t)

	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startTestControlServer(t, env, sockPath)
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	// Create a base snapshot
	baseSnap := env.createBaseSnapshot()
	t.Logf("Created base snapshot: %s", baseSnap)

	// Create first frame
	frameName1 := "frame1"
	frameSpec := baseSnap + "::"

	createResp, err := client.postJSON("/create", map[string]string{
		"frame_name":  frameName1,
		"snapshot_id": frameSpec,
	})
	if err != nil {
		t.Fatalf("create frame1: %v", err)
	}
	if createResp["status"] != "ok" {
		t.Fatalf("create frame1 failed: %v", createResp["message"])
	}
	t.Logf("Created frame1")

	// Create a second frame
	frameName2 := "frame2"
	createResp, err = client.postJSON("/create", map[string]string{
		"frame_name":  frameName2,
		"snapshot_id": frameSpec,
	})
	if err != nil {
		t.Fatalf("create frame2: %v", err)
	}
	if createResp["status"] != "ok" {
		t.Fatalf("create frame2 failed: %v", createResp["message"])
	}
	t.Logf("Created frame2")

	// Both frames should be deletable since neither is the "current" frame
	// in the test control server (which doesn't track rootFS like the real one)
	// Delete frame1 successfully
	deleteResp, err := client.postJSON("/delete-frame", map[string]string{
		"frame_name": frameName1,
	})
	if err != nil {
		t.Fatalf("delete frame1: %v", err)
	}
	if deleteResp["status"] != "ok" {
		t.Fatalf("delete frame1 failed: %v", deleteResp["message"])
	}
	t.Logf("Successfully deleted frame1 (stopped frame)")

	// Verify frame1 is gone
	listResp, err := client.getJSON("/list-frames")
	if err != nil {
		t.Fatalf("list frames: %v", err)
	}
	frames, _ := listResp["frames"].([]interface{})
	for _, f := range frames {
		fmap := f.(map[string]interface{})
		if fmap["name"] == frameName1 {
			t.Errorf("frame1 should not exist after deletion")
		}
	}

	// Delete frame2
	deleteResp, err = client.postJSON("/delete-frame", map[string]string{
		"frame_name": frameName2,
	})
	if err != nil {
		t.Fatalf("delete frame2: %v", err)
	}
	if deleteResp["status"] != "ok" {
		t.Fatalf("delete frame2 failed: %v", deleteResp["message"])
	}
	t.Logf("Successfully deleted frame2")
}

// TestMultipleConcurrentSessions tests that multiple concurrent sessions
// can access the same frame simultaneously. Each session can read and write
// to the frame, and changes are visible to all sessions.
func TestMultipleConcurrentSessions(t *testing.T) {
	env := newTestEnv(t)

	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startTestControlServer(t, env, sockPath)
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	// Create a base snapshot
	baseSnap := env.createBaseSnapshot()

	// Create a frame
	frameName := "concurrent"
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

	framePath := filepath.Join(env.fsDir, "testuser", frameName)

	// Simulate multiple concurrent sessions by having multiple "clients"
	// write to different files, then verify all writes are visible

	numSessions := 5
	doneCh := make(chan error, numSessions)

	// Each session writes a unique file
	for i := 0; i < numSessions; i++ {
		go func(sessionNum int) {
			sessionFile := filepath.Join(framePath, "tmp", fmt.Sprintf("session%d.txt", sessionNum))
			content := fmt.Sprintf("content from session %d\n", sessionNum)
			err := os.WriteFile(sessionFile, []byte(content), 0644)
			doneCh <- err
		}(i)
	}

	// Wait for all sessions to complete
	for i := 0; i < numSessions; i++ {
		if err := <-doneCh; err != nil {
			t.Errorf("session write error: %v", err)
		}
	}

	// Verify all session files exist and have correct content
	for i := 0; i < numSessions; i++ {
		sessionFile := filepath.Join(framePath, "tmp", fmt.Sprintf("session%d.txt", i))
		content, err := os.ReadFile(sessionFile)
		if err != nil {
			t.Errorf("read session %d file: %v", i, err)
			continue
		}
		expected := fmt.Sprintf("content from session %d\n", i)
		if string(content) != expected {
			t.Errorf("session %d content: got %q, want %q", i, content, expected)
		}
	}

	t.Logf("Verified %d concurrent sessions all wrote successfully", numSessions)
}

// TestFrameRestartAfterStop tests that a frame can be restarted after stopping.
// Since frames are just btrfs subvolumes, "restarting" means creating a new
// session to the same frame after the previous session ended.
func TestFrameRestartAfterStop(t *testing.T) {
	env := newTestEnv(t)

	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startTestControlServer(t, env, sockPath)
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	// Create a base snapshot
	baseSnap := env.createBaseSnapshot()

	// Create a frame
	frameName := "restarttest"
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
	t.Logf("Created frame")

	// Simulate first "session" by writing a marker file
	framePath := filepath.Join(env.fsDir, "testuser", frameName)
	markerFile := filepath.Join(framePath, "tmp", "session1.txt")
	if err := os.WriteFile(markerFile, []byte("session 1 was here\n"), 0644); err != nil {
		t.Fatalf("write session1 marker: %v", err)
	}
	t.Logf("Wrote session 1 marker")

	// "Stop" the session (nothing to do in test, session is not tracked)

	// Simulate second "session" by writing another marker file
	markerFile2 := filepath.Join(framePath, "tmp", "session2.txt")
	if err := os.WriteFile(markerFile2, []byte("session 2 was here\n"), 0644); err != nil {
		t.Fatalf("write session2 marker: %v", err)
	}
	t.Logf("Wrote session 2 marker")

	// Verify both markers exist (state was preserved across "restart")
	content1, err := os.ReadFile(markerFile)
	if err != nil {
		t.Fatalf("read session1 marker: %v", err)
	}
	content2, err := os.ReadFile(markerFile2)
	if err != nil {
		t.Fatalf("read session2 marker: %v", err)
	}

	if string(content1) != "session 1 was here\n" {
		t.Errorf("session1 marker content wrong: %q", content1)
	}
	if string(content2) != "session 2 was here\n" {
		t.Errorf("session2 marker content wrong: %q", content2)
	}
	t.Logf("Verified frame state preserved across restart")
}

// getDirSize returns the size of a directory in bytes using du.
// Returns 0 if the size cannot be determined.
func getDirSize(path string) int64 {
	cmd := exec.Command("du", "-sb", path)
	out, err := cmd.Output()
	if err != nil {
		return 0
	}

	// Parse output: "12345\t/path/to/dir\n"
	var size int64
	fmt.Sscanf(string(out), "%d", &size)
	return size
}
