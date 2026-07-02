# Linux Namespaces Architecture in Thundersnap

This document describes how thundersnap uses Linux namespaces and communication
channels to isolate containers and VMs.

## Overview

Thundersnap creates a multi-layered isolation hierarchy:

```
┌────────────────────────────────────────────────────────────┐
│                         HOST                               │
│  ┌──────────────────────────────────────────────────────┐  │
│  │                    thundersnapd                       │  │
│  │  • SSH server (Tailscale tsnet)                      │  │
│  │  • Frame/snapshot manager (btrfs)                    │  │
│  │  • VM orchestrator                                   │  │
│  │  • Container namespace manager                       │  │
│  └──────────────────────────────────────────────────────┘  │
│                           │                                │
│           ┌───────────────┴───────────────┐                │
│           │                               │                │
│           ▼                               ▼                │
│  ┌─────────────────┐             ┌─────────────────────┐   │
│  │   Host Mode     │             │      VM Mode        │   │
│  │  (containers)   │             │ (cloud-hypervisor)  │   │
│  └─────────────────┘             └─────────────────────┘   │
│           │                               │                │
│           │                      ┌────────┴────────┐       │
│           │                      │  Guest Kernel   │       │
│           │                      │  ┌───────────┐  │       │
│           │                      │  │   vshd    │  │       │
│           │                      │  │ (init=1)  │  │       │
│           │                      │  └───────────┘  │       │
│           │                      └─────────────────┘       │
│           │                               │                │
│           ▼                               ▼                │
│  ┌─────────────────────────────────────────────────────┐   │
│  │              Container Namespace                     │   │
│  │  ┌─────────────────────────────────────────────┐    │   │
│  │  │           ts container-init                  │    │   │
│  │  │  • Anchors PID/mount/UTS namespaces         │    │   │
│  │  │  • Creates /proc, /sys, /dev                │    │   │
│  │  │  • Waits for shutdown signal                │    │   │
│  │  └─────────────────────────────────────────────┘    │   │
│  │                       │                              │   │
│  │  ┌────────────────────┼────────────────────┐        │   │
│  │  │ Session 1          │ Session 2          │        │   │
│  │  │ (nsenter→shell)    │ (nsenter→shell)    │ ...    │   │
│  │  └────────────────────┴────────────────────┘        │   │
│  └─────────────────────────────────────────────────────┘   │
└────────────────────────────────────────────────────────────┘
```

## Namespace Types Used

| Namespace | Created By | Purpose |
|-----------|------------|---------|
| **PID** | container-init | Process isolation; container-init is PID 1 inside |
| **Mount** | container-init | Filesystem isolation; private /proc, /dev, chroot |
| **UTS** | container-init | Hostname isolation |
| **Network** | **NOT isolated** | Containers share VM/host network (Tailscale access) |
| **User** | Not used | Runs as real root within container |

## Key Components

### thundersnapd

The main daemon running on the host. It:
- Accepts SSH connections via Tailscale tsnet
- Manages btrfs snapshots and frames
- Orchestrates VM lifecycle (cloud-hypervisor)
- Manages container namespace lifecycle via `containerns.Manager`
- **Never enters container/VM namespaces itself** - stays outside for lifecycle management

### vshd (Shell Daemon)

Runs in two modes:

**VM Mode** (default):
- Runs as PID 1 (init) inside the VM guest
- Listens on vsock port 5222
- Handles session requests from thundersnapd

**Host Mode**:
- Runs as a subprocess of thundersnapd
- Listens on Unix socket `/path/to/rootfs/thunder.sock`
- Handles in-container `ts` commands

### ts container-init

A long-lived process that anchors shared namespaces for a container rootfs:

```
ts container-init --chroot=/path/to/rootfs [--hostname=X] [--domainname=Y]
```

Startup sequence:
1. Created with `CLONE_NEWPID | CLONE_NEWNS | CLONE_NEWUTS`
2. Makes all mounts private (`MS_REC | MS_PRIVATE`)
3. Bind-mounts chrootPath to itself (for nested container support)
4. Chroots into the container rootfs
5. Mounts `/proc`, `/sys`, and sets up `/dev`
6. Signals "READY\n" on stdout
7. Sits idle, reaping zombies (as PID 1)
8. Exits when stdin is closed (reference count reaches zero)

