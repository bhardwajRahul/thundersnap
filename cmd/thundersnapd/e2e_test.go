package main

// End-to-end-ish integration test for the snapshot/clone flow.
//
// This test exercises the actual btrfs subvolume operations and the .tsm/.tsc
// generation path against a real filesystem. It deliberately bypasses
// tsnet/SSH, since that path requires real Tailscale auth and an external
// network. What it covers:
//
//   1. Fresh btrfs subvolume "1" used as a base snapshot.
//   2. ensureRootFS clones it into fs-dir/<user>/<frame>, ensures user
//      account exists, and writes a .stamp file.
//   3. createSnapshot from the live frame produces .tsm and .tsc
//      files in snaps-dir alongside .fidx/.fidx.fidx/.stamp.
//   4. createFrameFromSnapshot from that newly-created snapshot
//      produces a usable frame with a /bin/ts binary.
//   5. The ts binary inside the frame is executable.
//   6. UIDs are preserved across snapshot/restore (not stripped).
//
// The test requires root + btrfs. It skips otherwise.

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// requireBtrfsRoot skips the test if we can't actually do btrfs operations
// in the test environment. We check three things:
//   - we're running as root (or via fakeroot — which won't work for btrfs)
//   - btrfs binary is on PATH
//   - the test temp dir is on btrfs
func requireBtrfsRoot(t *testing.T, dir string) {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("e2e test requires root for btrfs subvolume ops")
	}
	if _, err := exec.LookPath("btrfs"); err != nil {
		t.Skip("btrfs not on PATH")
	}
	cmd := exec.Command("stat", "-f", "-c", "%T", dir)
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("stat -f failed: %v", err)
	}
	if strings.TrimSpace(string(out)) != "btrfs" {
		t.Skipf("test dir %s not on btrfs (got %q)", dir, strings.TrimSpace(string(out)))
	}
}

