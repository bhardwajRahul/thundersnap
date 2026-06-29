# not-e2e-enough.md — Why the e2e suite catches so few bugs

## TL;DR

The e2e suite is **not end-to-end at all.** Of ~43 test functions across 33 files:

- **0** start a real `thundersnapd` process.
- **0** connect over SSH (or any network/login transport).
- **0** validate anything by running a command through a session the daemon set up.

Instead every test reconstructs a *fragment* of the pipeline by hand and validates it
in isolation. The whole "front door" of the product — the SSH server `Handler`, identity
resolution, policy/cap evaluation, `parseSSHUser`, frame resolution, greeting, isolation
routing, and the daemon's own session/control wiring — is **completely uncovered**. That is
exactly the layer where the bugs that motivated `missing-e2e-tests.md` lived (wrong PID for
PTY, `su` vs `su -`, command-vs-interactive divergence), and it is why the suite feels like
it catches nothing: it never exercises the code a user actually hits.

The `THUNDERSNAPD_BINARY` env var is *required* by `newTestEnv` (e2e_test.go:128) and then
**never executed** — `env.daemonBinary` is stored and forgotten. The `httpClient` /
`newHTTPClient` / `post` / `doRequest` helpers (e2e_test.go:218–304) that would talk to a
real control socket are **dead code** — never called. There is no `crypto/ssh` or
`gliderlabs/ssh` import anywhere in `e2e/`.

## The target ("truly end-to-end")

Per the request, a real e2e test must:

1. Spin up a real `thundersnapd` process (the actual binary / `main`).
2. Connect to it over SSH. *The single allowed shortcut is a local TCP port instead of real
   tsnet/Tailscale.* (tsnet/WhoIs identity would need a fake-identity seam — see below.)
3. Do **all** validation by running commands over that SSH session and inspecting the
   output / exit status.

Essentially: `ssh user@frame@host -- cmd`, assert on stdout/exit; and `ssh user@frame@host`
interactive, drive a PTY, assert on what comes back. Everything the product does (frames,
snaps, taints, refs, container vs vm isolation, uid handling, mesh) is reachable this way.

## What the tests actually do (four anti-patterns)

Each test falls into one of four buckets, none of which is "real e2e (A)":

| Bucket | Meaning | Why it's not e2e |
|---|---|---|
| **B — daemon-via-HTTP (real prod code, httptest)** | Mounts a *real* production handler in `httptest` | No real process, no SSH; only 1 test, and only for `/metrics` |
| **C — reimplemented daemon** | A test-local HTTP server re-codes the daemon's control endpoints | Tests the *test's copy* of the logic, not the daemon. Drifts silently. |
| **D — shells out directly** | Calls `btrfs` / `nsenter` / `ts drop-caps-and-run` / `vshd` / `virtiofsd` / `passt` / cloud-hypervisor by hand | Reconstructs a slice of the pipeline; the daemon's orchestration of those pieces is never run |
| **E — pure unit** | Imports `tsm`/`refs`/`frames`/`snaphash`/… and calls Go functions | A unit test wearing an `e2e` build tag |

### The "fake daemon": `startTestControlServer` (frame_test.go:257)

This is the single biggest problem. `startTestControlServer` (and its wrappers
`startMeshTestControlServer`, `startDockerTestControlServer`, `startStreamingTestServer`)
**reimplements the daemon's control protocol inside the test file**: it opens a unix socket,
speaks the `CONNECT`/`OK 5223` handshake, and hand-codes `/create`, `/list-frames`,
`/delete-frame`, `/snap`, `/list-snaps`, `/taint`, `/import-docker-tarball` with test-local
handlers that call `btrfs` directly (e.g. frame_test.go:393, 414, 430–454).

Consequences:
- The real daemon handlers in `cmd/thundersnapd/refs_handlers.go` (per-user stores,
  `refid.Ensure`, `resolveFrameForUser`, metrics, validation, error wording) are **never run**.
- The test reimplementation can — and does — diverge from production: e.g. it hardcodes the
  user as `"testuser"` and frame path `fs/testuser/<name>` (frame_test.go:379), whereas the
  real layout is `fs/<tailscale-user>/<uuid>` resolved through the ref store (`resolveFrameRootFS`,
  main.go:608). A frame-layout regression in the daemon cannot be caught here.
- A green `TestFrameLifecycleBasic` proves the *mock* works, not the product.

## Per-area findings

### Frames / Snapshots / Taints / Docker / Mesh / Streaming / Errors — bucket **C**
`frame_test.go` (11 tests), `snapshot_test.go` (8), `taint_test.go` (5),
`docker_test.go` (3), `mesh_test.go` (4), `streaming_test.go` (5), and 7/8 subtests of
`error_test.go` all run against `startTestControlServer`-family fakes. Validation is by
reading files / running `btrfs` directly, never via the daemon or SSH. `mesh_test.go` and
`streaming_test.go` additionally stand up inline `httptest`/`net.Listen` servers that
*simulate* the `/who-has`, `/download-snap`, NDJSON-streaming and HTTP-range behaviors rather
than scraping the real daemon.

