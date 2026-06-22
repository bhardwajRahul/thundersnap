// Package e2e contains integration/workflow end-to-end tests for thundersnap.
package e2e

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestIntegrationWorkflowBasic tests the basic workflow:
// 1. Create frame
// 2. Modify a file
// 3. Snap
// 4. Create new frame from snap
// 5. Verify modification present
func TestIntegrationWorkflowBasic(t *testing.T) {
	env := newTestEnv(t)

	// Create a base snapshot
	baseSnap := env.createBaseSnapshot()
	t.Logf("Created base snapshot: %s", baseSnap)

	// Step 1: Create a frame from the base snapshot
	frame1Path := filepath.Join(env.fsDir, "testuser", "workflow1")
	if err := os.MkdirAll(filepath.Dir(frame1Path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	snapPath := filepath.Join(env.snapshotsDir, baseSnap)
	cmd := exec.Command("btrfs", "subvolume", "snapshot", snapPath, frame1Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}
	t.Logf("Created frame1 at %s", frame1Path)

	// Step 2: Modify a file in the frame
	// Create a unique marker file that we can verify later
	markerContent := generateRandomMarker()
	markerFile := filepath.Join(frame1Path, "home", "user", "workflow-marker.txt")
	if err := os.WriteFile(markerFile, []byte(markerContent), 0644); err != nil {
		t.Fatalf("write marker file: %v", err)
	}
	t.Logf("Created marker file with content: %s", markerContent[:20]+"...")

	// Also modify an existing file
	profilePath := filepath.Join(frame1Path, "home", "user", ".profile")
	existingContent, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("read .profile: %v", err)
	}
	newContent := string(existingContent) + "\n# Modified by workflow test\nexport WORKFLOW_TEST=1\n"
	if err := os.WriteFile(profilePath, []byte(newContent), 0644); err != nil {
		t.Fatalf("write .profile: %v", err)
	}
	t.Logf("Modified .profile")

	// Step 3: Create a snapshot of the modified frame
	snap2ID := "workflow-snap-" + generateShortID()
	snap2Path := filepath.Join(env.snapshotsDir, snap2ID)
	cmd = exec.Command("btrfs", "subvolume", "snapshot", "-r", frame1Path, snap2Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot frame: %v\n%s", err, out)
	}
	t.Logf("Created snapshot %s from frame1", snap2ID)

	// Step 4: Create a new frame from the new snapshot
	frame2Path := filepath.Join(env.fsDir, "testuser", "workflow2")
	cmd = exec.Command("btrfs", "subvolume", "snapshot", snap2Path, frame2Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot to frame2: %v\n%s", err, out)
	}
	t.Logf("Created frame2 at %s from snapshot %s", frame2Path, snap2ID)

	// Step 5: Verify the modifications are present in the new frame
	// Check marker file
	markerFile2 := filepath.Join(frame2Path, "home", "user", "workflow-marker.txt")
	content, err := os.ReadFile(markerFile2)
	if err != nil {
		t.Fatalf("read marker file in frame2: %v", err)
	}
	if string(content) != markerContent {
		t.Errorf("marker file content mismatch:\n  got: %q\n  want: %q", string(content), markerContent)
	} else {
		t.Logf("Marker file content verified in frame2")
	}

	// Check modified .profile
	profilePath2 := filepath.Join(frame2Path, "home", "user", ".profile")
	content, err = os.ReadFile(profilePath2)
	if err != nil {
		t.Fatalf("read .profile in frame2: %v", err)
	}
	if string(content) != newContent {
		t.Errorf(".profile content mismatch in frame2")
	} else {
		t.Logf(".profile modification verified in frame2")
	}
}

// TestWorkflowHomeWorkSeparation tests that home and work subvolumes
// can be snapshotted and restored independently.
func TestWorkflowHomeWorkSeparation(t *testing.T) {
	env := newTestEnv(t)

	// Create a base snapshot
	baseSnap := env.createBaseSnapshot()

	// Create frame with separate home/work subvolumes
	framePath := filepath.Join(env.fsDir, "testuser", "homeworktest")
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Clone base to frame
	snapPath := filepath.Join(env.snapshotsDir, baseSnap)
	cmd := exec.Command("btrfs", "subvolume", "snapshot", snapPath, framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	// Replace home with an empty subvolume
	homePath := filepath.Join(framePath, "home")
	os.RemoveAll(homePath)
	cmd = exec.Command("btrfs", "subvolume", "create", homePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create home subvolume: %v\n%s", err, out)
	}
	os.Chown(homePath, 1000, 1000)

	// Create user directory in home
	userHome := filepath.Join(homePath, "user")
	if err := os.MkdirAll(userHome, 0755); err != nil {
		t.Fatalf("mkdir user home: %v", err)
	}
	os.Chown(userHome, 1000, 1000)

	// Replace work with an empty subvolume
	workPath := filepath.Join(framePath, "work")
	os.RemoveAll(workPath)
	cmd = exec.Command("btrfs", "subvolume", "create", workPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create work subvolume: %v\n%s", err, out)
	}
	os.Chown(workPath, 1000, 1000)

	// Add content to home
	homeFile := filepath.Join(userHome, "home-content.txt")
	if err := os.WriteFile(homeFile, []byte("home content\n"), 0644); err != nil {
		t.Fatalf("write home file: %v", err)
	}

	// Add content to work
	workFile := filepath.Join(workPath, "work-content.txt")
	if err := os.WriteFile(workFile, []byte("work content\n"), 0644); err != nil {
		t.Fatalf("write work file: %v", err)
	}

	// Snapshot home only
	homeSnap := filepath.Join(env.snapshotsDir, "home-snap")
	cmd = exec.Command("btrfs", "subvolume", "snapshot", "-r", homePath, homeSnap)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("snapshot home: %v\n%s", err, out)
	}
	t.Logf("Created home snapshot")

	// Snapshot work only
	workSnap := filepath.Join(env.snapshotsDir, "work-snap")
	cmd = exec.Command("btrfs", "subvolume", "snapshot", "-r", workPath, workSnap)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("snapshot work: %v\n%s", err, out)
	}
	t.Logf("Created work snapshot")

	// Create new frame from base + home snapshot + work snapshot
	frame2Path := filepath.Join(env.fsDir, "testuser", "homeworktest2")
	cmd = exec.Command("btrfs", "subvolume", "snapshot", snapPath, frame2Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone base to frame2: %v\n%s", err, out)
	}

	// Replace home with snapshot
	home2Path := filepath.Join(frame2Path, "home")
	os.RemoveAll(home2Path)
	cmd = exec.Command("btrfs", "subvolume", "snapshot", homeSnap, home2Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restore home: %v\n%s", err, out)
	}

	// Replace work with snapshot
	work2Path := filepath.Join(frame2Path, "work")
	os.RemoveAll(work2Path)
	cmd = exec.Command("btrfs", "subvolume", "snapshot", workSnap, work2Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restore work: %v\n%s", err, out)
	}

	// Verify content
	homeFile2 := filepath.Join(home2Path, "user", "home-content.txt")
	content, err := os.ReadFile(homeFile2)
	if err != nil {
		t.Fatalf("read home file in frame2: %v", err)
	}
	if string(content) != "home content\n" {
		t.Errorf("home content mismatch: %q", string(content))
	}
	t.Logf("Home content verified")

	workFile2 := filepath.Join(work2Path, "work-content.txt")
	content, err = os.ReadFile(workFile2)
	if err != nil {
		t.Fatalf("read work file in frame2: %v", err)
	}
	if string(content) != "work content\n" {
		t.Errorf("work content mismatch: %q", string(content))
	}
	t.Logf("Work content verified")
}

