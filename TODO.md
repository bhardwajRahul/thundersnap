# TODO — tracked fixes and improvements

Items here were surfaced by review (notably the gpt-5.5 + deepseek-v4-pro
cross-family review of the aggregated changes since `main@origin`) and
deliberately deferred. Cross them off as they land.

## Crash recovery / ops

- [ ] **Startup cleanup of orphaned `.tmp` snap artifacts.** If `thundersnapd`
  crashes during background indexing, the captured btrfs subvolume
  (`snaps/<jobid>-<kind>.tmp`) and its `.tmp.stamp` / `.tmp.tsm` / `.tmp.tsc`
  sidecars are left on disk. `main.go` skips `.tmp` entries when listing snaps,
  so they don't appear in `ts snaps` — but they occupy disk and are never
  reclaimed except by hand. `snapqueue.go` has `cleanupTmp` for error paths and
  `processJob` cleans un-indexed components on error, but there is no startup
  recovery pass. Fix: in `initSnapQueue` (or `main` after `--data-dir` is
  resolved) scan `snaps/` for `*.tmp` artifacts and remove them — `.tmp`
  subvols are by definition incomplete/un-indexed, so they are always safe to
  delete. (deepseek-v4-pro review, MEDIUM.)

## Temporary-file lifecycle

- [ ] **Automatically age out files under `/tmp`.** Add tmpreaper-style
      retention (or the equivalent systemd-tmpfiles policy) so MCP job logs and
      other abandoned temporary files do not accumulate indefinitely.
- [ ] **Consider clearing `/tmp` when container-init exits.** Decide whether a
      container namespace lifetime is the right ownership boundary, taking care
      not to delete files still used by concurrent sessions in the same frame.
- [ ] **Consider mounting `/tmp` as tmpfs.** Evaluate memory limits, large MCP
      job output, concurrency, and whether temporary data should survive a
      container-init restart before changing the backing store.
- [ ] **Exclude `/tmp` from the content-addressable snap index.** Even while
      btrfs snapshots still include `/tmp`, `ts snap` must not hash or publish
      it as indexed snapshot content, so temporary MCP logs and similar files
      are not replicated through the content-addressable store.

## `ts go` sessions / docker-image hygiene

