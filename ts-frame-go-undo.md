# ts frame / ts go / ts undo Design

## ts frame

Resolves or creates frames. Prints the resulting frame UUID to stdout.

### Syntax

- `ts frame` — print current frame UUID
- `ts frame <uuid>` — validate that UUID exists, print it; error if not found
- `ts frame <ref>` — resolve ref to UUID; error if ref or UUID missing
- `ts frame <root:home:work>` — create new frame from snap triplet

### Snap Triplet Syntax

Exactly two colons required (except for bare `ts frame` with no args).

- Empty components inherit from current frame (via `ts snap`)
- `ts frame <snap>::` — replace root, keep /home and /work
- `ts frame :<snap>:` — replace /home only
- `ts frame ::` — synonym for `ts frame` (current frame)
- `ts frame :::` — error (three colons invalid)
- `ts frame foo:bar` — error (one colon invalid)

## ts go

Creates/resolves a frame and starts a new session inside it. The current SSH
session connects to that new session. When the inner session exits, control
returns to the original shell.

### Syntax

Same arguments as `ts frame`, except `--ref` is not supported.

To update a ref after creating a frame: `ts ref move <ref> $(ts frame)`

### Implementation

1. Resolve/create frame via `ts frame <args>`
2. Connect to host via vsock using existing session creation protocol
3. Host allocates PTY in target frame, spawns shell (identical to SSH entry path)
4. `ts go` acts as PTY relay, forwarding stdin/stdout
5. Signal handling: forward SIGINT, SIGWINCH, etc. to inner session
6. On session exit, `ts go` exits, returning to original shell

### Notes

- vsock path skips VM spinup intentionally
- `ts go` stays in same isolation context as caller
- Revisit when isolation contexts are formalized
- When creating a new frame, clone parent's `ts log` history

## ts undo

Jumps backward in time by one snap.

### Behavior

1. Find most recent snap in `ts log` → `<prev>`
2. Run `ts snap` to record current state as `<current>`
3. Run `ts go <prev>::` to enter new frame based on `<prev>`
4. Prune both `<current>` and `<prev>` from the new frame's cloned log

### Successive Undo

Each undo peels back one layer:

```
Frame A: log = [s1, s2, s3], state matches s3
  ts undo →
Frame B: log = [s1, s2], state matches s3 (before any changes)
  ts undo →
Frame C: log = [s1], state matches s2
  ts undo →
Frame D: log = [], state matches s1
```

### Edge Cases

- Empty log after pruning is valid (no more undo available)
- `ts undo` with empty or single-entry log: allowed, results in empty log
- The frame's current state may not match its log tip (it's "one behind")

### Preservation

The snapshot taken before undo (`<current>`) remains accessible from the
parent frame if you need to recover forward.