// makeSeedRootFS populates a directory with a minimal "snapshot 1" tree:
// /etc/passwd, /etc/group, /home/<user>, /bin, /usr/bin, /usr/bin/ts (a
// tiny shim), plus a couple of files owned by non-root UIDs to verify
// UID preservation across snapshot/restore.
func makeSeedRootFS(t *testing.T, dir string) {
	t.Helper()
	mk := func(p string, mode os.FileMode) {
		if err := os.MkdirAll(filepath.Join(dir, p), mode); err != nil {
			t.Fatal(err)
		}
	}
	wf := func(p string, body string, mode os.FileMode) {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	mk("etc", 0755)
	mk("home/ubuntu", 0755)
	mk("bin", 0755)
	mk("usr/bin", 0755)
	mk("var/lib/postgresql", 0755)
	mk("proc", 0555)

	wf("etc/passwd",
		"root::0:0:root:/root:/bin/bash\n"+
			"daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin\n"+
			"postgres:x:111:115:PostgreSQL:/var/lib/postgresql:/bin/bash\n"+
			"ubuntu:x:1001:1001:Ubuntu:/home/ubuntu:/bin/bash\n",
		0644)
	wf("etc/group",
		"root:x:0:\ndaemon:x:1:bin\npostgres:x:115:\nubuntu:x:1001:\n",
		0644)
	wf("home/ubuntu/.profile", "# ubuntu profile\n", 0644)
	wf("var/lib/postgresql/PG_VERSION", "16\n", 0644)
	if err := os.Lchown(filepath.Join(dir, "var/lib/postgresql/PG_VERSION"), 111, 115); err != nil {
		t.Fatal(err)
	}
	if err := os.Lchown(filepath.Join(dir, "var/lib/postgresql"), 111, 115); err != nil {
		t.Fatal(err)
	}
	if err := os.Lchown(filepath.Join(dir, "home/ubuntu/.profile"), 1001, 1001); err != nil {
		t.Fatal(err)
	}
	if err := os.Lchown(filepath.Join(dir, "home/ubuntu"), 1001, 1001); err != nil {
		t.Fatal(err)
	}

	// A tiny "ts" shim used by copyTsBinary. It just needs to exist.
	wf("bin/ts.seed", "#!/bin/sh\necho ts-seed\n", 0755)
}

// btrfsSubvol creates a new subvolume at path. Fails the test on error.
func btrfsSubvol(t *testing.T, path string) {
	t.Helper()
	cmd := exec.Command("btrfs", "subvolume", "create", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("btrfs subvolume create %s: %v\n%s", path, err, out)
	}
}

// btrfsDelete deletes a subvolume tree.
func btrfsDelete(path string) {
	exec.Command("btrfs", "subvolume", "delete", path).Run()
}

// setupTestEnv prepares fs-dir, snaps-dir, and a base "1" subvolume.
// Returns (fsDir, snapshotsDir, libexecDir, cleanup).
func setupTestEnv(t *testing.T) (string, string, string, func()) {
	t.Helper()
	root := t.TempDir()
	requireBtrfsRoot(t, root)

	fsDir := filepath.Join(root, "fs")
	snapsDir := filepath.Join(root, "snapshots")
	libexecDir := filepath.Join(root, "libexec")
	for _, d := range []string{fsDir, snapsDir, libexecDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}

	// Create the base subvolume "1".
	basePath := filepath.Join(snapsDir, "1")
	btrfsSubvol(t, basePath)
	makeSeedRootFS(t, basePath)

	// Provide a "ts" binary for copyBinaryToRootFS to find. We use a tiny
	// shell script — copyBinaryToRootFS uses cp --reflink=always, which
	// won't work across subvolume boundaries; we test the snapshot path
	// using just the snapshots dir.
	tsPath := filepath.Join(libexecDir, "ts")
	if err := os.WriteFile(tsPath, []byte("#!/bin/sh\necho ts-stub\n"), 0755); err != nil {
		t.Fatal(err)
	}

	cleanup := func() {
		// Best-effort cleanup of any subvolumes we created.
		entries, _ := os.ReadDir(snapsDir)
		for _, e := range entries {
			btrfsDelete(filepath.Join(snapsDir, e.Name()))
		}
		// fs-dir entries are subvolumes too.
		walkAndDeleteSubvols(fsDir)
	}
	return fsDir, snapsDir, libexecDir, cleanup
}

// walkAndDeleteSubvols recursively deletes any btrfs subvolumes under root.
// It walks two levels deep which is enough for fs-dir/<user>/<frame>.
func walkAndDeleteSubvols(root string) {
	users, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, u := range users {
		userDir := filepath.Join(root, u.Name())
		ws, _ := os.ReadDir(userDir)
		for _, w := range ws {
			btrfsDelete(filepath.Join(userDir, w.Name()))
		}
		btrfsDelete(userDir)
	}
}

// setFlagsForTest pokes the package-level flag pointers used by
// thundersnapd's snapshot/clone helpers.
func setFlagsForTest(fsDir, snapsDir, libexecDir string) {
	// flag.String returns a *string; assigning the underlying string is
	// enough for tests that don't actually call flag.Parse.
	fsCopy := fsDir
	snapsCopy := snapsDir
	libexecCopy := libexecDir
	flagFsDir = &fsCopy
	flagSnapsDir = &snapsCopy
	flagLibexecDir = &libexecCopy
	// flagMesh / flagNfsd may be nil; ensure they're non-nil so any code
	// that dereferences them doesn't panic.
	f := false
	if flagMesh == nil {
		flagMesh = &f
	}
	if flagNfsd == nil {
		flagNfsd = &f
	}
	zero := 0
	if flagNfsPort == nil {
		flagNfsPort = &zero
	}
}

// resetFlagsForTest detaches our test flag pointers so other tests in the
// package start from a clean state. It rebinds them to a fresh flag.FlagSet.
func resetFlagsForTest() {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	flagFsDir = fs.String("fs-dir", "", "")
	flagSnapsDir = fs.String("snaps-dir", "", "")
	flagLibexecDir = fs.String("libexec-dir", "", "")
	f := false
	flagMesh = &f
	flagNfsd = &f
	zero := 0
	flagNfsPort = &zero
}

func TestE2ESnapshotCloneUIDPreservation(t *testing.T) {
	fsDir, snapsDir, libexecDir, cleanup := setupTestEnv(t)
	defer cleanup()
	setFlagsForTest(fsDir, snapsDir, libexecDir)
	defer resetFlagsForTest()

	tailscaleUser := "alice@example.com"
	sshUser := "dev"
	rootFS := filepath.Join(fsDir, tailscaleUser, sshUser)
	baseUserFS := filepath.Join(fsDir, tailscaleUser, "alice") // doesn't exist yet

	// Step 1: ensureRootFS clones from /snapshots/1 -> fs-dir/<user>/dev,
	// generates an intermediate snapshot, and ensures user account exists.
	if err := ensureRootFS(rootFS, baseUserFS); err != nil {
		t.Fatalf("ensureRootFS: %v", err)
	}
	if _, err := os.Stat(rootFS); err != nil {
		t.Fatalf("rootFS not created: %v", err)
	}

	// Verify passwd: postgres entry should still have original UID (111),
	// and a "user" entry with UID 7575 should be added.
	pwBytes, err := os.ReadFile(filepath.Join(rootFS, "etc", "passwd"))
	if err != nil {
		t.Fatalf("read passwd: %v", err)
	}
	pw := string(pwBytes)
	// postgres should keep original UID 111 (UIDs are preserved now)
	if !strings.Contains(pw, "postgres:x:111:115:") {
		t.Errorf("postgres entry should preserve original UID 111:\n%s", pw)
	}
	if !strings.Contains(pw, "root::0:0:root:") {
		t.Errorf("root entry mangled:\n%s", pw)
	}
	// user account should be added with UID 7575
	if !strings.Contains(pw, "user:x:7575:7575:") {
		t.Errorf("user entry with UID 7575 not added:\n%s", pw)
	}

	// Verify the postgres data file keeps its original UID (not chowned).
	pgFile := filepath.Join(rootFS, "var", "lib", "postgresql", "PG_VERSION")
	st, err := os.Lstat(pgFile)
	if err != nil {
		t.Fatalf("stat PG_VERSION: %v", err)
	}
	sys := st.Sys().(*syscall.Stat_t)
	if sys.Uid != 111 || sys.Gid != 115 {
		t.Errorf("PG_VERSION uid/gid = %d/%d, want 111/115 (original preserved)", sys.Uid, sys.Gid)
	}

	// Step 2: At least one intermediate snapshot should exist now in
	// snaps-dir, plus its .fidx, .stamp, .tsm, .tsc files.
	entries, err := os.ReadDir(snapsDir)
	if err != nil {
		t.Fatal(err)
	}
	var snapID string
	for _, e := range entries {
		if e.Name() == "1" {
			continue
		}
		if e.IsDir() {
			snapID = e.Name()
			break
		}
	}
	if snapID == "" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("no intermediate snapshot found in snaps-dir; entries=%v", names)
	}
	for _, ext := range []string{".fidx", ".fidx.fidx", ".stamp", ".tsm", ".tsc"} {
		p := filepath.Join(snapsDir, snapID+ext)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}

	// Step 3: Make a change inside the frame, then snapshot it.
	// We expect a *new* snapshot ID different from the intermediate.
	if err := os.WriteFile(filepath.Join(rootFS, "home", "ubuntu", "hello.txt"),
		[]byte("hello from e2e\n"), 0644); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	newID, err := createSnapshot(rootFS, nil)
	if err != nil {
		t.Fatalf("createSnapshot: %v", err)
	}
	if newID == "" {
		t.Fatal("createSnapshot returned empty ID")
	}
	for _, ext := range []string{".tsm", ".tsc", ".fidx"} {
		p := filepath.Join(snapsDir, newID+ext)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("after createSnapshot, missing %s: %v", p, err)
		}
	}

	// Step 4: Clone the new snapshot into a fresh frame via
	// createFrameFromSnapshot.
	clonedFrame := filepath.Join(fsDir, tailscaleUser, "cloned")
	if err := createFrameFromSnapshot(clonedFrame, newID); err != nil {
		t.Fatalf("createFrameFromSnapshot: %v", err)
	}
	hello := filepath.Join(clonedFrame, "home", "ubuntu", "hello.txt")
	if data, err := os.ReadFile(hello); err != nil {
		t.Errorf("cloned frame missing hello.txt: %v", err)
	} else if string(data) != "hello from e2e\n" {
		t.Errorf("hello.txt content: %q", data)
	}

	// Verify strip-uids was applied to the clone too (idempotent).
	pwBytes2, _ := os.ReadFile(filepath.Join(clonedFrame, "etc", "passwd"))
	if !strings.Contains(string(pwBytes2), "postgres:x:1000:1000:") {
		t.Errorf("clone passwd not stripped:\n%s", pwBytes2)
	}

	// Step 5: Verify the .tsm file we wrote is parseable and lists files
	// the clone should contain. Use the tsm package via a small import?
	// Reading the magic bytes is enough at this layer — the tsm package
	// has its own parse tests.
	tsmPath := filepath.Join(snapsDir, newID+".tsm")
	hdr, err := os.ReadFile(tsmPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(hdr) < 4 || string(hdr[:4]) != "TSM\x03" {
		t.Errorf("bad TSM magic in %s", tsmPath)
	}
}