Surfaced while building the `nix` ref (see `README.nix.md` footguns #18–#20).
The first three compound: the password-expiry hang makes an agent
`timeout`-kill the `ts go -c`, which then wedges the frame (the fourth
item is the isolation concern that prompted the investigation).

- [ ] **Fresh docker images can ship `user` with password expiry enforced,
      hanging every non-interactive `ts go -c` on the frame.** `debian:latest`
      (trixie) sets `user`'s shadow entry to "password must be changed"; `ts
      go -c` runs `su - user -c`, whose PAM prints "You are required to change
      your password immediately" and blocks reading a password — so
      `ts go <ref> -c '...'` never returns. SSH-as-root
      (`ssh root@<ref>@thundersnap`) is unaffected (no `su - user`).
      `tsm.EnsureSudoers` (`tsm/stripuids.go:189`, called from
      `cmd/thundersnapd/rootfs.go:282`) already writes a NOPASSWD sudoers
      drop-in for `user` at frame creation; add a sibling that also makes
      `user` non-interactively loginable — clear shadow password ageing
      (`chage -d today -m 0 -M 99999 -I -1 -E -1 user && passwd -d user`) — at
      least when `/etc/shadow` shows expiry. Mind the existing skip:
      `EnsureSudoers` returns nil when `/etc/sudoers.d` doesn't exist (no sudo
      installed, e.g. alpine), so the hygiene pass must either run after sudo
      is installed or be done over SSH in the runbook. (README.nix.md #18.)

- [ ] **`ts go -c` never sends a stdin EOF, so a stdin-reading child blocks
      forever.** `runVsockSession` (`cmd/ts/main.go`) skips stdin forwarding
      for `-c` ("Skip stdin forwarding when running a command") but never
      sends the empty `FrameStdin` that `servePipe` (`vshdsession/vshdsession.go`)
      treats as the EOF marker. So the child's stdin pipe stays open and
      unwritten for the whole session. A child that reads stdin — e.g.
      `su - user`'s PAM password prompt from the item above — blocks on that
      read indefinitely instead of getting EOF and erroring out. This is the
      *mechanism* of the password-expiry hang (and would hang any `ts go -c
      'cat'`-style command too). Fix: for `-c`, send an empty `FrameStdin`
      (EOF) right after the session starts, mirroring `ssh -T cmd` which
      closes stdin when there's no input to forward. (The non-`-c` path
      already gets EOF via `os.Stdin.Read` in its stdin-forward goroutine;
      `-c` never enters that goroutine.)

- [ ] **Killing a `ts go -c` mid-session wedges the frame (stale session
      count, no reap, no recovery CLI).** If a `ts go <ref> -c '...'`
      frontend is killed (agent `timeout`, SIGKILL) while the session runs,
      `ts frames` can keep a non-`stopped` session count for that frame with
      no live `session-serve` process to reap it; the frame then refuses all
      new `ts go -c` sessions (they hang on attach). `servePipe` does
      SIGHUP-then-SIGKILL the child when its *reader* errors
      (`killChildOnDisconnect`), but a killed *frontend* doesn't promptly
      close the server-side connection (the daemon's proxy can keep
      session-serve alive), so the daemon's session bookkeeping never
      decrements. There is no `ts frame --stop <ref>` / `ts session kill` CLI
      to force-clear it. (`ts frame -d` is fixed in source — the server
      accepts UUID or `frame_name`, `cmd/thundersnapd/main.go:2567` — so a
      wedged frame *can* be deleted once the running daemon is current; my
      failure was a stale PID-1 daemon predating that fix, not a regression.
      But deletion is a sledgehammer that loses the frame.) Fix: the daemon
      should reap sessions whose frontend has gone (watch the proxy
      connection / the session-serve reader EOF) and decrement the count; and
      add `ts frame --stop` / `ts session kill` for a manual unwedge without
      destroying the frame. Workaround today: abandon the frame and rebuild
      from the snap (`ts frame --ref <new> <snap>:nil:nil`). (README.nix.md
      #19; compounds with the two items above — the password-expiry +
      stdin-EOF hang is what prompts the kill.)

- [ ] **`ts go -c` tty isolation: `Setsid` the pipe-mode child so it can
      never reach a controlling tty (ssh-`-T` parity).** The caller's tty is
      *not* inherited as stdio: `ts go -c` sends `ptyFlag="0"`
      (`cmd/ts/main.go`, `isTTY = term.IsTerminal(...) && len(cmdArgs)==0`)
      and `servePipe` sets the child's stdin/stdout/stderr to the vshdproto
      pipe, not the caller's tty — so that part is already ssh-like. But
      `servePipe` (`vshdsession/vshdsession.go`) does **not** `Setsid` the
      child, unlike `servePTY` (which via `pty.Start` runs the child in a new
      session with the fresh pty as controlling tty). So the pipe-mode child
      shares `session-serve`'s session and could `open("/dev/tty")` to reach a
      controlling terminal if `session-serve` had one. In current launch
      paths `session-serve` has no controlling tty (vshd is a daemon), so
      `/dev/tty` opens fail — but that's implicit, not enforced. Desired:
      ssh-like isolation, where the pipe-mode child runs in its own session
      with no controlling tty (exactly what `ssh -T` does). Fix: set
      `cmd.SysProcAttr{Setsid: true}` (no Ctty) in `servePipe`, and add a
      regression test (`ts go <ref> -c '...'` asserting the child has no
      controlling tty and its fd 0/1/2 are not the caller's tty). (Surfaced
      building the nix ref; related to README.nix.md #18.)

## Test coverage gaps (real-e2e)

- [x] **Mesh `download-snap` between two daemons now has a real-e2e test.**
      Covered by `e2e/mesh_test.go::testMeshWhoHasAndDownload` (registered as
      `TestContainer/MeshWhoHasAndDownload`): it starts two independent
      `--test-http-listen` daemons, wires them as mesh peers via the real
      `/ts/ping`/`recordPeer` path, snaps on A over SSH, runs `ts who-has` and
      `ts download-snap <triplet>` on B through the production
      `handleWhoHas`/`handleDownloadSnap`/`doDownloadSnap`/
      `tsm.CheckPeersForSnapshot`/`bupdateFileServer`, then builds a frame from
      the downloaded triplet and verifies root+home markers survive. No
      product change was needed: `buildTestHTTPMux` already mounts `/ts/ping`
      and sets `globalMeshState`, so peers can be seeded in test mode. The
      deleted `TestE2EDownloadSnap` was the only prior two-daemon download
      test; the `not_e2e/mesh_test.go`/`streaming_test.go` fakes that
      replaced it were themselves deleted (they reimplemented the protocol
      and tested the test's own copy — false green).
- [ ] **`/home` and `/work` ownership not asserted by any real-e2e test.**
      `e2e/fidelity_test.go` now asserts a chown'd file's uid/gid survive a
      snap+fork, but does not assert the daemon-created `/home` and `/work`
      subvolumes are owned by `tsm.ThundersnapUID` (7575). Add a `stat -c
      %u:%g /home` and `/work` check after `ts frame`. (Both reviews, MEDIUM.)
- [x] **Session PTY visibility in container not asserted.** Covered by
      `e2e/container_test.go` `TestContainerConcurrentDistinctPTS`, which opens
      two concurrent PTY SSH sessions, runs `tty` in each, and asserts they get
      distinct `/dev/pts<N>` devices. (deepseek-v4-pro review, MEDIUM.)
- [ ] **`TestForkUndoRollsBackToForkPoint` reproduces `ts undo`'s effect by
      parsing `ts log` instead of driving `ts undo -c`.** `TestTsUndo` shows the
      `-c` infrastructure works; this test could run
      `ts undo -c 'read line < /marker && echo $line'` on the forked frame and
      assert the marker is `v2` — a stronger test of the actual fork-point fix.
      (deepseek-v4-pro review, LOW.)
- [ ] **Nested-cgroup and nested-namespace probes were dropped, not ported.**
      `not_e2e/nested_test.go` had `TestCgroupMultiLevelSubtreeControl` (a
      dedicated regression guard that intermediate `cgroup.subtree_control`
      writes succeed so leaf `memory.high`/`pids.max`/`cpu.weight` work),
      `TestNestedThundersnapCgroup`, and `TestNestedThundersnapNamespaceIsolation`
      (`unshare --pid --fork` inside a container). The replacements cited in
      the commit (`e2e/nested_test.go` TestNestedThundersnap, TestContainer-
      NamespaceSetup) assert SSH connectivity and single-level isolation, not
      multi-level cgroup controller propagation or nested namespace creation.
      `cgroup/cgroup_test.go` has no subtree_control test either. Port these as
      SSH-driven nested tests against a real daemon, or as package tests on the
      cgroup manager. Needs a writable cgroup v2 hierarchy (read-only in this
      dev container). (Both reviews, MEDIUM.)
- [x] **`handleWhoHas` has test coverage.** Covered by
      `e2e/mesh_test.go::testMeshWhoHasAndDownload`, which drives the real
      handler over SSH for both the happy path (who-has finds the peer that
      has the snap) and the empty/error path (who-has for a valid but
      nonexistent snap returns "No peers" and non-zero exit). The deleted
      `not_e2e` fakes reimplemented the protocol and never exercised the real
      handler; this test replaces them against the production handler.
- [ ] **`list-snaps` robustness against a partially-corrupt `snaps/` dir is
      untested.** The deleted `TestCorruptedSnapshotMetadata` planted a
      non-subvol entry among the snapshots and verified `ts snaps` skipped it
      and continued. No replacement asserts this; a stray file in `snaps/`
      could make listing error out. Add an e2e/package test that drops a plain
      file in the snaps dir and checks `ts snaps` still lists the real snaps.
      (deepseek-v4-pro review, MEDIUM.)
- [ ] **Setuid binaries are only checked for the *bit*, not that they execute.**
      `e2e/fidelity_test.go` asserts `test -u` (the setuid bit is set) after a
      snap+fork, but the deleted `TestSetuidBinaryExecution` verified the
      setuid bit is *functional* (the kernel honors it). A minimal-rootfs
      execution-as-non-root check is awkward (busybox + a setuid wrapper); at
      minimum add `test -x` and ideally a non-root run that observes the euid
      change. (deepseek-v4-pro review, MEDIUM.)

## not_e2e teardown (remaining)

The not_e2e suite has been largely emptied into the real e2e harness (see
`not_e2e/not-e2e-enough.md` for the plan). The remaining not_e2e files are all
VM tests that hand-spawn cloud-hypervisor and need a working KVM environment
to port onto the daemon-driven `vm/` SSH harness:

- [ ] **Port the deep VM tests to e2e.** `vm_test.go` (launch, networking,
      virtiofs, vshd comm, concurrent sessions, graceful shutdown, panic
      recovery, insufficient memory, user switching, process isolation),
      `vmx_test.go` (basic/concurrent/container-isolation/outer-shell/shared-
      VM), `minimal_shell_test.go` (shell features), `vshd_devpts_test.go`
      (devpts), and the VM helpers in `e2e_test.go`/`vshd_proto_test.go`. The
      daemon-driven VM SSH path is already covered by `e2e/vm_test.go`
      (TestVMSSHSessionMatrix, TestVMXPtyWinsize) and `TestVMNamespaceSetup`;
      the deep tests should go through `vm/` sessions against a real
      `thundersnapd` instead of hand-spawning cloud-hypervisor. Per
      not-e2e-enough.md W5, keep panic-recovery / insufficient-memory as
      targeted negatives that still go through the daemon.

## SFTP / scp

- [ ] **`sftpfs` Setstat ignores mtime/atime.** `Filecmd`'s `Setstat` handles
      only `Size` and `Permissions`, so SFTP `Chtimes` (and `scp -p`, which
      preserves mtimes) is a silent no-op. Honor `AttrFlags().Acmodtime` with
      `os.Chtimes`. Small, correct, standard-SFTP behavior. (Surfaced while
      writing `TestCrossInstanceSnapDeterminism`, which pins mtimes via busybox
      `touch` as a workaround.)
- [ ] **`parseSnapProgress` hardcodes 3 components** (root/home/work) and may
      mis-handle `nil:nil:nil` frames whose `ts snap` progress only has a root
      component. Make it handle 1–3 components. (deepseek-v4-pro review, NIT.)

## Internal subcommands / CLI hardening

- [ ] **Internal commands should live under `ts internal <cmd>` with purely
      positional, mandatory args and no flag parser.** The hidden in-binary
      commands that thundersnapd/vshd reexec into (`ts su`, `ts session-serve`,
      `ts drop-caps-and-run`, `ts join-and-run`, `ts container-init`,
      `ts nsenter`, `ts check-dev`, `ts check-isolation`) are today dispatched
      as top-level subcommands alongside the user-facing ones, and several
      hand-parse their args. The motivating case is `ts su` (`cmd/ts/su.go`,
      `runAsSu`): it scans `argv` for `-c`/`-s`/`-m` flags before treating the
      first non-flag element as the username, so a username starting with `-`
      (e.g. `su - -c '<cmd>'`) would be parsed as a flag and silently run the
      command as root. This is currently guarded only upstream — vshd's
      `validateTargetUser` rejects names starting with `-` before they reach
      `su` — so the `ts su` entry point itself is unsafe if anything ever
      calls it directly (including the `/bin/su -> ts` symlink in a frame).
      Move all of these under a `ts internal <cmd> <arg1> <arg2> ...` dispatcher
      that takes fixed-arity positional args with no getopt/flag parsing at
      all, so a value that happens to start with `-` is never reinterpreted as
      an option. This makes parse errors impossible by construction and lets
      each internal command assert its exact arg count up front. Surfaced by
      the `ts go <user>@<frame>` work (which added the vshd-side validation);
      the deeper fix is to not rely on validation alone.
