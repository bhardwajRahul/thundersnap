# MCP Background Bash Design

## Status

Design and implementation notes for supervised background jobs. The public
`thundersnap_jobs` tool batches launches and an optional wait into one request,
so all commands are started before waiting regardless of how the MCP client
schedules sibling tool calls. This also works with Aperture chat, which only
exposes terminal tool results.

## Goals

- Every bash launch returns promptly with a job ID; command execution continues
  independently of the `tools/call` request.
- A single Aperture conversation owns one task list, even when it launches jobs
  in many different frames.
- Different conversations belonging to the same user have separate task lists,
  even when they use the same frames.
- Waiting is event-driven rather than polling on a fixed sleep interval.
- Complete stdout and stderr remain inspectable inside the target frame while
  the job is running and after it exits.
- Jobs have explicit status, exit code, cancellation, and a hard runtime limit.
- The model can recover from forgotten job IDs by listing its conversation's
  jobs.
- The design works with Aperture's terminal-only tool results; MCP progress
  notification support is not required.

## Non-goals for the first implementation

- Persisting live processes across a `thundersnapd` restart.
- Preventing two conversations from intentionally modifying the same frame.
- Unsolicited model turns when a log line arrives. The model must call `wait`.
- A persistent interactive shell across jobs. Each job is a fresh `sh -c`.
- PTY programs or stdin interaction.
- Strong cryptographic binding of forwarded conversation identity. The current
  Aperture identity forwarding is already incomplete; hardening both user and
  conversation identity is tracked in `TODO-mcp.md`.
- Other clients' chat identity formats. Open WebUI's
  `X-OpenWebUI-Chat-Id` is a known future format, also tracked in
  `TODO-mcp.md`.

## Conversation identity

MCP's Streamable HTTP transport has an `Mcp-Session-Id`, but transport session
lifetime is not chat lifetime:

- Aperture currently pools downstream MCP sessions at connector/user scope, so
  several chats can share one MCP session.
- Open WebUI creates an MCP session per generation and closes it afterward, so
  successive turns in one chat use different MCP sessions.

Aperture forwards its stable conversation ID on each conversation-scoped MCP
request in the `X-Aperture-Conversation-Id` HTTP header. Thundersnap also
accepts the older `tools/call` `_meta` representation for compatibility:

```json
{
  "_meta": {
    "io.tailscale.aperture/conversation-id": "conv_..."
  }
}
```

When both are present, the HTTP header takes precedence.

Thundersnap derives the task-list key as:

```text
(resolved MCP user, Aperture conversation ID)
```

The frame is deliberately not part of this key. Every job records its resolved
frame UUID and log path, so one conversation can have jobs in arbitrarily many
frames.

There is **no fallback** to MCP session ID or a process-global task list. A
job-related call without a non-empty Aperture conversation ID immediately
returns an `IsError` tool result. Failing closed is less confusing than silently
mixing independent chats.

## MCP tools

### `thundersnap_jobs`

This is the single public command-execution tool. It launches zero or more
supervised non-PTY jobs and can then wait for them in the same serialized MCP
request:

```json
{
  "launch": [
    {"command":"make test", "label":"tests", "frame":"dev"},
    {"command":"make lint", "frame":"dev"}
  ],
  "wait": {"until":"all_exit", "timeout":60, "include_output":true}
}
```

Every launch is submitted in array order before waiting, so commands run
concurrently without relying on the MCP client to serialize sibling tool
calls. `label` is optional; it is mainly useful for distinguishing several or
long-running jobs and should be omitted for quick commands. Launch entries
also accept `workdir` (default `/work`), `user` (`user` by default or `root`),
and `hard_timeout` (default/maximum 7200 seconds).

The tool accepts at least one of `launch` or `wait`:

- `launch` only starts jobs and returns `reason:"started"` immediately.
- `wait` only monitors existing `wait.job_ids`; omitted IDs mean every job in
  the conversation.
- `launch` plus `wait` starts every job first, then selects exactly those new
  jobs. `wait.job_ids` is rejected in this form to avoid ambiguity.