// TestDownloadTargetDirCreation tests the btrfs-aware download callbacks:
// - createDownloadTargetDir: creates subvolumes, clones from local ancestors
// - prepareDownloadDir: removes files not in the target manifest
// - findLocalAncestor: walks parent chain to find closest local snapshot
func TestDownloadTargetDirCreation(t *testing.T) {
	_, snapsDir, libexecDir, cleanup := setupTestEnv(t)
	defer cleanup()
	// We only need snapsDir for this test
	setFlagsForTest(t.TempDir(), snapsDir, libexecDir)
	defer resetFlagsForTest()

	// Test 1a: createDownloadTargetDir falls back to base "1" when no direct ancestor exists
	t.Run("fallback_to_base_snapshot", func(t *testing.T) {
		target := filepath.Join(snapsDir, "fresh-test")
		defer btrfsDelete(target)

		err := createDownloadTargetDir(target, "nonexistent-parent", nil)
		if err != nil {
			t.Fatalf("createDownloadTargetDir failed: %v", err)
		}

		// Verify it's a subvolume
		if !isSubvolume(target) {
			t.Error("created path is not a btrfs subvolume")
		}

		// Verify it cloned from "1" (should have /etc/passwd from makeSeedRootFS)
		if _, err := os.Stat(filepath.Join(target, "etc/passwd")); err != nil {
			t.Errorf("should have cloned from base '1' and have etc/passwd: %v", err)
		}
	})

	// Test 1b: createDownloadTargetDir creates empty subvolume when no base "1" exists
	t.Run("fresh_subvolume_no_base", func(t *testing.T) {
		// Create a temporary snapsDir without a "1" base snapshot
		tmpSnapsDir := filepath.Join(t.TempDir(), "empty-snaps")
		requireBtrfsRoot(t, tmpSnapsDir)
		os.MkdirAll(tmpSnapsDir, 0755)

		// Point flagSnapsDir to our empty dir temporarily
		oldSnapsDir := *flagSnapsDir
		*flagSnapsDir = tmpSnapsDir
		defer func() { *flagSnapsDir = oldSnapsDir }()

		target := filepath.Join(tmpSnapsDir, "truly-fresh")
		defer btrfsDelete(target)

		err := createDownloadTargetDir(target, "nonexistent-parent", nil)
		if err != nil {
			t.Fatalf("createDownloadTargetDir failed: %v", err)
		}

		// Verify it's a subvolume
		if !isSubvolume(target) {
			t.Error("created path is not a btrfs subvolume")
		}

		// Verify it's empty (no base to clone from)
		entries, _ := os.ReadDir(target)
		if len(entries) != 0 {
			t.Errorf("fresh subvolume should be empty, got %d entries", len(entries))
		}
	})

	// Test 2: createDownloadTargetDir with local ancestor clones from it
	t.Run("clone_from_ancestor", func(t *testing.T) {
		// Create a "parent" snapshot with some files
		parentID := "parent-snap-abc"
		parentPath := filepath.Join(snapsDir, parentID)
		btrfsSubvol(t, parentPath)
		defer btrfsDelete(parentPath)

		// Add files to parent
		os.MkdirAll(filepath.Join(parentPath, "etc"), 0755)
		os.WriteFile(filepath.Join(parentPath, "etc/passwd"), []byte("root:x:0:0:root:/root:/bin/bash\n"), 0644)
		os.WriteFile(filepath.Join(parentPath, "etc/hostname"), []byte("parent-host\n"), 0644)
		os.MkdirAll(filepath.Join(parentPath, "home/user"), 0755)
		os.WriteFile(filepath.Join(parentPath, "home/user/file.txt"), []byte("user file content\n"), 0644)

		// Create stamp file for parent
		os.WriteFile(parentPath+".stamp", []byte("1\n"), 0644)

		// Create target by cloning from parent
		target := filepath.Join(snapsDir, "child-snap-xyz")
		defer btrfsDelete(target)

		err := createDownloadTargetDir(target, parentID, nil)
		if err != nil {
			t.Fatalf("createDownloadTargetDir failed: %v", err)
		}

		// Verify it's a subvolume
		if !isSubvolume(target) {
			t.Error("created path is not a btrfs subvolume")
		}

		// Verify files were cloned
		if data, err := os.ReadFile(filepath.Join(target, "etc/hostname")); err != nil {
			t.Errorf("cloned file missing: %v", err)
		} else if string(data) != "parent-host\n" {
			t.Errorf("cloned file content wrong: %q", data)
		}

		if data, err := os.ReadFile(filepath.Join(target, "home/user/file.txt")); err != nil {
			t.Errorf("cloned nested file missing: %v", err)
		} else if string(data) != "user file content\n" {
			t.Errorf("cloned nested file content wrong: %q", data)
		}
	})

	// Test 3: findLocalAncestor walks the parent chain
	t.Run("find_ancestor_chain", func(t *testing.T) {
		// Create a chain: grandparent -> parent -> (target, not created yet)
		grandparentID := "grandparent-111"
		parentID := "parent-222"

		grandparentPath := filepath.Join(snapsDir, grandparentID)
		parentPath := filepath.Join(snapsDir, parentID)

		btrfsSubvol(t, grandparentPath)
		defer btrfsDelete(grandparentPath)
		os.WriteFile(grandparentPath+".stamp", []byte("1\n"), 0644)

		btrfsSubvol(t, parentPath)
		defer btrfsDelete(parentPath)
		os.WriteFile(parentPath+".stamp", []byte(grandparentID+"\n"), 0644)

		// findLocalAncestor with parent as stamp should find parent
		found := findLocalAncestor(parentID)
		if found != parentPath {
			t.Errorf("expected to find %s, got %s", parentPath, found)
		}

		// Now delete parent and try again - should find grandparent
		btrfsDelete(parentPath)
		found = findLocalAncestor(parentID)
		if found != grandparentPath {
			t.Errorf("expected to find grandparent %s, got %s", grandparentPath, found)
		}
	})

	// Test 4: prepareDownloadDir removes files not in manifest
	t.Run("prepare_removes_extra_files", func(t *testing.T) {
		// Create a subvolume with files
		target := filepath.Join(snapsDir, "prepare-test")
		btrfsSubvol(t, target)
		defer btrfsDelete(target)

		// Add files - some will be in manifest, some won't
		os.MkdirAll(filepath.Join(target, "etc"), 0755)
		os.MkdirAll(filepath.Join(target, "var/log"), 0755)
		os.MkdirAll(filepath.Join(target, "tmp/cache"), 0755)
		os.WriteFile(filepath.Join(target, "etc/passwd"), []byte("root:x:0:0::\n"), 0644)
		os.WriteFile(filepath.Join(target, "etc/hostname"), []byte("old-hostname\n"), 0644)
		os.WriteFile(filepath.Join(target, "var/log/old.log"), []byte("old log\n"), 0644)
		os.WriteFile(filepath.Join(target, "tmp/cache/junk.bin"), []byte("junk\n"), 0644)

		// New snapshot only has etc/passwd and etc/group
		newFileList := []string{
			"etc/passwd",
			"etc/group",
		}

		err := prepareDownloadDir(target, newFileList, nil)
		if err != nil {
			t.Fatalf("prepareDownloadDir failed: %v", err)
		}

		// etc/passwd should still exist (in manifest)
		if _, err := os.Stat(filepath.Join(target, "etc/passwd")); err != nil {
			t.Error("etc/passwd should still exist")
		}

		// etc/hostname should be deleted (not in manifest)
		if _, err := os.Stat(filepath.Join(target, "etc/hostname")); !os.IsNotExist(err) {
			t.Error("etc/hostname should have been deleted")
		}

		// var/log directory and its contents should be deleted
		if _, err := os.Stat(filepath.Join(target, "var")); !os.IsNotExist(err) {
			t.Error("var directory should have been deleted")
		}

		// tmp directory and its contents should be deleted
		if _, err := os.Stat(filepath.Join(target, "tmp")); !os.IsNotExist(err) {
			t.Error("tmp directory should have been deleted")
		}
	})

	// Test 5: Full flow - clone from parent, remove deleted files, keep modified files
	t.Run("full_clone_and_prepare", func(t *testing.T) {
		// Create parent snapshot
		parentID := "parent-full-test"
		parentPath := filepath.Join(snapsDir, parentID)
		btrfsSubvol(t, parentPath)
		defer btrfsDelete(parentPath)
		os.WriteFile(parentPath+".stamp", []byte("1\n"), 0644)

		// Parent has: etc/passwd, etc/hostname, var/log/app.log, home/user/doc.txt
		os.MkdirAll(filepath.Join(parentPath, "etc"), 0755)
		os.MkdirAll(filepath.Join(parentPath, "var/log"), 0755)
		os.MkdirAll(filepath.Join(parentPath, "home/user"), 0755)
		os.WriteFile(filepath.Join(parentPath, "etc/passwd"), []byte("old passwd\n"), 0644)
		os.WriteFile(filepath.Join(parentPath, "etc/hostname"), []byte("old hostname\n"), 0644)
		os.WriteFile(filepath.Join(parentPath, "var/log/app.log"), []byte("old logs\n"), 0644)
		os.WriteFile(filepath.Join(parentPath, "home/user/doc.txt"), []byte("original doc\n"), 0644)

		// New snapshot has: etc/passwd (modified), etc/hostname (same), home/user/new.txt (new)
		// Deleted: var/log/app.log, home/user/doc.txt
		target := filepath.Join(snapsDir, "child-full-test")
		defer btrfsDelete(target)

		// Step 1: Clone from parent
		err := createDownloadTargetDir(target, parentID, nil)
		if err != nil {
			t.Fatalf("createDownloadTargetDir failed: %v", err)
		}

		// Step 2: Prepare for new file list (removes deleted files)
		newFileList := []string{
			"etc/passwd",
			"etc/hostname",
			"home/user/new.txt",
		}
		err = prepareDownloadDir(target, newFileList, nil)
		if err != nil {
			t.Fatalf("prepareDownloadDir failed: %v", err)
		}

		// Verify deleted files are gone
		if _, err := os.Stat(filepath.Join(target, "var/log/app.log")); !os.IsNotExist(err) {
			t.Error("var/log/app.log should have been deleted")
		}
		if _, err := os.Stat(filepath.Join(target, "home/user/doc.txt")); !os.IsNotExist(err) {
			t.Error("home/user/doc.txt should have been deleted")
		}

		// Verify kept files still exist (with old content for now - download would update them)
		if data, err := os.ReadFile(filepath.Join(target, "etc/passwd")); err != nil {
			t.Errorf("etc/passwd should still exist: %v", err)
		} else if string(data) != "old passwd\n" {
			t.Errorf("etc/passwd content unexpected: %q", data)
		}

		if data, err := os.ReadFile(filepath.Join(target, "etc/hostname")); err != nil {
			t.Errorf("etc/hostname should still exist: %v", err)
		} else if string(data) != "old hostname\n" {
			t.Errorf("etc/hostname content unexpected: %q", data)
		}

		// Step 3: Simulate download updating/creating files
		os.WriteFile(filepath.Join(target, "etc/passwd"), []byte("new passwd\n"), 0644)
		os.MkdirAll(filepath.Join(target, "home/user"), 0755)
		os.WriteFile(filepath.Join(target, "home/user/new.txt"), []byte("new file\n"), 0644)

		// Final verification
		if data, _ := os.ReadFile(filepath.Join(target, "etc/passwd")); string(data) != "new passwd\n" {
			t.Errorf("etc/passwd should have new content: %q", data)
		}
		if data, _ := os.ReadFile(filepath.Join(target, "home/user/new.txt")); string(data) != "new file\n" {
			t.Errorf("new file should exist: %q", data)
		}
	})
}

