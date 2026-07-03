// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// The tests in this file are the VM (vmx) analogues of the container SSH tests
// in ssh_test.go. They take the exact same steps but connect with a "vm/"
// isolation prefix in the SSH username (e.g. "vm/user@frame" instead of
// "user@frame"), which routes the session through runVMXSession: the daemon
// boots a cloud-hypervisor VM and runs the container *inside* it. The observable
// behavior (echo, root-vs-nonroot write-to-/, working directory) must be
// identical to the host-container path.
//
// They land in the "shell" tier via the ^Test(SSH|MinimalShell) regex in
// main_test.go (names start with "TestSSHVm"), and can be run in isolation with
//   -test.run TestSSHVm
// to avoid booting a VM during the cheap container SSH tests.

// TestSSHVmBasic is the VM analogue of TestSSHContainerBasic.
func TestSSHVmBasic(t *testing.T) {
	env := newTestEnv(t)

	frameUUID := "00000000-0000-0000-0000-0000000000a1"
	createTestFrame(t, env, frameUUID)
	createTestRef(t, env, "vmframe", frameUUID)

	d := startDaemon(t, env)

	// "vm/vmframe" -> isolation=vmx, default VM, frame=vmframe.
	output, exitCode, err := sshExec(t, d, "vm/vmframe", "echo hello")
	if err != nil {
		t.Fatalf("sshExec failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("Expected exit code 0, got %d (output: %q)", exitCode, output)
	}
	if !strings.Contains(output, "hello") {
		t.Errorf("Expected output to contain 'hello', got: %q", output)
	}
	t.Logf("Output: %s", output)
}

// TestSSHVmUserRoot is the VM analogue of TestSSHContainerUserRoot.
func TestSSHVmUserRoot(t *testing.T) {
	env := newTestEnv(t)

	frameUUID := "00000000-0000-0000-0000-0000000000a2"
	createTestFrame(t, env, frameUUID)
	createTestRef(t, env, "vmrootframe", frameUUID)

	d := startDaemon(t, env)

	output, exitCode, err := sshExec(t, d, "vm/root@vmrootframe", "echo hi > /rootprobe && echo OK")
	if err != nil {
		t.Fatalf("sshExec failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("Expected exit code 0 (root can write to /), got %d (output: %q)", exitCode, output)
	}
	if !strings.Contains(output, "OK") {
		t.Errorf("Expected output to contain OK, got: %q", output)
	}
	t.Logf("Output: %s", output)
}

// TestSSHVmUserNonRoot is the VM analogue of TestSSHContainerUserNonRoot.
func TestSSHVmUserNonRoot(t *testing.T) {
	env := newTestEnv(t)

	frameUUID := "00000000-0000-0000-0000-0000000000a3"
	createTestFrame(t, env, frameUUID)
	createTestRef(t, env, "vmuserframe", frameUUID)

	d := startDaemon(t, env)

	output, exitCode, err := sshExec(t, d, "vm/user@vmuserframe", "echo hi > /userprobe && echo OK")
	if err != nil {
		t.Fatalf("sshExec failed: %v", err)
	}
	if exitCode == 0 {
		t.Errorf("Expected non-zero exit (non-root cannot write to /), got 0 (output: %q)", output)
	}
	if strings.Contains(output, "OK") {
		t.Errorf("Did not expect OK (write to / should have failed), got: %q", output)
	}
	t.Logf("Output: %s (exit %d)", output, exitCode)
}

// TestSSHVmWorkingDir is the VM analogue of TestSSHContainerWorkingDir.
func TestSSHVmWorkingDir(t *testing.T) {
	env := newTestEnv(t)

	frameUUID := "00000000-0000-0000-0000-0000000000a4"
	createTestFrame(t, env, frameUUID)
	createTestRef(t, env, "vmcwdframe", frameUUID)

	d := startDaemon(t, env)

	output, exitCode, err := sshExec(t, d, "vm/user@vmcwdframe", "pwd")
	if err != nil {
		t.Fatalf("sshExec failed: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("Expected exit code 0, got %d", exitCode)
	}
	if !strings.Contains(output, "/home/user") {
		t.Errorf("Expected pwd to be /home/user, got: %q", output)
	}
	t.Logf("Output: %s", output)
}
