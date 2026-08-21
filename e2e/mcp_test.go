// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build e2e

// mcp_test.go is a real end-to-end test of the thundersnap MCP server. It
// starts a thundersnapd in test mode with --test-http-listen, drives the
// /v1/mcp endpoint with the official go-sdk MCP client, and asserts the
// sandbox and background-job tools work against a fresh btrfs-backed frame.
//
// Per CLAUDE.md: e2e tests NEVER SKIP. If the btrfs/root precondition is
// missing, requireBtrfsRoot calls t.Fatal — that is a misconfigured
// environment, not a skip.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// startDaemonWithHTTP is startDaemon but also serves the HTTP mux (and thus
// /v1/mcp) on a second free local port. Returns the daemon instance plus the
// base URL of the HTTP mux (e.g. "http://127.0.0.1:36213").
//
// The MCP endpoint lives at baseURL + "/v1/mcp".
func startDaemonWithHTTP(t *testing.T, env *testEnv) (*daemonInstance, string) {
	t.Helper()

	// Find a free port for the HTTP mux.
	httpPort, err := getFreePort()
	if err != nil {
		t.Fatalf("find free http port: %v", err)
	}
	httpAddr := fmt.Sprintf("127.0.0.1:%d", httpPort)

	// Reuse startDaemon's SSH setup, then start a second daemon with the HTTP
	// flag. We can't just call startDaemon (it doesn't set --test-http-listen),
	// so duplicate its body with the extra flag. To avoid drift, we invoke the
	// shared helper machinery by calling startDaemon and then... no: the SSH
	// and HTTP listeners must be in the SAME process. So build the args here.

	sshPort, err := getFreePort()
	if err != nil {
		t.Fatalf("find free ssh port: %v", err)
	}
	sshAddr := fmt.Sprintf("127.0.0.1:%d", sshPort)

	stateDir := filepath.Join(env.root, "state")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}

	vshdBinary := env.requireBinary("vshd")
	if err := copyFile(vshdBinary, filepath.Join(env.libexecDir, "vshd")); err != nil {
		t.Fatalf("copy vshd to libexec: %v", err)
	}

	policyPath := filepath.Join(env.root, "policy.json")
	policyContent := `{
		"grants": [
			{
				"principals": ["*"],
				"cap": {
					"role": "developer",
					"isolation": "container",
					"maxFrames": 10
				}
			}
		]
	}`
	if err := os.WriteFile(policyPath, []byte(policyContent), 0644); err != nil {
		t.Fatalf("write policy file: %v", err)
	}

	daemonArgs := []string{
		"--test-listen=" + sshAddr,
		"--test-http-listen=" + httpAddr,
		"--test-user=" + testUser,
		"--data-dir=" + env.root,
		"--state-dir=" + stateDir,
		"--libexec-dir=" + env.libexecDir,
		"--policy=" + policyPath,
	}
	if dir := vmDir(); dir != "" {
		if abs, err := filepath.Abs(dir); err == nil {
			daemonArgs = append(daemonArgs, "--vm-dir="+abs)
		}
	}

	cmd := exec.Command(env.daemonBinary, daemonArgs...)
	cmd.Stdout = os.Stderr // pipe daemon logs so failures are debuggable
	cmd.Stderr = os.Stderr
	cmd.Dir = env.root

	t.Logf("Starting daemon (SSH %s, HTTP %s): %s %v", sshAddr, httpAddr, cmd.Path, cmd.Args[1:])
	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}

	d := &daemonInstance{t: t, cmd: cmd, addr: sshAddr}
	t.Cleanup(func() { d.Stop() })

	if err := d.waitReady(10 * time.Second); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("daemon not ready: %v", err)
	}

	// Wait for the HTTP mux to accept a connection (it starts slightly after
	// the SSH listener). Give it a short deadline.
	httpReady := net.JoinHostPort("127.0.0.1", fmt.Sprint(httpPort))
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", httpReady, 500*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("HTTP mux on %s not ready: %v", httpAddr, err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	return d, "http://" + httpAddr
}

// mcpClient connects an MCP client session to the daemon's /v1/mcp endpoint
// and returns it (with a cleanup that closes the session). The session is
// already initialized (Connect runs the MCP handshake).
func mcpClient(t *testing.T, baseURL string) *mcp.ClientSession {
	return mcpClientWithHTTPClient(t, baseURL, nil)
}

func mcpClientWithHTTPClient(t *testing.T, baseURL string, httpClient *http.Client) *mcp.ClientSession {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	transport := &mcp.StreamableClientTransport{Endpoint: baseURL + "/v1/mcp", HTTPClient: httpClient}
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "thundersnap-e2e-test",
		Version: "test",
	}, nil)

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("MCP Connect to %s: %v", transport.Endpoint, err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Logf("MCP session close: %v", err)
		}
	})

	// Sanity: the server must report the sandbox and background-job tools.
	listCtx, listCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer listCancel()
	tools, err := session.ListTools(listCtx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := map[string]bool{}
	for _, tool := range tools.Tools {
		got[tool.Name] = true
	}
	for _, want := range []string{
		"jobs", "jobs_list", "jobs_kill",
		"view", "create_file", "str_replace", "list_frames",
	} {
		if !got[want] {
			t.Fatalf("MCP server missing tool %q; got %d tools", want, len(tools.Tools))
		}
	}
	for _, removed := range []string{"bash", "jobs_wait"} {
		if got[removed] {
			t.Fatalf("MCP server still advertises removed tool %q", removed)
		}
	}
	t.Logf("MCP server advertises %d tools (all expected tools present)", len(tools.Tools))
	return session
}

// callTool invokes a named tool with the given arguments and returns the text
// of the first TextContent block plus the IsError flag. It fatals on a
// transport-level error (the call didn't reach the server) but returns
// IsError=true results normally — those are tool-level failures the LLM is
// meant to see, and the test asserts on them.
func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) (text string, isError bool) {
	t.Helper()
	return callToolForConversation(t, session, "e2e-default-conversation", name, args)
}

