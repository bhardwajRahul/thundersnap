# Thundersnap

Thundersnap is a container and VM orchestration daemon built on
[Tailscale](https://tailscale.com) and [btrfs](https://btrfs.readthedocs.io/).
It provides instant-clone filesystem isolation where each incoming SSH
connection gets its own copy-on-write workspace, identified by the caller's
Tailscale identity. No SSH keys, no manual user provisioning — just
`ssh <username>@thundersnap` on your tailnet.

## Key Features

- **Instant snapshot and fork.** Every workspace is a btrfs subvolume.
  Snapshotting and cloning are O(1) metadata operations — you can fork a
  multi-gigabyte filesystem in milliseconds.

- **One SSH connection = one isolated environment.** SSH into any username
  and thundersnap creates (or resumes) an isolated container namespace for
  that name, chrooted into its own filesystem tree. Different usernames
  get different workspaces; same username resumes the same workspace.

- **Namespace separation by Tailscale identity.** Your Tailscale login
  determines your top-level directory. Within that, each SSH username maps
  to a separate workspace. User `alice@example.com` SSHing as `dev` and
  `test` gets two independent filesystems; user `bob@example.com` SSHing
  as `dev` gets his own, separate `dev`.

- **Content-addressable indexing and replication.** Thundersnap uses a
  content-defined chunking system inspired by [bup](https://bup.github.io/).
  Filesystem contents are indexed into `.tsm` (manifest) and `.tsc` (chunk
  index) files using a bupsplit rolling hash. Chunks are SHA-256 addressed,
  enabling deduplication, incremental snapshots, and efficient peer-to-peer
  transfer across a mesh of thundersnap nodes.

- **Tight Tailscale integration.** Thundersnap runs as a
  [tsnet](https://pkg.go.dev/tailscale.com/tsnet) application — it joins
  your tailnet directly, with no separate Tailscale daemon required.
  Authentication is via Tailscale WhoIs (no passwords, no SSH keys). Mesh
  discovery finds other thundersnap nodes on the tailnet for distributed
  snapshot storage and retrieval.

- **Btrfs keeps things fast and simple.** Rather than layered union
  filesystems or disk images, thundersnap relies on btrfs copy-on-write
  subvolumes. Clone, snapshot, and delete are all kernel-level btrfs
  operations. This removes an entire class of complexity.

- **VM mode (experimental).** In addition to container namespaces,
  thundersnap can launch [cloud-hypervisor](https://www.cloudhypervisor.org/)
  VMs with [virtiofs](https://virtio-fs.gitlab.io/) filesystem sharing and
  [passt](https://passt.top/) user-space networking. SSH into
  `vm/<name>@thundersnap` to get a real VM instead of a container namespace.

## How It Works

1. A user runs `ssh myworkspace@thundersnap` from their tailnet.
2. `thundersnapd` identifies the caller via Tailscale WhoIs.
3. If the workspace doesn't exist, thundersnap clones it from a base
   snapshot (or from the user's default workspace) using `btrfs subvolume
   snapshot`.
4. The session enters a container namespace: private mount namespace,
   chroot, `/proc` and `/sys` mounted, dangerous capabilities dropped.
5. The user gets a shell. Their changes are isolated to their btrfs
   subvolume.
6. `ts snap` creates a named, content-addressed snapshot. `ts create
   <name> <snap-id>` forks a new workspace from any snapshot.

## Getting Started

### Prerequisites

- A Linux host with a **btrfs** filesystem
- A [Tailscale](https://tailscale.com) account (the host does *not* need a
  separate `tailscaled` — thundersnap embeds tsnet)
- Go 1.25+ to build from source

### Building

```sh
# Build all packages (deb, rpm, tgz) for amd64 and arm64:
make build

# Or build just what you need:
make build-deb          # .deb packages only
make build-amd64        # amd64 only, all formats
make list               # show available targets
```

Packages include two binaries:
- `thundersnapd` — the main daemon (installed to `/usr/sbin/thundersnapd`)
- `ts` — the in-container client tool (installed to `/usr/libexec/thundersnap/ts`)

### Setting Up a Base Snapshot

Thundersnap needs at least one base filesystem snapshot named `1` in
`<data-dir>/snaps` before it can create workspaces. This should be an
extracted Linux root filesystem (a full directory tree with `/bin`,
`/etc`, `/usr`, etc.).

The easiest way to get one is to export a Docker/OCI container image:

```sh
# Create the snaps directory on your btrfs filesystem
sudo btrfs subvolume create /var/lib/thundersnap/snaps/1

# Export an Ubuntu image (or Debian, Alpine, etc.)
docker export $(docker create ubuntu:24.04) | \
  sudo tar -xf - -C /var/lib/thundersnap/snaps/1

# Or use debootstrap directly:
sudo debootstrap noble /var/lib/thundersnap/snaps/1
```

Good sources for base images:
- **Docker Hub**: `docker pull ubuntu:24.04`, `docker pull debian:bookworm`
- **LXC images**: https://images.linuxcontainers.org/ — pre-built rootfs
  tarballs for many distros
- **debootstrap**: `debootstrap <suite> <target>` for Debian/Ubuntu
- **Alpine**: https://alpinelinux.org/downloads/ — "Mini root filesystem"

### Running

```sh
# Create the required btrfs directories
sudo mkdir -p /var/lib/thundersnap/fs /var/lib/thundersnap/snaps

# Run directly (--data-dir defaults to /var/lib/thundersnap):
sudo thundersnapd --data-dir=/var/lib/thundersnap

# Or install the .deb and use systemd:
sudo dpkg -i dist/thundersnap_*_amd64.deb
# Edit /etc/default/thundersnapd if needed, then:
sudo systemctl start thundersnapd
```

On first run, thundersnapd will print a Tailscale login URL. Authenticate,
and then any device on your tailnet can SSH in:

```sh
ssh myworkspace@thundersnap
```

### In-Container Commands

Once inside a thundersnap workspace, use the `ts` tool:

```sh
ts ping                              # health check
ts snap                              # snapshot current workspace
ts snap <path>                       # snapshot just <path>'s subtree, re-rooted
ts frame newname <snapshot-id>       # fork a new workspace from a snapshot
ts who-has <snapshot-id>             # find which mesh peers have a snapshot
ts download-snap <snapshot-id>       # download a snapshot from the mesh
```

### Mesh Mode

Enable mesh discovery to share snapshots across multiple thundersnap nodes:

```sh
thundersnapd --mesh --data-dir=/var/lib/thundersnap
```

Mesh nodes discover each other via Tailscale and can transfer snapshots
using content-defined chunking — only changed chunks are transferred.

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
│                    btrfs filesystem                      │
│  data-dir/snaps/     data-dir/fs/<tailscale-user>/<name>/ │
│    1/  (base)              (live workspaces)             │
│    <snap-hash>/                                         │
└─────────────────────────────────────────────────────────┘
```

### Programs

| Binary | Description |
|--------|-------------|
| `thundersnapd` | Main daemon: tsnet SSH server, container/VM orchestration, mesh discovery, NFS export |
| `ts` | In-container client: snapshots, frame creation, mesh queries, capability dropping |
| `tsm` | Generates `.tsm`/`.tsc` manifest and chunk index files for content-addressed storage |
| `vshd` | Shell server that runs inside VMs, accepts vsock connections |
| `vsh` | Client for connecting to vshd inside VMs via vsock |
| `trivial-httpd` | Static file server with HTTP range support (for mesh chunk serving) |

## Current Limitations

This is early-stage software. Notable gaps:

- **No end-to-end integration tests.** The individual pieces work, but
  there is no automated test suite that stands up a thundersnapd, SSHes
  in, creates snapshots, and verifies the full flow.

- **No garbage collection of old snapshots.** Snapshots accumulate in
  `snaps-dir` indefinitely. There is no policy for expiring old or
  unreferenced snapshots — you'll need to clean them up manually.

- **No RAM/resource isolation between containers.** All containers share
  the host's memory and CPU without cgroup limits. One runaway process can
  starve others. VM mode has a fixed 512MB allocation but no dynamic
  adjustment.

- **Container init is your shell, not /sbin/init.** Thundersnap does not
  run the container image's init system (e.g., systemd). You get a login
  shell in a chroot with capabilities dropped. This means no systemd
  services, no proper process supervision, no `/tmp` cleanup, etc. Whether
  this is a feature or a bug depends on your use case — it's fast and
  simple, but you won't get a full "machine" experience without VM mode.

- **Lots of rough edges in session semantics.** Reconnecting to an
  existing workspace, handling multiple concurrent sessions to the same
  workspace, and cleanup on disconnect all have edge cases that need more
  work.

- **Capability dropping is not a full security boundary.** The container
  uses chroot and drops a set of dangerous capabilities, but it does not
  use full user namespaces, seccomp filters, or AppArmor/SELinux profiles.
  For stronger isolation, use VM mode.

## Use Case: Isolating AI Coding Agents

Thundersnap is well-suited as an isolation layer for autonomous AI coding
agents — tools like [Claude Code](https://docs.anthropic.com/en/docs/claude-code)
with `--dangerously-skip-permissions` or
[OpenClaw](https://github.com/openclaw/openclaw) (formerly Clawdbot).

**The problem:** Running an AI agent with full system access is risky.
`claude --dangerously-skip-permissions` bypasses all confirmation prompts,
giving the agent unrestricted shell access. The community consensus is
"containers or don't bother" — but setting up proper per-session isolation
with Docker is manual and doesn't naturally map to "one agent = one
environment."

**What thundersnap provides:**

- **Instant per-agent isolation.** SSH in as a unique username and the
  agent gets its own filesystem. No Dockerfile, no volume mounts, no
  cleanup scripts. `ssh agent-task-42@thundersnap` creates a fresh,
  isolated workspace.

- **Fork and experiment cheaply.** An agent can `ts snap` before a risky
  operation and `ts create` to fork a workspace. If the experiment fails,
  the original is untouched. Btrfs makes this essentially free.

- **Identity-based access via Tailscale.** No API keys to distribute to
  containers. Tailscale identity determines who can access what.

- **Mesh replication for base images.** Pre-built environments (with the
  right toolchains, dependencies, etc.) can be snapshotted once and
  distributed to any thundersnap node on the mesh.

**Combined with [Aperture](https://aperture.tailscale.com/):** Tailscale's
Aperture is an AI gateway that sits on your tailnet and provides
identity-based access control, session logging, and policy enforcement for
LLM API calls. Pairing thundersnap with Aperture gives you both halves of
the agent isolation problem:

- **Aperture** controls *what the agent can say* — which models it can
  call, what tools it can invoke, with full audit logs of every LLM
  interaction. API keys stay on the gateway, never in the sandbox.
- **Thundersnap** controls *what the agent can do* — filesystem isolation,
  capability dropping, and (in VM mode) hardware-level separation.

Together, they provide a defense-in-depth setup: even if a prompt injection
attack convinces the agent to run malicious commands, the blast radius is
limited to a disposable btrfs subvolume with no access to the host, other
agents, or sensitive credentials. Aperture's session logs provide the audit
trail to detect and investigate incidents.

This is not a theoretical concern. AI agents have been demonstrated to be
vulnerable to prompt injection via innocuous-looking files that exfiltrate
data. Disposable, isolated filesystems with network policy enforcement are
a practical mitigation.

## License

BSD-3-Clause. See [LICENSE](LICENSE) for details.
