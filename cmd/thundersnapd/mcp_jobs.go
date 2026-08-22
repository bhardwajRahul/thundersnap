// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tailscale/thundersnap/vshdproto"
)

const (
	apertureConversationIDMetaKey    = "io.tailscale.aperture/conversation-id"
	apertureConversationIDHeaderName = "X-Aperture-Conversation-Id"
	mcpJobDefaultHardTimeout         = 2 * time.Hour
	mcpJobMaxHardTimeout             = 2 * time.Hour
	mcpJobWaitDefaultTimeout         = 30 * time.Second
	mcpJobWaitMaxTimeout             = 60 * time.Second
)

type mcpJobScopeKey struct {
	user, conversation string
}

type mcpJobState string

const (
	mcpJobRunning  mcpJobState = "running"
	mcpJobExited   mcpJobState = "exited"
	mcpJobTimedOut mcpJobState = "timed_out"
	mcpJobKilled   mcpJobState = "killed"
	mcpJobLost     mcpJobState = "lost"
)

type mcpJob struct {
	id             string
	label          string
	command        string
	workdir        string
	unixUser       string
	frame          string
	combinedLog    string
	state          mcpJobState
	startedAt      time.Time
	endedAt        time.Time
	exitCode       *int
	combinedBytes  int64
	outputRevision uint64
	endRevision    uint64
	conn           net.Conn
	writeMu        sync.Mutex
	stopReason     mcpJobState
	timer          *time.Timer
}

func (j *mcpJob) terminal() bool {
	return j.state == mcpJobExited || j.state == mcpJobTimedOut || j.state == mcpJobKilled || j.state == mcpJobLost
}

type mcpJobList struct {
	mu       sync.Mutex
	jobs     map[string]*mcpJob
	nextID   uint64
	revision uint64
	changed  chan struct{}
}

func newMCPJobList() *mcpJobList {
	return &mcpJobList{jobs: make(map[string]*mcpJob), changed: make(chan struct{})}
}

func (l *mcpJobList) notifyLocked() uint64 {
	l.revision++
	close(l.changed)
	l.changed = make(chan struct{})
	return l.revision
}

type mcpJobManager struct {
	mu    sync.Mutex
	lists map[mcpJobScopeKey]*mcpJobList
}

var mcpJobs = &mcpJobManager{lists: make(map[mcpJobScopeKey]*mcpJobList)}

func (m *mcpJobManager) list(key mcpJobScopeKey) *mcpJobList {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.lists[key]
	if l == nil {
		l = newMCPJobList()
		m.lists[key] = l
	}
	return l
}

func mcpJobScopeFromRequest(ctx context.Context, req *mcp.CallToolRequest) (mcpJobScopeKey, error) {
	user := mcpUserFromContext(ctx)
	if user == "" {
		return mcpJobScopeKey{}, fmt.Errorf("no MCP user resolved for request")
	}
	if req == nil {
		return mcpJobScopeKey{}, fmt.Errorf("missing Aperture conversation ID in %s header or MCP _meta", apertureConversationIDHeaderName)
	}
	if req.Extra != nil {
		if conversation := req.Extra.Header.Get(apertureConversationIDHeaderName); strings.TrimSpace(conversation) != "" {
			return mcpJobScopeKey{user: user, conversation: conversation}, nil
		}
	}
	if req.Params != nil {
		if conversation, ok := req.Params.Meta[apertureConversationIDMetaKey].(string); ok && strings.TrimSpace(conversation) != "" {
			return mcpJobScopeKey{user: user, conversation: conversation}, nil
		}
	}
	return mcpJobScopeKey{}, fmt.Errorf("missing Aperture conversation ID in %s header or MCP _meta (%s)", apertureConversationIDHeaderName, apertureConversationIDMetaKey)
}

func mcpJobScopeDir(key mcpJobScopeKey) string {
	sum := sha256.Sum256([]byte(key.user + "\x00" + key.conversation))
	return hex.EncodeToString(sum[:12])
}

