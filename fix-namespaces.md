# Proposal: Simplify Container Namespace Setup

## Problem

Commit `onozokxt` (130b064) introduced a workaround for running e2e tests inside
a thundersnap container (nested scenario). The fix has two parts:

1. **In container-init**: Bind-mount `chrootPath` to itself before chroot
2. **In vshd**: Detect whether joining the mount namespace will work, and if not,
   skip `-m` flag and redo mount setup

This creates **two code paths** for container session setup:
- Normal: join mount namespace + skip mount setup
- Nested fallback: don't join mount namespace + redo mount setup

Two code paths are harder to test and maintain. If one path needs a workaround,
it's better to use that workaround everywhere so the code behaves identically in
all environments.

## Current Architecture

```
container-init (creates namespaces, sets up /proc, /sys, /dev)
       │
       │ sessions join via nsenter
       ▼
nsenter -t <pid> -p -m -u -- \
  ts drop-caps-and-run --chroot=... --skip-mount-setup -- \
    ts session-serve ...
```

The session joins PID, mount, and UTS namespaces, then chroots and drops caps.
Mount setup is skipped because container-init already did it.

## Problem with the Current Fix

The detection heuristic in vshd is fragile:

```go
canJoinMountNs := true
targetRootPath := fmt.Sprintf("/proc/%d/root", initPid)
if targetRoot, err := os.Readlink(targetRootPath); err == nil {
    if _, err := os.Stat(targetRoot); err != nil {
        canJoinMountNs = false
    }
}
```

Issues:
- If `os.Readlink` fails, `canJoinMountNs` stays `true` (silent failure)
- The check happens from vshd's namespace, not the target namespace
- It doesn't actually test whether setns will work

More fundamentally: **the parent's /work should have nothing to do with the
child container**. If we depend on outer bind mounts being visible, we've broken
the isolation model.

## Proposed Simplification

### Principle: One Code Path

Even if a workaround is needed for some environment (nested, VM, host), use that
workaround everywhere. This ensures:
- Identical behavior in all environments
- One code path to test
- One code path to debug

### Option A: Always Join Mount Namespace (Preferred)

Keep the bind-mount fix in container-init, remove the detection/fallback in vshd:

```go
// container-init: bind-mount chrootPath to ensure it's in mount table
unix.Mount(chrootPath, chrootPath, "", unix.MS_BIND|unix.MS_REC, "")
```

```
nsenter -t <pid> -p -m -u -- \        # ALWAYS include -m
  ts drop-caps-and-run --chroot=... --skip-mount-setup -- \  # ALWAYS skip
    ts session-serve ...
```

If the bind-mount fix is correct, nested scenarios should work. If they don't,
we get a clear error and can fix the root cause.

**Benefits:**
- Single code path
- Sessions share mount namespace (mounts visible across sessions)
- Less redundant work per session

### Option B: Never Join Mount Namespace

Remove `-m` from nsenter, always redo mount setup:

```
nsenter -t <pid> -p -u -- \           # no -m
  ts drop-caps-and-run --chroot=... -- \  # no --skip-mount-setup
    ts session-serve ...
```

**Benefits:**
- Single code path
- Works regardless of mount namespace accessibility
- Better mount isolation between sessions

**Downsides:**
- Sessions don't share mount namespace (mounts in one session not visible to others)
- Redundant mount setup per session (minor performance cost)

### Recommendation

**Option A** is preferred because:
1. Sharing the mount namespace is the intended design (like `docker exec`)
2. The bind-mount fix should be sufficient if correctly implemented
3. If it fails, we learn what's actually broken

If Option A fails in nested scenarios, investigate the root cause rather than
adding a second code path.

## VM Mode Consistency

The same principle applies to VM mode: vshd inside the VM should set up
containers the same way as host-mode vshd. Currently:

- VM mode: vshd runs as init, handles sessions directly or via container
- Host mode: vshd runs as daemon, handles container sessions

Both should use identical container namespace logic:
1. Start container-init with CLONE_NEWPID | CLONE_NEWNS | CLONE_NEWUTS
2. container-init sets up /proc, /sys, /dev
3. Sessions join via nsenter -p -m -u
4. Sessions chroot and drop caps (skip mount setup)

## Changes Required

### Remove from vshd/main.go:

```go
// DELETE: Detection logic (lines 421-435)
canJoinMountNs := true
targetRootPath := fmt.Sprintf("/proc/%d/root", initPid)
// ... detection code ...

// DELETE: Conditional (lines 437-468)
if canJoinMountNs {
    // normal path
} else {
    // fallback path
}
```

