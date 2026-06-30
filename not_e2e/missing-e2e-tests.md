# Missing E2E Tests

This document outlines gaps in end-to-end test coverage discovered while debugging
an issue where `ssh root@empty@hotdog` (interactive shell) failed immediately while
`ssh root@empty@hotdog echo hello` (command execution) worked.

## Key Principle: E2E Tests Must Run Real Processes

**An e2e test is not a unit test.** Calling Go functions directly (like `ensureRootFS()`,
`copyTsBinary()`, or `runShell()`) tests the function in isolation but misses:

- Process boundaries (fork, exec, namespace transitions)
- PTY allocation and terminal handling
- The actual nsenter/chroot/namespace dance
- Race conditions between processes
- File descriptor inheritance across exec

The bug that prompted this document was caused by using the wrong PID when opening
a PTY via `/proc/<pid>/root/dev/pts/ptmx`. Unit tests calling Go functions would
never catch this because the bug only manifests when:
1. nsenter forks (creating parent and child with different PIDs)
2. thundersnapd reads `/proc/<pid>/root` for the wrong PID
3. The child process tries to open a PTY path that doesn't exist in its namespace

**True e2e tests must:**
- Start `thundersnapd` as a real process
- Run `ts` commands via exec (not function calls)
- Use actual SSH connections or simulate them realistically
- Exercise the full nsenter → ts → shell pipeline

## Test Matrix: Four Major Dimensions

### 1. Container Type: Empty vs Real Unix

| Container Type | Current Coverage | Gap |
|----------------|------------------|-----|
| Real Unix (Debian/Ubuntu rootfs) | Good (most tests) | - |
| Empty/blank (`nil:nil:nil`) | Partial | Interactive shell, non-root users |

Empty containers use `ts` as `/bin/sh` via symlink. This code path differs significantly:
- No real `su` binary (falls back to `/bin/sh -l`)
- Shell is the `mvdan.cc/sh` interpreter, not bash/dash
- Minimal `/etc/passwd` with only root, user, nobody

**Missing tests:**
- Interactive shell in empty container with PTY
- Script execution in empty container
- Environment variable handling in empty container shell

### 2. SSH Mode: Command vs Interactive

| Mode | Current Coverage | Gap |
|------|------------------|-----|
| Command (`ssh host cmd`) | Good | - |
| Interactive (`ssh host`) | Poor | Empty containers, PTY allocation |
| Interactive with `-t` forced | None | All container types |

The bug was specifically in interactive mode because:
- Command mode: `su root -c "cmd"` → no PTY needed
- Interactive mode: `su - root` → needs PTY, login shell

**Missing tests:**
```bash
# These should all be tested:
ssh user@frame@host                     # Interactive, real container
ssh user@frame@host echo hello          # Command, real container
ssh root@empty@host                     # Interactive, empty container  <- THIS WAS BROKEN
ssh root@empty@host echo hello          # Command, empty container
ssh -tt user@frame@host                 # Forced PTY, real container
ssh -tt root@empty@host                 # Forced PTY, empty container
```

### 3. User: Root vs Non-Root

| User | Current Coverage | Gap |
|------|------------------|-----|
| root | Good | - |
| user (UID 1000) | Partial | Empty containers, home directory |
| Custom user | Poor | User creation, permissions |

Non-root users exercise different code paths:
- `su - user` vs `su - root`
- Home directory lookup and creation
- File ownership after strip-uids transformation

**Missing tests:**
- Non-root interactive shell in empty container
- User auto-detection when multiple users exist
- Permission errors when user doesn't exist

### 4. Isolation: Container vs VM

| Mode | Current Coverage | Gap |
|------|------------------|-----|
| Container (`isolation=container`) | Good | - |
| VM (`isolation=vm`) | Partial | Interactive shell, PTY in VM |

VM mode has completely different PTY handling:
- Uses virtio-vsock instead of devpts
- vshd handles shell sessions inside the VM
- Different namespace setup

**Missing tests:**
- Interactive shell in VM mode
- PTY window resize in VM mode
- VM mode with empty rootfs

## Proposed Test Structure

### Test: Interactive Shell in Empty Container