type mcpJobStatus struct {
	ID              string      `json:"id"`
	Label           string      `json:"label,omitempty"`
	State           mcpJobState `json:"state"`
	Frame           string      `json:"frame"`
	Workdir         string      `json:"workdir"`
	User            string      `json:"user"`
	Log             string      `json:"log"`
	Bytes           int64       `json:"bytes"`
	ExitCode        *int        `json:"exit_code,omitempty"`
	StartedAt       string      `json:"started_at"`
	EndedAt         string      `json:"ended_at,omitempty"`
	ElapsedMS       int64       `json:"elapsed_ms"`
	Output          *string     `json:"output,omitempty"`
	OutputTruncated *bool       `json:"output_truncated,omitempty"`
}

func jobStatusLocked(j *mcpJob, now time.Time) mcpJobStatus {
	end := j.endedAt
	if end.IsZero() {
		end = now
	}
	s := mcpJobStatus{
		ID: j.id, Label: j.label, State: j.state, Frame: j.frame,
		Workdir: j.workdir, User: j.unixUser, Log: j.combinedLog, Bytes: j.combinedBytes,
		ExitCode: j.exitCode, StartedAt: j.startedAt.UTC().Format(time.RFC3339Nano),
		ElapsedMS: end.Sub(j.startedAt).Milliseconds(),
	}
	if !j.endedAt.IsZero() {
		s.EndedAt = j.endedAt.UTC().Format(time.RFC3339Nano)
	}
	return s
}

func selectJobsLocked(l *mcpJobList, ids []string) ([]*mcpJob, error) {
	if len(ids) == 0 {
		ids = make([]string, 0, len(l.jobs))
		for id := range l.jobs {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool {
			var a, b uint64
			fmt.Sscanf(ids[i], "j%d", &a)
			fmt.Sscanf(ids[j], "j%d", &b)
			return a < b
		})
	}
	jobs := make([]*mcpJob, 0, len(ids))
	seen := make(map[string]bool)
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		j := l.jobs[id]
		if j == nil {
			return nil, fmt.Errorf("unknown job ID %q in this conversation", id)
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

func statusesLocked(jobs []*mcpJob) []mcpJobStatus {
	now := time.Now()
	out := make([]mcpJobStatus, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, jobStatusLocked(j, now))
	}
	return out
}

type mcpWaitJob struct {
	ID     string `json:"id"`
	Offset int64  `json:"offset"`
}

type mcpWaitStatus struct {
	ID         string      `json:"id"`
	State      mcpJobState `json:"state"`
	ExitCode   *int        `json:"exit_code,omitempty"`
	Output     string      `json:"output,omitempty"`
	NextOffset int64       `json:"next_offset"`
}

// waitStatuses returns only output ending in CR/LF while a job is running. At
// exit it also returns the final partial line. Offsets are raw combined-log byte
// positions, so callers do not need a server-side revision cursor.
func waitStatuses(key mcpJobScopeKey, jobs []*mcpJob, requested []mcpWaitJob) ([]mcpWaitStatus, error) {
	offsets := map[string]int64{}
	for _, r := range requested {
		offsets[r.ID] = r.Offset
	}
	out := make([]mcpWaitStatus, 0, len(jobs))
	for _, j := range jobs {
		offset := offsets[j.id]
		if offset < 0 || offset > j.combinedBytes {
			return nil, fmt.Errorf("offset %d for job %s is outside log size %d", offset, j.id, j.combinedBytes)
		}
		rootFS, _, err := resolveFrameRootFS(key.user, j.frame)
		if err != nil {
			return nil, err
		}
		hostPath := filepath.Join(rootFS, strings.TrimPrefix(j.combinedLog, "/"))
		f, err := os.Open(hostPath)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(io.NewSectionReader(f, offset, j.combinedBytes-offset), mcpMaxViewOutput))
		f.Close()
		if err != nil {
			return nil, err
		}
		end := len(data)
		if !j.terminal() {
			end = 0
			for i, b := range data {
				if b == '\n' || b == '\r' {
					end = i + 1
				}
			}
		}
		text := strings.ToValidUTF8(string(data[:end]), "")
		out = append(out, mcpWaitStatus{ID: j.id, State: j.state, ExitCode: j.exitCode, Output: text, NextOffset: offset + int64(end)})
	}
	return out, nil
}

