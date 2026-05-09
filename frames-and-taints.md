# Frames and Taints Design

This document describes the frame model for composable container filesystems
and the taint system for tracking provenance and security properties.

## Overview

A thundersnap container filesystem is composed of three independent
components that can be mixed and matched:

| Component | Mount point | Contains |
|-----------|-------------|----------|
| **rootfs** | `/` | OS, packages, `/var`, `/usr`, `/etc`, system state |
| **home** | `/home` | User dotfiles, shell config, editor settings, personal customizations |
| **work** | `/work` | Source code, project files, application state (databases, etc.) |

A **frame** is a named instance that combines these three components. Each
component is a btrfs subvolume that can be independently snapshotted,
shared, and evolved.

A **snap** is an immutable, content-addressed btrfs snapshot identified by
its SHA-256 hash. Snaps are the unit of sharing and versioning.

A **service** is simply a named frame with a Tailscale identity attached to
the name (managed via the coordinator and setec, as described in
network-and-keys-design.md). There is no separate "service" concept — it's
just a frame with a network identity.

---

## Frame Identifier Format

A frame's composition is expressed as:

```
rootfs:home:work
```

Examples:
```
abc123:def456:789xyz   # fully specified
abc123::               # specific rootfs, empty home, empty work
:def456:789xyz         # default rootfs, specific home, specific work
```

For now, rootfs and home are mandatory when creating a frame. An empty work
component (trailing `::` or `:home:`) means an empty `/work` directory.

---

## On-Disk Structure

### Snaps directory

Immutable, content-addressed snapshots:

```
snaps/
  abc123/              # btrfs snapshot (read-only subvolume)
  abc123.hujson        # metadata for this snap
  def456/
  def456.hujson
  789xyz/
  789xyz.hujson
```

Each snap's metadata file (`$snap.hujson`) contains:

```hujson
{
  // The snap this was derived from, if any.
  // Null for base images (e.g., downloaded from docker).
  parent: "previous-snap-id",

  // Taints accumulated on this snap. See "Taint Model" below.
  taints: ["pii:customers"],

  // Optional: where this snap originally came from.
  // Only present on snaps created by ts download-docker.
  // Not inherited by child snaps.
  source: {
    type: "docker",
    ref: "docker.io/library/ubuntu:24.04@sha256:abcd...",
  },
}
```

### Frames directory

Live, mutable frame instances organized by user:

```
fs/
  alice@example.com/
    dev.hujson          # frame metadata
    dev/                # live btrfs subvolume (the rootfs)
      bin/
      etc/
      home/             # nested btrfs subvolume (home component)
      usr/
      var/
      work/             # nested btrfs subvolume (work component)
    staging.hujson
    staging/
```

The frame directory itself IS the rootfs subvolume. The `/home` and `/work`
directories within it are separate btrfs subvolumes. This means:

- `btrfs subvolume snapshot` on the frame dir captures only the rootfs
- `/home` and `/work` are automatically excluded (btrfs subvolume semantics)
- Each component can be snapshotted independently

Frame metadata (`$frame.hujson`) contains:

```hujson
{
  // The three snaps this frame was created from.
  // These are the "base" versions; the live subvolumes may have diverged.
  rootfs: "abc123",
  home: "def456",
  work: "789xyz",

  // Taints on this frame (union of component taints, plus any acquired at runtime).
  taints: ["pii:customers"],

  // Service configuration, if this frame exports a Tailscale identity.
  // Null for developer frames.
  service: {
    name: "myservice",
    // tsnet state is managed externally via setec; not stored here.
  },
}
```

---

## Frame Lifecycle

### Creating a frame

```
ts create <frame-name> <rootfs>:<home>:<work>
```

1. Validate that the specified snaps exist (or use defaults/empty).
2. Create the frame directory: `btrfs subvolume snapshot snaps/$rootfs → fs/$user/$frame/`
3. Create the home subvolume: `btrfs subvolume snapshot snaps/$home → fs/$user/$frame/home`
4. Create the work subvolume: `btrfs subvolume snapshot snaps/$work → fs/$user/$frame/work`
   (or create an empty subvolume if work is empty)
5. Compute initial taints as the union of all three snaps' taints.
6. Write `fs/$user/$frame.hujson` with the metadata.

### Running a frame

At runtime, additional mounts are added (as we already do today):
- `/proc`, `/sys`, `/dev` — standard container mounts
- `/tmp` — tmpfs
- `/var/lib/tailscale` — virtiofs from host (Tailscale state, per network-and-keys-design.md)
- `/run` — tmpfs for runtime state

### Snapshotting a frame

```
ts snap <frame-name>
```

1. Snapshot each component (while the frame may still be running — btrfs handles this):
   - `btrfs subvolume snapshot -r fs/$user/$frame → snaps/$new-rootfs-id`
   - `btrfs subvolume snapshot -r fs/$user/$frame/home → snaps/$new-home-id`
   - `btrfs subvolume snapshot -r fs/$user/$frame/work → snaps/$new-work-id`

