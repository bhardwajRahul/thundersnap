// full_e2e_test.go contains a comprehensive end-to-end test for thundersnap.
//
// This test exercises the full flow without tsnet:
//   - Download a Docker image to create a base snap
//   - Create a frame from the snap with empty home/work
//   - Enter the frame via a container session (chroot)
//   - Verify that `ts snap` inside returns deterministic results
//   - Verify /home and /work ownership and permissions
//   - Verify apt-get update works
//
// The test requires root + btrfs. It skips otherwise.
// The test uses a local Unix socket instead of tsnet.
package main

import (
	"bufio"
	"fmt"
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

// testEnv represents a thundersnapd test environment.
type testEnv struct {
	fsDir        string
	snapshotsDir string
	libexecDir   string
}

func (te *testEnv) cleanup() {
	// Clean up any btrfs subvolumes
	cleanupAllSubvolumes(te.fsDir)
	cleanupAllSubvolumes(te.snapshotsDir)
}

func cleanupAllSubvolumes(dir string) {
	// Walk and delete subvolumes
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if e.IsDir() {
			// Try to delete nested subvolumes first
			nested, _ := os.ReadDir(path)
			for _, n := range nested {
				npath := filepath.Join(path, n.Name())
				if n.IsDir() {
					// Try nested nested (e.g., fs/user/frame/home)
					nn, _ := os.ReadDir(npath)
					for _, nnn := range nn {
						if nnn.IsDir() {
							exec.Command("btrfs", "subvolume", "delete", filepath.Join(npath, nnn.Name())).Run()
						}
					}
					exec.Command("btrfs", "subvolume", "delete", npath).Run()
				}
			}
			exec.Command("btrfs", "subvolume", "delete", path).Run()
		}
	}
}

func createTestEnv(t *testing.T, root, name string) *testEnv {
	t.Helper()

	baseDir := filepath.Join(root, name)
	fsDir := filepath.Join(baseDir, "fs")
	snapshotsDir := filepath.Join(baseDir, "snapshots")
	libexecDir := filepath.Join(baseDir, "libexec")

	for _, d := range []string{fsDir, snapshotsDir, libexecDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("creating directory %s: %v", d, err)
		}
	}

	return &testEnv{
		fsDir:        fsDir,
		snapshotsDir: snapshotsDir,
		libexecDir:   libexecDir,
	}
}

func requireBtrfsRoot2(t *testing.T) string {
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

func buildTsBinaryForTest(t *testing.T, root string) string {
	t.Helper()

	// Find the ts2 root
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	// We're in cmd/thundersnapd, go up to repo root
	repoRoot := filepath.Dir(filepath.Dir(cwd))
	tsBinary := filepath.Join(root, "ts")

	cmd := exec.Command("go", "build", "-o", tsBinary, "./cmd/ts")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("building ts binary: %v\noutput: %s", err, string(out))
	}

	return tsBinary
}

func downloadDockerImageForTest(t *testing.T, env *testEnv, imageRef string) string {
	t.Helper()

	// Set the flags for this environment
	setFlagsForTest(env.fsDir, env.snapshotsDir, env.libexecDir)
	defer resetFlagsForTest()

	// Download the Docker image
	snapID, cached, err := downloadDockerImage(imageRef, nil)
	if err != nil {
		t.Fatalf("downloadDockerImage: %v", err)
	}
	t.Logf("Downloaded %s: snap=%s cached=%v", imageRef, snapID, cached)

	return snapID
}

func createFrameForTest(t *testing.T, env *testEnv, frameName, frameSpec string) string {
	t.Helper()

	// Set the flags for this environment
	setFlagsForTest(env.fsDir, env.snapshotsDir, env.libexecDir)
	defer resetFlagsForTest()

	// Create frame path
	framePath := filepath.Join(env.fsDir, "testuser", frameName)

	// Parse frame spec
	rootfsSnap, homeSnap, workSnap := parseFrameSpec(frameSpec)

	// Create the frame
	if err := createFrame(framePath, rootfsSnap, homeSnap, workSnap, "container"); err != nil {
		t.Fatalf("createFrame: %v", err)
	}

	return framePath
}