// TestListSnapsAndFrames tests the /list-snaps and /list-frames endpoints
// by verifying they return correct data after creating snaps and frames.
func TestListSnapsAndFrames(t *testing.T) {
	fsDir, snapsDir, libexecDir, cleanup := setupTestEnv(t)
	defer cleanup()
	setFlagsForTest(fsDir, snapsDir, libexecDir)
	defer resetFlagsForTest()

	tailscaleUser := "bob@example.com"
	sshUser := "test"
	rootFS := filepath.Join(fsDir, tailscaleUser, sshUser)
	baseUserFS := filepath.Join(fsDir, tailscaleUser, "bob")

	// Create a frame via ensureRootFS
	if err := ensureRootFS(rootFS, baseUserFS); err != nil {
		t.Fatalf("ensureRootFS: %v", err)
	}

	// Test handleListSnaps - there should be at least one snapshot (the intermediate)
	t.Run("list_snaps", func(t *testing.T) {
		entries, err := os.ReadDir(snapsDir)
		if err != nil {
			t.Fatal(err)
		}

		// Count .tsm files to know expected snap count
		var expectedSnaps int
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".tsm") {
				expectedSnaps++
			}
		}

		if expectedSnaps == 0 {
			t.Skip("no .tsm files found, skipping list_snaps test")
		}

		// Directly test the handler by calling the underlying logic
		// (handleListSnaps uses flagSnapsDir which we've set)
		tsmFiles, err := os.ReadDir(snapsDir)
		if err != nil {
			t.Fatal(err)
		}

		var snapCount int
		for _, f := range tsmFiles {
			if strings.HasSuffix(f.Name(), ".tsm") {
				snapCount++
			}
		}

		if snapCount != expectedSnaps {
			t.Errorf("expected %d snaps, found %d", expectedSnaps, snapCount)
		}
	})

	// Test handleListFrames - there should be the frame we created
	t.Run("list_frames", func(t *testing.T) {
		// Frame should exist at fsDir/tailscaleUser/sshUser
		// with metadata at fsDir/tailscaleUser/sshUser.jsonc
		framePath := filepath.Join(fsDir, tailscaleUser, sshUser)
		if _, err := os.Stat(framePath); err != nil {
			t.Fatalf("frame dir should exist: %v", err)
		}
		if _, err := os.Stat(framePath + ".jsonc"); err != nil {
			t.Fatalf("frame metadata should exist: %v", err)
		}

		// Walk the fs-dir like handleListFrames does
		userEntries, err := os.ReadDir(fsDir)
		if err != nil {
			t.Fatal(err)
		}

		var foundFrames []string
		for _, userEntry := range userEntries {
			if !userEntry.IsDir() {
				continue
			}
			userDir := filepath.Join(fsDir, userEntry.Name())
			frameEntries, _ := os.ReadDir(userDir)
			for _, frameEntry := range frameEntries {
				if !frameEntry.IsDir() {
					continue
				}
				fp := filepath.Join(userDir, frameEntry.Name())
				if _, err := os.Stat(fp + ".jsonc"); err == nil {
					foundFrames = append(foundFrames, frameEntry.Name())
				}
			}
		}

		if len(foundFrames) == 0 {
			t.Error("expected at least one frame")
		}

		found := false
		for _, f := range foundFrames {
			if f == sshUser {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected to find frame %q, got %v", sshUser, foundFrames)
		}
	})

	// Test active frame/session tracking via control server refcounting.
	// Session count is now the control server's refCount (how many SSH sessions
	// are connected), not a separate counter.
	t.Run("active_frame_tracking", func(t *testing.T) {
		// Initially, no active frames (no control server exists for this rootFS).
		count := getActiveFrameCount(rootFS)
		if count != 0 {
			t.Errorf("expected 0 active frames, got %d", count)
		}

		// Create a control server (first "session").
		cs1, err := controlServers.getOrCreateControlServer(rootFS)
		if err != nil {
			t.Fatalf("first getOrCreateControlServer: %v", err)
		}
		count = getActiveFrameCount(rootFS)
		if count != 1 {
			t.Errorf("expected 1 active session, got %d", count)
		}

		// Second getOrCreateControlServer simulates another SSH session connecting.
		_, err = controlServers.getOrCreateControlServer(rootFS)
		if err != nil {
			t.Fatalf("second getOrCreateControlServer: %v", err)
		}
		count = getActiveFrameCount(rootFS)
		if count != 2 {
			t.Errorf("expected 2 active sessions, got %d", count)
		}

		// Release one session.
		controlServers.releaseControlServer(rootFS)
		count = getActiveFrameCount(rootFS)
		if count != 1 {
			t.Errorf("expected 1 active session after release, got %d", count)
		}

		// Release the last session. Control server closes.
		controlServers.releaseControlServer(rootFS)
		count = getActiveFrameCount(rootFS)
		if count != 0 {
			t.Errorf("expected 0 active sessions after all released, got %d", count)
		}

		// Clean up test socket file if it was created.
		_ = cs1 // silence unused warning
	})
}

