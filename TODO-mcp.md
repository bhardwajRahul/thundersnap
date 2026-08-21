# Thundersnap Sandbox MCP — TODO

Working task list for implementing `sandbox-mcp-design.md`. Check off items
as they're completed; re-read this file at the start of every turn
(especially after a context compaction). Order is roughly
prerequisites → core → tools → MCP surface → e2e → future.

Status legend: `[ ]` pending · `[~]` in progress · `[x]` done · `[!]` blocked

---

## Phase 0 — Prerequisites & plumbing

- [~] **Add MCP SDK dependency.** Add `github.com/modelcontextprotocol/go-sdk/mcp`
  to `go.mod` (aperture pins v1.5.0; match or newer). `make test` must stay green.
  DONE: `go get ...@v1.5.0` + `go mod tidy`; `make binaries` builds.
- [~] **Refactor `runTestMode` to serve the HTTP mux.** Today it starts only
  SSH on a local TCP port; the entire port-7575 HTTP mux (mesh, `/bupdate/`,
  metrics, and now MCP) is never instantiated in tests. Fix this pre-existing
  coverage gap: build the same `httpMux` and serve it on a local port in test
  mode. This is a prerequisite for every MCP e2e test and independently gives
  the existing-but-untested HTTP handlers coverage.
  IN PROGRESS: `mountMCP(mux)` helper added in `mcp.go`; wiring into both
  production `httpMux` and a test-mode mux next.
- [~] **Confirm `make test` green** before starting Phase 1; record a baseline.
  NOTE: `tsm.TestIncrementalDifferentSourceDirs` is a pre-existing test-
  isolation flake (passes in isolation, fails under full `make test`); unrelated
  to MCP. The MCP package is separate and not affected.

## Phase 1 — Exec primitive (the one thing everything else is built on)

- [x] **Port `CollectExec`** (from aperture `chat/sandbox/exec.go`) to read
  vshdproto frames (`FrameStdout`/`FrameStderr`/`FrameExit`) instead of
  `Backend.Exec` events. Keep the 1 MiB cap, the UTF-8-rune-boundary trim at
  the cap, the `cancel()`-on-truncation, and the final
  `strings.ToValidUTF8` cleanup. Output: a `thundersnap`-package
  `ExecResult{Output, ExitCode}`.
  DONE: `thundersnap/mcpexec/collect.go` — `CollectFrames(r io.Reader)` +
  `ExecResult{Output, ExitCode, Truncated}` + `FormatExit`. 10 unit tests in
  `collect_test.go` all pass (exit codes, stdout/stderr interleaving, cap +
  marker, rune-boundary trim, split-rune cleanup, EOF-after-exit, empty
  stream, post-cap drop). Daemon-agnostic (no thundersnapd import).
- [x] **Write the vshd one-shot launcher.** Dial host vshd, send the VMX
  header (`framePath`, user, `pty=0`, argc=1, arg=`sh -c <cmd>`), no
  `FrameWinsize`, read frames via the ported collector. Expose as a
  function taking `(ctx, frame, workdir, command) (*ExecResult, error)`.
  DONE: `runInFrame` in `cmd/thundersnapd/mcp.go`. Resolves frame via
  `resolveFrameRootFS`, prepares rootfs, anchors control server, dials
  `hostVshd.ensure()`, sends non-PTY one-shot via `writeVshdRequest`, collects
  via `mcpexec.CollectFrames`. ctx-deadline closes the vshd socket to reap
  the process group (T2 validates this). `// TODO: persistent shell per MCP
  session` left at the fresh-sh-per-call site.
