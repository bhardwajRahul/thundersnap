# Frames, Refs, and Snap Hashes Design

## Overview

This document describes the design for frame identity, refs (named pointers),
and snap hash encoding in thundersnap.

## Core Concepts

### Snap

A content-addressed tree, identified by a 256-bit SHA256 hash. Snaps are
immutable. The inheritance hierarchy between snaps (which snap was based on
which) is an internal optimization detail for replication efficiency and is
not exposed to users.

### Frame (UUID)

A frame is identified by a UUID and represents a filesystem with history.
Each `ts snap` adds a new snapshot entry to that frame's history. The UUID is
the "content lineage" - the same filesystem evolving over time.

### Ref

A mutable pointer from a name to a frame UUID, analogous to a git branch.
Refs can be created, moved to point at different UUIDs, and deleted. Each ref
maintains a reflog of which UUIDs it has pointed to over time.

### /id Directory

Private state that travels with the ref *name*, not the frame UUID. Contains
things like keys, tsnet state, and service identity. When you move a ref from
one UUID to another, the `/id/<refname>/` contents remain associated with that
ref name.

This enables blue/green deploys: build a new frame, test it, then atomically
move the ref (and its identity/keys) to the new frame.

## State Directory Structure

All state lives under a single `--state-dir`:

```
<state-dir>/
  fs/<uuid>/            # frame filesystems
  snaps/                # snap storage (internal implementation detail)
  refs/<refname>.jsonc  # ref config, autorun, reflog
  id/<refname>/         # private state per ref (keys, tsnet, etc.)
```

## Ref Configuration

Each ref has a jsonc file at `refs/<refname>.jsonc`:

```jsonc
{
  "uuid": "abc-123",
  "autorun": ["/usr/bin/nginx", "-g", "daemon off;"],
  "reflog": [
    {"uuid": "abc-123", "time": "2026-06-26T10:00:00Z"},
    {"uuid": "old-456", "time": "2026-06-25T15:00:00Z"}
  ]
}
```

- `uuid`: The frame UUID this ref currently points to
- `autorun`: Optional argv array for a program thundersnapd should keep running
- `reflog`: History of which UUIDs this ref has pointed to

## Commands

### Frame Operations

```
ts frame <root>:<home>:<work> [--ref <name>]
```

Create a new frame from snap components. Optionally create a new ref pointing
at it (fails if ref already exists).

```
ts snap
```

Snapshot the current frame, adding an entry to its UUID's history. Always runs
from inside a frame.

```
ts log [<uuid>]
```

Show the snapshot history of a frame UUID.

### Ref Operations

```
ts ref create <name> <uuid>
```

Create a new ref pointing at a UUID. Fails if the ref already exists.

```
ts ref move <name> <uuid> [-f/--force]
```

Move an existing ref to point at a different UUID. Fails if the ref doesn't
exist. Fails if the current frame (that the ref points to) has running
processes, unless `-f` is provided to kill them first.

The sequence when moving:
1. (If -f) Kill all processes in the frame the ref currently points to
2. Update ref to point at new UUID
3. `/id/<name>/` is now available to the new frame
4. If autorun is configured, start the program in the new frame

```
ts ref delete <name> [-f/--force]
```

Delete a ref. Fails if:
- The frame has running processes (unless -f)
- `/id/<name>/` is non-empty (unless -f)

```
ts reflog <name>
```

Show the history of which UUIDs this ref has pointed to.

### Service Management

```
ts autorun --ref <ref> <program> [args...]
```

Configure thundersnapd to keep a program running in the frame that `<ref>`
points to. The program is specified as an argv array, not a shell command.

thundersnapd will:
- Start the program on daemon startup
- Restart it if it crashes (with backoff to avoid CPU thrashing)
- Start it after `ts ref move` moves the ref to a new frame

```
ts autorun --ref <ref> --stop
```

Clear the autorun configuration for a ref.

### Status

```
ts ps
```

Show running sessions and services.

## Snap Hash Encoding

Snap hashes are 256-bit SHA256 values. Rather than hex encoding (64 chars),
we use base64url (RFC 4648) with a 1-bit shift for better ergonomics.

### Why base64url?

- Shorter: 43 chars vs 64 chars for hex
- URL-safe and filename-safe: uses `-` and `_` instead of `+` and `/`
- No padding needed: 256 bits encodes to exactly 43 chars

### The 1-bit shift trick

To ensure snap IDs never start with `-` or `_` (which could be confused with
command-line flags), we shift the hash left by 1 bit before encoding:

```
Encoding:
  hash (256 bits) → prepend 0 bit → 257 bits → base64url → 43 chars

Decoding:
  43 chars → base64url → 257 bits → drop first bit → 256 bits → hash
```

The first base64url character encodes 6 bits. With the prepended 0 bit, the
first char encodes `0` + bits 1-5 of the original hash. This means the first
char can only have values 0-31 in the base64url alphabet (`A-Z`, `a-f`), never
`-` (62) or `_` (63).

The last character encodes the remaining 5 bits (bits 252-256 of the original
hash), also giving values 0-31. So both the first and last characters are
guaranteed to be alphanumeric.

### Composite snap paths

Snap paths use the format `<root>:<home>:<work>` where each component is a
base64url-encoded snap hash:

```
aBcDeFgH...:xYzAbCdE...:pQrStUvW...
```

Each component is 43 characters, for a total path length of 131 characters
(43 * 3 + 2 colons).