Wait conditions are `output`, `any_exit`, and `all_exit`. `after_revision`
provides the existing race-free event cursor. A wait timeout is a successful
snapshot and never kills jobs. With `include_output:true`, each returned job
includes the final 16 KiB of its combined log plus `output_truncated`; the
complete combined/stdout/stderr logs remain in the frame.

The short job ID is unique within the conversation task list. The real
server-side key includes the user and conversation ID.

The implementation uses a revision plus a replace-on-change notification
channel (condition-variable pattern). Predicate check and subscription happen
under one lock, preventing lost wakeups between checking state and sleeping.

### `thundersnap_jobs_list`

Lists the current conversation's jobs, optionally filtered by job IDs. This is
the recovery mechanism when the model forgets an ID and the way a later chat
turn rediscovers jobs launched by an earlier turn.

Each status contains:

- ID and optional label
- frame UUID
- command, workdir, and Unix user
- `running`, `exited`, `timed_out`, `killed`, or `lost` state
- start/end timestamps and elapsed time
- combined/stdout/stderr log paths and byte counts
- exit code when known
- task-list revision

### `thundersnap_jobs_kill`

Requests termination of selected jobs. It closes each job's vshd connection,
which uses the existing vshd session process-group reap path. The response is a
fresh status listing. Killing an already-terminal job is idempotent.

The first implementation uses the same reliable whole-process-group teardown
as timeout/cancellation. A later version may expose TERM-with-grace versus
immediate kill if needed.

## Logs

Each job owns a directory inside its resolved frame:

```text
/tmp/.ts/jobs/<conversation-scope-hash>/<job-id>/
    combined.log
    stdout.log
    stderr.log
```

The scope directory uses a hash rather than embedding untrusted conversation
text in a host path. Log paths returned to the model are in-frame absolute
paths.

- `combined.log` is canonical for model inspection and preserves vshd frame
  arrival order.
- `stdout.log` and `stderr.log` preserve stream identity for diagnostics.
- Files are opened by the daemon through the frame rootfs, so minimal frames do
  not need `mkdir`, `tee`, or other utilities.
- Output is written incrementally and is visible before process exit.
- A job is not killed merely because model-facing output is large. Disk quotas,
  rotation, and retention are follow-up policy work; the initial hard runtime
  limit still prevents forgotten infinite jobs.

`thundersnap_view` gains `tail_lines` support so the model can inspect a live
log without knowing its total line count. `tail_lines` is mutually exclusive
with `view_range`. A future byte-offset read mode could make exact incremental
consumption cheaper, but wait responses already expose byte counts.

## Job lifecycle

1. Validate conversation identity and arguments.
2. Resolve and prepare the frame before reporting successful launch.
3. Allocate the short job ID and create log files.
4. Anchor the frame's control server and connect to host vshd.
5. Start a goroutine that reads stdout/stderr/exit frames and updates logs,
   counters, state, and task-list revisions.
6. Return the launch result without tying the goroutine to the MCP request
   context.
7. On normal exit, record the exit code and close logs/resources.
8. On hard timeout or explicit kill, close the vshd connection and wait for the
   collector to observe teardown; vshd reaps the whole process group.

An MCP transport disconnect does not kill jobs. The next request carrying the
same `(user, conversation ID)` rediscovers them.

On daemon shutdown, kernel fd closure tears down vshd sessions. Persistence and
`lost` reconstruction after daemon restart are future work; stale in-frame logs
remain ordinary files.

## Concurrency

The manager protects each conversation task list with a mutex. Different task
lists and different jobs run independently. Calls may arrive concurrently even
though Aperture currently dispatches them sequentially.

Jobs in one conversation may target the same frame or different frames. Jobs in
different conversations may also target the same frame. Filesystem races are
normal process races and are intentionally not serialized by the manager.

## Errors

Job protocol errors are returned as MCP tool error results (`IsError: true`) so
the model can inspect and correct them:

- missing conversation metadata
- unknown job ID
- invalid wait condition or timeout
- bad frame/workdir
- launch/setup failure

A command's non-zero exit is normal job state, not a failed `bash` launch. It is
reported later as `state: "exited", exit_code: N`.