func jsonToolResult(v any, isError bool) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return textResult(fmt.Sprintf("marshal result: %v", err), true)
	}
	return textResult(string(b), isError)
}

func callToolResultText(result *mcp.CallToolResult) string {
	if result == nil || len(result.Content) == 0 {
		return ""
	}
	if text, ok := result.Content[0].(*mcp.TextContent); ok {
		return text.Text
	}
	return ""
}

// nestedCallToolRequest builds an internal tool request while retaining the
// transport metadata from the original HTTP request. In particular, Extra
// carries X-Aperture-Conversation-Id; dropping it makes a jobs call pass its
// initial scope check and then fail when the nested launch or wait handler
// resolves the scope again.
func nestedCallToolRequest(req *mcp.CallToolRequest, arguments json.RawMessage) *mcp.CallToolRequest {
	// req.Params may be nil for a request that supplied only transport
	// metadata (e.g. the X-Aperture-Conversation-Id header) and no body; the
	// scope resolver tolerates that, so tolerate it here too instead of
	// nil-derefing req.Params.Meta.
	var meta mcp.Meta
	if req != nil && req.Params != nil {
		meta = req.Params.Meta
	}
	return &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Meta: meta, Arguments: arguments},
		Extra:  req.Extra,
	}
}

// mcpWaitParams is the wait sub-object of the jobs tool. It accepts the
// canonical object form {"jobs":[...],"until":...,"timeout":...,"pre_signal":...}
// and a common shorthand where the whole value is just the jobs array,
// [{"id":..,"offset":..}, ...]. The array form is a frequent LLM mistake
// (the schema says wait is an object whose first property is jobs); tolerating
// it turns a hard unmarshal error into a successful wait.
type mcpWaitParams struct {
	Jobs      []mcpWaitJob `json:"jobs"`
	Until     string       `json:"until"`
	Timeout   int          `json:"timeout"`
	PreSignal string       `json:"pre_signal"`
}

func (w *mcpWaitParams) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		// Shorthand: wait is the jobs array directly.
		return json.Unmarshal(trimmed, &w.Jobs)
	}
	type alias mcpWaitParams
	return json.Unmarshal(data, (*alias)(w))
}