### UID / Hardlinks / Integration / refid / nested — bucket **D**
`uid_test.go` (6), `hardlink_test.go` (3), `integration_test.go` (3), `refid_test.go` (3),
`nested_test.go` (3) bypass the daemon entirely: raw `btrfs subvolume snapshot`, `os.Chown`,
`os.Link`, `syscall.Stat`, plus `ts drop-caps-and-run` / `nsenter` invoked by hand. These do
exercise real on-disk behavior on real btrfs, but the daemon's *use* of it (and the SSH path
into it) is absent. `integration_test.go`'s "full workflow create→modify→snap→new-frame" — the
one test that *should* be a flagship e2e — is all hand-rolled `btrfs` + `os.WriteFile`.

### Container isolation / PTY / cwd / blank container — bucket **D**
`container_test.go` (6), `container_pts_test.go` (2), `container_cwd_test.go` (1),
`blank_container_test.go` (3) drive `ts drop-caps-and-run [--chroot --skip-mount-setup]` and
`nsenter` directly, validating via `ts check-isolation` / `ts check-dev` stdout. The daemon's
`buildSessionCommand`/`runContainerSession`/`hostVshd` wiring that *assembles* those exact
invocations from an SSH session is not run — so a regression in how the daemon builds the
command (the class of bug in `missing-e2e-tests.md`) slips through.

### VM / VMX / minimal-shell / "ssh"-cwd — bucket **D**
`vm_test.go` (11), `vmx_test.go` (6), `minimal_shell_test.go` (4), `ssh_cwd_test.go` (1),
`vshd_devpts_test.go` (1) hand-spawn `virtiofsd` + `passt` + cloud-hypervisor and talk to
`vshd` directly over vsock/unix via the test-local `vshd_proto_test.go` TLV helpers
(`runVshdCommand`, `startVshdPTY`). The test harness has essentially **rebuilt the daemon's VM
launch + proxy path**, so it validates the harness's copy, not the daemon's. Note the
misleading names: `ssh_cwd_test.go` and `minimal_shell_test.go` contain **no SSH** — they boot
a VM and poke vshd.

### TSM/TSC, refs, fixtures, snap-{incremental,subdir,progress} — bucket **E**
`tsm_test.go` (7), `refs_test.go` (mostly), `refs_edge_cases_test.go` (12),
`fixtures_test.go` (1), `snap_incremental_test.go`, `snap_subdir_test.go`,
`snap_progress_test.go` import the production Go packages and call them directly
(`tsm.NewIndexer`, `refs.NewStore`, `snapsubdir.Snapshot`, …). These are perfectly good unit
tests — they just shouldn't be the *e2e* suite, and several carry an `…E2E` suffix that
overstates them.

### Metrics — bucket **B** (the lone exception)
`metrics_test.go` is the only test that mounts **real production code** (`metrics.NewHandler`)
and scrapes it over HTTP via `httptest`. Still no real process and no SSH, but it at least runs
the actual handler instead of a reimplementation.

## What this leaves totally uncovered

Because nothing connects via SSH to a real daemon, the entire SSH front door
(`cmd/thundersnapd/main.go` SSH `Handler`, ~line 567 onward) is untested e2e:

- `getWhoIs` → `ResolveCap`/policy evaluation → role/isolation decision.
- `parseSSHUser` (the `user@frame`, `vmx:`/isolation-prefix parsing) — the exact source of the
  `root@empty@host` interactive-vs-command bug.
- `resolveFrameRootFS` / `selectTargetUser` / default-ref / unknown-frame error surfacing
  *as seen by a client*.
- The greeting / `isInteractiveSession` suppression logic.
- Isolation routing (`container` vs `vmx`) chosen from the live session.
- PTY allocation through the real session (`--pty-handshake-fd`, initPid selection), command
  vs interactive parity, window-resize end-to-end.
- The daemon actually starting, binding, accepting a connection, and tearing down.
- Per-user frame layout & isolation (`fs/<user>/<uuid>`) as the daemon produces it.

These are precisely the high-value, integration-shaped behaviors e2e tests are supposed to
protect, and they have **zero** coverage today.

## Recommended direction (not done here — review first)

1. **Build one real harness: `startDaemon(t)`**
   Launch the actual `thundersnapd` binary (`env.daemonBinary`, already required and built)
   with `--data-dir`/`--state-dir` in the btrfs temp root, on a **local TCP port** instead of
   tsnet. This needs a small product seam: a flag/env to listen on `127.0.0.1:<port>` and to
   accept a **fake identity** (so `getWhoIs`/policy resolve to a test user) without a real
   tailnet. That seam is the one sanctioned shortcut; everything else stays real.

