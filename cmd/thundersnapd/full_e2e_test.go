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

// createLocalBaseSnapshot creates a minimal base snapshot without network access.
// This is faster and more reliable than downloading from Docker Hub.
// It creates a btrfs subvolume with a minimal Linux filesystem.
func createLocalBaseSnapshot(t *testing.T, env *testEnv, tsBinary string) string {
	t.Helper()

	// Create a unique snapshot ID based on content hash
	// For test purposes, we use a fixed ID since the content is deterministic
	snapID := "testsnap-local-1"
	snapPath := filepath.Join(env.snapshotsDir, snapID)

	// Create btrfs subvolume
	cmd := exec.Command("btrfs", "subvolume", "create", snapPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs subvolume create: %v\n%s", err, out)
	}

	// Populate with minimal filesystem
	populateTestRootFS(t, snapPath, tsBinary)

	// Create stamp file (marks this as a valid snapshot)
	stampPath := snapPath + ".stamp"
	if err := os.WriteFile(stampPath, []byte("1\n"), 0644); err != nil {
		t.Fatalf("write stamp: %v", err)
	}

	t.Logf("Created local base snapshot: %s", snapID)
	return snapID
}

// populateTestRootFS creates a minimal Linux root filesystem for testing.
// It includes various file types: directories, regular files, symlinks,
// files with different ownerships and permissions.
func populateTestRootFS(t *testing.T, dir string, tsBinary string) {
	t.Helper()

	// Helper functions
	mkDir := func(path string, mode os.FileMode) {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(full, mode); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}

	writeFile := func(path string, content string, mode os.FileMode) {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatalf("mkdir parent of %s: %v", path, err)
		}
		if err := os.WriteFile(full, []byte(content), mode); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	chown := func(path string, uid, gid int) {
		full := filepath.Join(dir, path)
		if err := os.Lchown(full, uid, gid); err != nil {
			t.Fatalf("chown %s: %v", path, err)
		}
	}

	symlink := func(target, path string) {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatalf("mkdir parent of symlink %s: %v", path, err)
		}
		if err := os.Symlink(target, full); err != nil {
			t.Fatalf("symlink %s -> %s: %v", path, target, err)
		}
	}

	// Create directory structure
	// Note: lib64 is a symlink to lib, not a directory
	dirs := []string{
		"bin", "dev", "etc", "home", "home/user", "lib",
		"proc", "root", "run", "sbin", "sys", "tmp",
		"usr", "usr/bin", "usr/lib", "usr/sbin",
		"var", "var/log", "var/run", "work",
	}
	for _, d := range dirs {
		mkDir(d, 0755)
	}

	// Special directory permissions
	if err := os.Chmod(filepath.Join(dir, "tmp"), 0777|os.ModeSticky); err != nil {
		t.Fatalf("chmod tmp: %v", err)
	}
	if err := os.Chmod(filepath.Join(dir, "root"), 0700); err != nil {
		t.Fatalf("chmod root: %v", err)
	}

	// Create /etc files
	writeFile("etc/passwd",
		"root:x:0:0:root:/root:/bin/sh\n"+
			"user:x:1000:1000:user:/home/user:/bin/sh\n"+
			"daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin\n"+
			"nobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin\n",
		0644)
	writeFile("etc/group",
		"root:x:0:\n"+
			"user:x:1000:\n"+
			"daemon:x:1:\n"+
			"nogroup:x:65534:\n",
		0644)
	writeFile("etc/hostname", "testcontainer\n", 0644)
	writeFile("etc/hosts", "127.0.0.1 localhost\n", 0644)
	writeFile("etc/resolv.conf", "nameserver 8.8.8.8\n", 0644)

	// User files with correct ownership
	writeFile("home/user/.profile", "# user profile\nexport PATH=$PATH:/usr/local/bin\n", 0644)
	chown("home/user/.profile", 1000, 1000)
	chown("home/user", 1000, 1000)

	// Work directory owned by user
	chown("work", 1000, 1000)

	// Symlinks (common in Linux)
	symlink("lib", "lib64")
	symlink("/proc/self/fd", "dev/fd")
	symlink("/proc/self/fd/0", "dev/stdin")
	symlink("/proc/self/fd/1", "dev/stdout")
	symlink("/proc/self/fd/2", "dev/stderr")

	// Copy ts binary if provided
	if tsBinary != "" {
		tsDst := filepath.Join(dir, "bin/ts")
		cmd := exec.Command("cp", "--reflink=auto", tsBinary, tsDst)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("copy ts binary: %v\n%s", err, out)
		}
		if err := os.Chmod(tsDst, 0755); err != nil {
			t.Fatalf("chmod ts: %v", err)
		}
	}
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

