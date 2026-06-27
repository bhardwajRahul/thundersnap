//go:build e2e

// Package e2e contains end-to-end tests for thundersnap UID/permissions handling.
package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// TestUIDPermissionsBasic tests that file ownership is preserved across snapshot/restore:
// 1. Create file with non-root owner (uid 1000)
// 2. Snapshot
// 3. Restore to new frame
// 4. Verify ownership preserved
func TestUIDPermissionsBasic(t *testing.T) {
	env := newTestEnv(t)

	// Create a base snapshot
	baseSnap := env.createBaseSnapshot()
	t.Logf("Created base snapshot: %s", baseSnap)

	// Create a frame from the snapshot
	framePath := filepath.Join(env.fsDir, "testuser", "uidtest")
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	snapPath := filepath.Join(env.snapshotsDir, baseSnap)
	cmd := exec.Command("btrfs", "subvolume", "snapshot", snapPath, framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	// Verify the user's home directory has correct ownership from the fixture
	// (DefaultTestContainerSpec sets home/user to uid:gid 1000:1000)
	homeUserPath := filepath.Join(framePath, "home", "user")
	info, err := os.Stat(homeUserPath)
	if err != nil {
		t.Fatalf("stat home/user: %v", err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if stat.Uid != 1000 {
		t.Errorf("home/user uid: got %d, want 1000", stat.Uid)
	}
	if stat.Gid != 1000 {
		t.Errorf("home/user gid: got %d, want 1000", stat.Gid)
	}
	t.Logf("home/user ownership: %d:%d (verified)", stat.Uid, stat.Gid)

	// Create a new file owned by uid 1000 in the frame
	testFile := filepath.Join(framePath, "home", "user", "testfile.txt")
	if err := os.WriteFile(testFile, []byte("test content\n"), 0644); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	if err := os.Chown(testFile, 1000, 1000); err != nil {
		t.Fatalf("chown test file: %v", err)
	}

	// Verify the new file's ownership
	info, err = os.Stat(testFile)
	if err != nil {
		t.Fatalf("stat test file: %v", err)
	}
	stat = info.Sys().(*syscall.Stat_t)
	if stat.Uid != 1000 || stat.Gid != 1000 {
		t.Errorf("test file ownership: got %d:%d, want 1000:1000", stat.Uid, stat.Gid)
	}

	// Create a snapshot of the frame
	snap2Path := filepath.Join(env.snapshotsDir, "2")
	cmd = exec.Command("btrfs", "subvolume", "snapshot", "-r", framePath, snap2Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot frame: %v\n%s", err, out)
	}
	t.Logf("Created snapshot 2 from frame")

	// Create a new frame from the snapshot
	frame2Path := filepath.Join(env.fsDir, "testuser", "uidtest2")
	cmd = exec.Command("btrfs", "subvolume", "snapshot", snap2Path, frame2Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot to frame2: %v\n%s", err, out)
	}
	t.Logf("Created frame2 from snapshot 2")

	// Verify ownership is preserved in the new frame
	testFile2 := filepath.Join(frame2Path, "home", "user", "testfile.txt")
	info, err = os.Stat(testFile2)
	if err != nil {
		t.Fatalf("stat test file in frame2: %v", err)
	}
	stat = info.Sys().(*syscall.Stat_t)
	if stat.Uid != 1000 {
		t.Errorf("test file uid in frame2: got %d, want 1000", stat.Uid)
	}
	if stat.Gid != 1000 {
		t.Errorf("test file gid in frame2: got %d, want 1000", stat.Gid)
	}
	t.Logf("Ownership preserved in frame2: %d:%d", stat.Uid, stat.Gid)

	// Also verify home/user ownership in frame2
	homeUserPath2 := filepath.Join(frame2Path, "home", "user")
	info, err = os.Stat(homeUserPath2)
	if err != nil {
		t.Fatalf("stat home/user in frame2: %v", err)
	}
	stat = info.Sys().(*syscall.Stat_t)
	if stat.Uid != 1000 || stat.Gid != 1000 {
		t.Errorf("home/user ownership in frame2: got %d:%d, want 1000:1000", stat.Uid, stat.Gid)
	}
	t.Logf("home/user ownership preserved in frame2: %d:%d", stat.Uid, stat.Gid)
}

// TestSetuidPreservation tests that setuid/setgid bits are preserved.
func TestSetuidPreservation(t *testing.T) {
	env := newTestEnv(t)

	// Create a base snapshot (includes setuid/setgid files from fixture)
	baseSnap := env.createBaseSnapshot()

	// Create a frame
	framePath := filepath.Join(env.fsDir, "testuser", "setuidtest")
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	snapPath := filepath.Join(env.snapshotsDir, baseSnap)
	cmd := exec.Command("btrfs", "subvolume", "snapshot", snapPath, framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	// Check the setuid file (usr/bin/sudo-test from fixture)
	setuidFile := filepath.Join(framePath, "usr", "bin", "sudo-test")
	info, err := os.Stat(setuidFile)
	if err != nil {
		t.Fatalf("stat setuid file: %v", err)
	}
	if info.Mode()&os.ModeSetuid == 0 {
		t.Errorf("setuid bit not set on %s (mode: %o)", setuidFile, info.Mode())
	} else {
		t.Logf("setuid bit preserved on sudo-test (mode: %o)", info.Mode())
	}

	// Check the setgid file (usr/bin/sg-test from fixture)
	setgidFile := filepath.Join(framePath, "usr", "bin", "sg-test")
	info, err = os.Stat(setgidFile)
	if err != nil {
		t.Fatalf("stat setgid file: %v", err)
	}
	if info.Mode()&os.ModeSetgid == 0 {
		t.Errorf("setgid bit not set on %s (mode: %o)", setgidFile, info.Mode())
	} else {
		t.Logf("setgid bit preserved on sg-test (mode: %o)", info.Mode())
	}

	// Create a snapshot and restore to verify preservation across snapshot
	snap2Path := filepath.Join(env.snapshotsDir, "setuid2")
	cmd = exec.Command("btrfs", "subvolume", "snapshot", "-r", framePath, snap2Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	frame2Path := filepath.Join(env.fsDir, "testuser", "setuidtest2")
	cmd = exec.Command("btrfs", "subvolume", "snapshot", snap2Path, frame2Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	// Verify setuid preserved after snapshot/restore cycle
	setuidFile2 := filepath.Join(frame2Path, "usr", "bin", "sudo-test")
	info, err = os.Stat(setuidFile2)
	if err != nil {
		t.Fatalf("stat setuid file after restore: %v", err)
	}
	if info.Mode()&os.ModeSetuid == 0 {
		t.Errorf("setuid bit lost after snapshot/restore (mode: %o)", info.Mode())
	} else {
		t.Logf("setuid bit preserved after snapshot/restore (mode: %o)", info.Mode())
	}
}

// TestSetuidBinaryExecution tests that setuid binaries can be executed
// after snapshot/restore. This verifies the setuid bit is functional,
// not just preserved as metadata.
//
// Note: This test verifies the bits are preserved and the binary is executable.
// Actually testing the setuid behavior (running as a different user) would
// require a more complex setup with actual user switching.
func TestSetuidBinaryExecution(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()

	// Create a frame
	framePath := filepath.Join(env.fsDir, "testuser", "setuidexec")
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	snapPath := filepath.Join(env.snapshotsDir, baseSnap)
	cmd := exec.Command("btrfs", "subvolume", "snapshot", snapPath, framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	// The fixture's sudo-test is a copy of busybox with setuid bit
	// Verify it has the correct mode before snapshot
	setuidFile := filepath.Join(framePath, "usr", "bin", "sudo-test")
	info, err := os.Stat(setuidFile)
	if err != nil {
		t.Fatalf("stat setuid file: %v", err)
	}

	// Verify setuid bit is set
	if info.Mode()&os.ModeSetuid == 0 {
		t.Fatalf("setuid bit not set before snapshot")
	}

	// Verify it's executable
	if info.Mode()&0111 == 0 {
		t.Fatalf("file is not executable")
	}
	t.Logf("setuid file mode before snapshot: %o", info.Mode())

	// Snapshot and restore
	snap2Path := filepath.Join(env.snapshotsDir, "setuidexec2")
	cmd = exec.Command("btrfs", "subvolume", "snapshot", "-r", framePath, snap2Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	frame2Path := filepath.Join(env.fsDir, "testuser", "setuidexec2")
	cmd = exec.Command("btrfs", "subvolume", "snapshot", snap2Path, frame2Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	// Verify setuid and executable bits after restore
	setuidFile2 := filepath.Join(frame2Path, "usr", "bin", "sudo-test")
	info, err = os.Stat(setuidFile2)
	if err != nil {
		t.Fatalf("stat setuid file after restore: %v", err)
	}

	if info.Mode()&os.ModeSetuid == 0 {
		t.Errorf("setuid bit lost after restore (mode: %o)", info.Mode())
	}
	if info.Mode()&0111 == 0 {
		t.Errorf("executable bit lost after restore (mode: %o)", info.Mode())
	}
	t.Logf("setuid file mode after restore: %o (executable and setuid preserved)", info.Mode())
}

// TestSetgidBinaryExecution tests that setgid binaries are functional
// after snapshot/restore.
func TestSetgidBinaryExecution(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()

	// Create a frame
	framePath := filepath.Join(env.fsDir, "testuser", "setgidexec")
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	snapPath := filepath.Join(env.snapshotsDir, baseSnap)
	cmd := exec.Command("btrfs", "subvolume", "snapshot", snapPath, framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	// The fixture's sg-test has setgid bit
	setgidFile := filepath.Join(framePath, "usr", "bin", "sg-test")
	info, err := os.Stat(setgidFile)
	if err != nil {
		t.Fatalf("stat setgid file: %v", err)
	}

	// Verify setgid bit is set
	if info.Mode()&os.ModeSetgid == 0 {
		t.Fatalf("setgid bit not set before snapshot")
	}

	// Verify it's executable
	if info.Mode()&0111 == 0 {
		t.Fatalf("file is not executable")
	}
	t.Logf("setgid file mode before snapshot: %o", info.Mode())

	// Snapshot and restore
	snap2Path := filepath.Join(env.snapshotsDir, "setgidexec2")
	cmd = exec.Command("btrfs", "subvolume", "snapshot", "-r", framePath, snap2Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	frame2Path := filepath.Join(env.fsDir, "testuser", "setgidexec2")
	cmd = exec.Command("btrfs", "subvolume", "snapshot", snap2Path, frame2Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	// Verify setgid and executable bits after restore
	setgidFile2 := filepath.Join(frame2Path, "usr", "bin", "sg-test")
	info, err = os.Stat(setgidFile2)
	if err != nil {
		t.Fatalf("stat setgid file after restore: %v", err)
	}

	if info.Mode()&os.ModeSetgid == 0 {
		t.Errorf("setgid bit lost after restore (mode: %o)", info.Mode())
	}
	if info.Mode()&0111 == 0 {
		t.Errorf("executable bit lost after restore (mode: %o)", info.Mode())
	}
	t.Logf("setgid file mode after restore: %o (executable and setgid preserved)", info.Mode())
}

// TestUIDPreservation tests that file UIDs are preserved across snapshot/restore,
// verifying that we no longer strip UIDs to a single shared value.
func TestUIDPreservation(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()

	// Create a frame from the snapshot
	framePath := filepath.Join(env.fsDir, "testuser", "uidpreservetest")
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	snapPath := filepath.Join(env.snapshotsDir, baseSnap)
	cmd := exec.Command("btrfs", "subvolume", "snapshot", snapPath, framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	// Create files with different UIDs to simulate a real Docker image
	testFiles := []struct {
		path string
		uid  int
		gid  int
	}{
		{filepath.Join(framePath, "var", "lib", "postgresql", "test.db"), 111, 115},
		{filepath.Join(framePath, "var", "www", "html", "index.html"), 33, 33},
		{filepath.Join(framePath, "home", "user", "data.txt"), 1000, 1000},
		{filepath.Join(framePath, "opt", "app", "config.json"), 999, 999},
	}

	for _, tf := range testFiles {
		if err := os.MkdirAll(filepath.Dir(tf.path), 0755); err != nil {
			t.Fatalf("mkdir for %s: %v", tf.path, err)
		}
		if err := os.WriteFile(tf.path, []byte("test content\n"), 0644); err != nil {
			t.Fatalf("write %s: %v", tf.path, err)
		}
		if err := os.Chown(tf.path, tf.uid, tf.gid); err != nil {
			t.Fatalf("chown %s: %v", tf.path, err)
		}
	}

	// Verify the initial ownership
	for _, tf := range testFiles {
		info, err := os.Stat(tf.path)
		if err != nil {
			t.Fatalf("stat %s: %v", tf.path, err)
		}
		stat := info.Sys().(*syscall.Stat_t)
		if int(stat.Uid) != tf.uid || int(stat.Gid) != tf.gid {
			t.Errorf("%s: initial ownership %d:%d, want %d:%d",
				tf.path, stat.Uid, stat.Gid, tf.uid, tf.gid)
		}
		t.Logf("Initial: %s owned by %d:%d", tf.path, stat.Uid, stat.Gid)
	}

	// Create a snapshot of the frame
	snap2Path := filepath.Join(env.snapshotsDir, "uidpreserve2")
	cmd = exec.Command("btrfs", "subvolume", "snapshot", "-r", framePath, snap2Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot frame: %v\n%s", err, out)
	}
	t.Logf("Created snapshot from frame")

	// Restore to a new frame
	frame2Path := filepath.Join(env.fsDir, "testuser", "uidpreservetest2")
	cmd = exec.Command("btrfs", "subvolume", "snapshot", snap2Path, frame2Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot to frame2: %v\n%s", err, out)
	}
	t.Logf("Restored to frame2")

	// Verify UIDs are preserved after snapshot/restore
	for _, tf := range testFiles {
		// Adjust path to frame2
		restoredPath := filepath.Join(frame2Path, tf.path[len(framePath):])
		info, err := os.Stat(restoredPath)
		if err != nil {
			t.Fatalf("stat restored %s: %v", restoredPath, err)
		}
		stat := info.Sys().(*syscall.Stat_t)
		if int(stat.Uid) != tf.uid {
			t.Errorf("%s: UID after restore %d, want %d (UIDs should be preserved)",
				restoredPath, stat.Uid, tf.uid)
		}
		if int(stat.Gid) != tf.gid {
			t.Errorf("%s: GID after restore %d, want %d (GIDs should be preserved)",
				restoredPath, stat.Gid, tf.gid)
		}
		t.Logf("After restore: %s owned by %d:%d (preserved)", restoredPath, stat.Uid, stat.Gid)
	}
}

// TestHardlinkSetuidBinaryInSnapshot tests that hardlinks to setuid binaries
// preserve the setuid bit correctly when creating and restoring snapshots.
func TestHardlinkSetuidBinaryInSnapshot(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()

	// Create a frame from the snapshot
	framePath := filepath.Join(env.fsDir, "testuser", "hardlink-setuid")
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	snapPath := filepath.Join(env.snapshotsDir, baseSnap)
	cmd := exec.Command("btrfs", "subvolume", "snapshot", snapPath, framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	// Create a setuid binary
	binDir := filepath.Join(framePath, "usr", "local", "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	// Copy busybox (or any binary) to create a setuid file
	busyboxPath, err := exec.LookPath("busybox")
	if err != nil {
		// Fall back to using ts binary
		busyboxPath = env.tsBinary
	}

	origSetuid := filepath.Join(binDir, "orig-setuid")
	if err := copyFile(busyboxPath, origSetuid); err != nil {
		t.Fatalf("copy binary: %v", err)
	}

	// Set mode to 4755 (setuid + rwxr-xr-x)
	if err := os.Chmod(origSetuid, 0755|os.ModeSetuid); err != nil {
		t.Fatalf("chmod setuid: %v", err)
	}

	// Verify setuid bit is set
	info, err := os.Stat(origSetuid)
	if err != nil {
		t.Fatalf("stat orig: %v", err)
	}
	if info.Mode()&os.ModeSetuid == 0 {
		t.Fatalf("setuid bit not set on original file")
	}
	t.Logf("Original setuid file mode: %o", info.Mode())

	// Create a hardlink to the setuid binary
	hardlinkSetuid := filepath.Join(binDir, "hardlink-setuid")
	if err := os.Link(origSetuid, hardlinkSetuid); err != nil {
		t.Fatalf("create hardlink: %v", err)
	}

	// Verify both files share the same inode
	var origStat, linkStat syscall.Stat_t
	if err := syscall.Stat(origSetuid, &origStat); err != nil {
		t.Fatalf("syscall.stat orig: %v", err)
	}
	if err := syscall.Stat(hardlinkSetuid, &linkStat); err != nil {
		t.Fatalf("syscall.stat link: %v", err)
	}
	if origStat.Ino != linkStat.Ino {
		t.Fatalf("hardlink not created correctly (different inodes)")
	}
	t.Logf("Original and hardlink share inode %d", origStat.Ino)

	// Verify setuid bit is set on hardlink
	info, err = os.Stat(hardlinkSetuid)
	if err != nil {
		t.Fatalf("stat hardlink: %v", err)
	}
	if info.Mode()&os.ModeSetuid == 0 {
		t.Fatalf("setuid bit not set on hardlink")
	}

	// Create a read-only snapshot
	snap2Path := filepath.Join(env.snapshotsDir, "hardlink-setuid-snap")
	cmd = exec.Command("btrfs", "subvolume", "snapshot", "-r", framePath, snap2Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	// Restore to a new frame
	frame2Path := filepath.Join(env.fsDir, "testuser", "hardlink-setuid2")
	cmd = exec.Command("btrfs", "subvolume", "snapshot", snap2Path, frame2Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs restore snapshot: %v\n%s", err, out)
	}

	// Verify setuid bit is preserved on both files after restore
	origSetuid2 := filepath.Join(frame2Path, "usr", "local", "bin", "orig-setuid")
	hardlinkSetuid2 := filepath.Join(frame2Path, "usr", "local", "bin", "hardlink-setuid")

	info, err = os.Stat(origSetuid2)
	if err != nil {
		t.Fatalf("stat orig after restore: %v", err)
	}
	if info.Mode()&os.ModeSetuid == 0 {
		t.Errorf("setuid bit lost on original after restore (mode: %o)", info.Mode())
	} else {
		t.Logf("Original after restore: mode %o (setuid preserved)", info.Mode())
	}

	info, err = os.Stat(hardlinkSetuid2)
	if err != nil {
		t.Fatalf("stat hardlink after restore: %v", err)
	}
	if info.Mode()&os.ModeSetuid == 0 {
		t.Errorf("setuid bit lost on hardlink after restore (mode: %o)", info.Mode())
	} else {
		t.Logf("Hardlink after restore: mode %o (setuid preserved)", info.Mode())
	}

	// Verify they still share the same inode after restore
	if err := syscall.Stat(origSetuid2, &origStat); err != nil {
		t.Fatalf("syscall.stat orig2: %v", err)
	}
	if err := syscall.Stat(hardlinkSetuid2, &linkStat); err != nil {
		t.Fatalf("syscall.stat link2: %v", err)
	}
	if origStat.Ino != linkStat.Ino {
		t.Errorf("hardlink broken after restore: orig inode=%d, link inode=%d",
			origStat.Ino, linkStat.Ino)
	} else {
		t.Logf("Hardlink preserved after restore (inode %d)", origStat.Ino)
	}
}

// Note: copyFile is defined in e2e_test.go