### ts nsenter

A CGO-free namespace joiner that replaces util-linux `nsenter(1)`:

```
ts nsenter -t <pid> -p -m -u -- <command>
```

**Why custom?** Go's runtime is multithreaded, and `setns(CLONE_NEWNS)` fails on
multithreaded processes. The solution is a two-stage reexec:

**Stage 1** (multithreaded process):
- `setns(uts)` - allowed on multithreaded
- `setns(pid)` - allowed (takes effect for children)
- Fork and reexec stage 2, passing mount namespace fd

**Stage 2** (reexec'd child):
- `runtime.LockOSThread()` - lock to single thread
- `unshare(CLONE_FS)` - break thread's filesystem context sharing
- `setns(mnt)` - now succeeds on single thread
- `exec(command)` - collapses to single-threaded

### ts drop-caps-and-run

Performs container isolation before running the user's command:

```
ts drop-caps-and-run --chroot=/path --skip-mount-setup --keep-dev-caps -- /bin/ts session-serve
```

Actions:
1. Makes mounts private (unless `--skip-mount-setup`)
2. Chroots to the specified rootfs
3. Mounts `/proc`, `/sys`, `/dev` (unless already done)
4. Drops dangerous capabilities from the bounding set
5. Execs the specified command

**Capabilities dropped:**
- `CAP_NET_ADMIN` - iptables, routing
- `CAP_SYS_MODULE` - kernel modules
- `CAP_SYS_BOOT` - reboot
- `CAP_SYS_TIME` - clock changes
- `CAP_AUDIT_WRITE` - audit logs
- `CAP_SETFCAP` - file capabilities
- `CAP_MKNOD` - device nodes (unless `--keep-dev-caps` for nested thundersnap)

## Communication Channels

### /dev/vsock - VM ↔ Host

Virtual socket for VM-to-host communication:

```
┌─────────────┐                      ┌─────────────┐
│    Host     │                      │  VM Guest   │
│             │                      │             │
│ thundersnapd│◄─────vsock:5222─────►│    vshd     │
│             │                      │             │
└─────────────┘                      └─────────────┘
```

- **Device**: `/dev/vsock` in VM (mounted by `setupDev` in VM mode)
- **Port**: 5222 (`VshPort` constant)
- **Handshake**: Cloud-hypervisor protocol
  ```
  Client: "CONNECT 5222\n"
  Server: "OK 5222\n"
  ```
- **Then**: vshdproto TLV frames

### /thunder.sock - Container ↔ Host

Unix socket for in-container `ts` commands:

```
┌─────────────────────────────────────────────────────┐
│                   Container                          │
│                                                      │
│  ts snap ──────►  /thunder.sock  ◄────── vshd       │
│  ts frame                                (host mode) │
│                                                      │
└─────────────────────────────────────────────────────┘
```

- **Location**: `/<rootfs>/thunder.sock`
- **Protocol**: HTTP/JSON over Unix socket
- **Endpoints**: `/ping`, `/snap`, `/frame`, `/refs`, etc.
- **Client**: `ts` commands inside container

### vshdproto TLV Framing

Used for all session I/O:

```
┌──────────┬──────────────────┬─────────────────────────┐
│ type:u8  │ length:u32 (BE)  │    payload[length]      │
└──────────┴──────────────────┴─────────────────────────┘
```

**Frame types:**
| Type | Value | Direction | Purpose |
|------|-------|-----------|---------|
| `FrameStdin` | 1 | host→guest | Terminal input |
| `FrameStdout` | 2 | guest→host | Terminal output |
| `FrameStderr` | 3 | guest→host | Error output |
| `FrameWinsize` | 4 | host→guest | Terminal resize |
| `FrameExit` | 5 | guest→host | Exit code |

**PTY detection**: If first frame from host is `FrameWinsize`, allocate PTY;
otherwise use pipe mode.

## Session Lifecycle

### SSH to Container in VM

```
1. ssh user@thundersnap
   │
   ▼
2. thundersnapd accepts connection
   • WhoIs() → identify user
   • Policy → determine isolation mode (VM)
   • Boot/reuse cloud-hypervisor VM
   │
   ▼
3. thundersnapd sets up container
   • containerns.GetOrCreate(rootfs)
   • If first: start ts container-init
   • Wait for READY signal
   • Returns initPid (e.g., 5678)
   │
   ▼
4. thundersnapd connects to VM's vshd
   • Dial vsock
   • Handshake: "CONNECT 5222\n" → "OK 5222\n"
   • Send VMX request header with rootfs path
   │
   ▼
5. vshd (in VM) builds session command
   • ts nsenter -t 5678 -p -m -u -- \
       /bin/ts drop-caps-and-run --chroot=... --skip-mount-setup -- \
         /bin/ts session-serve
   │
   ▼
6. nsenter joins namespaces (two-stage)
   • Stage 1: setns(uts), setns(pid), fork
   • Stage 2: LockOSThread, unshare(CLONE_FS), setns(mnt), exec
   │
   ▼
7. drop-caps-and-run isolates
   • Chroot to rootfs
   • Drop capabilities
   • Exec session-serve
   │
   ▼
8. session-serve runs shell
   • Allocate PTY from container's devpts
   • Fork /bin/sh
   • Bridge stdin/stdout via vshdproto TLV
   │
   ▼
9. User interacts with shell
   • Input flows: SSH → thundersnapd → vsock → vshd → session-serve → PTY → shell
   • Output flows in reverse
   │
   ▼
10. User disconnects
    • vshd closes connection
    • containerns.Release(rootfs)
    • If refCount == 0: close container-init's stdin → namespace exits
```

## Nested Container Support

Thundersnap can run inside another thundersnap container (e.g., for development
or CI). This creates challenges:

**Problem**: When running inside a container, the outer bind mounts (like
`/work`) aren't automatically visible when joining a new mount namespace via
`setns(CLONE_NEWNS)`.

**Solution**: In container-init, bind-mount `chrootPath` to itself before chroot:

```go
unix.Mount(chrootPath, chrootPath, "", unix.MS_BIND|unix.MS_REC, "")
```

This ensures the path is explicitly in the mount table for the new namespace.
Sessions that later join via `setns(CLONE_NEWNS)` can then access the chrootPath
regardless of what outer bind mounts existed.

**Design principle**: Use the same code path for all environments (host,
VM, nested). If a workaround is needed for one environment, apply it
unconditionally so behavior is identical everywhere. See `fix-namespaces.md`
for details.

## Reference Counting

Both container namespaces and control servers are reference-counted:

```go
// containerns.Manager
map[rootFS] → {
    initPid:   PID of container-init
    initStdin: Pipe for shutdown signal
    refCount:  Active sessions
}
```

- `GetOrCreate()`: Returns existing or creates new, increments refcount
- `Release()`: Decrements refcount, shuts down when zero
- **Shutdown**: Closing container-init's stdin causes it to exit, which
  destroys the namespace

## Security Model

Thundersnap's isolation relies on:

1. **Namespaces**: PID, mount, and UTS isolation
2. **Chroot**: Filesystem boundary
3. **Capability dropping**: Removes dangerous capabilities
4. **No network isolation**: Intentional - allows Tailscale access

**Not used** (by design):
- seccomp (syscall filtering)
- LSM (AppArmor, SELinux)
- User namespaces

This provides a balance between isolation and usability. The container can
access the user's Tailscale network, which is the primary security boundary.

## Key Files

| File | Purpose |
|------|---------|
| `cmd/thundersnapd/` | Main daemon |
| `cmd/vshd/main.go` | Shell daemon |
| `cmd/ts/container_setup.go` | container-init, drop-caps-and-run |
| `cmd/ts/nsenter.go` | Namespace joiner |
| `containerns/containerns.go` | Namespace lifecycle manager |
| `vshdproto/vshdproto.go` | TLV protocol |
| `vshdsession/vshdsession.go` | PTY/pipe session handling |
