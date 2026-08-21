// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

// nested_test.go contains an end-to-end test for nested thundersnap: running
// a thundersnapd inside a thundersnap frame (frame inside a frame). This
// catches the btrfs subvolume + setns stale root dentry issues that only
// appear in nested mode. See docs/nested-btrfs-setns.md for the full
// explanation.
//
// Per CLAUDE.md: e2e tests MUST NEVER SKIP. If a precondition is missing, the
// test must fail (t.Fatal), not skip.
package e2e

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// copyBinaryWithDeps copies a dynamically-linked binary and all its shared
// library dependencies into a target rootfs directory. This is necessary for
// running dynamic binaries (like btrfs) inside minimal container rootfs that
// don't have /lib64 or /usr/lib.
//
// For static binaries, this copies just the binary (ldd either fails or
// reports no deps).
func copyBinaryWithDeps(t *testing.T, src, rootfs string) {
	t.Helper()

	// Copy the binary itself to the same path in the rootfs.
	dst := filepath.Join(rootfs, src)
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(dst), err)
	}
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copy %s: %v", src, err)
	}
	if err := os.Chmod(dst, 0755); err != nil {
		t.Fatalf("chmod %s: %v", dst, err)
	}

	// Get shared library dependencies via ldd. Static binaries will either
	// fail ldd or report "not a dynamic executable", which we handle by
	// returning early.
	out, err := exec.Command("ldd", src).CombinedOutput()
	if err != nil {
		return // static binary or ldd failed — no deps to copy
	}

	// Parse ldd output. Lines look like:
	//   libfoo.so.1 => /usr/lib/x86_64-linux-gnu/libfoo.so.1 (0x...)
	//   /lib64/ld-linux-x86-64.so.2 (0x...)
	//   linux-vdso.so.1 (0x...)       ← virtual, skip
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var libPath string
		if strings.Contains(line, "=>") {
			parts := strings.SplitN(line, "=>", 2)
			rest := strings.TrimSpace(parts[1])
			if idx := strings.Index(rest, " ("); idx >= 0 {
				libPath = rest[:idx]
			} else {
				libPath = rest
			}
		} else {
			fields := strings.Fields(line)
			if len(fields) > 0 && strings.HasPrefix(fields[0], "/") {
				libPath = fields[0]
			}
		}
		if libPath == "" || !strings.HasPrefix(libPath, "/") {
			continue // skip virtual libraries (linux-vdso, etc.)
		}
		libDst := filepath.Join(rootfs, libPath)
		if err := os.MkdirAll(filepath.Dir(libDst), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(libDst), err)
		}
		if err := copyFile(libPath, libDst); err != nil {
			t.Logf("copy lib %s: %v (continuing)", libPath, err)
		}
	}
}

// innerDaemon is a handle to an inner thundersnapd running inside a frame on
// the outer daemon. The inner daemon's SSH port is accessible from the test
// because thundersnap containers share the network namespace (no CLONE_NEWNET).
type innerDaemon struct {
	addr    string       // 127.0.0.1:PORT — accessible from the test
	sshSess *ssh.Session // the SSH session that keeps the daemon alive
	sshConn *ssh.Client  // the SSH connection to the outer frame
}

// Close kills the inner daemon by closing the SSH session that hosts it.
func (id *innerDaemon) Close() {
	if id.sshSess != nil {
		id.sshSess.Close()
	}
	if id.sshConn != nil {
		id.sshConn.Close()
	}
}