2. Compute the content hash (SHA-256) for each new snapshot.

3. For each component, check if a snap with that hash already exists:
   - If yes: delete the new snapshot (it's a duplicate) and use the existing snap ID.
   - If no: keep the new snapshot, write its `.hujson` metadata.

4. Handle taint reconciliation (see "Taint Deduplication" below).

5. Print the new frame identifier: `$rootfs:$home:$work`

### thundersnap restart behavior

On thundersnap restart:
1. Enumerate all frames by reading `fs/$user/*.hujson` files.
2. Clean up any runtime state (unmount /proc, /dev, etc.; kill any lingering processes).
3. Leave frame filesystem content as-is (preserve work-in-progress).
4. Frames are ready to be started again on next SSH connection.

---

## Taint Model

Taints track security-relevant provenance. Once a snap or frame acquires a
taint, it propagates through forks and snapshots unless proven otherwise.

### Taint types

| Taint | Meaning | Applied when |
|-------|---------|--------------|
| `unsafe-permissions` | Frame ran with `claude-code --dangerously-skip-permissions` | Detected at runtime |
| `pii:<dataset>` | Frame had access to the named PII dataset | Policy/access control event |
| `untrusted-code` | Arbitrary external code was executed | Policy decision |

Additional taints can be defined as needed. The taint value is an opaque
string; thundersnap doesn't interpret the contents beyond storing and
propagating them.

### Taint propagation rules

**Frame creation**: A new frame inherits the union of taints from its three
component snaps.

```
frame.taints = union(rootfs.taints, home.taints, work.taints)
```

**Runtime**: A frame can acquire additional taints during execution (e.g.,
when `--dangerously-skip-permissions` is detected). These are added to
`frame.taints` in the hujson file.

**Snapshotting**: When `ts snap` creates new snaps from a running frame,
each snap inherits the frame's current taints — subject to deduplication
(see below).

### Taint deduplication

When snapshotting produces a content hash that already exists in `snaps/`:

1. We now have two provenances for the same content:
   - The existing snap (with its taints)
   - The new snap (with the frame's taints)

2. The true taint set is the **intersection** of taints across all known
   provenances. If any provenance lacks a taint, that taint is not inherent
   to the content.

3. Update the existing snap's `.hujson` to reflect the intersection.

4. Discard the duplicate snapshot (keep the existing one).

**Example**:
```
Existing snap abc123: taints: ["pii:customers", "unsafe-permissions"]
New frame with taints: ["unsafe-permissions"]
After ts snap produces hash abc123:
  → abc123.taints = intersection(["pii:customers", "unsafe-permissions"], ["unsafe-permissions"])
  → abc123.taints = ["unsafe-permissions"]
```

The `pii:customers` taint is removed because we've proven the content can be
produced without PII access.

---

## Docker Image Import

```
ts download-docker <image-reference>
```

Downloads a Docker image from a registry, flattens all layers into a single
filesystem, and stores it as a snap.

### Process

1. Parse the image reference (e.g., `ubuntu:24.04`, `docker.io/library/golang:1.22`).
2. Resolve to a specific digest via registry API.
3. Check if we already have a snap with `source.ref` matching this digest:
   - Scan `snaps/*.hujson` files for matching `source.type == "docker"` and `source.ref`.
   - If found, print the existing snap ID and exit.
4. Pull and flatten the image layers (equivalent to `docker export`).
5. Create a btrfs subvolume from the flattened filesystem.
6. Compute the content hash.
7. Write the snap metadata:
   ```hujson
   {
     parent: null,
     taints: [],
     source: {
       type: "docker",
       ref: "docker.io/library/ubuntu:24.04@sha256:abcd...",
     },
   }
   ```
8. Print the snap ID.

### Usage

Docker images are typically used as rootfs:

```
$ ts download-docker ubuntu:24.04
abc123

$ ts create myframe abc123::
```

The `source` metadata is informational and not inherited by snaps derived
from this one. It answers "where did this base image come from?" without
implying anything about child snaps.

Private registries are not supported initially.

---

## Command Summary

| Command | Description |
|---------|-------------|
| `ts create <name> <rootfs>:<home>:<work>` | Create a new frame from snaps |
| `ts snap <name>` | Snapshot a frame's current state, print new snap IDs |
| `ts download-docker <image>` | Import a Docker image as a snap |

---

## Future Considerations

- **Default rootfs/home**: Allow `::work` syntax with configurable defaults
  per-user or per-tailnet.

- **Taint auditing**: Record taint changes with timestamps and reasons for
  compliance purposes.

- **Snap garbage collection**: Track which snaps are referenced by frames
  and clean up orphans.

- **Snap signing**: Cryptographically sign snaps to verify provenance
  across the mesh.