func callToolForConversation(t *testing.T, session *mcp.ClientSession, conversation, name string, args map[string]any) (text string, isError bool) {
	t.Helper()

	// Most background-job tests predate the combined jobs API. Translate their
	// single-launch/wait helper calls at the test boundary while still invoking
	// only the public jobs tool. Dedicated tests below exercise the
	// native batched shape directly.
	legacyLaunch := name == "bash"
	if args == nil {
		args = map[string]any{}
	}
	if legacyLaunch {
		if _, ok := args["user"]; !ok {
			args["user"] = "user"
		}
		name = "jobs"
		args = map[string]any{"launch": []any{args}}
	} else if name == "jobs_wait" {
		oldIDs, _ := args["job_ids"].([]string)
		jobs := make([]map[string]any, len(oldIDs))
		for i, id := range oldIDs {
			jobs[i] = map[string]any{"id": id, "offset": 0}
		}
		wait := map[string]any{"jobs": jobs, "until": args["until"], "timeout": args["timeout"]}
		if signal, ok := args["signal"]; ok {
			wait["signal"] = signal
		}
		args = map[string]any{"wait": wait}
		name = "jobs"
	} else if name == "view" || name == "create_file" {
		if _, ok := args["user"]; !ok {
			args["user"] = "user"
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	meta := mcp.Meta{}
	if conversation != "" {
		meta[apertureConversationIDMetaKeyE2E] = conversation
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Meta:      meta,
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("CallTool %q: %v", name, err)
	}
	if len(res.Content) == 0 {
		return "", res.IsError
	}
	if tc, ok := res.Content[0].(*mcp.TextContent); ok {
		if legacyLaunch && !res.IsError {
			var batch struct {
				Revision uint64           `json:"revision"`
				Jobs     []map[string]any `json:"jobs"`
			}
			if err := json.Unmarshal([]byte(tc.Text), &batch); err != nil || len(batch.Jobs) != 1 {
				t.Fatalf("decode jobs launch result %q: %+v err=%v", tc.Text, batch, err)
			}
			compat, _ := json.Marshal(map[string]any{"job_id": batch.Jobs[0]["id"], "revision": batch.Revision, "job": batch.Jobs[0]})
			return string(compat), false
		}
		return tc.Text, res.IsError
	}
	// Fall back to a JSON dump for non-text content.
	b, _ := json.Marshal(res.Content)
	return string(b), res.IsError
}

const (
	apertureConversationIDMetaKeyE2E = "io.tailscale.aperture/conversation-id"
	apertureConversationIDHeaderE2E  = "X-Aperture-Conversation-Id"
)

type apertureConversationTransport struct {
	conversation string
	base         http.RoundTripper
}

func (t apertureConversationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set(apertureConversationIDHeaderE2E, t.conversation)
	return t.base.RoundTrip(r)
}

// waitForMCPJob waits for one job to exit and returns its status.
func waitForMCPJob(t *testing.T, session *mcp.ClientSession, jobID string, revision uint64) map[string]any {
	t.Helper()
	out, isErr := callTool(t, session, "jobs_wait", map[string]any{
		"job_ids": []string{jobID}, "until": "any_exit", "timeout": 60,
	})
	if isErr {
		t.Fatalf("wait job %s: %s", jobID, out)
	}
	var result struct {
		Reason string           `json:"reason"`
		Jobs   []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("wait job %s unmarshal %q: %v", jobID, out, err)
	}
	if result.Reason != "any_exit" || len(result.Jobs) != 1 {
		t.Fatalf("wait job %s result = %s", jobID, out)
	}
	listed, listErr := callTool(t, session, "jobs_list", map[string]any{"job_ids": []string{jobID}})
	if listErr {
		t.Fatalf("list job %s: %s", jobID, listed)
	}
	var detail struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(listed), &detail); err != nil || len(detail.Jobs) != 1 {
		t.Fatalf("decode listed job %s: %s", jobID, listed)
	}
	return detail.Jobs[0]
}

func startAndWaitMCPBash(t *testing.T, session *mcp.ClientSession, args map[string]any) map[string]any {
	t.Helper()
	out, isErr := callTool(t, session, "bash", args)
	if isErr {
		t.Fatalf("start bash: %s", out)
	}
	var result struct {
		JobID    string `json:"job_id"`
		Revision uint64 `json:"revision"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil || result.JobID == "" {
		t.Fatalf("start bash result %q: job=%+v err=%v", out, result, err)
	}
	return waitForMCPJob(t, session, result.JobID, result.Revision)
}

// createFrameForMCP creates a frame the way the rest of the e2e suite does
// (via `ts frame` over SSH) so the MCP tools have a real frame to target. The
// SSH path is already proven by the other e2e tests; reusing it here keeps the
// MCP test focused on the MCP surface.
func createFrameForMCP(t *testing.T, d *daemonInstance, refName string) string {
	t.Helper()
	return createFrameViaDaemon(t, d, refName)
}

// installBusyboxAppletsInFrame installs the named busybox applets into a
// frame's /bin over the daemon's SFTP subsystem. A nil:nil:nil frame ships
// only the `ts` binary (plus /bin/sh and /bin/su symlinks to it); the MCP
// view/bash tool command builders call out to standard POSIX utilities
// (awk, find, head, sort, ...), so the e2e harness must install them the same
// way the rest of the suite does (see installBusyboxAppletInFrame). Each
// applet is a copy of the host's busybox-static, which dispatches on argv[0].
// create_file and str_replace are host-side and need no in-frame utilities.
func installBusyboxAppletsInFrame(t *testing.T, d *daemonInstance, refName string, applets ...string) {
	t.Helper()
	for _, a := range applets {
		installBusyboxAppletInFrame(t, d, refName, a)
	}
}

// mcpFrameApplets are the POSIX utilities the MCP tool command builders
// invoke inside a frame. They are installed once per frame so the bash/view
// tools work in a minimal nil:nil:nil frame (which has only `ts`). create_file
// and str_replace are host-side and need nothing in-frame, so they contribute
// no applets here; view needs awk/find/sort/head/stat, and the timeout/reap
// tests need sleep/ps.
var mcpFrameApplets = []string{
	"awk",   // view (file): awk line-numbering
	"find",  // view (dir): find -maxdepth 2
	"sort",  // view (dir): | sort
	"head",  // view (dir): | head -200
	"stat",  // view (image): stat -c %s (also useful for general tests)
	"sleep", // timeout/reap tests: foreground long-running command
	"ps",    // reap tests: inspect leftover processes
}

func TestMCPJobWithApertureConversationHeader(t *testing.T) {
	env := newTestEnv(t)
	d, httpBase := startDaemonWithHTTP(t, env)
	createFrameForMCP(t, d, "mcpheader")

	const conversation = "b8f9e7f1313506d1"
	httpClient := &http.Client{Transport: apertureConversationTransport{
		conversation: conversation,
		base:         http.DefaultTransport,
	}}
	session := mcpClientWithHTTPClient(t, httpBase, httpClient)

	// Deliberately omit MCP _meta: this models Aperture Chat, which supplies the
	// conversation only as an HTTP header. A combined launch+wait exercises both
	// internal requests created by the public jobs wrapper.
	out, isErr := callToolForConversation(t, session, "", "jobs", map[string]any{
		"launch": []any{map[string]any{
			"command": "echo aperture-header-ok",
			"frame":   "mcpheader",
			"user":    "root",
		}},
		"wait": map[string]any{"until": "all_exit", "timeout": 60},
	})
	if isErr {
		t.Fatalf("header-scoped job failed: %s", out)
	}
	if !strings.Contains(out, "aperture-header-ok") {
		t.Fatalf("header-scoped job output = %q, want marker", out)
	}
}

// TestMCPToolsRoundTrip is the core MCP e2e test (Phase 4, T5). It exercises
// every tool end-to-end against a fresh frame: list_frames before and after
// frame creation, jobs (zero + non-zero exit), view (file + dir +
// missing), create_file, str_replace (success + not-unique + not-found).
//
// It does NOT cover timeout/cap/cancellation (T2–T4) or identity (T8); those
// are separate, slower tests so this one stays fast and hits the happy path.
func TestMCPToolsRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	d, httpBase := startDaemonWithHTTP(t, env)
	session := mcpClient(t, httpBase)

	// --- list_frames on a fresh user is empty ---
	if out, isErr := callTool(t, session, "list_frames", nil); isErr {
		t.Fatalf("list_frames on fresh user: unexpected error %q", out)
	} else {
		var res struct {
			Frames []struct {
				UUID   string   `json:"uuid"`
				Refs   []string `json:"refs"`
				Status string   `json:"status"`
			} `json:"frames"`
		}
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("list_frames: unmarshal %q: %v", out, err)
		}
		if len(res.Frames) != 0 {
			t.Errorf("list_frames on fresh user: want 0 frames, got %d (%+v)", len(res.Frames), res.Frames)
		}
	}

	// --- create a real frame via SSH (ts frame), so MCP has something to target ---
	createFrameForMCP(t, d, "mcpframe")
	// nil:nil:nil frames ship only `ts`; install the POSIX utilities the tool
	// command builders call (mkdir/awk/find/head/sort/base64/...). str_replace
	// is host-side and needs nothing in-frame.
	installBusyboxAppletsInFrame(t, d, "mcpframe", mcpFrameApplets...)

	// --- list_frames now shows it, named by the ref ---
	{
		out, _ := callTool(t, session, "list_frames", nil)
		var res struct {
			Frames []struct {
				UUID   string   `json:"uuid"`
				Refs   []string `json:"refs"`
				Status string   `json:"status"`
			} `json:"frames"`
		}
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("list_frames after create: unmarshal %q: %v", out, err)
		}
		found := false
		for _, f := range res.Frames {
			if slices.Contains(f.Refs, "mcpframe") {
				found = true
				if f.Status != "stopped" {
					t.Errorf("frame %s status = %q, want stopped (no active sessions)", f.UUID, f.Status)
				}
			}
		}
		if !found {
			t.Fatalf("list_frames after create: ref %q not in %s", "mcpframe", out)
		}
	}

	// --- background bash: zero exit + live log ---
	{
		job := startAndWaitMCPBash(t, session, map[string]any{
			"command": "echo hello-mcp",
			"frame":   "mcpframe",
		})
		if job["state"] != "exited" || job["exit_code"] != float64(0) || job["user"] != "user" {
			t.Fatalf("bash echo job (including default non-root user) = %+v", job)
		}
	}

	// --- background bash: non-zero exit is recorded on the job ---
	{
		job := startAndWaitMCPBash(t, session, map[string]any{
			"command": "echo to-stderr >&2; exit 3",
			"frame":   "mcpframe",
			"user":    "root",
		})
		if job["state"] != "exited" || job["exit_code"] != float64(3) || job["user"] != "root" {
			t.Fatalf("bash exit 3 job = %+v", job)
		}
	}

	// --- create_file ---
	{
		out, isErr := callTool(t, session, "create_file", map[string]any{
			"path":      "/work/poem.txt",
			"file_text": "roses are red\nviolets are blue\nroses are red\n",
			"frame":     "mcpframe",
		})
		if isErr {
			t.Fatalf("create_file: unexpected error %q", out)
		}
		if !strings.Contains(out, "Created /work/poem.txt") {
			t.Errorf("create_file: output %q missing 'Created'", out)
		}
	}

	// --- view: the file we just created, with line numbers ---
	{
		out, isErr := callTool(t, session, "view", map[string]any{
			"path":  "/work/poem.txt",
			"frame": "mcpframe",
		})
		if isErr {
			t.Fatalf("view poem.txt: unexpected error %q", out)
		}
		if !strings.Contains(out, "roses are red") {
			t.Errorf("view poem.txt: output %q missing content", out)
		}
		// awk prefixes "    %6d\t" — line numbers should appear.
		if !strings.Contains(out, "\troses are red") {
			t.Errorf("view poem.txt: output %q missing line-numbered format", out)
		}
	}

	// --- view: a directory listing ---
	{
		out, isErr := callTool(t, session, "view", map[string]any{
			"path":  "/work",
			"frame": "mcpframe",
		})
		if isErr {
			t.Fatalf("view /work: unexpected error %q", out)
		}
		if !strings.Contains(out, "poem.txt") {
			t.Errorf("view /work: output %q does not list poem.txt", out)
		}
	}

	// --- view: missing path is an error result ---
	{
		out, isErr := callTool(t, session, "view", map[string]any{
			"path":  "/work/does-not-exist",
			"frame": "mcpframe",
		})
		if !isErr {
			t.Errorf("view missing path: expected IsError=true, got false (output %q)", out)
		}
		if !strings.Contains(out, "not found") {
			t.Errorf("view missing path: output %q missing 'not found'", out)
		}
	}

	// --- str_replace: success ---
	{
		out, isErr := callTool(t, session, "str_replace", map[string]any{
			"path":    "/work/poem.txt",
			"old_str": "violets are blue",
			"new_str": "violets are MCP",
			"frame":   "mcpframe",
		})
		if isErr {
			t.Fatalf("str_replace (unique): unexpected error %q", out)
		}
		if !strings.Contains(out, "Replaced in /work/poem.txt") {
			t.Errorf("str_replace (unique): output %q missing 'Replaced in'", out)
		}
	}

	// --- str_replace: not unique (roses are red appears twice) ---
	{
		out, isErr := callTool(t, session, "str_replace", map[string]any{
			"path":    "/work/poem.txt",
			"old_str": "roses are red",
			"new_str": "x",
			"frame":   "mcpframe",
		})
		if !isErr {
			t.Errorf("str_replace (not unique): expected IsError=true, got false (output %q)", out)
		}
		if !strings.Contains(out, "2 times") && !strings.Contains(out, "must be unique") {
			t.Errorf("str_replace (not unique): output %q missing uniqueness error", out)
		}
	}

	// --- str_replace: not found ---
	{
		out, isErr := callTool(t, session, "str_replace", map[string]any{
			"path":    "/work/poem.txt",
			"old_str": "this string is absent",
			"new_str": "x",
			"frame":   "mcpframe",
		})
		if !isErr {
			t.Errorf("str_replace (not found): expected IsError=true, got false (output %q)", out)
		}
		if !strings.Contains(out, "not found") {
			t.Errorf("str_replace (not found): output %q missing 'not found'", out)
		}
	}

	// --- verify the successful str_replace actually changed the file ---
	{
		out, _ := callTool(t, session, "view", map[string]any{
			"path":  "/work/poem.txt",
			"frame": "mcpframe",
		})
		if !strings.Contains(out, "violets are MCP") {
			t.Errorf("post-replace view: output %q does not contain the replacement", out)
		}
		if strings.Contains(out, "violets are blue") {
			t.Errorf("post-replace view: output %q still contains the old string", out)
		}
	}

	// --- bash start: bad frame errors cleanly ---
	{
		out, isErr := callTool(t, session, "bash", map[string]any{
			"command": "echo anything",
			"frame":   "this-frame-does-not-exist",
		})
		if !isErr {
			t.Errorf("bash bad frame: expected IsError=true, got false (output %q)", out)
		}
		if !strings.Contains(out, "resolve frame") && !strings.Contains(out, "not found") {
			t.Errorf("bash bad frame: output %q missing resolve/not-found error", out)
		}
	}
}

// TestMCPCreateFileHostSideMinimalFrame pins the host-side create_file
// implementation: it must create a file in a fresh nil:nil:nil frame that has
// NO POSIX utilities installed (no mkdir/dirname/base64). The old exec-based
// builder failed with "mkdir: executable file not found in $PATH" in such a
// frame, which is exactly what happens when an MCP caller omits the frame
// selector and lands in a throwaway minimal frame. This test also verifies
// the file is owned by the frame's "user" account (so shell jobs running as
// that user can overwrite it) and that parent directories are created.
func TestMCPCreateFileHostSideMinimalFrame(t *testing.T) {
	env := newTestEnv(t)
	d, httpBase := startDaemonWithHTTP(t, env)
	session := mcpClient(t, httpBase)

	// Create a real frame but deliberately do NOT install any busybox applets —
	// the frame ships only `ts`, /bin/sh, and /bin/su. create_file must not
	// depend on mkdir/dirname/base64 being present.
	frameUUID := createFrameForMCP(t, d, "minimalframe")
	hostFramePath := filepath.Join(env.fsDir, "e2e", frameUUID)

	// create_file into a nested path that does not exist yet (exercises
	// parent-dir creation) with non-ASCII content (exercises binary-safety).
	const content = "héllo, thundersnap! \n\ttab-indented\n"
	out, isErr := callTool(t, session, "create_file", map[string]any{
		"path":      "/work/sub/dir/app.py",
		"file_text": content,
		"frame":     "minimalframe",
	})
	if isErr {
		t.Fatalf("create_file in minimal frame: unexpected error %q", out)
	}
	if !strings.Contains(out, "Created /work/sub/dir/app.py") {
		t.Fatalf("create_file: output %q missing 'Created'", out)
	}

	// Confirm host-side that the file landed where we expect: under the
	// frame's work subvolume, owned by the thundersnap user (7575:7575) so
	// shell jobs running as that user can overwrite it.
	hostFile := filepath.Join(hostFramePath, "work/sub/dir/app.py")
	hInfo, err := os.Stat(hostFile)
	if err != nil {
		t.Fatalf("host-side stat %s: %v", hostFile, err)
	}
	if int64(len(content)) != hInfo.Size() {
		t.Errorf("host file size = %d, want %d", hInfo.Size(), len(content))
	}
	if stat, ok := hInfo.Sys().(*syscall.Stat_t); ok {
		if stat.Uid != 7575 || stat.Gid != 7575 {
			t.Errorf("host file owner = %d:%d, want 7575:7575", stat.Uid, stat.Gid)
		}
	}

	// Read the file back via a shell job that uses only sh builtins (test/echo)
	// so it works in the minimal frame with no POSIX applets installed. Use a
	// combined jobs launch+wait so the output is returned in one call (the
	// jobs_list helper used by startAndWaitMCPBash omits output).
	verifyOut, isErr := callTool(t, session, "jobs", map[string]any{
		"launch": []any{map[string]any{
			"command": "test -f /work/sub/dir/app.py && echo EXISTS; if command -v stat >/dev/null 2>&1; then stat -c '%u:%g' /work/sub/dir/app.py; else echo no-stat; fi",
			"frame":   "minimalframe",
			"user":    "user",
		}},
		"wait": map[string]any{"until": "all_exit", "timeout": 60},
	})
	if isErr {
		t.Fatalf("verify job in minimal frame: %s", verifyOut)
	}
	if !strings.Contains(verifyOut, "EXISTS") {
		t.Fatalf("verify job: file not reported as existing; output=%q", verifyOut)
	}
	// If stat is available, the owner should be 7575:7575 (the thundersnap
	// user); if not, the no-stat line is fine (the core pin is existence).
	if strings.Contains(verifyOut, "no-stat") {
		// stat unavailable in the minimal frame; skip the ownership check.
	} else if strings.Contains(verifyOut, ":") {
		// stat is available: extract the owner line and check it.
		for _, line := range strings.Split(verifyOut, "\n") {
			if strings.Contains(line, ":") && !strings.Contains(line, "EXISTS") {
				owner := strings.TrimSpace(line)
				if owner != "7575:7575" {
					t.Errorf("create_file: file owner = %q, want 7575:7575", owner)
				}
			}
		}
	}

	// Overwrite the file (must preserve existing owner/mode, not error) and
	// confirm the new content lands byte-for-byte.
	out, isErr = callTool(t, session, "create_file", map[string]any{
		"path":      "/work/sub/dir/app.py",
		"file_text": "overwritten\n",
		"frame":     "minimalframe",
	})
	if isErr {
		t.Fatalf("create_file overwrite: unexpected error %q", out)
	}

	// create_file must refuse to clobber a directory.
	out, isErr = callTool(t, session, "create_file", map[string]any{
		"path":      "/work/sub",
		"file_text": "x",
		"frame":     "minimalframe",
	})
	if !isErr {
		t.Errorf("create_file over directory: expected IsError=true, got false (output %q)", out)
	}
	if !strings.Contains(out, "is a directory") {
		t.Errorf("create_file over directory: output %q missing 'is a directory'", out)
	}
}

// TestMCPHTTPMuxResponds (Phase 4, T1) confirms the test-mode HTTP mux serves
// the non-MCP endpoints too — a quick smoke that the pre-existing coverage
// gap (HTTP handlers never instantiated in tests) is closed. It only pokes
// /ts/servers.json and /v1/mcp (initialize) since mesh/metrics are exercised
// in their own package tests.
func TestMCPHTTPMuxResponds(t *testing.T) {
	env := newTestEnv(t)
	_, httpBase := startDaemonWithHTTP(t, env)

	// /v1/mcp must exist and speak MCP (the client does a full initialize).
	session := mcpClient(t, httpBase)
	if err := session.Close(); err != nil {
		t.Logf("MCP session close: %v", err)
	}
}

// TestMCPTimeoutAndReap pins background-job hard-timeout cleanup: closing the
// vshd connection must reap the whole process group, including grandchildren.
func TestMCPTimeoutAndReap(t *testing.T) {
	env := newTestEnv(t)
	d, httpBase := startDaemonWithHTTP(t, env)
	session := mcpClient(t, httpBase)
	createFrameForMCP(t, d, "timeoutframe")
	installBusyboxAppletsInFrame(t, d, "timeoutframe", "sleep", "ps")

	start := time.Now()
	out, isErr := callTool(t, session, "bash", map[string]any{
		"command":      "sleep 30 & sleep 30",
		"frame":        "timeoutframe",
		"hard_timeout": 2,
	})
	if isErr {
		t.Fatalf("start timeout job: %s", out)
	}
	var started struct {
		JobID    string `json:"job_id"`
		Revision uint64 `json:"revision"`
	}
	if err := json.Unmarshal([]byte(out), &started); err != nil {
		t.Fatalf("unmarshal timeout start %q: %v", out, err)
	}
	job := waitForMCPJob(t, session, started.JobID, started.Revision)
	elapsed := time.Since(start)
	if job["state"] != "timed_out" {
		t.Fatalf("timeout job state = %+v", job)
	}
	if elapsed > 20*time.Second {
		t.Errorf("timeout job took %v, expected ~2s", elapsed)
	}
	t.Logf("timeout job returned in %v: %+v", elapsed, job)

	// Give vshd a moment to tear down the process group after the conn close.
	time.Sleep(1500 * time.Millisecond)

	// Assert no leftover `sleep 30` processes in the frame's PID namespace.
	// ps (busybox) lists every process in the namespace; parse in Go so we
	// don't need a `grep` applet.
	psOut, _, err := sshExec(t, d, "root@timeoutframe", "ps")
	if err != nil {
		t.Fatalf("ssh ps after timeout: %v", err)
	}
	t.Logf("ps after timeout:\n%s", psOut)
	// ps itself and the sh that ran it appear in the output; only `sleep 30`
	// would indicate a leak.
	if n := strings.Count(psOut, "sleep 30"); n > 0 {
		t.Errorf("timeout left %d 'sleep 30' process(es) behind (process-group reap failed):\n%s", n, psOut)
	}
}

// TestMCPFrameResolution (Phase 4, T6) covers the `frame` argument semantics:
//   - frame="" auto-creates a fresh unattached frame and runs there; the new
//     frame then appears in list_frames (proving runInFrame's auto-create path
//     and the frames.Store sidecar are wired together).
//   - frame=<uuid> resolves a frame by UUID.
//   - frame=<ref> resolves a frame by ref name.
//   - frame=<bad> errors cleanly with a resolve error.
//
// It does not depend on timeout/reap; it's purely the frame-resolution matrix.
func TestMCPFrameResolution(t *testing.T) {
	env := newTestEnv(t)
	d, httpBase := startDaemonWithHTTP(t, env)
	session := mcpClient(t, httpBase)

	// --- frame="" auto-creates a fresh frame ---
	// A fresh user has no frames; bash with frame="" must auto-create one and
	// run there. We write a marker file we can later read back by UUID.
	autoJob := startAndWaitMCPBash(t, session, map[string]any{
		"command": "echo auto-created > /work/marker.txt",
		"frame":   "",
	})
	if autoJob["state"] != "exited" || autoJob["exit_code"] != float64(0) {
		t.Fatalf("bash frame=\"\" auto-create job: %+v", autoJob)
	}

	// list_frames must now show exactly one frame (the auto-created one). It's
	// unattached (no ref), so it's listed by UUID.
	listOut, isErr := callTool(t, session, "list_frames", nil)
	if isErr {
		t.Fatalf("list_frames after auto-create: unexpected error %q", listOut)
	}
	var lf struct {
		Frames []struct {
			UUID   string   `json:"uuid"`
			Refs   []string `json:"refs"`
			Status string   `json:"status"`
		} `json:"frames"`
	}
	if err := json.Unmarshal([]byte(listOut), &lf); err != nil {
		t.Fatalf("list_frames: unmarshal %q: %v", listOut, err)
	}
	if len(lf.Frames) != 1 {
		t.Fatalf("list_frames after auto-create: want 1 frame, got %d (%+v)", len(lf.Frames), lf.Frames)
	}
	autoUUID := lf.Frames[0].UUID
	if _, perr := uuidParse(autoUUID); perr != nil {
		t.Fatalf("auto-created frame name %q is not a UUID: %v", autoUUID, perr)
	}
	t.Logf("auto-created frame UUID: %s", autoUUID)
	installBusyboxAppletsInFrame(t, d, autoUUID, "awk")

	// --- frame=<uuid> resolves the auto-created frame and reads the marker ---
	// Use shell builtins (read/echo) rather than `cat`, which isn't present in a
	// nil:nil:nil frame; this keeps the auto-create path applet-free.
	uuidOut, isErr := callTool(t, session, "view", map[string]any{
		"path":       "/work/marker.txt",
		"tail_lines": 10,
		"frame":      autoUUID,
	})
	if isErr || !strings.Contains(uuidOut, "auto-created") {
		t.Errorf("view frame=<uuid>: isErr=%v output=%q", isErr, uuidOut)
	}

	// --- frame=<ref> resolves a named ref ---
	refUUID := createFrameForMCP(t, d, "namedframe")
	installBusyboxAppletsInFrame(t, d, "namedframe", "awk")
	refJob := startAndWaitMCPBash(t, session, map[string]any{
		"command": "echo via-ref > /work/from-ref.txt",
		"frame":   "namedframe",
	})
	if refJob["state"] != "exited" || refJob["exit_code"] != float64(0) {
		t.Fatalf("bash frame=<ref> job: %+v", refJob)
	}
	// Verify via UUID that the ref-resolved call landed in the right frame.
	uuidReadOut, isErr := callTool(t, session, "view", map[string]any{
		"path":       "/work/from-ref.txt",
		"tail_lines": 10,
		"frame":      refUUID,
	})
	if isErr {
		t.Fatalf("view frame=<ref-uuid> readback: unexpected error %q", uuidReadOut)
	}
	if !strings.Contains(uuidReadOut, "via-ref") {
		t.Errorf("frame=<ref> did not land in the ref's frame: readback %q", uuidReadOut)
	}

	// --- frame=<bad> errors cleanly ---
	badOut, isErr := callTool(t, session, "bash", map[string]any{
		"command": "echo nope",
		"frame":   "definitely-not-a-real-frame",
	})
	if !isErr {
		t.Errorf("bash frame=<bad>: expected IsError=true, got false (output %q)", badOut)
	}
	if !strings.Contains(badOut, "resolve frame") && !strings.Contains(badOut, "no such frame") && !strings.Contains(badOut, "not found") {
		t.Errorf("bash frame=<bad>: output %q missing resolve/not-found error", badOut)
	}
}

// TestMCPImplicitFrameIsStableAcrossSessions verifies the frame omitted by an
// MCP caller is stable once the user's only frame has been auto-created. This
// is important for callers that have a bootstrapping MCP session followed by a
// secondary session: resolving "" to a new UUID on each request silently
// changes both the filesystem and the installed command set, which looks like
// intermittent PATH and wrong-frame failures.
func TestMCPImplicitFrameIsStableAcrossSessions(t *testing.T) {
	env := newTestEnv(t)
	_, httpBase := startDaemonWithHTTP(t, env)
	sessionA := mcpClient(t, httpBase)
	sessionB := mcpClient(t, httpBase)

	// The first call creates the user's only unattached frame. Use only shell
	// builtins plus the always-present ts binary, so this also checks the
	// deterministic session PATH in a nil:nil:nil frame.
	first := startAndWaitMCPBash(t, sessionA, map[string]any{
		"command": "printf '%s\\n' \\\"$PATH\\\" > /work/path.txt; printf '%s\\n' \\\"$PWD\\\" > /work/pwd.txt; command -v ts > /work/tspath.txt",
		"frame":   "",
	})
	if first["state"] != "exited" || first["exit_code"] != float64(0) {
		t.Fatalf("implicit-frame bootstrap job: %+v", first)
	}

	// A second session, with the same user but a distinct MCP connection, must
	// land in the same frame when no frame is supplied.
	second := startAndWaitMCPBash(t, sessionB, map[string]any{
		"command": "test -f /work/path.txt && test -f /work/pwd.txt && test -x /bin/ts && test -s /work/tspath.txt",
		"frame":   "",
	})
	if second["state"] != "exited" || second["exit_code"] != float64(0) {
		t.Fatalf("implicit-frame secondary job landed in a different frame or lost PATH: %+v", second)
	}

	// list_frames must still contain exactly one frame, not one fresh frame per
	// omitted-frame request.
	out, isErr := callTool(t, sessionB, "list_frames", nil)
	if isErr {
		t.Fatalf("list_frames after implicit-frame calls: %s", out)
	}
	var listed struct {
		Frames []struct {
			UUID string `json:"uuid"`
		} `json:"frames"`
	}
	if err := json.Unmarshal([]byte(out), &listed); err != nil {
		t.Fatalf("list_frames after implicit-frame calls: %v (%s)", err, out)
	}
	if len(listed.Frames) != 1 {
		t.Fatalf("implicit-frame calls created %d frames, want 1: %s", len(listed.Frames), out)
	}
}

// TestMCPJobsBatch launches two jobs and waits for exactly that batch in one
// tool call. It pins the primary public API, concurrent execution, inline
// output, and job-ID log lookup without copying a frame or path.
func TestMCPJobsBatch(t *testing.T) {
	env := newTestEnv(t)
	d, httpBase := startDaemonWithHTTP(t, env)
	session := mcpClient(t, httpBase)
	createFrameForMCP(t, d, "batchframe")
	installBusyboxAppletsInFrame(t, d, "batchframe", "sleep")

	start := time.Now()
	out, isErr := callTool(t, session, "jobs", map[string]any{
		"launch": []any{
			map[string]any{"command": "sleep 2; echo first", "frame": "batchframe", "label": "first", "user": "user"},
			map[string]any{"command": "sleep 2; echo second", "frame": "batchframe", "user": "user"},
		},
		"wait": map[string]any{"until": "all_exit", "timeout": 10},
	})
	if isErr {
		t.Fatalf("batched jobs: %s", out)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("two 2s jobs took %v; expected concurrent execution", elapsed)
	}
	var result struct {
		Reason string           `json:"reason"`
		Jobs   []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil || result.Reason != "all_exit" || len(result.Jobs) != 2 {
		t.Fatalf("batched jobs result %q: %+v err=%v", out, result, err)
	}
	for i, want := range []string{"first", "second"} {
		if result.Jobs[i]["state"] != "exited" || result.Jobs[i]["exit_code"] != float64(0) || !strings.Contains(result.Jobs[i]["output"].(string), want) {
			t.Errorf("job %d = %+v; want exited output containing %q", i, result.Jobs[i], want)
		}
	}
}

// TestMCPBackgroundJobWaitAndKill covers live-output waits, reading a log
// before exit, explicit kill, and the terminal killed state.
func TestMCPBackgroundJobWaitAndKill(t *testing.T) {
	env := newTestEnv(t)
	d, httpBase := startDaemonWithHTTP(t, env)
	session := mcpClient(t, httpBase)
	createFrameForMCP(t, d, "killframe")
	installBusyboxAppletsInFrame(t, d, "killframe", "sleep")

	out, isErr := callTool(t, session, "bash", map[string]any{
		"command": "echo ready; sleep 30 & sleep 30",
		"frame":   "killframe",
	})
	if isErr {
		t.Fatalf("start kill job: %s", out)
	}
	var started struct {
		JobID    string `json:"job_id"`
		Revision uint64 `json:"revision"`
		Job      struct {
			Log string `json:"log"`
		} `json:"job"`
	}
	if err := json.Unmarshal([]byte(out), &started); err != nil {
		t.Fatalf("unmarshal start %q: %v", out, err)
	}

	out, isErr = callTool(t, session, "jobs_wait", map[string]any{
		"job_ids": []string{started.JobID}, "after_revision": started.Revision,
		"until": "output", "timeout": 10,
	})
	if isErr || !strings.Contains(out, `"reason":"output"`) {
		t.Fatalf("wait output: isErr=%v output=%q", isErr, out)
	}

	// Signals are delivered to the shared vshdsession process group without
	// escalation. STOP and CONT are both accepted; the job remains tracked.
	out, isErr = callTool(t, session, "jobs_wait", map[string]any{
		"job_ids": []string{started.JobID}, "until": "all_exit", "timeout": 1, "signal": "STOP",
	})
	if isErr || !strings.Contains(out, `"reason":"timeout"`) {
		t.Fatalf("STOP wait: isErr=%v output=%q", isErr, out)
	}
	out, isErr = callTool(t, session, "jobs_wait", map[string]any{
		"job_ids": []string{started.JobID}, "until": "output", "timeout": 1, "signal": "CONT",
	})
	if isErr {
		t.Fatalf("CONT wait: %s", out)
	}

	out, isErr = callTool(t, session, "jobs_kill", map[string]any{"job_ids": []string{started.JobID}})
	if isErr {
		t.Fatalf("kill job: %s", out)
	}
	var killed struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(out), &killed); err != nil || len(killed.Jobs) != 1 {
		t.Fatalf("unmarshal kill %q: %+v err=%v", out, killed, err)
	}
	if killed.Jobs[0]["state"] != "killed" {
		t.Fatalf("killed job state = %+v", killed.Jobs[0])
	}
}

// TestMCPBackgroundJobsConversationScope covers the chat-level ownership
// contract and actual parallel execution despite sequential tool dispatch.
func TestMCPBackgroundJobsConversationScope(t *testing.T) {
	env := newTestEnv(t)
	d, httpBase := startDaemonWithHTTP(t, env)
	sessionA := mcpClient(t, httpBase)
	sessionB := mcpClient(t, httpBase)
	createFrameForMCP(t, d, "jobsframe")
	installBusyboxAppletsInFrame(t, d, "jobsframe", "sleep")

	if out, isErr := callToolForConversation(t, sessionA, "", "jobs_list", nil); !isErr || !strings.Contains(out, "conversation ID") {
		t.Fatalf("missing conversation metadata: isErr=%v output=%q", isErr, out)
	}

	start := func(session *mcp.ClientSession, conversation, command string) (string, uint64) {
		out, isErr := callToolForConversation(t, session, conversation, "bash", map[string]any{
			"command": command,
			"frame":   "jobsframe",
		})
		if isErr {
			t.Fatalf("start %s: %s", conversation, out)
		}
		var r struct {
			JobID    string `json:"job_id"`
			Revision uint64 `json:"revision"`
		}
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("unmarshal start %q: %v", out, err)
		}
		return r.JobID, r.Revision
	}

	wallStart := time.Now()
	jobA1, revA1 := start(sessionA, "conversation-a", "sleep 2; echo a1")
	jobA2, revA2 := start(sessionA, "conversation-a", "sleep 2; echo a2")
	jobB1, revB1 := start(sessionB, "conversation-b", "sleep 2; echo b1")
	if jobA1 != "j1" || jobA2 != "j2" || jobB1 != "j1" {
		t.Fatalf("conversation-local IDs: A=(%s,%s) B=%s", jobA1, jobA2, jobB1)
	}

	// A separate MCP connection with the same conversation sees A's jobs.
	out, isErr := callToolForConversation(t, sessionB, "conversation-a", "jobs_list", nil)
	if isErr || !strings.Contains(out, `"id":"j1"`) || !strings.Contains(out, `"id":"j2"`) {
		t.Fatalf("same conversation across MCP sessions: isErr=%v output=%s", isErr, out)
	}
	// Conversation B cannot address conversation A's j2.
	if out, isErr := callToolForConversation(t, sessionB, "conversation-b", "jobs_list", map[string]any{"job_ids": []string{"j2"}}); !isErr || !strings.Contains(out, "unknown job") {
		t.Fatalf("cross-conversation lookup: isErr=%v output=%q", isErr, out)
	}

	wait := func(conversation string, ids []string, revision uint64) {
		out, isErr := callToolForConversation(t, sessionA, conversation, "jobs_wait", map[string]any{
			"job_ids": ids, "after_revision": revision, "until": "all_exit", "timeout": 10,
		})
		if isErr || !strings.Contains(out, `"reason":"all_exit"`) {
			t.Fatalf("wait %s: isErr=%v output=%q", conversation, isErr, out)
		}
	}
	wait("conversation-a", []string{jobA1, jobA2}, revA2)
	wait("conversation-b", []string{jobB1}, revB1)
	if elapsed := time.Since(wallStart); elapsed > 5*time.Second {
		t.Errorf("three 2s jobs took %v; expected concurrent execution", elapsed)
	}
	_ = revA1
}

// TestMCPBackgroundJobWaitSemantics covers the wait contract that
// TestMCPBackgroundJobWaitAndKill does not: timeout results, already-satisfied
// all_exit returning immediately, and the output/any_exit revision predicates.
func TestMCPBackgroundJobWaitSemantics(t *testing.T) {
	env := newTestEnv(t)
	d, httpBase := startDaemonWithHTTP(t, env)
	session := mcpClient(t, httpBase)
	createFrameForMCP(t, d, "waitframe")
	installBusyboxAppletsInFrame(t, d, "waitframe", "sleep")

	// A job that stays alive long enough for the wait-timeout path, then exits.
	out, isErr := callTool(t, session, "bash", map[string]any{
		"command": "sleep 10",
		"frame":   "waitframe",
	})
	if isErr {
		t.Fatalf("start long job: %s", out)
	}
	var started struct {
		JobID    string `json:"job_id"`
		Revision uint64 `json:"revision"`
	}
	if err := json.Unmarshal([]byte(out), &started); err != nil {
		t.Fatalf("unmarshal start %q: %v", out, err)
	}

	// --- wait timeout returns reason=timeout, does NOT kill, no IsError ---
	waitStart := time.Now()
	out, isErr = callTool(t, session, "jobs_wait", map[string]any{
		"job_ids": []string{started.JobID}, "after_revision": started.Revision,
		"until": "all_exit", "timeout": 1,
	})
	if isErr {
		t.Fatalf("wait timeout: unexpected error %q", out)
	}
	if elapsed := time.Since(waitStart); elapsed > 5*time.Second {
		t.Errorf("wait timeout took %v, expected ~1s", elapsed)
	}
	if !strings.Contains(out, `"reason":"timeout"`) {
		t.Fatalf("wait timeout: output %q missing reason=timeout", out)
	}
	// The job must still be running (timeout does not kill).
	var toResult struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(out), &toResult); err != nil || len(toResult.Jobs) != 1 {
		t.Fatalf("wait timeout unmarshal %q: %+v err=%v", out, toResult, err)
	}
	if toResult.Jobs[0]["state"] != "running" {
		t.Fatalf("after timeout, job state = %+v (want running)", toResult.Jobs[0])
	}

	// --- kill it so the conversation has a terminal job ---
	if out, isErr := callTool(t, session, "jobs_kill", map[string]any{"job_ids": []string{started.JobID}}); isErr {
		t.Fatalf("kill long job: %s", out)
	}

	// --- already-satisfied all_exit returns immediately ---
	satStart := time.Now()
	out, isErr = callTool(t, session, "jobs_wait", map[string]any{
		"job_ids": []string{started.JobID}, "until": "all_exit", "timeout": 10,
	})
	if isErr || !strings.Contains(out, `"reason":"all_exit"`) {
		t.Fatalf("already-satisfied all_exit: isErr=%v output=%q", isErr, out)
	}
	if elapsed := time.Since(satStart); elapsed > 2*time.Second {
		t.Errorf("already-satisfied all_exit took %v, expected immediate", elapsed)
	}

	// --- all_exit on a fresh (jobless) conversation returns immediately ---
	// A different conversation that never started a job: all_exit is vacuously
	// satisfied and must not block for the full wait timeout.
	emptyStart := time.Now()
	out, isErr = callToolForConversation(t, session, "empty-conversation", "jobs_wait", map[string]any{
		"until": "all_exit", "timeout": 10,
	})
	if isErr || !strings.Contains(out, `"reason":"all_exit"`) {
		t.Fatalf("jobless all_exit: isErr=%v output=%q", isErr, out)
	}
	if elapsed := time.Since(emptyStart); elapsed > 2*time.Second {
		t.Errorf("jobless all_exit took %v, expected immediate", elapsed)
	}
}

// TestMCPBackgroundJobInputValidation covers the reject-before-launch checks
// and the tail_lines/view_range mutual exclusion, which the happy-path tests
// don't exercise.
func TestMCPBackgroundJobInputValidation(t *testing.T) {
	env := newTestEnv(t)
	d, httpBase := startDaemonWithHTTP(t, env)
	session := mcpClient(t, httpBase)
	createFrameForMCP(t, d, "validframe")
	installBusyboxAppletsInFrame(t, d, "validframe", "sleep")

	// --- empty command is rejected ---
	if out, isErr := callTool(t, session, "bash", map[string]any{"command": "", "frame": "validframe"}); !isErr || !strings.Contains(out, "command is required") {
		t.Errorf("empty command: isErr=%v output=%q", isErr, out)
	}
	// --- bad user is rejected ---
	if out, isErr := callTool(t, session, "bash", map[string]any{"command": "true", "frame": "validframe", "user": "nobody"}); !isErr || !strings.Contains(out, "user is required") {
		t.Errorf("bad user: isErr=%v output=%q", isErr, out)
	}
	// --- bad until is rejected ---
	if out, isErr := callTool(t, session, "jobs_wait", map[string]any{"until": "never"}); !isErr || !strings.Contains(out, "invalid until") {
		t.Errorf("bad until: isErr=%v output=%q", isErr, out)
	}
	// --- kill with no job_ids is rejected ---
	if out, isErr := callTool(t, session, "jobs_kill", map[string]any{}); !isErr || !strings.Contains(out, "job_ids is required") {
		t.Errorf("kill no ids: isErr=%v output=%q", isErr, out)
	}
	// --- unknown job id is rejected ---
	if out, isErr := callTool(t, session, "jobs_list", map[string]any{"job_ids": []string{"j999"}}); !isErr || !strings.Contains(out, "unknown job") {
		t.Errorf("unknown job: isErr=%v output=%q", isErr, out)
	}
	// --- tail_lines and view_range are mutually exclusive ---
	if out, isErr := callTool(t, session, "view", map[string]any{"path": "/work", "tail_lines": 5, "view_range": []int{1, 10}, "frame": "validframe"}); !isErr || !strings.Contains(out, "mutually exclusive") {
		t.Errorf("tail+range: isErr=%v output=%q", isErr, out)
	}
	// --- negative tail_lines is rejected ---
	if out, isErr := callTool(t, session, "view", map[string]any{"path": "/work", "tail_lines": -1, "frame": "validframe"}); !isErr || !strings.Contains(out, "positive") {
		t.Errorf("negative tail: isErr=%v output=%q", isErr, out)
	}
}

// TestMCPBackgroundJobMultiFrameAndCounters covers one conversation launching
// jobs in different frames and listing them together, kill idempotency on a
// terminal job, and agreement between combined/stdout/stderr byte counters.
func TestMCPBackgroundJobMultiFrameAndCounters(t *testing.T) {
	env := newTestEnv(t)
	d, httpBase := startDaemonWithHTTP(t, env)
	session := mcpClient(t, httpBase)
	createFrameForMCP(t, d, "frameA")
	createFrameForMCP(t, d, "frameB")
	installBusyboxAppletsInFrame(t, d, "frameA", "sleep")
	installBusyboxAppletsInFrame(t, d, "frameB", "sleep")

	// Start one job in each frame; the conversation lists both together.
	jobA := startAndWaitMCPBash(t, session, map[string]any{
		"command": "echo stdout-only; echo stderr-only >&2",
		"frame":   "frameA",
	})
	jobB := startAndWaitMCPBash(t, session, map[string]any{
		"command": "echo b",
		"frame":   "frameB",
	})
	if jobA["frame"] == jobB["frame"] {
		t.Fatalf("jobs in different frames share a frame: %+v / %+v", jobA, jobB)
	}

	// List all jobs in the conversation: both must appear.
	out, isErr := callTool(t, session, "jobs_list", nil)
	if isErr {
		t.Fatalf("list all: %s", out)
	}
	if !strings.Contains(out, jobA["id"].(string)) || !strings.Contains(out, jobB["id"].(string)) {
		t.Fatalf("list all does not contain both jobs: %s", out)
	}

	// --- kill is idempotent on a terminal job ---
	out, isErr = callTool(t, session, "jobs_kill", map[string]any{"job_ids": []string{jobA["id"].(string)}})
	if isErr {
		t.Fatalf("kill terminal job: %s", out)
	}
	var killed struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(out), &killed); err != nil || len(killed.Jobs) != 1 {
		t.Fatalf("kill terminal unmarshal %q: %+v err=%v", out, killed, err)
	}
	// A terminal job keeps its original terminal state (exited), not "killed".
	if killed.Jobs[0]["state"] != "exited" {
		t.Errorf("kill on terminal job changed state to %v; want exited", killed.Jobs[0]["state"])
	}
}

// uuidParse parses a UUID string and returns an error if it's malformed. Used
// by TestMCPFrameResolution to assert the auto-created frame is listed by UUID.
func uuidParse(s string) (struct{}, error) {
	if len(s) != 36 || strings.Count(s, "-") != 4 {
		return struct{}{}, fmt.Errorf("not a UUID: %q", s)
	}
	return struct{}{}, nil
}
