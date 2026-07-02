# ts go / ts undo e2e Testing Problem

## The Issue

`ts go` and `ts undo` cannot be tested in the current e2e harness because of a transport mismatch:

1. **e2e harness**: Connects to thundersnapd via **TCP SSH** (`--test-listen=127.0.0.1:port`)
2. **ts go**: Connects to vshd via **vsock** (assumes it's inside a container/VM)

When you SSH into a frame and run `ts go <frame>`, the `ts go` command tries to dial vsock CID 2 (the host) on port 5222. But in the e2e test environment:
- There is no vsock device (`/dev/vsock` doesn't exist)
- The test is running on the host, not inside a VM/container
- The daemon is listening on TCP, not vsock

## Code Path

```
e2e test
  └── ssh.Dial("tcp", "127.0.0.1:port")  // TCP connection
        └── thundersnapd (--test-listen)
              └── runContainerSession()
                    └── frame shell via vshd

Inside that shell:
  ts go <newframe>
    └── vsock.Dial(hostCID=2, vshPort=5222)  // FAILS: no /dev/vsock
```

## Why This Matters

`ts go` is designed to work from *inside* an already-running session (container or VM). It uses vsock because:
- In a VM: vsock connects directly to the host
- In a container: there's a vsock-emulating Unix socket

The e2e harness runs on the host and connects via TCP, so there's no vsock path available when commands run inside the SSH session.

## Solution: Multiplex vshd sessions over /thunder.sock

The control socket (`/thunder.sock`) already exists in every container and uses a
port-based CONNECT handshake (cloud-hypervisor vsock style). Currently it only
accepts `CONNECT 5223` for control HTTP. The fix is to also accept `CONNECT 5222`
and proxy those connections to `host-vshd.sock`.

### Current architecture

| Port | Protocol | Purpose |
|------|----------|---------|
| 5222 | vshdproto TLV | vshd sessions (currently vsock-only) |
| 5223 | HTTP | control API (snap, log, refs, etc.) |

### Changes required

1. **thundersnapd control server** (`cmd/thundersnapd/main.go`): In `handleConn()`,
   parse the CONNECT port and dispatch:
   - `CONNECT 5223`: existing HTTP control path
   - `CONNECT 5222`: proxy bidirectionally to `host-vshd.sock`

2. **ts go** (`cmd/ts/main.go`): In `runVsockSession()`, use the same transport
   detection as `thunderclient.Dial()`:
   - If `/dev/vsock` exists: use `vsock.Dial(hostCID, 5222)` (VM mode)
   - Otherwise: dial `/thunder.sock` with `CONNECT 5222\n` handshake (container mode)

3. **thunderproto** (`thunderproto/thunderproto.go`): Export VshPort (5222) alongside
   Port (5223) so both sides use the same constant.

### Why this works

- No new sockets or filenames - reuses existing `/thunder.sock`
- Follows the established pattern: port number selects protocol
- `ts go` becomes transport-agnostic like other `ts` commands
- e2e tests work because container sessions have `/thunder.sock`

### Validation

After implementation, these must all pass:
```
make test
make e2e
make not_e2e
```