// TestCrossFrameDataSharingViaWorkVolume tests that two frames can share
// data through a common work volume. The work volume is snapshotted and
// mounted in both frames, allowing cross-frame data sharing.
func TestCrossFrameDataSharingViaWorkVolume(t *testing.T) {
	env := newTestEnv(t)

	baseSnap := env.createBaseSnapshot()

	// Create a shared work snapshot
	sharedWorkPath := filepath.Join(env.snapshotsDir, "shared-work")
	cmd := exec.Command("btrfs", "subvolume", "create", sharedWorkPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create shared work: %v\n%s", err, out)
	}
	os.Chown(sharedWorkPath, 1000, 1000)

	// Add initial content to shared work
	sharedFile := filepath.Join(sharedWorkPath, "shared-data.txt")
	if err := os.WriteFile(sharedFile, []byte("initial shared content\n"), 0644); err != nil {
		t.Fatalf("write shared file: %v", err)
	}

	// Snapshot the shared work
	sharedWorkSnap := filepath.Join(env.snapshotsDir, "shared-work-snap")
	cmd = exec.Command("btrfs", "subvolume", "snapshot", "-r", sharedWorkPath, sharedWorkSnap)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("snapshot shared work: %v\n%s", err, out)
	}
	t.Logf("Created shared work snapshot")

	// Create frame1 with the shared work snapshot
	frame1Path := filepath.Join(env.fsDir, "testuser", "share1")
	if err := os.MkdirAll(filepath.Dir(frame1Path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	snapPath := filepath.Join(env.snapshotsDir, baseSnap)
	cmd = exec.Command("btrfs", "subvolume", "snapshot", snapPath, frame1Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone base to frame1: %v\n%s", err, out)
	}

	// Replace frame1's work with a writable clone of shared work
	work1Path := filepath.Join(frame1Path, "work")
	os.RemoveAll(work1Path)
	cmd = exec.Command("btrfs", "subvolume", "snapshot", sharedWorkSnap, work1Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone shared work to frame1: %v\n%s", err, out)
	}
	t.Logf("Created frame1 with shared work")

	// Create frame2 with the SAME shared work snapshot
	frame2Path := filepath.Join(env.fsDir, "testuser", "share2")
	cmd = exec.Command("btrfs", "subvolume", "snapshot", snapPath, frame2Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone base to frame2: %v\n%s", err, out)
	}

	work2Path := filepath.Join(frame2Path, "work")
	os.RemoveAll(work2Path)
	cmd = exec.Command("btrfs", "subvolume", "snapshot", sharedWorkSnap, work2Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone shared work to frame2: %v\n%s", err, out)
	}
	t.Logf("Created frame2 with shared work")

	// Verify both frames have the initial content
	content1, err := os.ReadFile(filepath.Join(work1Path, "shared-data.txt"))
	if err != nil {
		t.Fatalf("read from frame1: %v", err)
	}
	content2, err := os.ReadFile(filepath.Join(work2Path, "shared-data.txt"))
	if err != nil {
		t.Fatalf("read from frame2: %v", err)
	}

	if string(content1) != string(content2) {
		t.Errorf("initial content differs: frame1=%q frame2=%q", content1, content2)
	}
	t.Logf("Both frames start with same shared content")

	// Modify in frame1
	modifiedFile := filepath.Join(work1Path, "frame1-addition.txt")
	if err := os.WriteFile(modifiedFile, []byte("added by frame1\n"), 0644); err != nil {
		t.Fatalf("write from frame1: %v", err)
	}

	// Snapshot frame1's work
	work1Snap := filepath.Join(env.snapshotsDir, "work1-snap")
	cmd = exec.Command("btrfs", "subvolume", "snapshot", "-r", work1Path, work1Snap)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("snapshot frame1 work: %v\n%s", err, out)
	}

	// "Share" the updated work to frame2 by replacing its work with the new snapshot
	// In real usage, this simulates syncing work directories between frames
	os.RemoveAll(work2Path)
	cmd = exec.Command("btrfs", "subvolume", "snapshot", work1Snap, work2Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sync work to frame2: %v\n%s", err, out)
	}

	// Verify frame2 now sees frame1's addition
	content2New, err := os.ReadFile(filepath.Join(work2Path, "frame1-addition.txt"))
	if err != nil {
		t.Fatalf("read synced file from frame2: %v", err)
	}
	if string(content2New) != "added by frame1\n" {
		t.Errorf("synced content wrong: got %q", content2New)
	}
	t.Logf("Frame2 successfully received Frame1's work updates via shared snapshot")
}

func generateRandomMarker() string {
	b := make([]byte, 32)
	rand.Read(b)
	return "WORKFLOW_MARKER_" + hex.EncodeToString(b) + "\n"
}

func generateShortID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