// setupInnerDaemon sets up an inner thundersnapd inside a frame on the outer
// daemon. It:
//  1. Copies the necessary binaries (thundersnapd, vshd, busybox, btrfs) into
//     the outer frame's rootfs via the host filesystem (the test runs as root).
//  2. Creates a policy file inside the frame.
//  3. Starts the inner daemon via SSH (using Session.Start so it runs as long
//     as the session is open).
//  4. Waits for the daemon to accept SSH connections.
//
// Returns an innerDaemon handle. Call Close() (or use t.Cleanup) to stop the
// daemon when done.
func setupInnerDaemon(t *testing.T, d *daemonInstance, env *testEnv, outerRef, outerUUID string) *innerDaemon {
	t.Helper()

	framePath := filepath.Join(env.root, "fs", "shared", outerUUID)

	// --- 1. Copy binaries into the frame rootfs ---

	// thundersnapd (static, built with CGO_ENABLED=0 by Makefile) → /bin/thundersnapd
	if err := copyFile(env.daemonBinary, filepath.Join(framePath, "bin", "thundersnapd")); err != nil {
		t.Fatalf("copy thundersnapd: %v", err)
	}
	os.Chmod(filepath.Join(framePath, "bin", "thundersnapd"), 0755)

	// vshd (static) → /bin/vshd
	vshdPath := env.requireBinary("vshd")
	if err := copyFile(vshdPath, filepath.Join(framePath, "bin", "vshd")); err != nil {
		t.Fatalf("copy vshd: %v", err)
	}
	os.Chmod(filepath.Join(framePath, "bin", "vshd"), 0755)

	// busybox (static) → /bin/busybox. Provides cp --reflink and setsid, both
	// needed by the inner daemon. ts (the shell) is already in /bin/ts from
	// the outer daemon's prepareContainerRootFS.
	busyboxPath, err := exec.LookPath("busybox")
	if err != nil {
		t.Fatalf("busybox required for nested test: %v", err)
	}
	if err := copyFile(busyboxPath, filepath.Join(framePath, "bin", "busybox")); err != nil {
		t.Fatalf("copy busybox: %v", err)
	}
	os.Chmod(filepath.Join(framePath, "bin", "busybox"), 0755)
	// Create applet symlinks: cp and setsid. The daemon shells out to
	// `cp --reflink=always` (rootfs.go) and we need `setsid` to detach the
	// inner daemon from the SSH session.
	for _, applet := range []string{"cp", "setsid", "nohup", "cat"} {
		link := filepath.Join(framePath, "bin", applet)
		os.Remove(link)
		if err := os.Symlink("busybox", link); err != nil {
			t.Fatalf("symlink %s: %v", applet, err)
		}
	}

	// btrfs (dynamic) → /usr/bin/btrfs + shared libs. The daemon uses this
	// via the btrfsutil package for subvolume create/snapshot/delete.
	btrfsPath, err := exec.LookPath("btrfs")
	if err != nil {
		t.Fatalf("btrfs command required for nested test: %v", err)
	}
	copyBinaryWithDeps(t, btrfsPath, framePath)

	// --- 2. Create policy file ---
	policy := `{
		"grants": [
			{
				"principals": ["*"],
				"cap": {
					"role": "developer",
					"isolation": "container",
					"maxFrames": 10
				}
			}
		]
	}`
	policyPath := filepath.Join(framePath, "tmp", "policy.json")
	if err := os.WriteFile(policyPath, []byte(policy), 0644); err != nil {
		t.Fatalf("write policy: %v", err)
	}

	// --- 3. Start the inner daemon via SSH ---
	port, err := getFreePort()
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	// Connect to the outer frame to start the inner daemon.
	// We use Session.Start (not Run) so the daemon runs as the session's
	// command. When we close the session, the daemon is killed — perfect
	// for test cleanup.
	outerConn, err := ssh.Dial("tcp", d.addr, sshConfig("root@"+outerRef))
	if err != nil {
		t.Fatalf("dial outer frame: %v", err)
	}
	innerSess, err := outerConn.NewSession()
	if err != nil {
		t.Fatalf("new session on outer frame: %v", err)
	}

	// The inner daemon command. PATH includes /bin (ts, vshd, busybox) and
	// /usr/bin (btrfs) so the daemon and its children can find all the tools
	// they need. --libexec-dir=/bin so the daemon finds ts and vshd there.
	// Output goes to /tmp/inner.log for debugging.
	daemonCmd := fmt.Sprintf(
		"PATH=/bin:/usr/bin:/usr/libexec /bin/thundersnapd --test-listen=%s --test-user=e2e "+
			"--data-dir=/tmp/inner-data --state-dir=/tmp/inner-state "+
			"--libexec-dir=/bin --policy=/tmp/policy.json "+
			"--vm-dir=/vm > /tmp/inner.log 2>&1",
		addr,
	)

	if err := innerSess.Start(daemonCmd); err != nil {
		t.Fatalf("start inner daemon: %v", err)
	}

	id := &innerDaemon{addr: addr, sshSess: innerSess, sshConn: outerConn}
	t.Cleanup(id.Close)

	// --- 4. Wait for the inner daemon to accept connections ---
	deadline := time.Now().Add(20 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			// Read the inner daemon's log for debugging.
			logConn, _ := ssh.Dial("tcp", d.addr, sshConfig("root@"+outerRef))
			if logConn != nil {
				logSess, _ := logConn.NewSession()
				logOutput, _ := logSess.CombinedOutput("cat /tmp/inner.log 2>&1; true")
				logSess.Close()
				logConn.Close()
				t.Fatalf("inner daemon not ready on %s: %v\nInner daemon log:\n%s",
					addr, err, logOutput)
			}
			t.Fatalf("inner daemon not ready on %s: %v", addr, err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	return id
}

// innerSSHExec runs a command on the inner daemon.
func innerSSHExec(t *testing.T, addr, user, cmd string) (string, int, error) {
	t.Helper()
	client, err := ssh.Dial("tcp", addr, sshConfig(user))
	if err != nil {
		return "", -1, fmt.Errorf("dial inner daemon: %w", err)
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return "", -1, fmt.Errorf("new session: %w", err)
	}
	defer session.Close()
	output, err := session.CombinedOutput(cmd)
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*ssh.ExitError); ok {
			exitCode = exitErr.ExitStatus()
		} else {
			return string(output), -1, fmt.Errorf("run command: %w", err)
		}
	}
	return string(output), exitCode, nil
}

