# Network, Identity, and Key Management Design

This document describes the design for multi-user isolation, Tailscale
identity management, service orchestration, and key lifecycle in
thundersnap.

## Principles

1. **Users are isolated from each other at the VM boundary.** Each user
   gets their own cloud-hypervisor VM. Workspaces within a user's VM are
   btrfs chroot containers sharing the VM's network namespace. Workspaces
   for the same user do not need mutual isolation.

2. **Tailscale identity belongs to the user, not the snapshot.** A user's
   Tailscale node keys live outside the snapshotable filesystem. Cloning a
   workspace never clones Tailscale state. Service-mode containers have
   their own tsnet state managed separately via setec.

3. **Policy is expressed as Tailscale grants.** Access control, isolation
   levels, and resource limits are configured using the same grant/CapMap
   syntax as Tailscale ACL policy files. A local hujson config file
   provides defaults for tailnets that haven't (yet) configured grants.

4. **Services run at most once.** A service-mode container that exports a
   tsnet identity has a singleton constraint enforced by a coordination
   service. Any thundersnap node in the mesh can run the service, but
   exactly one does at a time.

---

## 1. Identity Model

### Connecting principals

When a user SSHes into thundersnap, `getTailscaleUser()` calls
`LocalClient.WhoIs()` and classifies the connecting peer as one of:

| Peer type | Identified by | Filesystem namespace |
|-----------|---------------|---------------------|
| Human user | `UserProfile.LoginName` (email) | `fs/<email>/` |
| Tagged node | `Node.Tags` | `fs/<primary-tag>/` or `fs/<Node.StableID>/` |

For tagged nodes, the namespace key is the first tag alphabetically (to
avoid order-dependence on the full tag set), or `Node.StableID` if a stable
per-device identity is preferred. This replaces the current behavior of
formatting the full tag list as a string path component.

### Grants and capabilities

Thundersnap reads structured policy from the `tailscale.com/cap/thundersnap`
capability in the WhoIs response's CapMap. The capability structure:

```go
type ThundersnapCap struct {
    // Role determines the session type.
    // "developer" (default), "admin", "ephemeral", "service"
    Role string `json:"role"`

    // Isolation determines the execution environment.
    // "vm" (default): user gets a dedicated VM, containers inside it
    // "container": direct chroot container on the host (no VM)
    // "none": no sub-isolation (single-user thundersnap instance)
    Isolation string `json:"isolation"`

    // MaxWorkspaces limits how many concurrent workspaces the user can have.
    // -1 means unlimited. Default: 10.
    MaxWorkspaces int `json:"maxWorkspaces"`

    // Ephemeral means workspaces are deleted when the last session disconnects.
    Ephemeral bool `json:"ephemeral"`
}
```

Extracted from the WhoIs response:

```go
caps, err := tailcfg.UnmarshalCapJSON[ThundersnapCap](
    who.CapMap, "tailscale.com/cap/thundersnap",
)
```

### Policy resolution order

1. **CapMap grant** from the tailnet policy file (highest priority)
2. **Local policy file** match (`/etc/thundersnap/policy.jsonc`)
3. **Hardcoded default**: `role=developer, isolation=vm, maxWorkspaces=5`

---

## 2. Local Policy File

The local policy file uses hujson (JSON with comments and trailing commas),
with the same grant structure as Tailscale policy files. This allows
cut-and-paste between the local file and the tailnet policy.

Path: `/etc/thundersnap/policy.jsonc`

```jsonc
{
  "grants": [
    {
      // Default for any tailnet member
      "src": ["autogroup:member"],
      "dst": ["tag:thundersnap"],
      "app": {
        "tailscale.com/cap/thundersnap": [{
          "role": "developer",
          "isolation": "vm",
          "maxWorkspaces": 10,
          "ephemeral": false,
        }],
      },
    },
    {
      // CI workers get ephemeral containers, no VM
      "src": ["tag:ci-worker"],
      "dst": ["tag:thundersnap"],
      "app": {
        "tailscale.com/cap/thundersnap": [{
          "role": "ephemeral",
          "isolation": "container",
          "maxWorkspaces": 50,
          "ephemeral": true,
        }],
      },
    },
    {
      // Single-user mode: operator is already in an isolated VM
      "src": ["alice@example.com"],
      "dst": ["tag:thundersnap"],
      "app": {
        "tailscale.com/cap/thundersnap": [{
          "role": "admin",
          "isolation": "none",
          "maxWorkspaces": -1,
        }],
      },
    },
  ],
}
```