2. **Build `sshExec(t, "user@frame", "cmd") (stdout, exit)` and `sshInteractive(t, …)`**
   using `golang.org/x/crypto/ssh` against that port, with a PTY variant. All assertions go
   through these.

3. **Port the existing scenarios onto it.** Most C-bucket tests become: `ssh … -- ts snap`,
   then `ssh … -- ts snaps` and assert; D-bucket container/VM tests become: create frame via
   the control API on the daemon, then `ssh user@frame@host -- <probe>`.

4. **Demote the E-bucket package tests** out of `e2e/` into their packages' `_test.go` (they're
   unit tests), or keep them but stop counting them as e2e coverage.

5. **Delete the fakes** (`startTestControlServer` family, the inline httptest mesh/streaming
   servers, the dead `httpClient`/`post` helpers) once their scenarios run against the real
   daemon — they are a liability that gives false green.

## Loose ends spotted along the way (not the main ask)

- **Dead requirement:** `THUNDERSNAPD_BINARY` is required at setup but never run; the
  `httpClient`/`newHTTPClient`/`post`/`doRequest` block in e2e_test.go is unused. Either wire
  it up (preferred) or it's pure dead weight.
- **`TestTierCoverage` does not exist.** `main_test.go:39` claims "Every top-level test must be
  matched by exactly one tier (verified by `TestTierCoverage`)," but no such test exists. Tier
  regexes are maintained by hand, so a new test that matches no tier is **silently never run**
  (this already bit `TestTsNsenter*` historically). A real coverage guard should be added.
- Several unit-level tests carry an `…E2E` suffix (`refs_edge_cases_test.go`,
  `snap_incremental`/`subdir`/`progress`) that overstates what they verify.

---

## Consolidation: tests that collapse into a few sequential SSH workflows

A huge fraction of the suite is the *same setup repeated* with one knob changed (one
`startTestControlServer` + one btrfs snapshot + one assertion per test). Once we have the real
`startDaemon` + `sshExec`/`sshInteractive` harness, most of these collapse into a handful of
**sequential workflow tests** that each drive many commands over SSH against one running daemon.
This is both more realistic (it exercises ordering, state carryover, and the SSH front door) and
far cheaper (one daemon boot amortized over many assertions).

Below, each proposed workflow lists the existing tests it would **supersede**.

### W1 — "Image → frame → snap → re-derive" core lifecycle (the flagship)
One empty frame, pull a container from a **local** registry/tarball server, land it in a frame,
modify it, snap, attach a ref, ssh into the ref'd frame, verify the modification, fork a new
frame from the snap. This single sequential test exercises frames + docker import + snapshot +
refs + integration end-to-end over SSH.
Supersedes / absorbs:
- `frame_test.go`: TestFrameLifecycleBasic, TestFrameWithHomeSpec, TestFrameWithAllThreeSpecs,
  TestFrameFromNonExistentSnapshot, TestDeleteRunningFrame, TestFrameRestartAfterStop,
  TestFrameUserGroupCreated, TestFrameHomeWorkSymlink(+NotOverwritten)
- `snapshot_test.go`: TestSnapshotOperationsBasic, TestSnapshotWithModifiedFiles,
  TestNestedSnapshotTree, TestDeleteSnapshotWithReference (delete-still-referenced = error)
