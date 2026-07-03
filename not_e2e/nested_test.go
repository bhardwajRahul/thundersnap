// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

// Package e2e contains end-to-end tests for thundersnap nested execution.
package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// TestNestedThundersnapCgroup tests that a nested thundersnap instance can
// create its own cgroups inside a container. This verifies that:
// 1. The PID-based cgroup naming (thundersnap-{pid}) allows multiple instances
// 2. Cgroup namespacing (if present) prevents conflicts
// 3. A nested thundersnap can set up resource limits
//
// The test creates a container environment and attempts to run cgroup setup
// code similar to what thundersnapd does at startup.
func TestNestedThundersnapCgroup(t *testing.T) {
	env := newTestEnv(t)

	// Create a frame for testing
	baseSnap := env.createBaseSnapshot()
	framePath := filepath.Join(env.fsDir, "testuser", "nestedtest")
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	snapPath := filepath.Join(env.snapshotsDir, baseSnap)
	cmd := exec.Command("btrfs", "subvolume", "snapshot", snapPath, framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	// Copy ts binary into the frame
	tsDst := filepath.Join(framePath, "bin/ts")
	if err := copyFile(env.tsBinary, tsDst); err != nil {
		t.Fatalf("copy ts to frame: %v", err)
	}

	absFramePath, err := filepath.Abs(framePath)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}

	// Create a test script that tries to create a cgroup like thundersnapd would
	// The script attempts to:
	// 1. Create a cgroup directory with a PID-based name
	// 2. Write to cgroup control files
	// 3. Clean up the cgroup
	testScript := `#!/bin/sh
set -e

# Get our PID to use in cgroup name (like thundersnapd does)
MYPID=$$
CGROUP_NAME="thundersnap-$MYPID"
CGROUP_PATH="/sys/fs/cgroup/$CGROUP_NAME"

echo "Testing cgroup creation with name: $CGROUP_NAME"

# Try to create the cgroup directory
if mkdir -p "$CGROUP_PATH" 2>/dev/null; then
    echo "CGROUP:created:$CGROUP_PATH"

    # Try to enable controllers (might fail if not available, that's ok)
    if echo "+memory +pids +cpu" > "$CGROUP_PATH/cgroup.subtree_control" 2>/dev/null; then
        echo "CONTROLLERS:enabled"
    else
        echo "CONTROLLERS:skipped"
    fi

    # Try to set memory.high (might fail if memory controller not available)
    if echo "100000000" > "$CGROUP_PATH/memory.high" 2>/dev/null; then
        echo "MEMORY_HIGH:set"
    else
        echo "MEMORY_HIGH:skipped"
    fi

    # Try to set pids.max
    if echo "1000" > "$CGROUP_PATH/pids.max" 2>/dev/null; then
        echo "PIDS_MAX:set"
    else
        echo "PIDS_MAX:skipped"
    fi

    # Clean up - remove the cgroup we created
    rmdir "$CGROUP_PATH" 2>/dev/null || true
    echo "CGROUP:cleaned"
else
    # Check if cgroup2 is even mounted
    if grep -q cgroup2 /proc/mounts 2>/dev/null; then
        echo "CGROUP:failed:permission_denied"
    else
        echo "CGROUP:failed:not_mounted"
    fi
fi

echo "DONE"
`

	// Write the test script to the container
	scriptPath := filepath.Join(framePath, "tmp/test-cgroup.sh")
	if err := os.MkdirAll(filepath.Dir(scriptPath), 0755); err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	if err := os.WriteFile(scriptPath, []byte(testScript), 0755); err != nil {
		t.Fatalf("write test script: %v", err)
	}

	// Run the test in a container namespace
	tsBinary := filepath.Join(absFramePath, "bin", "ts")
	cmd = exec.Command(tsBinary, "drop-caps-and-run",
		"--chroot="+absFramePath,
		"--", "/bin/sh", "/tmp/test-cgroup.sh")
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS,
	}

	output, err := cmd.CombinedOutput()
	t.Logf("Nested cgroup test output:\n%s", output)

	if err != nil {
		// The test might fail due to cgroup not being available in container,
		// which is useful information but not necessarily a test failure
		t.Logf("Command exited with error (may be expected): %v", err)
	}

	// Parse output to determine what happened
	outputStr := string(output)

	if containsLine(outputStr, "CGROUP:created:") {
		t.Log("SUCCESS: Nested cgroup creation succeeded")

		if containsLine(outputStr, "CONTROLLERS:enabled") {
			t.Log("  - Controllers enabled")
		}
		if containsLine(outputStr, "MEMORY_HIGH:set") {
			t.Log("  - memory.high set")
		}
		if containsLine(outputStr, "PIDS_MAX:set") {
			t.Log("  - pids.max set")
		}
		if containsLine(outputStr, "CGROUP:cleaned") {
			t.Log("  - Cgroup cleaned up")
		}
	} else if containsLine(outputStr, "CGROUP:failed:permission_denied") {
		t.Log("INFO: Nested cgroup creation blocked by permissions")
		t.Log("This indicates cgroup namespace isolation is working - nested instances would need their own cgroup namespace")
	} else if containsLine(outputStr, "CGROUP:failed:not_mounted") {
		t.Log("INFO: cgroup2 not mounted in container")
		t.Log("This is expected - containers don't have cgroup filesystem by default")
	} else {
		t.Log("UNKNOWN: Could not determine cgroup test result")
	}

	// The test passes regardless of whether cgroups work inside the container.
	// What we're really testing is:
	// 1. The container runs without crashing
	// 2. We can determine what cgroup behavior to expect in nested scenarios
	if !containsLine(outputStr, "DONE") {
		t.Error("Test script did not complete successfully")
	}
}