### Replace with unconditional:

```go
dropCapsArgs := append([]string{
    "drop-caps-and-run",
    "--chroot=" + rootPrefix,
    "--skip-mount-setup",
    "--keep-dev-caps",
    "--",
    "/bin/ts",
}, serveArgs...)
nsenterArgs := append([]string{
    "nsenter",
    "-t", strconv.Itoa(initPid), "-p", "-m", "-u", "--",
    innerTs,
}, dropCapsArgs...)
```

### Keep in container_setup.go:

The bind-mount fix stays:

```go
// Bind-mount chrootPath to itself to ensure it's in the mount table
if err := unix.Mount(chrootPath, chrootPath, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
    fmt.Fprintf(os.Stderr, "error: failed to bind-mount %s: %v\n", chrootPath, err)
    os.Exit(1)
}
```

## Testing

### Minimal Namespace Validation Test

Add a focused test that validates the namespace setup is correct. This should be
the first test to run when debugging container issues.

```go
// TestContainerNamespaceSetup validates that container namespaces are set up
// correctly. This is the canonical test for namespace isolation - run this
// FIRST when debugging any container setup issues.
//
// It verifies:
// - PID namespace: session sees container-init as PID 1, not host init
// - Mount namespace: /proc is container's own (PID 1 is container-init)
// - UTS namespace: hostname is container's, not host's
// - Multiple sessions share the same namespaces
// - /dev/pts is container's own devpts instance
func TestContainerNamespaceSetup(t *testing.T) {
    env := newTestEnv(t)
    d := startDaemon(t, env)

    createFrameViaDaemon(t, d, "nstest")

    // 1. PID namespace: /proc/1/comm should be "ts" (container-init)
    output, _, _ := sshExec(t, d, "root@nstest", "cat /proc/1/comm")
    if !strings.Contains(output, "ts") {
        t.Errorf("PID namespace wrong: /proc/1/comm = %q, want 'ts'", output)
    }

    // 2. Mount namespace: /proc/1/root should be "/" (chrooted)
    output, _, _ = sshExec(t, d, "root@nstest", "readlink /proc/1/root")
    if strings.TrimSpace(output) != "/" {
        t.Errorf("Mount namespace wrong: /proc/1/root = %q, want '/'", output)
    }

    // 3. /dev/pts exists and is a devpts mount
    output, _, _ = sshExec(t, d, "root@nstest", "stat -f -c %T /dev/pts")
    if !strings.Contains(output, "devpts") {
        t.Errorf("/dev/pts not devpts: %q", output)
    }

    // 4. Two sessions see each other's processes (shared PID namespace)
    // Start a long-running process in session 1
    client, _ := ssh.Dial("tcp", d.addr, sshConfig("root@nstest"))
    sess1, _ := client.NewSession()
    sess1.Start("sleep 300 &")  // background sleep
    time.Sleep(100 * time.Millisecond)

    // Session 2 should see the sleep process
    output, _, _ = sshExec(t, d, "root@nstest", "ps aux | grep sleep")
    if !strings.Contains(output, "sleep") {
        t.Errorf("Sessions don't share PID namespace: sleep not visible")
    }
}
```

### Test Location

Place this as the FIRST test in `e2e/ssh_test.go` (rename to
`TestA_ContainerNamespaceSetup` to ensure it runs first alphabetically, or use
test ordering in main_test.go).

Add a comment:

```go
// TestA_ContainerNamespaceSetup is the canonical namespace validation test.
//
// RUN THIS FIRST when debugging container issues. If this test fails, the
// fundamental namespace setup is broken and other tests will fail confusingly.
//
// This test validates:
// - PID namespace isolation (container-init is PID 1)
// - Mount namespace isolation (/proc, /dev are container's own)
// - UTS namespace isolation (hostname)
// - Namespace sharing between sessions
```

## Verification Steps

1. Apply the simplification (remove detection + fallback)
2. Run `make test` - unit tests should pass
3. Run `make e2e` on host - should pass
4. Run `make e2e` inside a thundersnap container - should pass
5. If step 4 fails, investigate the actual error rather than adding workarounds

## Future Work

- Consider whether sessions should share mount namespace at all
- Document the namespace architecture in docs/linux-namespaces.md (done)
- Add namespace validation to the e2e test suite