func copyTsBinaryToFrameForTest(t *testing.T, tsBinary, framePath string) {
	t.Helper()

	// Copy ts to /bin/ts in the frame
	dst := filepath.Join(framePath, "bin", "ts")
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(dst), err)
	}

	// Use cp --reflink=auto for COW copy
	cmd := exec.Command("cp", "--reflink=auto", tsBinary, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cp ts binary: %v\noutput: %s", err, string(out))
	}

	if err := os.Chmod(dst, 0755); err != nil {
		t.Fatalf("chmod ts: %v", err)
	}
}

// frameCtrlServer is a control server bound to a specific frame.
type frameCtrlServer struct {
	sockPath string
	listener net.Listener
	rootFS   string
	done     chan struct{}
}

func (fcs *frameCtrlServer) Close() {
	fcs.listener.Close()
	<-fcs.done
	os.Remove(fcs.sockPath)
}

func startFrameControlSocket(t *testing.T, env *testEnv, framePath string) *frameCtrlServer {
	t.Helper()

	// Set the flags for this environment
	setFlagsForTest(env.fsDir, env.snapshotsDir, env.libexecDir)
	// Note: don't reset flags here, we need them to stay set for the server

	sockPath := filepath.Join(framePath, "thunder.sock")

	// Remove existing socket
	os.Remove(sockPath)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen %s: %v", sockPath, err)
	}

	if err := os.Chmod(sockPath, 0666); err != nil {
		t.Logf("warning: chmod socket: %v", err)
	}

	fcs := &frameCtrlServer{
		sockPath: sockPath,
		listener: ln,
		rootFS:   framePath,
		done:     make(chan struct{}),
	}

	// Create handler
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", handlePing)
	mux.HandleFunc("/snap", makeSnapHandler(framePath))

	// Start server goroutine
	go func() {
		defer close(fcs.done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleTestControlConn(conn, mux)
		}
	}()

	return fcs
}

func handleTestControlConn(conn net.Conn, handler http.Handler) {
	defer conn.Close()

	// Read vsock handshake
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return
	}

	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "CONNECT ") {
		fmt.Fprintf(conn, "ERROR invalid handshake\n")
		return
	}

	// Send OK
	fmt.Fprintf(conn, "OK 5223\n")

	// Handle HTTP request
	req, err := http.ReadRequest(reader)
	if err != nil {
		return
	}

	rw := newControlResponseWriter(conn)
	handler.ServeHTTP(rw, req)
	rw.finish()
}

func runTsSnapInFrameForTest(t *testing.T, tsBinary, framePath, sockPath string) string {
	t.Helper()

	// Run the ts binary with the socket path
	cmd := exec.Command(tsBinary, "--sock", sockPath, "snap")
	cmd.Dir = framePath
	cmd.Env = []string{
		"PATH=/bin:/usr/bin",
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ts snap failed: %v\noutput: %s", err, string(out))
	}

	// Output might have progress on stderr, snap ID on stdout
	// Split by newlines and take the last non-empty line
	lines := strings.Split(string(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			return line
		}
	}

	return strings.TrimSpace(string(out))
}

func verifyOwnership(t *testing.T, framePath, subdir string, expectedUID, expectedGID int) {
	t.Helper()

	path := filepath.Join(framePath, subdir)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	stat := info.Sys().(*syscall.Stat_t)
	if int(stat.Uid) != expectedUID {
		t.Errorf("%s uid: got %d, want %d", subdir, stat.Uid, expectedUID)
	}
	if int(stat.Gid) != expectedGID {
		t.Errorf("%s gid: got %d, want %d", subdir, stat.Gid, expectedGID)
	}

	// Also verify permissions (should be 755 for directories)
	mode := info.Mode()
	if mode.Perm() != 0755 {
		t.Errorf("%s permissions: got %o, want 755", subdir, mode.Perm())
	}
}