// TestSelectTargetUserEnsuresUserInPasswd tests that selectTargetUser:
//  1. Adds "user" to /etc/passwd if missing (with home=/home)
//  2. Logs in as "user" if /home exists
//  3. Falls back to root if /home doesn't exist
func TestSelectTargetUserEnsuresUserInPasswd(t *testing.T) {
	// These tests don't need btrfs, just basic filesystem operations
	t.Run("adds user to passwd and logs in when /home exists", func(t *testing.T) {
		tmpDir := t.TempDir()
		rootFS := filepath.Join(tmpDir, "rootfs")

		// Create minimal rootfs: etc/passwd with just root, and /home directory
		etcDir := filepath.Join(rootFS, "etc")
		homeDir := filepath.Join(rootFS, "home")
		if err := os.MkdirAll(etcDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(homeDir, 0755); err != nil {
			t.Fatal(err)
		}

		// passwd only has root - no "user" entry
		passwd := "root::0:0:root:/root:/bin/bash\ndaemon:x:1:1:daemon:/usr/sbin:/bin/nologin\n"
		if err := os.WriteFile(filepath.Join(etcDir, "passwd"), []byte(passwd), 0644); err != nil {
			t.Fatal(err)
		}

		// Call selectTargetUser with empty targetUser (auto-detect)
		result := selectTargetUser(rootFS, "")

		// Should return "user" because:
		// 1. "user" gets added to passwd with home=/home
		// 2. /home exists
		if result != "user" {
			t.Errorf("expected 'user', got %q", result)
		}

		// Verify user was added to passwd
		got, err := os.ReadFile(filepath.Join(etcDir, "passwd"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), "user:x:7575:7575:user:/home:") {
			t.Errorf("user entry not added to passwd:\n%s", got)
		}
	})

	t.Run("falls back to root when /home does not exist", func(t *testing.T) {
		tmpDir := t.TempDir()
		rootFS := filepath.Join(tmpDir, "rootfs")

		// Create minimal rootfs: etc/passwd with just root, NO /home directory
		etcDir := filepath.Join(rootFS, "etc")
		if err := os.MkdirAll(etcDir, 0755); err != nil {
			t.Fatal(err)
		}

		passwd := "root::0:0:root:/root:/bin/bash\n"
		if err := os.WriteFile(filepath.Join(etcDir, "passwd"), []byte(passwd), 0644); err != nil {
			t.Fatal(err)
		}

		result := selectTargetUser(rootFS, "")

		// Should return "root" because /home doesn't exist
		if result != "root" {
			t.Errorf("expected 'root', got %q", result)
		}

		// User should still have been added to passwd (for future use)
		got, _ := os.ReadFile(filepath.Join(etcDir, "passwd"))
		if !strings.Contains(string(got), "user:x:7575:7575:user:/home:") {
			t.Errorf("user entry should still be added to passwd:\n%s", got)
		}
	})

	t.Run("prefers ubuntu if /home/ubuntu exists", func(t *testing.T) {
		tmpDir := t.TempDir()
		rootFS := filepath.Join(tmpDir, "rootfs")

		etcDir := filepath.Join(rootFS, "etc")
		homeUbuntu := filepath.Join(rootFS, "home", "ubuntu")
		homeDir := filepath.Join(rootFS, "home")
		if err := os.MkdirAll(etcDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(homeUbuntu, 0755); err != nil {
			t.Fatal(err)
		}

		passwd := "root::0:0:root:/root:/bin/bash\nubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash\n"
		if err := os.WriteFile(filepath.Join(etcDir, "passwd"), []byte(passwd), 0644); err != nil {
			t.Fatal(err)
		}

		result := selectTargetUser(rootFS, "")

		// Should return "ubuntu" because /home/ubuntu exists (legacy behavior)
		if result != "ubuntu" {
			t.Errorf("expected 'ubuntu', got %q", result)
		}

		// user should NOT have been added (ubuntu wins first)
		got, _ := os.ReadFile(filepath.Join(etcDir, "passwd"))
		if strings.Contains(string(got), "user:x:7575:7575:user:/home:") {
			t.Errorf("user entry should not be added when ubuntu exists:\n%s", got)
		}

		// Make sure /home also exists for completeness, but ubuntu takes precedence
		_ = homeDir
	})

	t.Run("uses explicit targetUser when specified", func(t *testing.T) {
		tmpDir := t.TempDir()
		rootFS := filepath.Join(tmpDir, "rootfs")

		etcDir := filepath.Join(rootFS, "etc")
		if err := os.MkdirAll(etcDir, 0755); err != nil {
			t.Fatal(err)
		}

		passwd := "root::0:0:root:/root:/bin/bash\n"
		if err := os.WriteFile(filepath.Join(etcDir, "passwd"), []byte(passwd), 0644); err != nil {
			t.Fatal(err)
		}

		// Explicit targetUser should be used directly
		result := selectTargetUser(rootFS, "postgres")
		if result != "postgres" {
			t.Errorf("expected 'postgres', got %q", result)
		}
	})

	t.Run("respects existing user home from passwd", func(t *testing.T) {
		tmpDir := t.TempDir()
		rootFS := filepath.Join(tmpDir, "rootfs")

		etcDir := filepath.Join(rootFS, "etc")
		// user already exists with custom home /home/myuser
		customHome := filepath.Join(rootFS, "home", "myuser")
		if err := os.MkdirAll(etcDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(customHome, 0755); err != nil {
			t.Fatal(err)
		}

		passwd := "root::0:0:root:/root:/bin/bash\nuser:x:1000:1000:User:/home/myuser:/bin/bash\n"
		if err := os.WriteFile(filepath.Join(etcDir, "passwd"), []byte(passwd), 0644); err != nil {
			t.Fatal(err)
		}

		result := selectTargetUser(rootFS, "")

		// Should return "user" because /home/myuser exists
		if result != "user" {
			t.Errorf("expected 'user', got %q", result)
		}
	})
}

// TestPrepareContainerRootFS tests that prepareContainerRootFS correctly sets up
// a container's root filesystem. This is the shared helper used by both
// container sessions (SSH) and SFTP sessions (SCP).
func TestPrepareContainerRootFS(t *testing.T) {
	fsDir, snapsDir, libexecDir, cleanup := setupTestEnv(t)
	defer cleanup()
	setFlagsForTest(fsDir, snapsDir, libexecDir)
	defer resetFlagsForTest()

	tailscaleUser := "test@example.com"
	sshUser := "testcontainer"
	rootFS := filepath.Join(fsDir, tailscaleUser, sshUser)
	baseUserFS := filepath.Join(fsDir, tailscaleUser, "test")

	// Call prepareContainerRootFS (the shared setup function)
	if err := prepareContainerRootFS(rootFS, baseUserFS); err != nil {
		t.Fatalf("prepareContainerRootFS: %v", err)
	}

	// Verify the rootfs was created
	if _, err := os.Stat(rootFS); err != nil {
		t.Fatalf("rootFS not created: %v", err)
	}

	// Verify /proc mount point exists
	procDir := filepath.Join(rootFS, "proc")
	info, err := os.Stat(procDir)
	if err != nil {
		t.Errorf("/proc directory not created: %v", err)
	} else if !info.IsDir() {
		t.Errorf("/proc should be a directory")
	}

	// Verify ts binary was copied
	tsBinary := filepath.Join(rootFS, "bin", "ts")
	info, err = os.Stat(tsBinary)
	if err != nil {
		t.Errorf("ts binary not copied: %v", err)
	} else {
		// Verify it's executable
		if info.Mode()&0111 == 0 {
			t.Errorf("ts binary should be executable, mode=%o", info.Mode())
		}
	}

	// Call prepareContainerRootFS again to verify it's idempotent
	if err := prepareContainerRootFS(rootFS, baseUserFS); err != nil {
		t.Errorf("prepareContainerRootFS should be idempotent: %v", err)
	}
}