- [x] **SPIKE (e2e task #2): cancellation reaps the process group.** Confirm
  that closing the vshd socket mid-stream on a non-PTY one-shot reaps the
  whole PG, including a backgrounded child (`sleep 7200 &`). This is the one
  assumption not already proven by the SSH path (SSH always reaches a clean
  `FrameExit` or PTY close). If reap is unreliable, fix before relying on
  the timeout/cap behaviour. `[!]` blocks Phase 4 timeout/cap tests until
  resolved.
  RESOLVED (T2): the spike found reap was unreliable and FIXED it in
  `vshdsession` (Setpgid + group kill on disconnect). See Phase 4 T2 for
  details. `TestMCPTimeoutAndReap` pins the contract.

## Phase 2 — Tools (copy-and-paste from aperture, no module dep)

- [x] **Copy the `Tool`→MCP adapter** (`chatToolToMCPHandler`, ~12 lines) or
  build `*mcp.Tool`+handlers directly. Decide once; use everywhere.
  DONE: built handlers directly. `textResult(text, err)` in `mcp.go` is the
  adapter: nil err → normal result; non-nil err → `IsError: true` result with
  the error text in Content (so the LLM sees it and self-corrects). Matches
  aperture's `chatToolToMCPHandler` split.
- [x] **`thundersnap_bash`** — copy `tool_bash.go`; drop `(instanceID,
  convID)` plumbing; add `frame` (optional, "" = default auto-create) and
  `workdir` (optional, default `/work`); raise ceiling to 7200 s, default
  600 s; clamp `timeout > 7200` to 7200. State both numbers in the
  description.
  DONE: `mcpBashToolHandler`. Default 600s, max 7200s, clamped. Description
  states both.
- [x] **`thundersnap_view`** — copy `tool_view.go` verbatim
  (`buildViewCommand`, `truncateUTF8`, 16 KB cap, 30 s timeout); add
  optional `frame`.
  DONE: `mcpViewToolHandler` + `buildViewCommand` + `truncateUTF8`. 16KB cap,
  30s timeout.
- [x] **`thundersnap_create_file`** — copy `tool_create_file.go` verbatim
  (`buildCreateFileCommand`, base64+heredoc, 30 s timeout); add optional
  `frame`.
  DONE: `mcpCreateFileToolHandler` + `buildCreateFileCommand`. 30s timeout.
- [x] **`thundersnap_str_replace`** — copy `tool_str_replace.go` verbatim
  (`strReplaceScript` w/ surrogateescape, `buildStrReplaceCommand`, 30 s
  timeout); add optional `frame`.
  DONE — with a deliberate, documented deviation: implemented HOST-SIDE in
  `mcpStrReplaceToolHandler` instead of piping Aperture's embedded Python
  program through a heredoc. Thundersnap's daemon has direct rootfs access
  (a local btrfs subvolume), so the read/count/replace/write is done in-Go;
  this is binary-safe (raw []byte, so non-UTF-8 files survive byte-for-byte —
  the same property Aperture's surrogateescape buys) and works in EVERY frame,
  including nil:nil:nil frames that ship no python3 (and no /lib64 for a
  dynamic one). `strReplaceScript`/`buildStrReplaceCommand` were removed as
  dead code. `isWithinRootFS` guards the host-side path against traversal
  (a tool path like /../../etc/shadow stays in-frame). 30 s timeout. The
  contract is unchanged: error on 0 or >1 occurrences, replace exactly once,
  preserve the file's existing mode/owner.
- [x] **`thundersnap_list_frames`** — reuse `handleListFrames` logic
  (enumerate user's frame store, annotate with bound ref name or UUID,
  report active-session count). No container launched. Returns
  `{ "frames": [{ "name", "status" }] }`.
  DONE: `mcpListFramesToolHandler`. Reads `userFrameStore`/`userRefStore`
  directly; returns `{frames:[{name,status}]}`.
- [x] **`thundersnap_list_refs`** — reuse `handleListRefs` logic
  (enumerate user's ref store). No container launched. Returns
  `{ "refs": [{ "name", "uuid", "autorun" }] }`.
  DONE: `mcpListRefsToolHandler`. Returns `{refs:[{name,uuid,autorun}]}`.
- [x] **Source note:** leave a `// TODO: persistent shell per MCP session`
  comment at the fresh-`sh -c`-per-call site (matches aperture; deferred).
  DONE: comment on `runInFrame`.

## Phase 3 — MCP server surface

- [x] **Mount `/v1/mcp`** on the existing port-7575 `httpMux` (alongside
  `/ts/ping`, `/bupdate/`, metrics) using `mcp.NewServer` +
  `mcp.NewStreamableHTTPHandler`. Register the six tools from Phase 2
  (prefixed `thundersnap_`). `make binaries` must build.
  DONE: `newMCPServer` + `mcpHTTPHandler` + `mountMCP` in `mcp.go`. Mounted
  into production `httpMux` after `registerMetrics`. `make test` green,
  `make binaries` builds.
- [x] **Test-mode HTTP mux.** `runTestMode` now takes `--test-http-listen`
  and serves `buildTestHTTPMux()` (metrics, bupdate, mesh w/ empty FQDN, MCP)
  so the e2e harness can drive `/v1/mcp` without tsnet. Closes the Phase 0
  coverage gap.
  DONE: `--test-http-listen` flag + `buildTestHTTPMux` in `main.go`.
- [~] **Identity: `X-Aperture-Login` header.** Resolve the effective user as:
  (1) if peer `WhoIs` matches the configured trusted Aperture node and the
  header is present → use it; (2) else peer `WhoIs` login name. Frames key
  by that string. `TODO: forward/record stable Aperture UserID` comment.
  SCAFFOLD: `resolveMCPUser` + `mcpAuthMiddleware` + `mcpUserKey` context
  plumbing in place. Test mode returns `testModeUser`. Production `peerIsTrustedAperture`/`peerWhoIsLogin` are TODO stubs (return false/"unknown") —
  wire to tsnet LocalClient when the production HTTP listener is hardened.
  The temporary `unknown` principal currently maps through policy like every
  other principal and therefore uses the default `shared` workspace namespace;
  this makes MCP see the default frames without pretending the identity bug is
  fixed.
  T8 e2e covers the test-mode path now; production identity is future.
- [ ] **`--mcp-register-url` flag (opt-in auto-register).** When set, run a
  `maintainRegistration` goroutine (copy the ~40-line loop from
  `internal/mcpserver`) that POSTs `{"url":"http://<host>:7575/v1/mcp"}`
  to the given `<aperture>/v1/mcp/register` and holds the connection open,
  reconnecting after drops. Flag absent → serve passively only.
  DEFERRED: not needed for direct-tailnet e2e (T1–T8 don't use it). Flag +
  loop land when the aperture-side `accept_registrations` work happens.
- [ ] **Capability policy: TODO, none in v1.** Leave a `// TODO: apply
  policy.jsonc / ResolveCap` comment at the authz seam. v1: every
  authenticated peer gets full tool access, scoped only by the identity
  model (you drive your own frames).
  DONE (the comment): `// TODO: apply policy.jsonc / ResolveCap` on
  `mcpAuthMiddleware`. Actual enforcement is Phase 5.

## Phase 4 — e2e tests (`e2e/`, build tag `e2e`, never skip)

Tiered, simplest-to-hardest. Per `CLAUDE.md`: log to a file, 1–2 min timeout,
`make e2e E2E_ARGS="-test.run=TestName" 2>&1 | tee e2e.log`.

- [x] **T1: HTTP mux in test mode.** Mesh/metrics endpoints respond on the
  test port (closes the pre-existing gap from Phase 0).
  DONE: `--test-http-listen` flag + `buildTestHTTPMux` in `main.go`; mesh
  (empty-FQDN), metrics, bupdate, and MCP all mounted. `TestMCPHTTPMuxResponds`
  in `e2e/mcp_test.go` covers it (initialize + tool list).
- [x] **T2: cancellation/reap spike** (from Phase 1) — `sleep 30 & sleep 30`
  then timeout-close; assert no leftover process.
  DONE: `TestMCPTimeoutAndReap` in `e2e/mcp_test.go`. The spike found the reap
  was UNRELIABLE: vshdsession's `killChildOnDisconnect` only signalled the
  direct child (sh), so a backgrounded `sleep N &` was orphaned (reparented to
  init) and kept running — and the outer vshd blocked in `cmd.Wait()` on the
  inner `ts session-serve`'s idle stdout. FIXED in `vshdsession`: `servePipe`
  now sets `Setpgid` so the child leads a process group, and
  `killChildOnDisconnect` kills the whole group (`kill(-pgid)`) with SIGHUP→
  SIGKILL escalation; `servePTY` already got a group via `pty.Start`'s Setsid
  and now also group-kills on disconnect. This also fixes the SSH-disconnect-
  from-a-backgrounded-command leak (pre-existing, untested). Full `make e2e`
  green — no SSH/VM regression.
- [ ] **T3: output truncation** — `yes`-style infinite stream hits 1 MiB
  cap, returns truncated result with marker, tears down promptly (not the
  full deadline).
  NOT YET WRITTEN: `mcpexec.CollectFrames` has 10 unit tests covering the cap
  + rune-boundary trim; the end-to-end teardown timing needs a real frame
  with the `yes` applet installed. Lower priority (unit coverage is solid).
- [x] **T4: timeout** — command sleeping past (test-shrunken) timeout
  returns a timeout result and leaves no process behind.
  DONE: `TestMCPTimeoutAndReap` covers it together with T2 (the setup is
  identical: a 2s timeout on a 30s command with a backgrounded child).
  Asserts IsError=true + 'timed out' marker + returns near the 2s deadline
  (not the full 30s) + no leftover `sleep 30` in the frame PID namespace.
  `runInFrame` returns `errMCPCommandTimeout` (a distinct sentinel the
  handler branches on) and surfaces the partial output.
- [x] **T5: tool round-trips** — `bash` (zero & non-zero exit), `view`
  (file/dir/image/`view_range`), `create_file`, `str_replace` (success,
  not-found, not-unique), `list_frames`, `list_refs` — against a fresh
  frame and against a named ref.
  DONE: `TestMCPToolsRoundTrip` covers all six tools, zero + non-zero exit,
  file/dir/missing view, create_file, str_replace success/not-unique/not-
  found, post-replace verification, and bad-frame error handling. The e2e
  harness installs busybox applets (mkdir/dirname/base64/awk/find/sort/head/
  stat) into the nil:nil:nil frame since the tool command builders call out to
  those POSIX utilities. Missing: image view and `view_range` (minor; the
  code paths are shared and `TestBuildViewCommand` unit-tests the range
  plumbing). Non-zero exits now correctly return `IsError=true` (the original
  handler returned `IsError=false`, contradicting the design intent and the
  test assertions).
- [x] **T6: `frame` arg semantics** — `frame=""` auto-creates default;
  `frame=<uuid>` and `frame=<ref>` resolve; bad frame errors cleanly.
  DONE: `TestMCPFrameResolution` covers all four: `frame=""` auto-creates a
  fresh frame (visible in `list_frames` by UUID), `frame=<uuid>` resolves it
  and reads back a marker written via the auto-create call, `frame=<ref>`
  resolves a named ref (verified by readback via UUID), `frame=<bad>` errors
  cleanly with a resolve/not-found message.
- [ ] **T7: concurrency** — two `bash` calls same frame in one turn share
  FS (both see a file one writes); two calls different frames isolated
  (neither sees the other's writes); outputs/exit codes map to right calls.
  NOT YET WRITTEN: the per-call fresh-`sh -c` design means same-frame sharing
  is via the shared rootfs subvolume (already proven by the SSH path);
  cross-frame isolation is the btrfs subvolume boundary (also proven). A
  dedicated MCP-level concurrency test is still worthwhile.
- [~] **T8: identity header** — direct-tailnet uses peer `WhoIs`;
  trusted-peer + `X-Aperture-Login` keys frames by header; header from
  non-trusted peer ignored (falls back to peer `WhoIs`).
  PARTIAL: the test-mode path (testModeUser) is exercised by every MCP e2e
  test. The production `peerIsTrustedAperture`/`peerWhoIsLogin` stubs are
  still TODO (fail-closed); a dedicated production-identity test needs tsnet.
- [ ] **T9: auto-register** (if enabled) — with `accept_registrations:`
  true` on a test aperture, thundersnap's tools appear under `auto<N>_` and
  disappear when the registration connection closes.
  DEFERRED: depends on the aperture-side `accept_registrations` work.

### Unit tests (`make test`, no root/btrfs needed)

- [x] `mcpexec/collect_test.go`: 10 tests — exit codes, stdout/stderr
  interleaving, 1 MiB cap + marker, rune-boundary trim, split-rune cleanup,
  EOF-after-exit, empty stream, post-cap drop.
- [x] `cmd/thundersnapd/mcp_test.go`: `textResult` IsError semantics,
  `isWithinRootFS` path-traversal guard (8 cases), `buildViewCommand` range
  validation + structure, `buildCreateFileCommand` base64 round-trip,
  `truncateUTF8` rune-boundary trim, `shellQuote` escaping,
  `errMCPCommandTimeout` sentinel `errors.Is`-matchable.

## Background bash — next implementation phase

Design: `mcp-background-bash.md`. Replace the blocking bash result with
conversation-scoped supervised jobs so Aperture's sequential tool dispatcher
can launch concurrent work and observe it through short wait calls.

- [x] Accept Aperture's stable conversation ID from the
  `X-Aperture-Conversation-Id` HTTP header, with the older MCP `tools/call`
  `_meta` key `io.tailscale.aperture/conversation-id` retained for compatibility.
  Thundersnap job tools reject missing/empty identity; there is no MCP session-ID
  fallback because silently combining chats is worse than failing.
- [x] Implement conversation-scoped job manager keyed by `(resolved user,
  Aperture conversation ID)`, with short per-scope job IDs and jobs that may
  target any frame.
- [x] Add `thundersnap_jobs`, the single public execution tool: it launches a
  batch of supervised jobs before optionally waiting for exactly that batch,
  preserving combined/stdout/stderr logs, exit state/counters, hard runtime
  limits, and revision-based race-free `output`, `any_exit`, and `all_exit`
  waits. `include_output` returns a bounded combined-log tail. The earlier
  separate `thundersnap_bash` and `thundersnap_jobs_wait` tools were removed
  before release to keep the consumer API unambiguous.
- [x] Add `thundersnap_jobs_list` and `thundersnap_jobs_kill`; killing uses
  the existing whole-process-group vshd disconnect cleanup.
- [x] Add `tail_lines` to `thundersnap_view` for live and completed job logs;
  `job_id` plus optional combined/stdout/stderr stream selection infers the
  frame and log path.
- [~] Add unit and e2e coverage for the edge-case matrix in
  `mcp-background-bash.md`; use that checklist during code review. Current
  coverage includes required conversation metadata, same-conversation reuse
  across MCP connections, cross-conversation isolation, short per-chat IDs,
  concurrent jobs, output waits/live tail reads, normal and non-zero exits,
  explicit kill, hard-timeout process-group reap, wait timeout with reason=
  timeout and no kill, already-satisfied all_exit returning immediately,
  input validation (empty command, bad user/until, unknown job, kill without
  ids, tail_lines/view_range mutual exclusion, negative tail_lines), one
  conversation spanning multiple frames, kill idempotency on a terminal job,
  and combined/stdout/stderr counter agreement. The remaining matrix items
  stay intentionally unchecked in the design document.

## Phase 5 — Future (out of scope for MCP-first phase; see design doc §Future work)

- [ ] Drop-in `chatsandbox.Backend` HTTP-client impl + supervisor wiring
  (brings chat-UI parity: `sandbox_push`/`present_files`/`download_file`/
  drain). Deferred — sandbox APIs expected to change.
- [ ] `present_files` / chat-UI attachment rows for the connector path
  (generalize Aperture outputs handler, or thundersnap-served download URLs).
- [ ] Stable user-ID frame key (forward/record Aperture immutable `UserID`
  so renames don't orphan frame dirs).
- [ ] Harden forwarded chat identity as a security boundary: authenticate a
  stable Aperture user ID and trust/bind the forwarded conversation ID (for
  example trusted-peer validation or signed metadata). Initial background-job
  scoping is for correctness and avoids accidental cross-chat mixing, not a
  claim of strong cross-conversation authorization.
- [ ] Support additional trusted client chat-scope formats for background jobs,
  notably Open WebUI's `X-OpenWebUI-Chat-Id` (paired with stable user and
  deployment identity). Keep explicit chat identity mandatory; do not silently
  fall back to `Mcp-Session-Id`, whose lifetime differs across clients.
- [ ] Capability policy (`policy.jsonc` / `ResolveCap`) on MCP tool access
  + frame launch isolation.
- [ ] Persistent shell per MCP session (carry `cd`/`export` across calls).
- [ ] MCP `notifications/progress` for live stdout streaming, once a
  mainstream client renders them.

---

## Notes

- Aperture-side dependency (not tracked here): Aperture must learn to send
  `X-Aperture-Login` on proxied calls to a "trust-proxy-identity" connector.
  The direct-tailnet deployment (harness → thundersnap, no Aperture) needs
  no Aperture change and is the first thing e2e T8 covers.
- Don't import the aperture module — copy-and-paste only (per project rule).
  ~400 lines total: tool defs + command builders + `CollectExec` + adapter.