// mcpJobsToolHandler is the public command-execution API. It composes the
// internal launch and wait handlers so all launches are submitted in array
// order before waiting. This avoids relying on an MCP client's sibling-tool
// call scheduling while still allowing the launched commands to run in
// parallel.
func mcpJobsToolHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	key, err := mcpJobScopeFromRequest(ctx, req)
	if err != nil {
		return textResult(err.Error(), true)
	}
	var params struct {
		Launch []json.RawMessage `json:"launch"`
		Wait   *mcpWaitParams    `json:"wait"`
	}
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &params); err != nil {
			return textResult(fmt.Sprintf("invalid input: %v", err), true)
		}
	}
	if len(params.Launch) == 0 && params.Wait == nil {
		return textResult("at least one of launch or wait is required", true)
	}
	if len(params.Launch) > 0 && params.Wait != nil && len(params.Wait.Jobs) > 0 {
		return textResult("wait.jobs must be omitted when launch is present; the wait automatically selects jobs launched by this call", true)
	}

	launchedIDs := make([]string, 0, len(params.Launch))
	for i, launch := range params.Launch {
		result, err := mcpBashToolHandler(ctx, nestedCallToolRequest(req, launch))
		if err != nil {
			return nil, err
		}
		if result.IsError {
			return textResult(fmt.Sprintf("launch[%d] failed after starting jobs %v: %s", i, launchedIDs, callToolResultText(result)), true)
		}
		var started struct {
			JobID string `json:"job_id"`
		}
		if err := json.Unmarshal([]byte(callToolResultText(result)), &started); err != nil || started.JobID == "" {
			return textResult(fmt.Sprintf("launch[%d] returned an invalid result after starting jobs %v", i, launchedIDs), true)
		}
		launchedIDs = append(launchedIDs, started.JobID)
	}

	if params.Wait != nil {
		waitArgs := *params.Wait
		if len(launchedIDs) > 0 {
			waitArgs.Jobs = make([]mcpWaitJob, len(launchedIDs))
			for i, id := range launchedIDs {
				waitArgs.Jobs[i] = mcpWaitJob{ID: id, Offset: 0}
			}
		}
		arguments, err := json.Marshal(waitArgs)
		if err != nil {
			return textResult(fmt.Sprintf("marshal wait request: %v", err), true)
		}
		return mcpJobsWaitToolHandler(ctx, nestedCallToolRequest(req, arguments))
	}

	l := mcpJobs.list(key)
	l.mu.Lock()
	jobs, err := selectJobsLocked(l, launchedIDs)
	if err != nil {
		l.mu.Unlock()
		return textResult(err.Error(), true)
	}
	result := map[string]any{"reason": "started", "revision": l.revision, "jobs": statusesLocked(jobs)}
	l.mu.Unlock()
	return jsonToolResult(result, false)
}

// mcpBashToolHandler is an internal single-job launch primitive used by
// mcpJobsToolHandler. It is deliberately not registered as an MCP tool.
func mcpBashToolHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	key, err := mcpJobScopeFromRequest(ctx, req)
	if err != nil {
		return textResult(err.Error(), true)
	}
	var params struct {
		Command     string `json:"command"`
		Frame       string `json:"frame"`
		Workdir     string `json:"workdir"`
		Label       string `json:"label"`
		User        string `json:"user"`
		HardTimeout int    `json:"hard_timeout"`
	}
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &params); err != nil {
			return textResult(fmt.Sprintf("invalid input: %v", err), true)
		}
	}
	if params.Command == "" {
		return textResult("command is required", true)
	}
	if params.Workdir == "" {
		params.Workdir = "/work"
	}
	if params.User != "user" && params.User != "root" {
		return textResult("user is required and must be either \"user\" or \"root\"", true)
	}
	hardTimeout := mcpJobDefaultHardTimeout
	if params.HardTimeout > 0 {
		hardTimeout = time.Duration(params.HardTimeout) * time.Second
		if hardTimeout > mcpJobMaxHardTimeout {
			hardTimeout = mcpJobMaxHardTimeout
		}
	}

	rootFS, uuid, err := resolveFrameRootFS(key.user, params.Frame)
	if err != nil {
		return textResult(fmt.Sprintf("resolve frame: %v", err), true)
	}
	if err := prepareContainerRootFS(rootFS, ""); err != nil {
		return textResult(fmt.Sprintf("prepare frame rootfs: %v", err), true)
	}
	workdirHost := filepath.Join(rootFS, strings.TrimPrefix(filepath.Clean("/"+params.Workdir), "/"))
	if !isWithinRootFS(workdirHost, rootFS) {
		return textResult(fmt.Sprintf("workdir %q escapes the frame", params.Workdir), true)
	}
	if info, err := os.Stat(workdirHost); err != nil || !info.IsDir() {
		if err == nil {
			err = fmt.Errorf("not a directory")
		}
		return textResult(fmt.Sprintf("invalid workdir %q: %v", params.Workdir, err), true)
	}
	if _, err := controlServers.getOrCreateControlServer(rootFS); err != nil {
		return textResult(fmt.Sprintf("start control socket: %v", err), true)
	}
	releaseControl := true
	defer func() {
		if releaseControl {
			controlServers.releaseControlServer(rootFS)
		}
	}()

	l := mcpJobs.list(key)
	id, logDirInFrame, combined, err := createMCPJobLog(l, key, rootFS)
	if err != nil {
		return textResult(err.Error(), true)
	}
	closeLogs := func() { combined.Close() }

	sockPath, err := hostVshd.ensure()
	if err != nil {
		closeLogs()
		return textResult(fmt.Sprintf("start host vshd: %v", err), true)
	}
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		closeLogs()
		return textResult(fmt.Sprintf("dial host vshd: %v", err), true)
	}
	absRootFS, err := filepath.Abs(rootFS)
	if err != nil {
		conn.Close()
		closeLogs()
		return textResult(fmt.Sprintf("abs rootfs: %v", err), true)
	}
	wrapped := "cd " + shellQuote(params.Workdir) + " && " + params.Command
	writeVshdRequest(conn, strings.TrimPrefix(absRootFS, "/"), params.User, false, []string{"sh", "-c", wrapped}, thundersnapSessionEnv(key.user, uuid))

	j := &mcpJob{
		id: id, label: params.Label, command: params.Command, workdir: params.Workdir, unixUser: params.User,
		frame: uuid.String(), state: mcpJobRunning, startedAt: time.Now(), conn: conn,
		combinedLog: filepath.Join(logDirInFrame, "combined.log"),
	}
	l.mu.Lock()
	l.jobs[id] = j
	revision := l.notifyLocked()
	status := jobStatusLocked(j, time.Now())
	l.mu.Unlock()

	j.timer = time.AfterFunc(hardTimeout, func() { requestMCPJobStop(l, j, mcpJobTimedOut) })
	releaseControl = false
	go collectMCPJob(l, j, conn, combined, func() { controlServers.releaseControlServer(rootFS) })

	return jsonToolResult(map[string]any{"job_id": id, "revision": revision, "job": status}, false)
}