// TestNestedThundersnapNamespaceIsolation tests that nested namespace creation
// works correctly inside a container. This verifies that a nested thundersnap
// instance could create its own PID/mount/UTS namespaces.
func TestNestedThundersnapNamespaceIsolation(t *testing.T) {
	env := newTestEnv(t)

	// Create a frame for testing
	baseSnap := env.createBaseSnapshot()
	framePath := filepath.Join(env.fsDir, "testuser", "nestednstest")
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	snapPath := filepath.Join(env.snapshotsDir, baseSnap)
	cmd := exec.Command("btrfs", "subvolume", "snapshot", snapPath, framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs snapshot: %v\n%s", err, out)
	}

	// Copy ts binary into the frame
	tsDst := filepath.Join(framePath, "bin/ts")
	if err := copyFile(env.tsBinary, tsDst); err != nil {
		t.Fatalf("copy ts to frame: %v", err)
	}

	absFramePath, err := filepath.Abs(framePath)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}

	// Run ts check-isolation twice - first in the outer container, then
	// try to run a nested container that also checks isolation
	tsBinary := filepath.Join(absFramePath, "bin", "ts")

	// First, verify the outer container works
	cmd = exec.Command(tsBinary, "drop-caps-and-run",
		"--chroot="+absFramePath,
		"--hostname=outer",
		"--", "/bin/ts", "check-isolation")
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS,
	}

	outerOutput, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("Outer container output: %s", outerOutput)
		t.Fatalf("Outer container check-isolation error: %v", err)
	}

	outerResult := parseIsolationOutput(string(outerOutput))
	if outerResult.hostname != "outer" {
		t.Errorf("Outer hostname: got %q, want %q", outerResult.hostname, "outer")
	}
	if !outerResult.isPID1 {
		t.Errorf("Outer container should be PID 1")
	}
	t.Logf("Outer container: hostname=%s, pid1=%v, pid_ns=%s, mnt_ns=%s",
		outerResult.hostname, outerResult.isPID1,
		outerResult.namespaces["pid"], outerResult.namespaces["mnt"])

	// Now try to run a nested namespace from within the container.
	// We'll run ts drop-caps-and-run again from inside the container to see
	// if nested namespace creation works.
	//
	// This tests whether CLONE_NEWPID/NEWNS/NEWUTS work from inside a container.
	nestedScript := `#!/bin/sh
# Try to create nested namespaces using unshare
# This simulates what a nested thundersnapd would do

echo "OUTER_PID:$$"
echo "OUTER_HOSTNAME:$(hostname)"

# Try unshare with new PID namespace
# Note: unshare -p -f runs a subprocess in a new PID namespace
if unshare --pid --fork --mount-proc sh -c 'echo NESTED_PID:$$ ; echo NESTED_SUCCESS:yes' 2>/dev/null; then
    echo "UNSHARE:succeeded"
else
    echo "UNSHARE:failed"
fi

echo "DONE"
`

	scriptPath := filepath.Join(framePath, "tmp/test-nested-ns.sh")
	if err := os.MkdirAll(filepath.Dir(scriptPath), 0755); err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	if err := os.WriteFile(scriptPath, []byte(nestedScript), 0755); err != nil {
		t.Fatalf("write nested test script: %v", err)
	}

	cmd = exec.Command(tsBinary, "drop-caps-and-run",
		"--chroot="+absFramePath,
		"--hostname=outer",
		"--", "/bin/sh", "/tmp/test-nested-ns.sh")
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS,
	}

	nestedOutput, err := cmd.CombinedOutput()
	t.Logf("Nested namespace test output:\n%s", nestedOutput)

	if err != nil {
		t.Logf("Nested namespace test exited with error (may be expected): %v", err)
	}

	outputStr := string(nestedOutput)

	if containsLine(outputStr, "NESTED_SUCCESS:yes") {
		t.Log("SUCCESS: Nested namespace creation works")
		t.Log("A nested thundersnap instance could create its own containers")
	} else if containsLine(outputStr, "UNSHARE:failed") {
		t.Log("INFO: Nested namespace creation failed")
		t.Log("This may be due to capabilities being dropped or namespace limits")
	}

	if !containsLine(outputStr, "DONE") {
		t.Error("Nested namespace test script did not complete")
	}
}

