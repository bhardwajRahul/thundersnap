# Shared PID Namespaces for Container Sessions

## Problem

Currently, each SSH session to a container gets its own PID namespace via `CLONE_NEWPID`. This means:
- Session 1 starts a process, it gets PID 1 in its namespace
- Session 2 connects to the same container, it also gets PID 1 in a *different* namespace
- Session 2 cannot see session 1's processes via `ps` or `/proc`

Expected behavior: Multiple sessions to the same container should share the same PID namespace, so processes are visible across sessions (like `docker exec`).

## Key Constraints

From `man setns`:
> Reassociating the calling thread with a PID namespace changes only the PID namespace that subsequently created child processes of the caller will be placed in; it does not change the PID namespace of the caller itself.

This means:
- `setns(CLONE_NEWPID)` affects **children** only, not the calling process
- After `setns()`, we must `fork()` for the child to be in the target namespace
- `exec()` alone doesn't work because it replaces the current process, not creates a child

From `man pid_namespaces`:
> If the "init" process of a PID namespace terminates, the kernel terminates all of the processes in the namespace via a SIGKILL signal... it is not possible to create a new process in a PID namespace whose "init" process has terminated.

This means:
- We need at least one process alive in the namespace to join it
- The first session's shell process serves as PID 1 (init) for the namespace
- If that process exits and no others remain, the namespace is destroyed

## Design

### Approach

Use the first session's process as the namespace anchor. Track active sessions per rootFS and have subsequent sessions join the existing namespace.

### Data Structure

Add `containerPidNsManager` in thundersnapd:

```go
type containerPidNsManager struct {
    mu      sync.Mutex
    entries map[string]*pidNsEntry // key: rootFS path
}

type pidNsEntry struct {
    hostPid  int  // host PID of a process in this namespace
    refCount int
}

var containerPidNs = &containerPidNsManager{
    entries: make(map[string]*pidNsEntry),
}
```

### Flow

#### First session to a container:
1. Check `containerPidNs` - no entry exists
2. Start `ts drop-caps-and-run` with `CLONE_NEWPID | CLONE_NEWNS | CLONE_NEWUTS` (current behavior)
3. Register `cmd.Process.Pid` in `containerPidNs` with refCount=1

#### Subsequent sessions to the same container:
1. Check `containerPidNs` - entry exists with hostPid
2. Verify process is alive: `kill(hostPid, 0)` or check `/proc/<hostPid>/ns/pid` exists
3. If alive:
   - Start `ts drop-caps-and-run` with `CLONE_NEWNS | CLONE_NEWUTS` (NO `CLONE_NEWPID`)
   - Pass `--join-pid-ns=/proc/<hostPid>/ns/pid`
   - Increment refCount
4. If dead:
   - Remove stale entry
   - Fall back to first-session behavior

#### Session ends:
1. Decrement refCount
2. If refCount == 0, remove entry (namespace will die when last process exits naturally)

### Changes to `ts drop-caps-and-run`

Add `--join-pid-ns=<path>` flag:

```go
func cmdDropCapsAndRun(args []string) {
    var joinPidNsPath string
    // ... parse --join-pid-ns=<path> ...

    if joinPidNsPath != "" {
        // Open the namespace fd
        fd, err := unix.Open(joinPidNsPath, unix.O_RDONLY, 0)
        if err != nil {
            fmt.Fprintf(os.Stderr, "error: failed to open pid namespace %s: %v\n", joinPidNsPath, err)
            os.Exit(1)
        }

        // Join the PID namespace (affects children only)
        if err := unix.Setns(fd, unix.CLONE_NEWPID); err != nil {
            unix.Close(fd)
            fmt.Fprintf(os.Stderr, "error: failed to join pid namespace: %v\n", err)
            os.Exit(1)
        }
        unix.Close(fd)

        // Fork - child will be in the target PID namespace
        pid, _, errno := unix.Syscall(unix.SYS_FORK, 0, 0, 0)
        if errno != 0 {
            fmt.Fprintf(os.Stderr, "error: fork failed: %v\n", errno)
            os.Exit(1)
        }

        if pid > 0 {
            // Parent: wait for child and exit with its status
            var status unix.WaitStatus
            unix.Wait4(int(pid), &status, 0, nil)
            os.Exit(status.ExitStatus())
        }
        // Child: continue with normal setup
    }

    // ... rest of drop-caps-and-run (mounts, chroot, etc.) ...
}
```

### Changes to `runContainerSession` in thundersnapd

```go
func runContainerSession(...) error {
    // ... existing setup code ...

    // Check for existing PID namespace
    existingPid := containerPidNs.getExistingPid(rootFS)

    var cloneFlags uintptr = syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS

    if existingPid > 0 {
        // Join existing namespace
        tsArgs = append(tsArgs, fmt.Sprintf("--join-pid-ns=/proc/%d/ns/pid", existingPid))
    } else {
        // Create new namespace
        cloneFlags |= syscall.CLONE_NEWPID
    }

    cmd.SysProcAttr = &syscall.SysProcAttr{
        Cloneflags: cloneFlags,
    }

    // ... start command ...

    if existingPid == 0 {
        // Register new namespace
        containerPidNs.register(rootFS, cmd.Process.Pid)
    } else {
        containerPidNs.incRef(rootFS)
    }

    defer containerPidNs.release(rootFS)

    // ... wait for command ...
}
```

## Edge Cases

### Race condition: process exits between check and setns
- `setns()` will fail with ESRCH or similar
- `ts drop-caps-and-run` should detect this and exit with a specific error code
- `thundersnapd` can retry with a new namespace

### Mount namespace sharing
Each session should have its own mount namespace (`CLONE_NEWNS`) because:
- Each PTY session needs its own `/dev/pts` mount
- File operations in one session shouldn't affect another's mounts

Only the PID namespace is shared.

### UTS namespace sharing
Could be shared (hostname is per-container), but keeping separate is simpler and matches current behavior. Can revisit if needed.

## Testing

The existing test `TestContainerSharedPIDNamespace` in `e2e/container_test.go` verifies:
1. Session 1 starts a sleep process
2. Session 2 can see that process via `/proc`

Currently this test fails (documenting the bug). After implementation, it should pass.