The `dst` field is ignored during local evaluation (you're already talking
to this thundersnap instance), but is present so the stanza is valid if
pasted into a tailnet policy file.

The `src` field is matched against the connecting identity using the same
semantics as Tailscale ACLs: user login names, tags, and autogroups.

---

## 3. User VM Lifecycle

### Architecture

Each user who connects with `isolation: "vm"` gets a dedicated
cloud-hypervisor VM. All of the user's workspace containers run inside
this VM as chroots sharing the VM's network namespace.

```
thundersnapd (host)
 └─ cloud-hypervisor (alice's VM)
     ├─ tailscaled (alice's Tailscale identity)
     ├─ /workspaces/dev/      ← btrfs chroot container
     ├─ /workspaces/staging/  ← btrfs chroot container
     └─ /workspaces/test/     ← btrfs chroot container
```

### VM creation

When a user's first SSH session arrives and no VM exists:

1. Create a btrfs subvolume for the user's root filesystem (cloned from
   the base snapshot).
2. Boot cloud-hypervisor with:
   - virtiofs sharing the user's rootfs
   - virtio-balloon with `deflate_on_oom=on, free_page_reporting=on`
   - passt for initial network connectivity (before Tailscale auth)
3. Start `tailscaled` inside the VM (full daemon, not tsnet).
4. Enter the **auth gate** (see below).

### Auth gate (first-time Tailscale authentication)

The first time a user's VM starts (or any time its Tailscale state is
missing), the SSH session enters an auth gate:

```
$ ssh dev@thundersnap
* Hello <alice@example.com>, starting your VM...
* Tailscale is not yet authenticated in your VM.
* To authenticate, visit:
*   https://login.tailscale.com/a/XXXXXXX
* Waiting for authentication...
* Authenticated! Connecting you to <dev>...
```

The flow:

