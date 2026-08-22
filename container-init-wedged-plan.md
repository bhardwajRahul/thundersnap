# Plan: container-init wedge under MCP load on Linux-VM-on-macOS

## Symptoms (from the field)

- An Aperture/LLM agent launches jobs via the MCP server. The MCP server runs
  on a Linux VM hosted on macOS. The targeted frame **wedges almost every time**.
- The wedge is **frame-specific**: you can still enter a *different* frame; the
  daemon as a whole is fine.
- On the wedged frame, **new sessions hang**: SSH answers with the greeting, then
  nothing; MCP reports the job launched but it never finishes.
- **Having an existing SSH session open to the frame when MCP starts calling in
  prevents the wedge.** (Strong environmental clue.)
- While that SSH session is open, **defunct/zombie processes accumulate** from
  some MCP sessions; container-init (PID 1 of the container PID namespace) is
  not reaping them.
- **Closing the last SSH session** ("exit" the shell) wedges that SSH session
  itself, and from then on every new session to the frame also wedges
  (permanent).
- `ts refs` (sibling frame) shows the wedged frame with **session count >= 1**
  (first column) — a session there is stuck holding the refcount up.

## Architecture recap (who owns what)

```
daemon (thundersnapd, in the VM)
├── controlServers (per-rootFS refcount; MCP + SSH both acquire/release)
│     └── controlServer  : HTTP-over-unix-socket at <rootFS>/id/thunder.sock
│         (on the VMX frame this socket lives on the virtiofs mount!)
├── host vshd (unix socket)  ◀── MCP jobs always go through this
│     ├── containerNs.Manager (per-rootFS refcount of container-init)
│     │     └── spawns "ts container-init" (PID 1 of container PID+mnt+uts+cgroup ns)
│     └── per session: ts nsenter -t <initPid> ... → ts join-and-run → ts session-serve
│                     (session-serve is the TLV endpoint; vshd just splices bytes)
└── SSH path for plain-container frames also goes through host vshd
    (so SSH + MCP SHARE host-vshd's containerNs for the same frame rootfs)
```

Key facts established by reading the code:

1. **MCP jobs are container sessions.** `mcpBashToolHandler` dials host vshd and
   writes a VMX request with `rootPrefix = absRootFS`. Host vshd's
   `buildSessionCmd` calls `containerNs.GetOrCreate(rootFS, ...)` and runs
   `ts nsenter -t <initPid> ... ts session-serve`. So every MCP job is exactly
   like an SSH container session, just non-PTY and driven by `collectMCPJob`.

2. **Two independent per-frame refcounts** gate teardown:
   - `controlServers` (in the daemon): MCP **and** SSH both get/release it.
   - `containerNs` (in host vshd): only sessions (MCP or SSH) get/release it.

3. **The only `wait4(-1)` reaper in the whole tree is in container-init**
   (`cmd/ts/container_setup.go`, `cmdContainerInit`). Neither vshd nor the
   daemon competing-reap with `os/exec`, so host vshd's `cmd.Wait()` of
   container-init is safe from reap-racing.

4. `stopEntry` (the container-init teardown) is **bounded**: it closes
   container-init's stdin, waits up to `initShutdownTimeout` (10s), then
   SIGKILLs and reaps, then `RemoveContainer` (best-effort rmdir, no retry).
   So stopEntry itself **cannot hang forever** — at worst ~10s.

5. The control-server `Close()` is bounded by `controlServerCloseTimeout`
   (2s, recently added). So it also cannot hang forever.

## Why "existing SSH session prevents the wedge" is the decisive clue

