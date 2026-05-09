# Tmux Integration Design

This document describes a design for integrating tmux-style session multiplexing
into thundersnap, with support for native terminal apps (like Attach on iOS) and
cross-mesh session aggregation.

## Motivation

Currently, thundersnap container sessions are ephemeral—each SSH connection spawns
a new shell, and disconnecting kills it (though the filesystem persists). This is
painful for:

- Long-running processes that shouldn't die on disconnect
- Network hiccups or laptop sleep
- Moving between devices
- Managing multiple workspaces in a single view

Terminal apps like [Attach](https://apps.apple.com/app/id1505372eli), iTerm2, and
Blink Shell support tmux **control mode** (`tmux -CC`), which outputs structured
events instead of terminal escape codes. These apps render panes natively and
provide a superior mobile/tablet experience.

## Goals

1. **Session persistence**: Shells survive disconnects; reconnecting resumes where
   you left off.

2. **Native app support**: Clients using tmux control mode get native pane
   management without relying on tmux being installed inside containers.

3. **Mesh aggregation**: In `--mesh` mode, present sessions from all thundersnap
   nodes as a unified list. Attach to any session from any entry point.

4. **Zero container requirements**: The multiplexer runs on the host; containers
   just provide shells.

## Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│                          thundersnapd                                │
│  ┌─────────────────────────────────────────────────────────────────┐│
│  │              Session Multiplexer (tmux or native)               ││
│  │  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────────────┐ ││
│  │  │ window 0 │  │ window 1 │  │ window 2 │  │ window 3         │ ││
│  │  │ → dev    │  │ → test   │  │ → vm/gpu │  │ → ts2:prod       │ ││
│  │  │ container│  │ container│  │ (VM)     │  │ (proxied from    │ ││
│  │  │          │  │          │  │          │  │  mesh peer)      │ ││
│  │  └──────────┘  └──────────┘  └──────────┘  └──────────────────┘ ││
│  └─────────────────────────────────────────────────────────────────┘│
│         ↑ control mode protocol                                      │
│         │                                                            │
│    SSH/tsnet connection                                              │
└─────────────────────────────────────────────────────────────────────┘
```

The tmux server (or native equivalent) runs **outside** containers but spawns
shells **inside** them via `ts drop-caps-and-run`. From the client's perspective,
each workspace appears as a tmux window.

### Mesh Session Aggregation

```
┌──────────────┐     ┌──────────────┐     ┌──────────────┐
│ thundersnap-1│     │ thundersnap-2│     │ thundersnap-3│
│  sessions:   │     │  sessions:   │     │  sessions:   │
│   - dev      │     │   - prod     │     │   - staging  │
│   - test     │     │   - monitor  │     │              │
└──────────────┘     └──────────────┘     └──────────────┘
        \                   |                    /
         └──────────────────┼───────────────────┘
                            │
                    ┌───────┴───────┐
                    │  Client app   │
                    │  (e.g. Attach)│
                    │               │
                    │ Sessions:     │
                    │  ts1:dev      │
                    │  ts1:test     │
                    │  ts2:prod     │
                    │  ts2:monitor  │
                    │  ts3:staging  │
                    └───────────────┘
```

Connect to any thundersnap node; it queries all mesh peers and presents a unified
session list. Selecting a remote session proxies the connection transparently via
tsnet.

## Tmux Control Mode Protocol

Control mode (`tmux -CC`) uses a line-based protocol:

```
%begin 1234567890
%end 1234567890
%output %0 SGVsbG8gV29ybGQK
%window-add @1
%window-renamed @1 dev
%session-changed $1 main
%layout-change @1 bb62,80x24,0,0,0
%exit
```

Events are prefixed with `%`. Output data (`%output`) is base64-encoded.
Commands are sent as plain text (e.g., `split-window -h\n`).

Reference: https://github.com/tmux/tmux/wiki/Control-Mode

## Implementation Options

### Option A: Host Actual Tmux

Run a real tmux server per user on the host. Windows spawn container shells.

```go
type userSession struct {
    tmuxSock string  // /run/thundersnap/tmux/<user-hash>.sock
    proc     *exec.Cmd
}

func attachWorkspace(user, workspace string, controlMode bool) {
    sess := getOrCreateTmuxServer(user)
    
    if !sess.hasWindow(workspace) {
        // Create window running shell inside container
        exec.Command("tmux", "-S", sess.sock,
            "new-window", "-n", workspace,
            "/usr/sbin/ts", "drop-caps-and-run",
            "--chroot="+rootFS, "--", "su", "-", runAsUser,
        ).Run()
    }
    
    if controlMode {
        exec.Command("tmux", "-S", sess.sock, "-CC", "attach").Run()
    } else {
        exec.Command("tmux", "-S", sess.sock, "attach").Run()
    }
}
```

**Pros:**
- Battle-tested, feature-complete
- Attach/iTerm2/Blink work immediately
- Minimal implementation effort

**Cons:**
- Requires tmux installed on host
- Protocol not extensible for thundersnap-specific features

### Option B: Native Control Mode Implementation

Implement the control mode protocol directly in thundersnapd. This is more work
(~1-2k lines) but enables deeper integration.

```go
type ControlEvent interface{ encode() string }

type OutputEvent struct {
    PaneID string
    Data   []byte
}

func (e OutputEvent) encode() string {
    return fmt.Sprintf("%%output %%%s %s", e.PaneID, base64.StdEncoding.EncodeToString(e.Data))
}

type WindowAddEvent struct {
    WindowID string
    Name     string
}

// ... etc
```

**Pros:**
- No external dependencies
- Can add thundersnap-specific extensions (see below)
- Tighter integration with workspace/snapshot model

**Cons:**
- More implementation effort
- Risk of protocol incompatibilities with clients

### Thundersnap Protocol Extensions

If implementing natively, we can extend the protocol:

```
# Thundersnap-specific events (prefixed with %ts-)

%ts-workspace %0 dev
# Associates pane with workspace name

%ts-snapshot %0 abc123def456
# Emitted when `ts snap` completes in this pane

%ts-node %0 ts2.corp.ts.net
# Indicates pane is hosted on a remote mesh node

%ts-health %0 healthy
# Container/VM health status
```

Thundersnap-aware clients could:
- Display workspace names in tab titles
- Show snapshot IDs inline after `ts snap`
- Indicate which mesh node hosts each pane
- Show health indicators per pane

Standard tmux clients ignore unknown `%` events, so this is backward-compatible.

## SSH Interface

```bash
# Standard attach (creates session/window if needed)
ssh workspace@thundersnap

# Control mode for apps like Attach
ssh cc/workspace@thundersnap

# Control mode session picker (list all windows)
ssh cc@thundersnap

# List all sessions as JSON (for scripting)
ssh sessions@thundersnap

# Attach to specific session by ID (for mesh roaming)
ssh attach/abc123@thundersnap
```

Detection of control mode could also use:
- SSH environment variables (`LC_TERMINAL=Attach`)
- SSH channel exec request inspection
- Explicit username prefix (as above)

## Mesh Session API

### GET /ts/sessions.json

Returns sessions visible to the requesting user:

```json
[
  {
    "id": "abc123",
    "workspace": "dev",
    "user": "alice@example.com",
    "node": "ts1.corp.ts.net",
    "created": "2024-01-15T10:30:00Z",
    "last_active": "2024-01-15T14:22:00Z",
    "windows": ["dev", "test"]
  },
  {
    "id": "def456",
    "workspace": "prod",
    "user": "alice@example.com",
    "node": "ts2.corp.ts.net",
    "created": "2024-01-14T09:00:00Z",
    "last_active": "2024-01-15T14:20:00Z",
    "windows": ["prod", "logs"]
  }
]
```

### Session Aggregation

In mesh mode, querying `/ts/sessions.json` returns sessions from all peers:

```go
func (m *meshState) getAllSessions(tailscaleUser string) []Session {
    var all []Session
    
    // Local sessions
    all = append(all, m.localSessions(tailscaleUser)...)
    
    // Query mesh peers in parallel
    var wg sync.WaitGroup
    var mu sync.Mutex
    
    for _, peer := range m.peers {
        wg.Add(1)
        go func(p meshPeer) {
            defer wg.Done()
            sessions, err := fetchJSON[[]Session](p.URL + "/ts/sessions.json")
            if err != nil {
                return
            }
            mu.Lock()
            for _, s := range sessions {
                s.Node = p.Hostname
                all = append(all, s)
            }
            mu.Unlock()
        }(peer)
    }
    
    wg.Wait()
    return all
}
```

### Remote Session Proxy

When attaching to a session on a remote node:

```go
func attachRemoteSession(clientConn net.Conn, peer meshPeer, sessionID string) error {
    // Connect to remote peer via tsnet
    remoteConn, err := tsnetDialSession(peer, sessionID)
    if err != nil {
        return err
    }
    defer remoteConn.Close()
    
    // Bidirectional proxy
    go io.Copy(remoteConn, clientConn)
    io.Copy(clientConn, remoteConn)
    return nil
}
```

The client doesn't know or care which physical node hosts the session.

## Implementation Phases

### Phase 1: Host-Side Tmux (~200 lines)

- Manage tmux server per tailscale user
- Create windows for workspaces on demand
- Spawn container shells via `ts drop-caps-and-run`
- Pass through control mode when requested
- Basic attach/detach lifecycle

### Phase 2: Mesh Aggregation (~300 lines)

- Add `/ts/sessions.json` endpoint
- Query mesh peers for their sessions
- Proxy connections to remote sessions via tsnet
- Unified session list regardless of entry point

### Phase 3: Native Control Protocol (~1500 lines, optional)

- Replace tmux with native Go implementation
- Implement core control mode events
- Add thundersnap-specific extensions
- Full pane/window/session management
- Remove tmux host dependency

## Open Questions

1. **Session persistence across thundersnapd restart?**
   - Tmux servers die when parent dies
   - Could daemonize tmux separately and reconnect
   - Or accept that restarts kill sessions (filesystems survive)

2. **Window-per-workspace vs session-per-workspace?**
   - Window: all workspaces in one session, switch with Ctrl-B n
   - Session: each workspace isolated, must detach/attach to switch
   - Recommendation: windows, with session-per-user

3. **Control mode detection mechanism?**
   - Username prefix (`cc/workspace`)
   - Environment variable (`LC_TERMINAL`)
   - SSH subsystem (`ssh -s tmux-cc`)
   - Recommendation: username prefix for explicitness

4. **Authorization for mesh session access?**
   - Currently: same tailscale user can access their sessions on any node
   - Future: could allow delegation (user A grants user B access to session X)

## References

- [Tmux Control Mode Wiki](https://github.com/tmux/tmux/wiki/Control-Mode)
- [iTerm2 tmux Integration](https://iterm2.com/documentation-tmux-integration.html)
- [Attach App](https://apps.apple.com/app/id1505372141)
- [Blink Shell](https://blink.sh/)