## Test and review matrix

The following edge cases should be checked against unit/e2e coverage during
code review. E2e tests must never skip.

### Conversation/task-list identity

- [x] Missing conversation header and `_meta` reject every job start/list/wait/kill call.
- [ ] Empty/non-string conversation identity rejects cleanly. *(empty covered;
      non-string metadata only unit-tested)*
- [x] Same user + same conversation across separate MCP connections sees the
      same jobs.
- [x] Same user + different conversations cannot list, wait for, or kill each
      other's jobs, even when both use the same frame and short ID `j1`.
- [x] One conversation launches jobs in multiple frames and lists/waits for all
      of them together.
- [x] Job IDs remain short and are unique within one conversation under
      concurrent launches.

### Launch and execution

- [ ] Start returns promptly while a long command remains running.
- [ ] Invalid frame and invalid workdir fail before a successful job ID is
      returned. *(invalid frame covered; invalid workdir not)*
- [x] Empty command is rejected.
- [x] Default frame auto-creation still works.
- [x] Ref and UUID frame resolution both record the resolved UUID.
- [x] Commands in different frames execute concurrently despite sequential MCP
      start calls.
- [ ] Commands in one frame share its filesystem and may run concurrently.
- [x] Non-zero exits are recorded with the correct code and do not turn the
      original start into an error.
- [ ] Empty-output success records exit code 0.
- [x] stdout-only, stderr-only, and interleaved output reach the right logs and
      counters; combined order matches frame arrival order.
- [ ] Split UTF-8 across frames does not corrupt log bytes (logs are raw bytes).
- [ ] A command producing more than the old 1 MiB cap is not killed and its
      complete output remains in the log.

### Wait semantics

- [x] `output` blocks until output after `after_revision`, then wakes promptly.
- [x] `any_exit` wakes for the correct selected job.
- [x] `all_exit` waits until every selected job is terminal.
- [x] Already-satisfied `all_exit` returns immediately.
- [x] Empty job selection means all jobs in this conversation.
- [x] Unknown IDs reject rather than being silently ignored.
- [x] Wait timeout returns `reason: timeout`, preserves running jobs, and does
      not set `IsError`.
- [ ] Output arriving between predicate check and sleep cannot be lost.
- [ ] Several simultaneous waiters all wake on a relevant change.
- [ ] Changes in unselected jobs do not falsely satisfy selected-job
      predicates (spurious wake/recheck is acceptable).
- [x] Revision values never decrease and are usable across list/start/wait.
- [ ] Cancellation of a wait call does not cancel jobs.

### Kill, timeout, and cleanup

- [x] Kill terminates foreground process and background grandchildren.
- [x] Kill is idempotent for terminal jobs.
- [x] Hard timeout marks `timed_out` and reaps the entire process group.
- [x] Explicit kill is distinguishable from hard timeout and normal exit.
- [ ] Killing one job does not affect sibling jobs in the same frame or
      conversation.
- [ ] Closing the MCP transport does not stop a running job.
- [ ] Log files are closed/flushed on every terminal path.
- [ ] Daemon shutdown leaves no live child process.

### Logs and view

- [x] Logs are inside the intended frame and cannot escape its rootfs.
- [x] Conversation IDs and labels cannot inject path traversal.
- [x] `combined.log`, `stdout.log`, and `stderr.log` contents/counters agree.
- [x] Logs are readable while the job is still running.
- [x] `tail_lines` returns the requested final lines for a completed log.
- [x] `tail_lines` returns the currently available tail for a running log.
- [x] `tail_lines` and `view_range` together reject as ambiguous.
- [x] Tail handles empty files, no final newline, long lines, and UTF-8 text.

### Robustness/future policy

- [ ] Many completed jobs do not make status responses unbounded.
- [ ] Disk quota/rotation behavior is specified and tested before production
      exposure to untrusted workloads.
- [ ] Retention/cleanup does not delete logs for running jobs.
- [ ] Daemon-restart behavior is explicit (`lost` or reconstructed state).
- [ ] Stable user identity and trusted conversation forwarding are enforced
      before treating task-list isolation as a security boundary.
