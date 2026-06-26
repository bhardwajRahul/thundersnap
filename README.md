# Thundersnap

Thundersnap is a container and VM orchestration daemon built on
[Tailscale](https://tailscale.com) and [btrfs](https://btrfs.readthedocs.io/).
It provides instant-clone filesystem isolation where each incoming SSH
connection gets its own copy-on-write workspace, identified by the caller's
Tailscale identity. No SSH keys, no manual user provisioning — just
`ssh <ref>@thundersnap` on your tailnet.

## Quick Start: Your First Frame from Docker

This guide sets up a basic frame using a Debian Docker image — no manual rootfs
preparation needed.

### 1. Install and start thundersnapd

```sh
# Build (requires Go 1.25+)
make binaries

# Create btrfs directories (must be on a btrfs filesystem)
sudo mkdir -p /var/lib/thundersnap/fs /var/lib/thundersnap/snaps

# Start the daemon
sudo ./bin/thundersnapd \
  --fs-dir=/var/lib/thundersnap/fs \
  --snaps-dir=/var/lib/thundersnap/snaps
```

On first run, thundersnapd prints a Tailscale auth URL. Visit it to join your
tailnet.

### 2. Create a frame from a Docker image

SSH in and download a Docker image as a snap:

```sh
# SSH into thundersnap (creates a temporary blank session)
ssh root@thundersnap

# Download debian:latest as a snap (returns the snap ID)
ts download-docker debian:latest
# Output: abc123def456...

# Create a frame from the snap with a ref name
ts frame abc123def456:nil:nil --ref mydev
# Output: Created frame 550e8400-... with ref mydev
```

### 3. Connect to your frame

```sh
# Exit the temporary session and reconnect to your ref
exit
ssh mydev@thundersnap
```

You now have a persistent Debian environment. Any changes you make are saved
to your frame. Use `ts snap` to create point-in-time snapshots.

## Core Concepts

### Snaps

A **snap** is an immutable, content-addressed snapshot of a filesystem tree.
Snaps are identified by SHA-256 hashes (e.g., `abc123def456...`) and stored in
`snaps-dir/`. They serve as the building blocks for frames.

Create snaps from:
- Docker images: `ts download-docker ubuntu:24.04`
- Mesh peers: `ts download-snap <snap-id>`
- The current frame: `ts snap`

### Frames

A **frame** is a running filesystem environment composed of three layers:

| Layer | Purpose | Mount Point |
|-------|---------|-------------|
| **rootfs** | Base OS, packages, system configuration | `/` |
| **home** | User dotfiles, shell config, personal settings | `/home` |
| **work** | Project files, source code, application data | `/work` |

Each layer is a btrfs subvolume that can be snapshotted and restored
independently. When creating a frame, specify all three components using the
`rootfs:home:work` format:

```sh
# Full frame spec
ts frame abc123:def456:ghi789 --ref prod

# Rootfs only, empty home and work
ts frame abc123:nil:nil --ref dev

# Rootfs with existing home, empty work
ts frame abc123:def456:nil --ref test
```

Frames are identified by UUIDs and stored at `fs-dir/<uuid>/`. The frame's
metadata lives in `fs-dir/<uuid>.jsonc`.

### Refs

A **ref** is a mutable pointer from a name to a frame UUID, similar to a git
branch. Refs provide stable SSH targets that can be moved between frames.

```sh
# Create a ref
ts ref create prod 550e8400-e29b-41d4-a716-446655440000

# Move a ref to a different frame
ts ref move prod 660f9500-f39c-52e5-b827-557766551111

# Delete a ref
ts ref delete staging

# List all refs
ts refs
```

Each ref maintains a reflog of which UUIDs it has pointed to over time:

```sh
ts reflog prod
# 660f9500-...  2024-01-15T10:30:00Z
# 550e8400-...  2024-01-14T09:00:00Z
```

### Directory Structure

Thundersnap uses three main directories within your state directory:

```
/var/lib/thundersnap/
├── fs/                     # Frame filesystems
│   ├── <uuid>/            # A frame's root (contains /home, /work subvolumes)
│   └── <uuid>.jsonc       # Frame metadata (rootfs, home, work snap hashes)
├── snaps/                  # Immutable snapshots
│   ├── <hash>/            # A snap's filesystem tree
│   └── <hash>.jsonc       # Snap metadata (source, taints, size)
├── refs/                   # Ref configurations
│   └── <name>.jsonc       # Ref -> UUID mapping, autorun config, reflog
└── id/                     # Per-ref private state
    └── <name>/            # Identity-specific data (keys, tsnet state)
```

**`/home` vs `/work` vs `/id`**:

- **`/home`** (inside a frame at `fs/<uuid>/home/`): User-specific configuration
  that travels with the frame — shell dotfiles, editor settings, SSH keys. This
  is a btrfs subvolume that can be snapshotted separately from rootfs.

- **`/work`** (inside a frame at `fs/<uuid>/work/`): Project and application
  data — source code, databases, build artifacts. Also a separate btrfs
  subvolume for independent snapshotting.

- **`/id`** (outside frames at `id/<refname>/`): Per-ref identity state that
  persists across frame changes — Tailscale tsnet keys, service credentials,
  anything tied to the ref's identity rather than its filesystem. This is
  *not* a btrfs subvolume and is not snapshotted.

### Autorun Services

Refs can have an **autorun** command that thundersnapd keeps running whenever
the ref's frame exists:

```sh
# Set autorun for a ref
ts autorun --ref mydev /usr/bin/my-daemon --port 8080

# Clear autorun
ts autorun --ref mydev --stop

# View autorun configuration
ts refs
# mydev -> 550e8400-... [autorun: /usr/bin/my-daemon --port 8080]
```

Autorun processes are started when the daemon starts and restarted if they
exit. This is useful for:

- Long-running development servers
- Database processes
- Background workers
- Any service that should always be available in a frame

## Key Features

- **Instant snapshot and fork.** Every workspace is a btrfs subvolume.
  Snapshotting and cloning are O(1) metadata operations — you can fork a
  multi-gigabyte filesystem in milliseconds.

- **Three-layer frame model.** Separate rootfs, home, and work means you can
  upgrade your OS without losing dotfiles, or share a home config across
  multiple projects.

- **Content-addressable indexing.** Filesystem contents are indexed into
  `.tsm` (manifest) and `.tsc` (chunk index) files using a bupsplit rolling
  hash. Chunks are SHA-256 addressed, enabling deduplication, incremental
  snapshots, and efficient peer-to-peer transfer.

- **Tight Tailscale integration.** Runs as a
  [tsnet](https://pkg.go.dev/tailscale.com/tsnet) application — joins your
  tailnet directly. Authentication is via Tailscale WhoIs (no passwords, no
  SSH keys).

- **Mesh replication.** Enable `--mesh` to share snapshots across multiple
  thundersnap nodes. Discover peers via Tailscale; transfer only changed
  chunks.

- **VM mode (experimental).** SSH into `vm/<name>@thundersnap` to get a real
  [cloud-hypervisor](https://www.cloudhypervisor.org/) VM with
  [virtiofs](https://virtio-fs.gitlab.io/) filesystem sharing.

## Building

```sh
# Build all packages (deb, rpm, tgz) for amd64 and arm64:
make build

# Build just the binaries:
make binaries    # outputs to bin/
make ts          # just the ts binary

# Run tests:
make test
```

Packages include:
- `thundersnapd` — the main daemon (`/usr/sbin/thundersnapd`)
- `ts` — the in-container client tool (`/usr/libexec/thundersnap/ts`)

## Running

```sh
# Create required btrfs directories
sudo mkdir -p /var/lib/thundersnap/fs /var/lib/thundersnap/snaps

# Run directly:
sudo thundersnapd \
  --fs-dir=/var/lib/thundersnap/fs \
  --snaps-dir=/var/lib/thundersnap/snaps

# Or install the .deb and use systemd:
sudo dpkg -i dist/thundersnap_*_amd64.deb
sudo systemctl start thundersnapd
```

## In-Container Commands

Once inside a thundersnap frame, use the `ts` tool:

```sh
# Frame management
ts snap                          # snapshot current frame (returns rootfs:home:work)
ts snaps                         # list all snapshots with sizes
ts frame <spec> [--ref=name]     # create a new frame from snap spec
ts frames                        # list all frames with status

# Ref management
ts refs                          # list all refs
ts ref create <name> <uuid>      # create a new ref
ts ref move <name> <uuid>        # move ref to different frame
ts ref delete <name>             # delete a ref
ts reflog <name>                 # show ref history

# Docker images
ts download-docker <image>       # download Docker image as a snap

# Mesh operations (requires --mesh)
ts who-has <snap-id>             # find which peers have a snap
ts download-snap <snap-id>       # download a snap from the mesh

# Services
ts autorun --ref <name> <cmd>    # configure autorun for a ref

# Frame history
ts log [uuid]                    # show frame's snapshot history

# Taints (for tracking sensitive data)
ts taint <name>                  # mark frame as containing sensitive data
```

## Setting Up Base Snapshots

### From Docker Hub

The easiest way to get started:

```sh
ssh root@thundersnap
ts download-docker debian:bookworm   # or ubuntu:24.04, alpine:latest, etc.
ts frame <snap-id>:nil:nil --ref myenv
```

### From debootstrap

For a minimal Debian/Ubuntu without Docker:

```sh
sudo btrfs subvolume create /var/lib/thundersnap/snaps/mybase
sudo debootstrap bookworm /var/lib/thundersnap/snaps/mybase
```

### From LXC images

Download pre-built rootfs tarballs from https://images.linuxcontainers.org/

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│                     thundersnapd                        │
│  tsnet SSH server (:22) + mesh discovery (:7575)        │
├──────────────┬──────────────┬───────────────────────────┤
│  Container   │   VM mode    │  Snapshot management      │
│  namespaces  │ (cloud-hv +  │  (btrfs + TSM/TSC         │
│  (chroot +   │  virtiofs +  │   content-addressed       │
│  drop-caps)  │  passt)      │   chunking)               │
├──────────────┴──────────────┴───────────────────────────┤
│                    btrfs filesystem                     │
│  fs/<uuid>/             snaps/<hash>/                   │
│    (live frames with      (immutable snapshots)         │
│     /home, /work)                                       │
└─────────────────────────────────────────────────────────┘
```

### Programs

| Binary | Description |
|--------|-------------|
| `thundersnapd` | Main daemon: tsnet SSH server, container/VM orchestration, mesh discovery |
| `ts` | In-container client: snapshots, frame creation, mesh queries, autorun |
| `tsm` | Generates `.tsm`/`.tsc` manifest and chunk index files |
| `vshd` | Shell server inside VMs (vsock) |
| `vsh` | Client for connecting to vshd |

## Use Case: Isolating AI Coding Agents

Thundersnap is well-suited as an isolation layer for autonomous AI coding
agents. Each agent gets its own frame:

```sh
# Create a frame for each agent task
ts download-docker ubuntu:24.04
ts frame abc123:nil:nil --ref agent-task-42

# Agent SSHes in and works
ssh agent-task-42@thundersnap

# Snapshot before risky operations
ts snap

# Fork to experiment
ts frame abc123:def456:nil --ref agent-experiment
```

The blast radius of any agent mistake is limited to a disposable btrfs
subvolume. Combined with [Tailscale Aperture](https://aperture.tailscale.com/)
for API access control, you get defense-in-depth isolation.

## Current Limitations

- **No garbage collection.** Snapshots accumulate indefinitely.
- **No cgroup limits.** All containers share host resources.
- **Container init is your shell.** No systemd, no process supervision.
- **Capability dropping is not a full security boundary.** Use VM mode for
  stronger isolation.

## License

BSD-3-Clause. See [LICENSE](LICENSE) for details.