Both refcounts (#2) hit zero **only when no session at all is attached to the
frame.** With an SSH session open:

- `controlServers[rootFS].refCount >= 1` for the whole time → the control
  server is **never torn down and recreated** while MCP jobs churn.
- `containerNs[rootFS].refCount >= 1` for the whole time → container-init is
  **never torn down and recreated** while MCP jobs churn.

So the wedge lives in **one of the two teardown/recreate paths** (control server
or container-init), OR in something those paths disturb (the virtiofs unix
socket, the cgroup, the mount namespace). The "prevented by an open session"
pattern is the signature of a **teardown/recreate race or residue**.

## The two failure modes are probably TWO consequences of ONE root cause

- *Before* the last session exits: zombies accumulate (container-init's reaper
  is not reaping). New sessions still connect (refcount just increments).
- *After* the last session exits: permanent wedge (new GetOrCreate never
  succeeds for that rootfs).

If container-init's **reaper/signal handling is wedged** (its single goroutine
stuck, or SIGCHLD lost), BOTH follow:
  - zombies not reaped (the reaper isn't draining them),
  - on the final `Release`, `stopEntry` closes stdin; container-init's
    `case <-stdinClosed` can't fire (its select loop is wedged) → it never
    `os.Exit`s → `stopEntry`'s 10s kill fires → container-init is SIGKILLed →
    a NEW container-init must start for the next session → **if creation then
    also wedges** (see candidate C), the frame is permanently stuck.

So the leading theory is a **wedge in container-init itself** plus a
**re-creation failure** that makes it permanent.

## Candidate root causes (ranked)

### A. Reaper signal-delivery race (FRAGILE pattern, HIGH suspicion)

Current reaper (`container_setup.go`):

```go
sigchld := make(chan os.Signal, 16)
signal.Notify(sigchld, syscall.SIGCHLD)
...
for {
  select {
  case <-sigchld:
    for { pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil); ... { } }
  case <-sigterm: os.Exit(0)
  case <-stdinClosed: os.Exit(0)
  }
}
```

Problems:
- **Buffered channel of 16.** Standard signals (SIGCHLD is not realtime)
  coalesce, and Go's `signal.Notify` **drops** a signal when the channel is
  full and no goroutine is currently blocked receiving. Under bursty MCP load
  (many grandchildren exiting at once) the 16-slot buffer can overflow exactly
  when the reaper goroutine is mid-`Wait4` loop. A dropped SIGCHLD is normally
  recovered by the *next* exit's SIGCHLD — but if exits become rare after the
  burst, zombies stick around until the next exit, and **if the burst was the
  last activity, they stick forever**.
- **Reaper shares the main loop with shutdown.** Not a wedge per se, but it
  means any future blocking call in the reap branch delays shutdown.
- The pattern is the textbook-fragile Go PID-1 reaper. The robust form used by
  real container inits (tini, containerd) is a **dedicated reaper goroutine**
  plus a **periodic safety-net reap** so a missed SIGCHLD can never strand a
  zombie.

This matches "zombies not reaped by container-init, suggesting it's stuck."

### B. `os.Stdin.Read` poller / virtiofs interaction

The shutdown path depends on a goroutine `os.Stdin.Read` returning EOF when
container-init's stdin pipe is closed by `stopEntry`. On virtiofs-hosted
frames the fd is a pipe (not virtiofs), so this *should* be fine — but if the
Go poller for fd 0 ever wedges (VM/ARM64 timing), `stdinClosed` never fires
and container-init won't self-exit. Then `stopEntry`'s 10s SIGKILL is the only
exit. This is **bounded** (10s) so it explains stalls, not the *permanent*
wedge — unless combined with C.

### C. Re-creation of container-init fails for the wedged rootfs (the PERMANENT wedge)

After stopEntry kills container-init, the next `GetOrCreate` spawns a fresh
`ts container-init` and waits up to **10s for "READY"**. If setup never prints
READY, `GetOrCreate` returns an error and **the frame has no entry**; the next
session tries again, fails again, … → looks permanent (each attempt = 10s
timeout). Candidates for why startup wedges:

- **Cgroup residue.** `cgroupName` is derived purely from `absRootFS`
  (`ContainerName`), so every incarnation reuses the *same* cgroup dir.
  `stopEntry` calls `RemoveContainer` (best-effort `rmdir` walk). If a
  **detached/nested process survived PID-1 shutdown** (e.g. a setsid job, or a
  nested-thundersnap child living under this cgroup in a sub-hierarchy), the
  cgroup `rmdir` returns EBUSY, the dir persists with stale limits/processes,
  and the next `PrepareContainer` reuses it. A stale cgroup with
  `pids.max`/`memory.oom.group`/leftover procs can make the new container-init
  fail to start or hang in setup.
- **Mount/residue on virtiofs.** setup does `Mount(chrootPath, chrootPath,
  MS_BIND)`, `pivot_root`, mounts proc/sys/cgroup2, `setupDev`. If any of these
  wedges on the VM's virtiofs/btrfs combo (slow or EBUSY-on-rebind), READY is
  never printed in 10s.
- **Stale `thunder.sock`** from the control server (separate path, but the
  same "residue on virtiofs" theme): rapid get/close of a unix socket on
  virtiofs can leave the socket file or listener half-released; the next
  `net.Listen("unix", …)` blocks or errors. (The control-server fix already
  adds a 2s bounded close, but residue could still break the *next* bind.)

### D. containerns refcount/`stopping` race (LOW — mostly already fixed)

`GetOrCreate`'s "init died" branch sets `e.stopping` and reaps without holding
`m.mu`; concurrent `GetOrCreate`/`Release` observers wait on `e.stopped` then
retry. This was the prior fix (`mykuptuo`). One residual: if the init is found
dead while a session is still using it, that session's later `Release` may
decrement a **different** (recreated) entry's refcount. Unlikely to be the
primary wedge (requires init to die mid-session) but worth a guard.

## What "locks" are involved / why wait() might not reap

- There is **no explicit mutex inside container-init the process.** The only
  lock-like thing is the **single goroutine select loop**: if something makes
  that goroutine not reach `select` (a blocking reap call) or not receive
  SIGCHLD (a dropped signal), reaping and shutdown both stall. That is the
  "lock" the symptoms describe.
- `Wait4(-1, WNOHANG, nil)` never blocks (WNOHANG), so the loop can't wedge *in*
  the reap syscall — **unless** a future change makes it blocking, or EINTR is
  mishandled. The realistic failure is the **signal not arriving**, not Wait4
  blocking.
- In the **manager** (host vshd), the lock is `m.mu`; the prior fix already
  keeps it unheld during `stopEntry`/`Wait`. So a wedged init can't freeze the
  manager for other frames.

## Reproduction strategy

The field is slow (ARM64 thunderboot VM) and bursty (LLM agent). To reproduce
on a fast box we must **widen the teardown window** and **hammer the
teardown/recreate path while checking zombies**. The existing WIP change
(`pxlxouzmlwow`) already adds `TS_TEST_STOP_ENTRY_DELAY` to widen stopEntry;
build on that.

Repro test (e2e, `TestContainerInitReapUnderChurn`):
1. Create a frame; install busybox `sleep`, `ps`, `setsid`.
2. In a tight loop (N iterations, no lingering SSH session, so refcount hits 0
   each time): launch an MCP job that **backgrounds a trapped child**
   (`(trap '' TERM; sleep 5) &`) then exits. This strands a grandchild that
   reparents to container-init, and (if not reaped) stays as a zombie, and
   (because it traps TERM) stretches the session teardown into the
   SIGHUP→SIGKILL escalation window.
3. After the loop, open an SSH session and run `ps`; assert **no `Z`
   (defunct/zombie) processes**. If zombies exist → reaper is wedged (candidate A).
4. Concurrently with the loop, probe a *sibling* frame's SSH with a short
   deadline to assert no global stall, and probe the **same frame**'s SSH to
   detect the permanent wedge (candidate C): after the loop, a fresh SSH
   `echo hi` must succeed within e.g. 3s.
5. Toggle `TS_TEST_STOP_ENTRY_DELAY` to widen the window on fast machines.

A focused **unit test** in `containerns` is harder (needs real namespaces +
cgroups), but a smaller test can at least exercise `Manager.GetOrCreate`/`Release
` churn with a fake init that "hangs on shutdown" to pin the `stopping`/`stopped
` coordination and the bounded kill.

## Proposed fixes

1. **Robust container-init reaper** (addresses A; the highest-leverage change):
   - Dedicated reaper goroutine (decoupled from the shutdown select).
   - **Periodic safety-net reap** (e.g. every 1s) so a dropped/lost SIGCHLD can
     never strand a zombie. `Wait4(-1, WNOHANG)` loop, treating `ECHILD` as
     "nothing to reap" (not an error to abort on).
   - Larger signal channel buffer (or unbounded via the runtime) to avoid
     drops under bursty load.
   - Shutdown select stays on `sigterm`/`stdinClosed` only.
   This is strictly safer and is the same shape real container inits use. It
   does not change any external behavior.

2. **Defensive cgroup reuse** (addresses C's cgroup-residue angle):
   - `stopEntry`: before `RemoveContainer`, write `cgroup.kill` for the
     container root (kills any detached leftover) and briefly wait for the
     cgroup to drain, so a stale cgroup doesn't survive into the next
     incarnation.
   - `PrepareContainer`/`DelegateContainer`: when reusing an existing dir,
     reset the dangerous settings (`cgroup.kill`, then clear procs) so a
     leftover cgroup can't poison the new container-init.

3. **Keep/extend the bounded control-server teardown** already landed
   (2s close, lock released before Close). Add a retry/unlink-safety around the
   next `net.Listen` so a stale socket file on virtiofs doesn't break the next
   bind.

4. **Guard the manager refcount against the dead-init-during-session race**
   (addresses D): `Release` should no-op only when the entry it holds is the
   current one; otherwise it must not decrement a recreated entry.

5. Leave the `TS_TEST_STOP_ENTRY_DELAY` test knob in (from the WIP change) —
   it's test-only and genuinely useful for widening the window.

## Verification

- `gofmt -w .`
- `make test`
- `make e2e` (must include the new churn/reap test and the existing
  `TestMCPTimeoutAndReap` / `testContainerCrashLifecycle`).
- `make not_e2e`

## Open questions for the morning

- Is the wedged frame a **plain container** (host vshd, SSH+MCP share
  containerNs) or a **VMX frame** (SSH→VM vshd, MCP→host vshd, *different*
  containerNs)? This determines which refcount the "open SSH" clue is about.
  The fix above covers both, but the repro test should match production.
- Are the zombies `Z` (exited, unreaped) or `<defunct>` detached *live* procs
  holding the cgroup? `ps -o pid,ppid,stat,cmd` in-frame will tell us which
  candidate (A vs C) dominates.