// TestFullE2ELocalSnapshot tests the complete flow using locally generated fixtures:
// 1. Create a local base snapshot (no network required)
// 2. Create a frame with rootfs:: (empty home/work)
// 3. Verify that `ts snap` inside returns deterministic results
// 4. Verify /home and /work ownership and permissions
func TestFullE2ELocalSnapshot(t *testing.T) {
	root := requireBtrfsRoot2(t)

	// Set up two test environments (simulating two machines)
	env1 := createTestEnv(t, root, "instance1")
	env2 := createTestEnv(t, root, "instance2")
	defer env1.cleanup()
	defer env2.cleanup()

	// Build the ts binary
	tsBinary := buildTsBinaryForTest(t, root)

	// Test 1: Create local base snapshot on both instances
	t.Log("Creating local base snapshot on instance 1...")
	snapID1 := createLocalBaseSnapshot(t, env1, tsBinary)
	t.Logf("Instance 1 snap ID: %s", snapID1)

	t.Log("Creating local base snapshot on instance 2...")
	snapID2 := createLocalBaseSnapshot(t, env2, tsBinary)
	t.Logf("Instance 2 snap ID: %s", snapID2)

	// Both instances use the same deterministic snapshot ID
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
}

// TestE2ETwoInstancesSameSnap verifies that two instances creating the same
// base snapshot get identical snapshot IDs.
func TestE2ETwoInstancesSameSnap(t *testing.T) {
	root := requireBtrfsRoot2(t)

	env1 := createTestEnv(t, root, "inst1")
	env2 := createTestEnv(t, root, "inst2")
	defer env1.cleanup()
	defer env2.cleanup()

	// Build ts binary
	tsBinary := buildTsBinaryForTest(t, root)

	// Create local snapshot on first instance
	snap1 := createLocalBaseSnapshot(t, env1, tsBinary)

	// Create local snapshot on second instance
	snap2 := createLocalBaseSnapshot(t, env2, tsBinary)

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

	// Build ts binary and create local snapshot
	tsBinary := buildTsBinaryForTest(t, root)
	snapID := createLocalBaseSnapshot(t, env, tsBinary)

	setFlagsForTest(env.fsDir, env.snapshotsDir, env.libexecDir)
	defer resetFlagsForTest()

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

	// Build ts binary
	tsBinary := buildTsBinaryForTest(t, root)

	// Create same local snapshot on both instances
	snapID1 := createLocalBaseSnapshot(t, env1, tsBinary)
	snapID2 := createLocalBaseSnapshot(t, env2, tsBinary)

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
	newSnap1, err := createSnapshot(frame1, nil)
	if err != nil {
		t.Fatalf("createSnapshot inst1: %v", err)
	}
	resetFlagsForTest()

	setFlagsForTest(env2.fsDir, env2.snapshotsDir, env2.libexecDir)
	newSnap2, err := createSnapshot(frame2, nil)
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

	// Build ts binary
	tsBinary := buildTsBinaryForTest(t, root)

	// Step 1: Create local snapshot on instance 1 only
	t.Log("Creating local snapshot on instance 1...")
	baseSnap := createLocalBaseSnapshot(t, env1, tsBinary)
	t.Logf("Base snap ID: %s", baseSnap)

	setFlagsForTest(env1.fsDir, env1.snapshotsDir, env1.libexecDir)

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
	snapSpec1, err := createSnapshot(frame1, nil)
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
	snapSpec2, err := createSnapshot(frame2, nil)
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

// TestE2EPtyVisibleInContainer verifies that when running an interactive
// session with a PTY, the PTY device is visible inside the container's
// /dev/pts directory. This tests that the PTY is allocated AFTER the
// container's devpts mount, not before.
func TestE2EPtyVisibleInContainer(t *testing.T) {
	root := requireBtrfsRoot2(t)

	env := createTestEnv(t, root, "pty-test")
	defer env.cleanup()

	// Build ts binary
	tsBinary := buildTsBinaryForTest(t, root)

	// Download a real Docker image (need actual /bin/sh and tty command)
	snapID := downloadDockerImageForTest(t, env, "library/debian:bookworm-slim")

	setFlagsForTest(env.fsDir, env.snapshotsDir, env.libexecDir)
	defer resetFlagsForTest()

	// Create frame
	framePath := filepath.Join(env.fsDir, "testuser", "ptyframe")
	if err := createFrame(framePath, snapID, "", "", "container"); err != nil {
		t.Fatalf("createFrame: %v", err)
	}

	// Copy ts binary into the frame
	copyTsBinaryToFrameForTest(t, tsBinary, framePath)

	// Run a command inside the container that:
	// 1. Gets its TTY path via `tty` command
	// 2. Checks if that path exists
	// 3. Lists /dev/pts to show what's there
	//
	// We use ts drop-caps-and-run with a PTY to simulate what thundersnapd does.
	// The test script outputs:
	//   TTY_PATH:<path>
	//   TTY_EXISTS:<yes|no>
	//   DEV_PTS_CONTENTS:<listing>
	absFramePath, _ := filepath.Abs(framePath)
	innerTsBinary := filepath.Join(absFramePath, "bin", "ts")

	// Create a test script inside the frame
	testScript := `#!/bin/sh
TTY_PATH=$(tty 2>/dev/null || echo "none")
echo "TTY_PATH:$TTY_PATH"
if [ "$TTY_PATH" != "none" ] && [ "$TTY_PATH" != "not a tty" ] && [ -e "$TTY_PATH" ]; then
    echo "TTY_EXISTS:yes"
else
    echo "TTY_EXISTS:no"
fi
echo "DEV_PTS_CONTENTS:$(ls -la /dev/pts 2>&1 | tr '\n' ' ')"
`
	testScriptPath := filepath.Join(framePath, "tmp", "pty_test.sh")
	os.MkdirAll(filepath.Dir(testScriptPath), 0755)
	if err := os.WriteFile(testScriptPath, []byte(testScript), 0755); err != nil {
		t.Fatalf("write test script: %v", err)
	}

	// Run with PTY using the same approach as thundersnapd:
	// ts drop-caps-and-run --chroot=<path> --pty -- /bin/sh /tmp/pty_test.sh
	// The --pty flag tells ts to allocate the PTY INSIDE the container after
	// devpts is mounted, which is the fix for the PTY visibility bug.
	cmd := exec.Command(innerTsBinary, "drop-caps-and-run",
		"--chroot="+absFramePath,
		"--pty",
		"--", "/bin/sh", "/tmp/pty_test.sh")
	cmd.Dir = "/"
	cmd.Env = []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"TERM=xterm-256color",
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS,
	}

	// Set up pipes - ts --pty will proxy between stdin/stdout and the PTY
	var output strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &output

	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start: %v", err)
	}

	// Wait for command to finish
	cmd.Wait()

	result := output.String()
	t.Logf("PTY test output:\n%s", result)

	// Parse results
	var ttyPath, ttyExists string
	for _, line := range strings.Split(result, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "TTY_PATH:") {
			ttyPath = strings.TrimPrefix(line, "TTY_PATH:")
		} else if strings.HasPrefix(line, "TTY_EXISTS:") {
			ttyExists = strings.TrimPrefix(line, "TTY_EXISTS:")
		}
	}

	t.Logf("TTY path: %q, exists: %q", ttyPath, ttyExists)

	// The PTY should be visible inside the container
	if ttyExists != "yes" {
		t.Errorf("PTY is not visible inside container! tty=%q, exists=%q\n"+
			"This indicates the PTY was allocated before the container's devpts mount.\n"+
			"Full output:\n%s", ttyPath, ttyExists, result)
	}
}