// createMCPJobLog allocates a process-local job ID that is also unused in the
// target frame. Logs outlive the daemon (and may be copied when a frame is
// cloned), while nextID does not, so after a restart the first candidate can
// already exist. Never overwrite that output: consume IDs until mkdir reserves
// a new job directory atomically.
func createMCPJobLog(l *mcpJobList, key mcpJobScopeKey, rootFS string) (id, logDirInFrame string, combined *os.File, err error) {
	logRootInFrame := filepath.Join("/tmp/.ts/jobs", mcpJobScopeDir(key))
	logRootHost := filepath.Join(rootFS, strings.TrimPrefix(logRootInFrame, "/"))
	if !isWithinRootFS(logRootHost, rootFS) {
		return "", "", nil, fmt.Errorf("internal job log path escaped frame")
	}
	if err := os.MkdirAll(logRootHost, 0700); err != nil {
		return "", "", nil, fmt.Errorf("create job log root: %w", err)
	}

	for {
		l.mu.Lock()
		l.nextID++
		id = fmt.Sprintf("j%d", l.nextID)
		l.mu.Unlock()

		logDirInFrame = filepath.Join(logRootInFrame, id)
		logDirHost := filepath.Join(logRootHost, id)
		if err := os.Mkdir(logDirHost, 0700); os.IsExist(err) {
			continue
		} else if err != nil {
			return "", "", nil, fmt.Errorf("create job log directory: %w", err)
		}
		combined, err = os.OpenFile(filepath.Join(logDirHost, "combined.log"), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
		if err != nil {
			_ = os.Remove(logDirHost)
			return "", "", nil, fmt.Errorf("create combined log: %w", err)
		}
		return id, logDirInFrame, combined, nil
	}
}

func requestMCPJobStop(l *mcpJobList, j *mcpJob, reason mcpJobState) {
	l.mu.Lock()
	if j.terminal() || j.stopReason != "" {
		l.mu.Unlock()
		return
	}
	j.stopReason = reason
	conn := j.conn
	l.notifyLocked()
	l.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func collectMCPJob(l *mcpJobList, j *mcpJob, conn net.Conn, combined *os.File, release func()) {
	defer release()
	defer conn.Close()
	defer combined.Close()
	gotExit := false
	for {
		typ, payload, err := vshdproto.ReadFrame(conn)
		if err != nil {
			if err != io.EOF && err != net.ErrClosed {
				// The terminal state below records this as lost unless a stop was requested.
			}
			break
		}
		switch typ {
		case vshdproto.FrameStdout, vshdproto.FrameStderr:
			_, _ = combined.Write(payload)
			l.mu.Lock()
			j.combinedBytes += int64(len(payload))
			j.outputRevision = l.notifyLocked()
			l.mu.Unlock()
		case vshdproto.FrameExit:
			if code, err := vshdproto.DecodeExit(payload); err == nil {
				c := int(code)
				l.mu.Lock()
				j.exitCode = &c
				l.mu.Unlock()
			}
			gotExit = true
			break
		}
		if gotExit {
			break
		}
	}
	if j.timer != nil {
		j.timer.Stop()
	}
	l.mu.Lock()
	if j.stopReason != "" {
		j.state = j.stopReason
	} else if gotExit {
		j.state = mcpJobExited
	} else {
		j.state = mcpJobLost
	}
	j.conn = nil
	j.endedAt = time.Now()
	j.endRevision = l.notifyLocked()
	l.mu.Unlock()
}

func mcpJobsListToolHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	key, err := mcpJobScopeFromRequest(ctx, req)
	if err != nil {
		return textResult(err.Error(), true)
	}
	var params struct {
		JobIDs []string `json:"job_ids"`
	}
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &params); err != nil {
			return textResult(fmt.Sprintf("invalid input: %v", err), true)
		}
	}
	l := mcpJobs.list(key)
	l.mu.Lock()
	jobs, err := selectJobsLocked(l, params.JobIDs)
	if err != nil {
		l.mu.Unlock()
		return textResult(err.Error(), true)
	}
	result := map[string]any{"revision": l.revision, "jobs": statusesLocked(jobs)}
	l.mu.Unlock()
	return jsonToolResult(result, false)
}