func runAptGetUpdateForTest(t *testing.T, framePath string) {
	t.Helper()

	// We need to actually chroot and run apt-get
	// Use unshare to create isolated namespaces
	cmd := exec.Command("unshare", "--mount", "--pid", "--fork", "--root="+framePath,
		"/bin/sh", "-c", "mount -t proc proc /proc && apt-get update -qq")
	cmd.Env = []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"DEBIAN_FRONTEND=noninteractive",
	}

	// Give it a timeout
	done := make(chan error, 1)
	go func() {
		out, err := cmd.CombinedOutput()
		if err != nil {
			done <- fmt.Errorf("apt-get update failed: %v\noutput: %s", err, string(out))
		} else {
			done <- nil
		}
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(60 * time.Second):
		cmd.Process.Kill()
		t.Fatal("apt-get update timed out after 60s")
	}
}

// TestFullE2EDockerToFrame tests the complete flow:
// 1. Download debian:12.14-slim via ts download-docker
// 2. Create a frame with rootfs:: (empty home/work)
// 3. SSH into the frame and verify:
//   - ts snap returns a deterministic ID with nil home and nil work
//   - /home and /work are owned by uid 1000
//   - apt-get update succeeds
func TestFullE2EDockerToFrame(t *testing.T) {
	root := requireBtrfsRoot2(t)

	// Set up two test environments (simulating two machines)
	env1 := createTestEnv(t, root, "instance1")
	env2 := createTestEnv(t, root, "instance2")
	defer env1.cleanup()
	defer env2.cleanup()

	// Build the ts binary
	tsBinary := buildTsBinaryForTest(t, root)

	// Test 1: Download Docker image on both instances, expect same snap ID
	t.Log("Downloading Docker image on instance 1...")
	snapID1 := downloadDockerImageForTest(t, env1, "debian:12.14-slim")
	t.Logf("Instance 1 snap ID: %s", snapID1)

	t.Log("Downloading Docker image on instance 2...")
	snapID2 := downloadDockerImageForTest(t, env2, "debian:12.14-slim")
	t.Logf("Instance 2 snap ID: %s", snapID2)

	if snapID1 != snapID2 {
		t.Errorf("Snap IDs differ: instance1=%s, instance2=%s", snapID1, snapID2)
	}

	// Test 2: Create frame with empty home/work on both instances
	frameName := "testframe"
	frameSpec := snapID1 + "::"

	t.Log("Creating frame on instance 1...")
	framePath1 := createFrameForTest(t, env1, frameName, frameSpec)
	t.Logf("Instance 1 frame path: %s", framePath1)

	t.Log("Creating frame on instance 2...")
	framePath2 := createFrameForTest(t, env2, frameName, frameSpec)
	t.Logf("Instance 2 frame path: %s", framePath2)

	// Test 3: Copy ts binary into frames
	copyTsBinaryToFrameForTest(t, tsBinary, framePath1)
	copyTsBinaryToFrameForTest(t, tsBinary, framePath2)

	// Test 4: Start control socket inside each frame
	ctrl1 := startFrameControlSocket(t, env1, framePath1)
	ctrl2 := startFrameControlSocket(t, env2, framePath2)
	defer ctrl1.Close()
	defer ctrl2.Close()

	// Test 5: Run ts snap inside each frame and verify results match
	t.Log("Running ts snap in instance 1...")
	result1 := runTsSnapInFrameForTest(t, tsBinary, framePath1, ctrl1.sockPath)
	t.Logf("Instance 1 ts snap result: %s", result1)

	t.Log("Running ts snap in instance 2...")
	result2 := runTsSnapInFrameForTest(t, tsBinary, framePath2, ctrl2.sockPath)
	t.Logf("Instance 2 ts snap result: %s", result2)

	if result1 != result2 {
		t.Errorf("ts snap results differ: instance1=%s, instance2=%s", result1, result2)
	}

	// Parse frame spec to verify nil home and nil work
	parts := strings.Split(result1, ":")
	if len(parts) != 3 {
		t.Fatalf("Expected frame spec with 3 parts, got %d: %s", len(parts), result1)
	}
	if parts[1] != "nil" {
		t.Errorf("Expected nil home, got %s", parts[1])
	}
	if parts[2] != "nil" {
		t.Errorf("Expected nil work, got %s", parts[2])
	}

	// Test 6: Verify /home and /work ownership
	verifyOwnership(t, framePath1, "home", 1000, 1000)
	verifyOwnership(t, framePath1, "work", 1000, 1000)
	verifyOwnership(t, framePath2, "home", 1000, 1000)
	verifyOwnership(t, framePath2, "work", 1000, 1000)

	// Test 7: Verify apt-get update works in frame
	t.Log("Running apt-get update in instance 1...")
	runAptGetUpdateForTest(t, framePath1)
	t.Log("apt-get update succeeded")
}