1. thundersnapd detects that the VM's tailscaled is in `NeedsLogin` state.
2. thundersnapd retrieves the login URL from the VM's tailscaled
   (via vsock control protocol or the VM's local API).
3. thundersnapd prints the URL to the user's SSH stderr.
4. thundersnapd polls until tailscaled reports `Running` state.
5. Session proceeds to create/resume the requested workspace container.

Subsequent SSH sessions to the same VM skip the auth gate entirely —
tailscaled is already authenticated.

### VM shutdown

When the last SSH session to a user's VM disconnects, the VM shuts down
immediately. This reclaims all memory and CPU resources.

On next connection, the VM boots again. Tailscale state persists on the
host (see below), so no re-authentication is needed.

### Memory overcommit

Even with immediate shutdown on idle, there will be periods with multiple
active VMs. To handle this efficiently:

- **virtio-balloon** with `deflate_on_oom=on` and `free_page_reporting=on`:
  idle memory inside a VM is reported to the host and can be reclaimed.
- **KSM** (Kernel Samepage Merging) on the host: since all VMs run similar
  base images, identical memory pages are deduplicated. Enable via
  `ksmtuned`.
- **Host swap** as a safety net for transient memory pressure.

---

## 4. Tailscale State Isolation

### The problem

Tailscale node keys (`tailscaled.state`) are a unique identity. If they
end up inside a btrfs snapshot and that snapshot is cloned, two nodes
fight over the same identity, causing connectivity flapping.

### Solution: state lives on the host, outside snapshotable storage

```
Host filesystem (NOT a btrfs subvolume, not snapshotted):
  /var/lib/thundersnap/ts-state/<user>/
    tailscaled.state          ← user's Tailscale node keys

User's btrfs subvolumes (snapshotted by `ts snap`):
  /var/lib/thundersnap/fs/<user>/<workspace>/
    /home, /usr, /etc, ...    ← workspace filesystem
```

The host-side state directory is mounted into the VM via a second virtiofs
share at `/var/lib/tailscale`. The VM's tailscaled reads and writes its
state there. This path is never part of any btrfs snapshot operation.

### What survives what

| Event | Tailscale state | Workspaces |
|-------|----------------|------------|
| VM shuts down (idle) | Persists on host | Persist on host (btrfs) |
| VM reboots | Persists | Persist |
| `ts snap` | NOT included | Included |
| `ts create` (clone) | NOT cloned | Cloned |
| Host reboot | Persists | Persist |
| Host lost/replaced | Lost (user re-auths) | Lost (restore from mesh) |

### Defensive check on clone

As a safety measure, the container init sequence should detect if it's
running with Tailscale state that doesn't belong to it. On workspace
creation via `ts create`, thundersnapd writes a boot-id file into the
workspace. If the boot-id doesn't match at container start, any Tailscale
state found inside the workspace filesystem (as opposed to the mounted
host-side state) should be deleted.

---

## 5. Container Networking Inside the VM

Containers inside a user's VM are chroots with `CLONE_NEWPID`,
`CLONE_NEWNS`, and `CLONE_NEWUTS`, but **not** `CLONE_NEWNET`. They share
the VM's network namespace, which means they inherit the VM's Tailscale
connectivity.

From inside any container, the user can:

- Reach any tailnet service their ACLs allow (e.g., Aperture)
- Run tools like `claude-code` that connect to Aperture for LLM access
- Use `git`, `curl`, `ssh` — all routed through their Tailscale identity
- Access the internet via Tailscale exit nodes (if configured)

No proxy configuration is needed. The VM's network stack is the user's
Tailscale network.

### Container-only mode (no VM)

For `isolation: "container"` sessions (e.g., CI workers), containers run
directly on the host as they do today. These containers get thundersnapd's
network identity (`tag:thundersnap`), not the connecting user's identity.
This is acceptable for CI where outbound tailnet access from inside the
workspace is not needed.

If a container-mode session needs tailnet access, the container can run
tailscaled in userspace mode (`--tun=userspace-networking`) and
authenticate independently. But this is the exception, not the default.

### Single-user mode (no isolation)

For `isolation: "none"`, the session runs directly on the host — no VM,
no network namespace separation. The host's own Tailscale instance (which
may be thundersnapd's tsnet, or a separate tailscaled) provides
networking. This mode is for single-user thundersnap instances that are
already running inside an isolated VM (e.g., on EC2).

---

## 6. Service-Mode Containers

### Overview

A service-mode container runs a long-lived process that exports a tsnet
identity (e.g., a web service reachable at `myservice.tailnet`). Unlike
developer workspaces, service containers have a **singleton constraint**:
exactly one instance of a given service runs across the entire mesh at
any time.

### Why singleton matters

The service's tsnet state (node keys) is its identity on the tailnet.
Two instances running with the same keys causes the same
duplicate-node-key problem described in section 4. The singleton
constraint is not just an optimization — it's a correctness requirement.

### tsnet runs inside the container

The tsnet instance runs inside the service container itself. The container
owns its network identity. This keeps the service self-contained and
portable — it can be moved between thundersnap nodes by the coordination
service.

On startup:
1. thundersnapd retrieves the service's tsnet state from setec.
2. The state is mounted into the container at the expected path.
3. The container starts, tsnet connects using the existing state.
4. A file watcher (or periodic sync) writes state changes back to setec.

### Service definition

Services are defined in a configuration file (or via the coordination
service API):

```jsonc
{
  "services": [
    {
      "name": "myservice",
      "snapshot": "abc123...",    // snapshot ID, or "latest"
      "tsnetHostname": "myservice",
      "replicas": 1,              // always 1 for now
      "setecPrefix": "thundersnap/services/myservice",
    },
  ],
}
```

### Developer forks of services

When a developer runs `ts create myservice-dev <snapshot-id>`, they get a
clone of the service's filesystem. No tsnet state is injected — this is a
dev instance. If the developer wants their fork to be reachable on the
tailnet, they authenticate a new tsnet identity (e.g.,
`myservice-dev.tailnet`) interactively. That state lives in the
developer's VM state directory, not in the workspace.

---

## 7. Coordination Service (thundersnap-coord)

### Purpose

`thundersnap-coord` is a separate binary that provides:

1. **Lease management** for service-mode containers (singleton enforcement)
2. **Service registry** (which services exist, which snapshot to run)
3. **State brokering** (mediates access to setec for tsnet state)

It runs as its own tsnet node on the tailnet (`tag:thundersnap-coord`).

### Lease protocol

Thundersnap nodes acquire leases before starting service-mode containers:

```
POST /lease/acquire
{
  "service": "myservice",
  "holder": "thundersnap-node-3.tailnet",
  "ttl": "5m"
}
→ 200 {"ok": true, "lease_id": "..."}
→ 409 {"ok": false, "holder": "thundersnap-node-7.tailnet", "expires": "..."}
```

```
POST /lease/renew
{
  "lease_id": "...",
  "ttl": "5m"
}
→ 200 {"ok": true}
→ 410 {"ok": false, "reason": "lease expired"}
```

```
POST /lease/release
{
  "lease_id": "..."}
→ 200 {"ok": true}
```

Lease holders must renew before TTL expiry (recommended: every TTL/3).
If a holder crashes, the lease expires after TTL and another node can
acquire it.

### tsnet state lifecycle for services

```
Service starts on thundersnap-node-3:
  1. node-3 acquires lease from thundersnap-coord
  2. node-3 fetches tsnet state:
       GET thundersnap-coord /state/myservice
       (coord retrieves from setec)
  3. node-3 starts container with state mounted
  4. Container's tsnet connects as "myservice.tailnet"
  5. Periodic: node-3 syncs state changes back:
       PUT thundersnap-coord /state/myservice
       (coord writes to setec)
  6. node-3 renews lease every TTL/3

Service migrates to thundersnap-node-7 (node-3 crashed):
  1. node-3's lease expires (TTL elapsed with no renewal)
  2. node-7 acquires lease from thundersnap-coord
  3. node-7 fetches tsnet state from coord (same state as node-3 had)
  4. node-7 starts container — tsnet reconnects as "myservice.tailnet"
  5. Service is back online with the same identity
```

### Coordinator availability

The coordinator is a single process. If it's down:

- **Existing leases continue** — holders keep running and renewing is
  a no-op until the coordinator returns.
- **New leases cannot be acquired** — this is fail-safe. No service
  starts without coordination, preventing duplicates.
- **State retrieval fails** — new service starts are blocked.

For higher availability, the coordinator could be deployed with a
hot standby that takes over using its own lease mechanism (or simply
restarted by systemd). The coordinator's own state is in setec, so
any instance can recover it.

### Supplementary safety check

Even with the lease system, a thundersnap node should verify that the
tsnet identity isn't already online before starting a service. After
fetching the tsnet state and before starting the container:

1. Attempt to connect tsnet.
2. If connection succeeds — proceed (you are the sole holder).
3. If "node key already in use" — abort and release the lease.
   Another instance is still running despite the lease expiry
   (clock skew, network partition).

This provides defense-in-depth against split-brain scenarios.

---

## 8. Mesh Peer Discovery

### Current behavior

The existing `shouldPingPeer` function filters mesh peers by shared tags
(if thundersnapd is tagged) or same user ID (if untagged). This remains
appropriate for the infrastructure-level mesh.

### Mesh groups

Mesh discovery uses tags to form groups:

- `tag:thundersnap` nodes mesh with each other for snapshot replication
- `tag:thundersnap-coord` is the coordination service (does not mesh
  for snapshots)
- `tag:thundersnap-staging` could form a separate mesh for staging
  environments

### Service placement

The coordination service can make placement decisions based on:

- Which nodes have the snapshot locally (avoid download)
- Which nodes have capacity (CPU, memory headroom)
- Which nodes are healthy (recent mesh pings)

This is future work — initial implementation can use simple "first node
to acquire the lease wins" semantics.

---

## 9. Security Boundaries

### Threat model

| Attacker | What they can do | What they can't do |
|----------|------------------|--------------------|
| Compromised workspace | Full access within the btrfs chroot; shared network with other workspaces in the same VM | Escape VM boundary; access other users' VMs; access host filesystem |
| Compromised user VM | Full access to all of that user's workspaces; access tailnet as that user | Access other users' VMs; access host; access thundersnapd's tsnet keys |
| Stolen thundersnapd tsnet keys (`tag:thundersnap`) | Receive incoming SSH connections; mesh with other thundersnap nodes | Make outbound connections (if ACLs restrict `tag:thundersnap` outbound); access user data (VMs provide isolation) |
| Stolen service tsnet keys | Impersonate that service on the tailnet | Access other services' keys (setec ACLs); access user VMs |

### Key principle: least privilege for thundersnapd

thundersnapd itself runs as `tag:thundersnap` with minimal ACL permissions:

```jsonc
{
  "acls": [
    {
      // thundersnap can accept incoming SSH (port 22)
      "action": "accept",
      "src": ["autogroup:member"],
      "dst": ["tag:thundersnap:22"],
    },
    {
      // thundersnap nodes can mesh with each other (port 7575)
      "action": "accept",
      "src": ["tag:thundersnap"],
      "dst": ["tag:thundersnap:7575"],
    },
    {
      // thundersnap can reach the coordinator
      "action": "accept",
      "src": ["tag:thundersnap"],
      "dst": ["tag:thundersnap-coord:443"],
    },
  ],
}
```

thundersnapd does NOT have outbound access to the general tailnet. Users
get outbound access via their own Tailscale identity inside their VM.

---

## 10. Session Flow Summary

### Developer session (isolation: "vm")

```
1. ssh dev@thundersnap
2. thundersnapd: WhoIs → alice@example.com
3. thundersnapd: resolve grant → {role: developer, isolation: vm}
4. thundersnapd: VM for alice exists?
   NO → boot VM, enter auth gate (one-time Tailscale auth)
   YES → skip to step 5
5. thundersnapd: workspace "dev" exists in alice's VM?
   NO → btrfs clone from base snapshot
   YES → resume existing subvolume
6. thundersnapd: start chroot container "dev" inside alice's VM
7. alice gets a shell; network is her own Tailscale identity
8. alice disconnects
9. thundersnapd: any other sessions in alice's VM?
   NO → shut down VM immediately
   YES → VM stays running
```

### CI session (isolation: "container")

```
1. ssh job-42@thundersnap (from tag:ci-worker node)
2. thundersnapd: WhoIs → tags: [tag:ci-worker]
3. thundersnapd: resolve grant → {role: ephemeral, isolation: container}
4. thundersnapd: btrfs clone from base snapshot → job-42 workspace
5. thundersnapd: start chroot container on host (no VM)
6. CI job runs; network is thundersnapd's tag:thundersnap identity
7. CI job disconnects
8. thundersnapd: ephemeral=true → delete workspace
```

### Service-mode container

```
1. thundersnap-coord assigns "myservice" to thundersnap-node-3
2. node-3 acquires lease
3. node-3 fetches snapshot (local or mesh) and tsnet state (via coord/setec)
4. node-3 starts container with tsnet state mounted
5. Container's tsnet registers as myservice.tailnet
6. Service runs; node-3 heartbeats lease and syncs state to setec
7. node-3 goes down
8. Lease expires
9. thundersnap-coord assigns "myservice" to thundersnap-node-7
10. node-7 repeats steps 2-6; myservice.tailnet comes back online
```

---

## Open Items

- **Snapshot garbage collection**: Snapshots accumulate indefinitely. The
  coordination service could track which snapshots are in use (by services
  and active workspaces) and which are eligible for deletion.

- **Service health checking**: The coordination service should monitor
  whether a service is actually healthy (not just that the lease is held).
  TCP health checks to the service's tsnet address would suffice.

- **Multi-region placement**: The current design assumes a single mesh.
  For multi-region deployments, the coordinator would need region-aware
  placement and potentially region-local setec instances.

- **Quota enforcement**: `maxWorkspaces` is defined in the grant but not
  yet enforced. thundersnapd needs to count active workspaces per user
  and reject new workspace creation when the limit is reached.

- **Nested VM isolation**: If a user wants stronger isolation for a
  specific workspace (e.g., running untrusted code), we could support a
  nested VM or a more locked-down container (no network, read-only rootfs,
  additional dropped capabilities) within the user's VM. Design TBD.