func parseJobSignal(name string) (syscall.Signal, error) {
	signals := map[string]syscall.Signal{"HUP": syscall.SIGHUP, "INT": syscall.SIGINT, "TERM": syscall.SIGTERM, "USR1": syscall.SIGUSR1, "USR2": syscall.SIGUSR2, "STOP": syscall.SIGSTOP, "CONT": syscall.SIGCONT}
	if name == "" {
		return 0, nil
	}
	if sig, ok := signals[strings.TrimPrefix(strings.ToUpper(name), "SIG")]; ok {
		return sig, nil
	}
	return 0, fmt.Errorf("invalid signal %q (want HUP, INT, TERM, USR1, USR2, STOP, or CONT)", name)
}

func mcpJobsWaitToolHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	key, err := mcpJobScopeFromRequest(ctx, req)
	if err != nil {
		return textResult(err.Error(), true)
	}
	var params struct {
		Jobs      []mcpWaitJob `json:"jobs"`
		Until     string       `json:"until"`
		Timeout   int          `json:"timeout"`
		PreSignal string       `json:"pre_signal"`
	}
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &params); err != nil {
			return textResult(fmt.Sprintf("invalid input: %v", err), true)
		}
	}
	if params.Until == "" {
		params.Until = "any_exit"
	}
	if params.Until != "output" && params.Until != "any_exit" && params.Until != "all_exit" {
		return textResult(fmt.Sprintf("invalid until %q (want output, any_exit, or all_exit)", params.Until), true)
	}
	sig, err := parseJobSignal(params.PreSignal)
	if err != nil {
		return textResult(err.Error(), true)
	}
	waitTimeout := mcpJobWaitDefaultTimeout
	if params.Timeout > 0 {
		waitTimeout = min(time.Duration(params.Timeout)*time.Second, mcpJobWaitMaxTimeout)
	}
	waitCtx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	ids := make([]string, len(params.Jobs))
	for i, job := range params.Jobs {
		ids[i] = job.ID
	}
	l := mcpJobs.list(key)
	signalSent := false
	for {
		l.mu.Lock()
		jobs, err := selectJobsLocked(l, ids)
		if err != nil {
			l.mu.Unlock()
			return textResult(err.Error(), true)
		}
		if sig != 0 && !signalSent {
			for _, j := range jobs {
				if !j.terminal() {
					j.writeMu.Lock()
					// j.conn is set to nil by collectMCPJob under l.mu once the
					// connection tears down; a job observed as non-terminal can
					// still race with teardown and have a nil conn, which would
					// panic WriteFrame. Skip signaling in that case -- the job
					// is already stopping.
					if j.conn != nil {
						err = vshdproto.WriteFrame(j.conn, vshdproto.FrameSignal, vshdproto.EncodeSignal(int32(sig)))
					}
					j.writeMu.Unlock()
					if err != nil {
						l.mu.Unlock()
						return textResult(fmt.Sprintf("signal job %s: %v", j.id, err), true)
					}
				}
			}
			signalSent = true
		}
		statuses, err := waitStatuses(key, jobs, params.Jobs)
		if err != nil {
			l.mu.Unlock()
			return textResult(err.Error(), true)
		}
		satisfied := false
		switch params.Until {
		case "output":
			for _, s := range statuses {
				satisfied = satisfied || s.Output != "" || s.State != mcpJobRunning
			}
		case "any_exit":
			for _, j := range jobs {
				satisfied = satisfied || j.terminal()
			}
		case "all_exit":
			satisfied = true
			for _, j := range jobs {
				satisfied = satisfied && j.terminal()
			}
		}
		if satisfied {
			l.mu.Unlock()
			return jsonToolResult(map[string]any{"reason": params.Until, "jobs": statuses}, false)
		}
		changed := l.changed
		l.mu.Unlock()
		select {
		case <-changed:
		case <-waitCtx.Done():
			l.mu.Lock()
			jobs, err := selectJobsLocked(l, ids)
			if err != nil {
				l.mu.Unlock()
				return textResult(err.Error(), true)
			}
			statuses, err := waitStatuses(key, jobs, params.Jobs)
			l.mu.Unlock()
			if err != nil {
				return textResult(err.Error(), true)
			}
			return jsonToolResult(map[string]any{"reason": "timeout", "jobs": statuses}, false)
		}
	}
}

func mcpJobsKillToolHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	key, err := mcpJobScopeFromRequest(ctx, req)
	if err != nil {
		return textResult(err.Error(), true)
	}
	var params struct {
		JobIDs []string `json:"job_ids"`
	}
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &params); err != nil {
			return textResult(fmt.Sprintf("invalid input: %v", err), true)
		}
	}
	if len(params.JobIDs) == 0 {
		return textResult("job_ids is required for kill", true)
	}
	l := mcpJobs.list(key)
	l.mu.Lock()
	jobs, err := selectJobsLocked(l, params.JobIDs)
	l.mu.Unlock()
	if err != nil {
		return textResult(err.Error(), true)
	}
	for _, j := range jobs {
		requestMCPJobStop(l, j, mcpJobKilled)
	}
	// Do not report success while the process group may still be alive. Wait
	// for each collector to observe connection teardown and publish terminal
	// state. The request context remains the upper bound if vshd wedges.
	for {
		l.mu.Lock()
		allTerminal := true
		for _, j := range jobs {
			allTerminal = allTerminal && j.terminal()
		}
		if allTerminal {
			result := map[string]any{"revision": l.revision, "jobs": statusesLocked(jobs)}
			l.mu.Unlock()
			return jsonToolResult(result, false)
		}
		changed := l.changed
		l.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return textResult(fmt.Sprintf("kill did not complete: %v", ctx.Err()), true)
		}
	}
}