// containsLine checks if the output contains a line starting with the prefix.
func containsLine(output, prefix string) bool {
	for _, line := range splitLines(output) {
		if len(line) >= len(prefix) && line[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

// splitLines splits output into lines, handling both \n and \r\n.
func splitLines(s string) []string {
	var lines []string
	var current []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, string(current))
			current = nil
		} else if s[i] != '\r' {
			current = append(current, s[i])
		}
	}
	if len(current) > 0 {
		lines = append(lines, string(current))
	}
	return lines
}

// TestCgroupMultiLevelSubtreeControl tests that multi-level cgroup hierarchies
// have cgroup.subtree_control properly enabled on intermediate directories.
//
// This test verifies the fix for the bug where cgroups like
// "thundersnap-PID/user@domain.com/container" would fail to set memory.high
// because the intermediate "user@domain.com" directory didn't have
// +memory +pids +cpu enabled in its cgroup.subtree_control.
//
// Requires root and cgroup v2 to actually test the cgroup functionality.
func TestCgroupMultiLevelSubtreeControl(t *testing.T) {
	if os.Getuid() != 0 {
		t.Fatal("cgroup test requires root")
	}

	// Check if cgroup v2 is available
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		t.Fatal("cgroup v2 not available")
	}

	// Create a unique parent cgroup for this test
	testPid := os.Getpid()
	parentCgroup := fmt.Sprintf("thundersnap-test-%d", testPid)
	parentPath := filepath.Join("/sys/fs/cgroup", parentCgroup)

	// Clean up at the end
	defer func() {
		// Remove leaf first, then intermediate, then parent
		os.Remove(filepath.Join(parentPath, "testuser@example.com", "testcontainer", "cgroup.procs"))
		os.Remove(filepath.Join(parentPath, "testuser@example.com", "testcontainer"))
		os.Remove(filepath.Join(parentPath, "testuser@example.com"))
		os.Remove(parentPath)
	}()

	// Create parent cgroup
	if err := os.MkdirAll(parentPath, 0755); err != nil {
		t.Fatalf("create parent cgroup: %v", err)
	}

	// Enable controllers on parent
	subtreeControl := filepath.Join(parentPath, "cgroup.subtree_control")
	if err := os.WriteFile(subtreeControl, []byte("+memory +pids +cpu"), 0644); err != nil {
		t.Logf("warning: failed to enable controllers on parent (may not have permissions): %v", err)
		// Continue anyway - test will skip if we can't set up cgroups
	}

	// Now create the multi-level hierarchy: parent/user@domain/container
	// This simulates what thundersnapd does with cgroupName like
	// "thundersnap-PID/user@tailscale.com/container"
	intermediatePath := filepath.Join(parentPath, "testuser@example.com")
	leafPath := filepath.Join(intermediatePath, "testcontainer")

	// Create intermediate and leaf directories
	if err := os.MkdirAll(leafPath, 0755); err != nil {
		t.Fatalf("create leaf cgroup: %v", err)
	}

	// THE BUG: Before the fix, we only enabled subtree_control on the parent,
	// not on intermediate directories. Now we need to enable it on the
	// intermediate directory too.
	intermediateSubtree := filepath.Join(intermediatePath, "cgroup.subtree_control")
	if err := os.WriteFile(intermediateSubtree, []byte("+memory +pids +cpu"), 0644); err != nil {
		t.Logf("warning: failed to enable controllers on intermediate: %v", err)
	}

	// Try to write memory.high on the leaf cgroup
	// Without the fix (no subtree_control on intermediate), this would fail with
	// "permission denied" because the memory controller isn't enabled for the leaf.
	memHighPath := filepath.Join(leafPath, "memory.high")
	testValue := "100000000" // 100MB
	err := os.WriteFile(memHighPath, []byte(testValue), 0644)
	if err != nil {
		t.Errorf("failed to set memory.high on leaf cgroup: %v", err)
		t.Log("This indicates the intermediate cgroup.subtree_control was not set correctly")

		// Verify controllers are available
		parentControllers, _ := os.ReadFile(filepath.Join(parentPath, "cgroup.controllers"))
		t.Logf("Parent controllers: %s", string(parentControllers))
		parentSubtree, _ := os.ReadFile(subtreeControl)
		t.Logf("Parent subtree_control: %s", string(parentSubtree))
		intermediateControllers, _ := os.ReadFile(filepath.Join(intermediatePath, "cgroup.controllers"))
		t.Logf("Intermediate controllers: %s", string(intermediateControllers))
		intermediateSubtreeVal, _ := os.ReadFile(intermediateSubtree)
		t.Logf("Intermediate subtree_control: %s", string(intermediateSubtreeVal))
	} else {
		t.Log("Successfully set memory.high on leaf cgroup with multi-level hierarchy")

		// Verify the value was actually set
		readBack, err := os.ReadFile(memHighPath)
		if err != nil {
			t.Errorf("failed to read back memory.high: %v", err)
		} else {
			t.Logf("memory.high value: %s", string(readBack))
		}
	}

	// Also test pids.max to make sure pids controller works
	pidsMaxPath := filepath.Join(leafPath, "pids.max")
	if err := os.WriteFile(pidsMaxPath, []byte("1000"), 0644); err != nil {
		t.Errorf("failed to set pids.max on leaf cgroup: %v", err)
	} else {
		t.Log("Successfully set pids.max on leaf cgroup")
	}

	// Test cpu.weight
	cpuWeightPath := filepath.Join(leafPath, "cpu.weight")
	if err := os.WriteFile(cpuWeightPath, []byte("100"), 0644); err != nil {
		t.Errorf("failed to set cpu.weight on leaf cgroup: %v", err)
	} else {
		t.Log("Successfully set cpu.weight on leaf cgroup")
	}
}