// TestNestedThundersnap runs thundersnap inside thundersnap: the outer daemon
// starts a container, inside which an inner daemon starts its own container.
// This catches the btrfs subvolume + setns stale root dentry issues that only
// appear in nested mode (see docs/nested-btrfs-setns.md).
//
// Flow:
//  1. Outer daemon → create frame "outer-nested"
//  2. Copy binaries into the frame, start inner daemon inside it
//  3. Inner daemon → `ts frame --ref=inner nil:nil:nil`
//  4. SSH to inner frame → `echo hello-nested`
//  5. Verify output
//  6. If /dev/kvm exists on the host, also check it's propagated into the
//     nested container (so nested VMs would work). We don't boot a VM — that
//     would be KVM-inside-container-inside-container, which is slow and
//     fragile. The container-inside-kvm path is already covered by
//     TestVMNamespaceSetup, and container-inside-kvm is architecturally the
//     same as container-inside-root (the container-init code is identical).
func TestNestedThundersnap(t *testing.T) {
	env := newTestEnv(t)
	d := startDaemon(t, env)

	// Create the outer frame.
	outerUUID := createFrameViaDaemon(t, d, "outer-nested")
	t.Logf("outer frame: %s", outerUUID)

	// Set up and start the inner daemon inside the outer frame.
	id := setupInnerDaemon(t, d, env, "outer-nested", outerUUID)
	t.Logf("inner daemon ready on %s", id.addr)

	// Create a frame on the inner daemon.
	innerOutput, exitCode, err := innerSSHExec(t, id.addr, "root@",
		"ts frame --ref=inner nil:nil:nil")
	if err != nil {
		t.Fatalf("ts frame on inner daemon: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("ts frame on inner daemon: exit %d, output: %s", exitCode, innerOutput)
	}
	t.Logf("inner frame created: %s", strings.TrimSpace(innerOutput))

	// SSH to the inner frame and run a command.
	output, exitCode, err := innerSSHExec(t, id.addr, "inner", "echo hello-nested")
	if err != nil {
		t.Fatalf("ssh to inner frame: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("echo in inner frame: expected exit 0, got %d (output: %q)", exitCode, output)
	}
	if !strings.Contains(output, "hello-nested") {
		t.Errorf("echo in inner frame: output %q does not contain %q", output, "hello-nested")
	} else {
		t.Logf("nested container OK: %q", strings.TrimSpace(output))
	}

	// Check /dev/kvm propagation if the host has KVM. setupDev in
	// container_setup.go propagates /dev/kvm via mknod using the device
	// number captured by container-init before chrooting. If this fails,
	// nested VMs won't work — a real nesting bug. We check this in the same
	// inner frame (no extra setup needed) so it's essentially free.
	if _, err := os.Stat("/dev/kvm"); err == nil {
		output, _, err := innerSSHExec(t, id.addr, "inner",
			"test -c /dev/kvm && echo KVM_OK || echo KVM_MISSING")
		if err != nil {
			t.Fatalf("check /dev/kvm in inner frame: %v", err)
		}
		if strings.Contains(output, "KVM_MISSING") {
			t.Errorf("NESTING BUG: /dev/kvm not propagated into nested container. "+
				"setupDev in container_setup.go should mknod it via the device "+
				"number captured by container-init. output: %q", output)
		} else if strings.Contains(output, "KVM_OK") {
			t.Logf("nested KVM OK: /dev/kvm accessible inside nested container")
		} else {
			t.Errorf("unexpected output checking /dev/kvm: %q", output)
		}
	} else {
		t.Logf("no /dev/kvm on host — skipping nested KVM propagation check")
	}
}