- `docker_test.go`: TestDockerImportBasic, TestDockerImportCaching, TestDockerImportInvalidReference
- `integration_test.go`: TestIntegrationWorkflowBasic, TestWorkflowHomeWorkSeparation,
  TestCrossFrameDataSharingViaWorkVolume (write in one frame's /work, read from another)
- `refs_test.go`: TestFrameRefResolution
- `refid_test.go`: all three (Ensure/Move/force-delete observed via what the *next* ssh session sees)

### W2 — "Snapshot fidelity" sweep (file-type / perms / dedup, verified after restore)
Build a frame with the full fixture (setuid/setgid, hardlinks, symlinks, devices, non-root
owners, a big tree), snap it, fork a fresh frame from that snap, ssh in and verify every
property survived: `stat -c %a`, `id`, hardlink inode equality, setuid bit, symlink target,
file count, dedup (same content → same chunk). Add a second snap of an unchanged tree and assert
byte-identical TSM (incremental no-op).
Supersedes / absorbs:
- `uid_test.go`: all 6 (TestUIDPermissionsBasic, TestSetuid/SetgidPreservation+Execution,
  TestUIDPreservation, TestHardlinkSetuidBinaryInSnapshot)
- `hardlink_test.go`: all 3
- `snapshot_test.go`: TestSnapshotDeduplication, TestLargeDirectoryTree
- `snap_incremental_test.go`: TestSnapshotIncrementalNoReindex
- `snap_subdir_test.go`: TestSnapshotSubdir (snap a subdir, verify resulting frame's root)
- much of `tsm_test.go` / `fixtures_test.go` content is implied (keep the format-level ones as
  unit tests, but the *observable* outcome is covered here)

### W3 — "Taint propagation" sequential test
ssh-create frame, add taints (pii:/unsafe-permissions/untrusted-code), snap, fork, query taints
on the child and assert propagation + dedup, all via `ts taint`/`ts taints` over SSH.
Supersedes / absorbs:
- `taint_test.go`: all 5 (Basic, MultipleTaintsOnFrame, Propagation, Deduplication, QueryFrameTaints)

### W4 — "Container session matrix" over SSH (the bug-class this whole doc is about)
One daemon, one real-unix frame + one empty (`nil:nil:nil`) frame. For each of {root, non-root}
× {command `ssh … -- cmd`, interactive PTY `ssh …`} assert: correct `whoami`/`id -u`, `$HOME`,
cwd is the home dir, `/proc` mounted, dangerous caps dropped (`ts check-isolation` via ssh),
hostname/domainname, distinct `/dev/pts/N` per concurrent session, and **command-vs-interactive
parity** (the `missing-e2e-tests.md` matrix). This is the test that would have caught the
original PTY/`su -` bug.
Supersedes / absorbs:
- `container_test.go`: all 6
- `container_pts_test.go`: both (concurrent-distinct-pts via two parallel ssh sessions)
- `container_cwd_test.go`: TestContainerLoginShellWorkingDirectory
- `blank_container_test.go`: all 3
- `ssh_cwd_test.go`: TestSSHCommandWorkingDirectory (container half)
- realizes the whole `missing-e2e-tests.md` test matrix (empty/real × cmd/interactive × root/user)

### W5 — "VM (vmx) session matrix" over SSH
Same shape as W4 but with isolation=vm/vmx selected via the SSH username prefix, against a real
booted VM the **daemon** launches (not a hand-spawned chv). Assert command + interactive + PTY
winsize (`stty size` then resize) + shared-VM/multi-frame + concurrent sessions + graceful
shutdown, all over SSH.
Supersedes / absorbs:
- `vm_test.go`: all 11
- `vmx_test.go`: all 6 (incl. TestVMXPtyWinsize)
- `minimal_shell_test.go`: all 4 (shell features become commands run over the vm ssh session)
- `ssh_cwd_test.go`: VM half
- `vshd_devpts_test.go`: TestVshdContainerPTYDevpts (observed via the real session's tty)
- keeps `vm_test.go`'s insufficient-memory / panic-recovery as targeted negative cases (they
  need a deliberately broken VM, so they stay separate but still go through the daemon)

### W6 — "Mesh replication" two-daemon sequential test
Start daemon A and daemon B (two real processes, local ports). Create+snap on A via SSH; from B
run `ts who-has`/`ts download-snap` against A; ssh into a B frame built from the downloaded snap
and verify contents. Covers mesh + content-addressed download for real.
Supersedes / absorbs:
- `mesh_test.go`: all 4
- `error_test.go`: TestErrorWhoHasNonexistent (negative who-has on the live mesh)
- (streaming/range behaviors below)

### W7 — "Errors & protocol" negative cases against the live daemon
With one real daemon: connection-refused (don't start it), unknown/invalid snapshot id,
delete-nonexistent-frame, snap-nonexistent-frame, corrupted snapshot metadata on disk, symlink
loop, and the streaming/NDJSON/range-batch progress behaviors observed on a real long snap over
SSH.
Supersedes / absorbs:
- `error_test.go`: all 8 subtests (incl. the lone real one, testConnectionRefused)
- `streaming_test.go`: all 5 (progress is observed incrementally on a real `ts snap`)
- `nested_test.go`: the cgroup/namespace probes become assertions inside a W4/W5 session

### Keep as unit tests (do NOT fold into SSH workflows)
Pure format/algorithm checks belong next to their packages, not in `e2e/`:
`tsm_test.go` (TSC sort order, chunk-ref resolution, metadata encoding),
`refs_test.go`/`refs_edge_cases_test.go` (name validation, path traversal, reflog edge cases),
`snaphash`/`frameid` encoders, `fixtures_test.go`. These are fast, deterministic, and don't
benefit from a daemon — but they should stop carrying the `e2e`/`…E2E` label.

### Net effect
~43 scattered tests (≈30 of them just re-running the same fake-daemon setup) collapse to **~7
sequential SSH workflows + a handful of targeted negatives + a thin layer of genuine unit
tests.** Fewer daemon boots, dramatically more of the real product exercised, and the
front-door bugs that currently slip through become catchable.