```go
// TestEmptyContainerInteractiveShell verifies that interactive SSH sessions
// work in empty (nil:nil:nil) containers where ts acts as /bin/sh.
//
// This is a TRUE e2e test: it starts thundersnapd, creates a real SSH
// connection with PTY, and verifies bidirectional I/O.
func TestEmptyContainerInteractiveShell(t *testing.T) {
    // 1. Start thundersnapd with test config
    daemon := startTestThundersnapd(t, ...)
    defer daemon.Stop()

    // 2. Create empty frame (nil:nil:nil)
    createFrame(t, daemon, "testuser", "emptyframe", "nil:nil:nil")

    // 3. Open SSH connection with PTY (simulating `ssh -tt`)
    session := openSSHSession(t, daemon, "root@emptyframe", withPTY(80, 24))
    defer session.Close()

    // 4. Verify we get a prompt
    expectOutput(t, session, "$ ", 5*time.Second)

    // 5. Send a command and verify output
    session.Write([]byte("echo hello\n"))
    expectOutput(t, session, "hello", 5*time.Second)

    // 6. Send exit and verify clean shutdown
    session.Write([]byte("exit\n"))
    expectExit(t, session, 0, 5*time.Second)
}
```

### Test: PTY Allocation Uses Correct PID

```go
// TestPTYUsesContainerInitPID verifies that PTY allocation uses the
// container-init PID, not the nsenter parent PID.
//
// This test catches the specific bug where openContainerPTY() was called
// with cmd.Process.Pid (nsenter parent) instead of initPid (container-init).
func TestPTYUsesContainerInitPID(t *testing.T) {
    // 1. Start daemon and create container
    // 2. Open interactive session
    // 3. Verify /proc/<pid>/root points to container rootfs, not host /
    // 4. Verify PTY slave path is /dev/pts/0 (fresh devpts), not /dev/pts/NN (host)
}
```

### Test: Non-Root User in Empty Container

```go
// TestEmptyContainerNonRootUser verifies that non-root users can get
// interactive shells in empty containers.
func TestEmptyContainerNonRootUser(t *testing.T) {
    // 1. Create empty frame
    // 2. SSH as "user" (UID 1000) not root
    // 3. Verify shell works and $HOME is set correctly
    // 4. Verify user can't access /root
}
```

### Test: Command vs Interactive Parity

```go
// TestCommandVsInteractiveParity verifies that commands produce the same
// results whether run via `ssh host cmd` or typed into `ssh host` interactively.
func TestCommandVsInteractiveParity(t *testing.T) {
    testCases := []struct {
        name      string
        frameSpec string  // e.g., "nil:nil:nil" or "debian::"
        user      string
        command   string
    }{
        {"empty_root", "nil:nil:nil", "root", "echo $HOME"},
        {"empty_user", "nil:nil:nil", "user", "echo $HOME"},
        {"debian_root", "debian::", "root", "whoami"},
        {"debian_user", "debian::", "user", "id -u"},
    }

    for _, tc := range testCases {
        // Run via command mode
        cmdOutput := runSSHCommand(t, tc.user, tc.frameSpec, tc.command)

        // Run via interactive mode
        session := openInteractiveSSH(t, tc.user, tc.frameSpec)
        session.Write([]byte(tc.command + "\n"))
        interactiveOutput := readUntilPrompt(t, session)
        session.Write([]byte("exit\n"))

        // Compare (ignoring prompt artifacts)
        if !outputsMatch(cmdOutput, interactiveOutput) {
            t.Errorf("outputs differ: cmd=%q interactive=%q", cmdOutput, interactiveOutput)
        }
    }
}
```

## Implementation Notes

### How to Simulate SSH with PTY in Tests

The existing tests use `exec.Command` to run `ts drop-caps-and-run`. To test
interactive sessions properly, we need to:

1. **Allocate a real PTY** using `github.com/creack/pty` or similar
2. **Set up the same handshake** that thundersnapd does (READY → slave path)
3. **Proxy I/O** between the test and the PTY master

Or, better yet, run actual SSH connections against a test thundersnapd instance
listening on a Unix socket or localhost port.

### Test Fixtures Needed

- `createEmptyFrame(t, path)` - creates nil:nil:nil frame with ts as /bin/sh
- `createDebianFrame(t, path)` - creates frame from cached Debian image
- `startTestDaemon(t, config)` - starts thundersnapd with test config
- `openSSHWithPTY(t, daemon, user, frame)` - opens SSH session with PTY

### Avoiding Flaky Tests

Interactive tests are prone to timing issues. Use:
- `expectOutput(t, session, pattern, timeout)` with reasonable timeouts
- Explicit synchronization (wait for prompt before sending next command)
- Deterministic test containers (no network, no random delays)

## Priority Order

1. **Interactive shell in empty container** (root) - the exact bug we hit
2. **Interactive shell in empty container** (non-root user)
3. **PTY allocation correctness** (verify initPid is used)
4. **Command vs interactive parity** across container types
5. **VM mode interactive shell**