// TestE2ETwoInstancesSameSnap is a simpler test that just verifies
// two instances downloading the same Docker image get the same snap ID.
func TestE2ETwoInstancesSameSnap(t *testing.T) {
	root := requireBtrfsRoot2(t)

	env1 := createTestEnv(t, root, "inst1")
	env2 := createTestEnv(t, root, "inst2")
	defer env1.cleanup()
	defer env2.cleanup()

	// Download on first instance
	setFlagsForTest(env1.fsDir, env1.snapshotsDir, env1.libexecDir)
	snap1, _, err := downloadDockerImage("debian:12.14-slim", nil)
	if err != nil {
		t.Fatalf("download on inst1: %v", err)
	}
	resetFlagsForTest()

	// Download on second instance
	setFlagsForTest(env2.fsDir, env2.snapshotsDir, env2.libexecDir)
	snap2, _, err := downloadDockerImage("debian:12.14-slim", nil)
	if err != nil {
		t.Fatalf("download on inst2: %v", err)
	}
	resetFlagsForTest()

	if snap1 != snap2 {
		t.Errorf("snap IDs differ: %s vs %s", snap1, snap2)
	} else {
		t.Logf("Both instances got same snap ID: %s", snap1)
	}
}

// TestE2EFrameHomeWorkOwnership verifies that when creating a frame with
// empty home/work (via rootfs::), the directories are owned by uid 1000.
func TestE2EFrameHomeWorkOwnership(t *testing.T) {
	root := requireBtrfsRoot2(t)

	env := createTestEnv(t, root, "test")
	defer env.cleanup()

	// Download a minimal image
	setFlagsForTest(env.fsDir, env.snapshotsDir, env.libexecDir)
	defer resetFlagsForTest()

	snapID, _, err := downloadDockerImage("debian:12.14-slim", nil)
	if err != nil {
		t.Fatalf("download: %v", err)
	}

	// Create frame with empty home/work
	framePath := filepath.Join(env.fsDir, "testuser", "testframe")
	if err := createFrame(framePath, snapID, "", "", "container"); err != nil {
		t.Fatalf("createFrame: %v", err)
	}

	// Verify home exists and is a subvolume with correct ownership
	homePath := filepath.Join(framePath, "home")
	info, err := os.Stat(homePath)
	if err != nil {
		t.Fatalf("stat home: %v", err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if stat.Uid != 1000 || stat.Gid != 1000 {
		t.Errorf("home ownership: got %d:%d, want 1000:1000", stat.Uid, stat.Gid)
	}

	// Verify work exists and is a subvolume with correct ownership
	workPath := filepath.Join(framePath, "work")
	info, err = os.Stat(workPath)
	if err != nil {
		t.Fatalf("stat work: %v", err)
	}
	stat = info.Sys().(*syscall.Stat_t)
	if stat.Uid != 1000 || stat.Gid != 1000 {
		t.Errorf("work ownership: got %d:%d, want 1000:1000", stat.Uid, stat.Gid)
	}

	// Verify both are btrfs subvolumes
	if !isSubvolume(homePath) {
		t.Error("home is not a btrfs subvolume")
	}
	if !isSubvolume(workPath) {
		t.Error("work is not a btrfs subvolume")
	}
}

// TestE2ESnapDeterministic verifies that snapping an identical frame
// produces the same snap ID.
func TestE2ESnapDeterministic(t *testing.T) {
	root := requireBtrfsRoot2(t)

	env1 := createTestEnv(t, root, "inst1")
	env2 := createTestEnv(t, root, "inst2")
	defer env1.cleanup()
	defer env2.cleanup()

	// Download same image on both instances
	setFlagsForTest(env1.fsDir, env1.snapshotsDir, env1.libexecDir)
	snapID1, _, err := downloadDockerImage("debian:12.14-slim", nil)
	if err != nil {
		t.Fatalf("download on inst1: %v", err)
	}
	resetFlagsForTest()

	setFlagsForTest(env2.fsDir, env2.snapshotsDir, env2.libexecDir)
	snapID2, _, err := downloadDockerImage("debian:12.14-slim", nil)
	if err != nil {
		t.Fatalf("download on inst2: %v", err)
	}
	resetFlagsForTest()

	if snapID1 != snapID2 {
		t.Errorf("base snap IDs differ: %s vs %s", snapID1, snapID2)
	}

	// Create identical frames
	setFlagsForTest(env1.fsDir, env1.snapshotsDir, env1.libexecDir)
	frame1 := filepath.Join(env1.fsDir, "user", "frame")
	if err := createFrame(frame1, snapID1, "", "", ""); err != nil {
		t.Fatalf("createFrame inst1: %v", err)
	}
	resetFlagsForTest()

	setFlagsForTest(env2.fsDir, env2.snapshotsDir, env2.libexecDir)
	frame2 := filepath.Join(env2.fsDir, "user", "frame")
	if err := createFrame(frame2, snapID2, "", "", ""); err != nil {
		t.Fatalf("createFrame inst2: %v", err)
	}
	resetFlagsForTest()

	// Snap both frames
	setFlagsForTest(env1.fsDir, env1.snapshotsDir, env1.libexecDir)
	newSnap1, err := createSnapshot(frame1, nil, false)
	if err != nil {
		t.Fatalf("createSnapshot inst1: %v", err)
	}
	resetFlagsForTest()

	setFlagsForTest(env2.fsDir, env2.snapshotsDir, env2.libexecDir)
	newSnap2, err := createSnapshot(frame2, nil, false)
	if err != nil {
		t.Fatalf("createSnapshot inst2: %v", err)
	}
	resetFlagsForTest()

	// Both should produce identical frame specs
	if newSnap1 != newSnap2 {
		t.Errorf("frame snap specs differ: %s vs %s", newSnap1, newSnap2)
	}

	// Parse and verify nil home/work
	parts1 := strings.Split(newSnap1, ":")
	if len(parts1) == 3 {
		if parts1[1] != "nil" {
			t.Errorf("inst1 home not nil: %s", parts1[1])
		}
		if parts1[2] != "nil" {
			t.Errorf("inst1 work not nil: %s", parts1[2])
		}
	}

	t.Logf("Both instances produced identical snap: %s", newSnap1)
}

// TestE2EDownloadSnap verifies the mesh download-snap functionality:
// 1. Download a Docker image on instance 1
// 2. Create a frame and add a file
// 3. Snap the frame
// 4. Start an HTTP server on instance 1 to serve /bupdate/
// 5. Download the snap on instance 2 via mesh
// 6. Snap the result on instance 2
// 7. Verify both snaps have the same ID
func TestE2EDownloadSnap(t *testing.T) {
	root := requireBtrfsRoot2(t)

	env1 := createTestEnv(t, root, "inst1")
	env2 := createTestEnv(t, root, "inst2")
	defer env1.cleanup()
	defer env2.cleanup()

	// Step 1: Download Docker image on instance 1 only
	t.Log("Downloading Docker image on instance 1...")
	setFlagsForTest(env1.fsDir, env1.snapshotsDir, env1.libexecDir)
	baseSnap, _, err := downloadDockerImage("debian:12.14-slim", nil)
	if err != nil {
		t.Fatalf("download on inst1: %v", err)
	}
	t.Logf("Base snap ID: %s", baseSnap)

	// Step 2: Create a frame on instance 1
	t.Log("Creating frame on instance 1...")
	frame1 := filepath.Join(env1.fsDir, "user", "frame")
	if err := createFrame(frame1, baseSnap, "", "", ""); err != nil {
		t.Fatalf("createFrame inst1: %v", err)
	}

	// Add a unique file to the frame
	testFile := filepath.Join(frame1, "home", "testfile.txt")
	testContent := "e2e test content " + time.Now().String()
	if err := os.WriteFile(testFile, []byte(testContent), 0644); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	// Step 3: Snap the frame
	t.Log("Snapping frame on instance 1...")
	snapSpec1, err := createSnapshot(frame1, nil, false)
	if err != nil {
		t.Fatalf("createSnapshot inst1: %v", err)
	}
	t.Logf("Snap spec: %s", snapSpec1)
	resetFlagsForTest()

	// Parse the frame spec to get the rootfs ID
	rootfsSnap, homeSnap, _ := parseFrameSpec(snapSpec1)
	t.Logf("rootfs=%s home=%s", rootfsSnap, homeSnap)

	// Step 4: Start HTTP server on instance 1 to serve /bupdate/
	t.Log("Starting bupdate server on instance 1...")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	serverAddr := ln.Addr().String()
	t.Logf("Server listening on %s", serverAddr)

	// Create HTTP server with bupdate endpoint
	mux := http.NewServeMux()
	bfs := &bupdateFileServer{root: env1.snapshotsDir}
	mux.Handle("/bupdate/", http.StripPrefix("/bupdate", bfs))
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	// Step 5: Configure mesh state for instance 2 and download
	t.Log("Downloading snap on instance 2 via mesh...")
	setFlagsForTest(env2.fsDir, env2.snapshotsDir, env2.libexecDir)

	// Set up a temporary mesh state pointing to instance 1
	oldMeshState := globalMeshState
	globalMeshState = newMeshState("localhost")
	globalMeshState.recordPeer(MeshPing{
		URL:      "http://" + serverAddr,
		Hostname: "inst1",
	})
	defer func() { globalMeshState = oldMeshState }()

	// Download the rootfs snap
	t.Logf("Downloading rootfs snap %s...", rootfsSnap)
	result, err := doDownloadSnap(rootfsSnap, nil, false)
	if err != nil {
		t.Fatalf("download rootfs snap: %v", err)
	}
	t.Logf("Downloaded to %s (already had: %v)", result.SnapshotPath, result.AlreadyExists)

	// Download the home snap if it exists
	if homeSnap != "" && homeSnap != "nil" {
		t.Logf("Downloading home snap %s...", homeSnap)
		result, err = doDownloadSnap(homeSnap, nil, false)
		if err != nil {
			t.Fatalf("download home snap: %v", err)
		}
		t.Logf("Downloaded home to %s", result.SnapshotPath)
	}

	// Step 6: Create the same frame structure on instance 2 and snap it
	t.Log("Creating frame on instance 2 from downloaded snap...")
	frame2 := filepath.Join(env2.fsDir, "user", "frame")
	homeSnapArg := homeSnap
	if homeSnapArg == "nil" {
		homeSnapArg = ""
	}
	if err := createFrame(frame2, rootfsSnap, homeSnapArg, "", ""); err != nil {
		t.Fatalf("createFrame inst2: %v", err)
	}

	// Check that the test file exists in the downloaded frame
	testFile2 := filepath.Join(frame2, "home", "testfile.txt")
	data, err := os.ReadFile(testFile2)
	if err != nil {
		t.Fatalf("read test file on inst2: %v", err)
	}
	if string(data) != testContent {
		t.Errorf("test file content mismatch: got %q, want %q", string(data), testContent)
	}

	// Snap the frame on instance 2
	t.Log("Snapping frame on instance 2...")
	snapSpec2, err := createSnapshot(frame2, nil, false)
	if err != nil {
		t.Fatalf("createSnapshot inst2: %v", err)
	}
	t.Logf("Snap spec inst2: %s", snapSpec2)
	resetFlagsForTest()

	// Step 7: Verify both snap specs match
	if snapSpec1 != snapSpec2 {
		t.Errorf("snap specs differ: inst1=%s, inst2=%s", snapSpec1, snapSpec2)
	} else {
		t.Logf("SUCCESS: Both instances have same snap spec: %s", snapSpec1)
	}
}
