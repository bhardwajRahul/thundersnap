// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// mcp.go implements the thundersnap MCP server, exposing sandbox-equivalent
// file/frame tools plus conversation-scoped background bash job tools on the
// daemon's HTTP listener at /v1/mcp.
//
// This is a port of Aperture's chat/sandbox tool set (tool_bash.go,
// tool_view.go, tool_create_file.go, tool_str_replace.go) and the
// Tool→MCP adapter (proxy/mcp.go chatToolToMCPHandler), per
// sandbox-mcp-design.md. Per the project rule we do NOT import the
// aperture module: the self-contained command builders and the adapter
// are copied here.
//
// The exec primitive is mcpexec.CollectFrames (thundersnap/mcpexec), the
// vshdproto-frame port of Aperture's CollectExec. The launcher below
// (runInFrame) is the daemon-specific glue: it resolves a frame, prepares
// its rootfs, dials the host vshd, sends the VMX one-shot header, and
// collects the result — the same path runContainerSession uses for SSH,
// minus the PTY.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tailscale/thundersnap/frameid"
	"github.com/tailscale/thundersnap/mcpexec"
	"github.com/tailscale/thundersnap/tsm"
	"golang.org/x/sys/unix"
)

// --- Globals set by main() from flags (production-only; empty in test mode) --
//
// These are the production MCP knobs. In test mode (--test-listen/--test-user)
// none of them are set: the endpoint serves with testModeUser as the identity
// and no trusted-peer / self-registration behaviour. See resolveMCPUser.

// mcpTrustedAperture is the Tailscale identity (login name) of the Aperture
// node permitted to set X-Aperture-Login on proxied MCP calls. Set via
// --mcp-trusted-aperture. Empty in test mode (no tsnet, so no trusted peer).
var mcpTrustedAperture string

// mcpRegisterURL, when non-empty, is the Aperture /v1/mcp/register URL to
// self-register with. Set via --mcp-register-url. Empty → serve passively.
var mcpRegisterURL string

// mcpDaemonVersion is the version reported in the MCP server handshake. There
// is no build-time version injection in this repo today; TODO: wire to jj/git
// describe or an ldflag once one exists. Hardcoded for now so the handshake
// has a non-empty version string (the MCP spec requires Implementation.Version).
const mcpDaemonVersion = "dev"

// --- Identity ---------------------------------------------------------------
//
// Thundersnap keys frames by Tailscale login name (fs/<login>/<uuid>/). The
// MCP endpoint resolves the effective user per HTTP request:
//
//   - Test mode (--test-user): testModeUser is the identity for every
//     connection. There is no tsnet/WhoIs, and X-Aperture-Login is never
//     honoured (no trusted peer to authenticate it). This mirrors the SSH
//     --test-user seam and is what the e2e harness drives.
//   - Production: if X-Aperture-Login is present AND the peer's WhoIs matches
//     mcpTrustedAperture, use the header (so Aperture-fronted users keep
//     distinct frame dirs instead of collapsing into Aperture's own identity).
//     Otherwise fall back to the peer's own WhoIs login name (the direct-
//     tailnet case: harness → thundersnap, no Aperture).
//
// TODO: forward/record Aperture's stable UserID so a Tailscale rename doesn't
// orphan a user's frame directory. v1 uses login name to match SSH behaviour.

// mcpUserKey is the context key carrying the resolved MCP user from the HTTP
// auth middleware into the tool handlers. The streamable handler propagates
// req.Context() into the tool-call ctx, so a value set by the middleware is
// visible to every ToolHandler.
type mcpUserKey struct{}

// resolveMCPUser returns the workspace namespace for an MCP HTTP request.
// The production principal lookup remains intentionally unfinished below; its
// current "unknown" result maps to the default shared namespace.
func resolveMCPUser(r *http.Request) string {
	principal := testModeUser
	if principal == "" {
		// Production path: honour X-Aperture-Login only from the trusted Aperture
		// node, else fall back to the peer's own WhoIs login.
		if h := r.Header.Get("X-Aperture-Login"); h != "" && mcpTrustedAperture != "" && peerIsTrustedAperture(r) {
			principal = h
		} else {
			principal = peerWhoIsLogin(r)
		}
	}
	return globalPolicy.NamespaceForPrincipal(principal)
}

// peerIsTrustedAperture reports whether the TCP peer's Tailscale identity
// matches mcpTrustedAperture. Requires tsnet/WhoIs; in test mode this is
// never called (resolveMCPUser returns early). TODO: implement against the
// tsnet LocalClient once the production HTTP listener is wired; for now it
// returns false so a bare header from any peer is ignored (fail-closed).
func peerIsTrustedAperture(r *http.Request) bool {
	// TODO: who = getWhoIs(r.Context(), globalLocalClient, r.RemoteAddr);
	// return who.UserProfile.LoginName == mcpTrustedAperture
	return false
}

// peerWhoIsLogin returns the Tailscale login name of the TCP peer. Requires
// tsnet/WhoIs; in test mode this is never called. TODO: implement; returns
// "unknown" so a misconfigured production deployment fails loudly rather than
// silently keying everyone to the same dir.
func peerWhoIsLogin(r *http.Request) string {
	// TODO: who = getWhoIs(r.Context(), globalLocalClient, r.RemoteAddr)
	// if who != nil && who.UserProfile != nil { return who.UserProfile.LoginName }
	return "unknown"
}

// mcpUserFromContext returns the MCP user stashed in ctx by the auth
// middleware, or "" if absent (which would indicate a wiring bug — the
// middleware must wrap the handler before mounting).
func mcpUserFromContext(ctx context.Context) string {
	if u, _ := ctx.Value(mcpUserKey{}).(string); u != "" {
		return u
	}
	return ""
}

// mcpAuthMiddleware resolves the effective user for each MCP HTTP request and
// stashes it in the request context, from which the streamable handler
// propagates it into every tool-call ctx. It does NOT reject requests: in the
// direct-tailnet deployment every authenticated peer (which tsnet guarantees)
// gets full tool access scoped to its own frames. Capability policy is a
// TODO (see sandbox-mcp-design.md §Capability policy).
//
// TODO: apply policy.jsonc / ResolveCap here — hide write tools for a
// read-only role, honour cap.Isolation when launching.
func mcpAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := resolveMCPUser(r)
		r = r.WithContext(context.WithValue(r.Context(), mcpUserKey{}, user))
		next.ServeHTTP(w, r)
	})
}

// errMCPCommandTimeout is returned by runInFrame when the per-call context
// deadline fires before the command produced a FrameExit. Handlers surface
// the partial output (with the timeout marker runInFrame appends) as an
// IsError=true result so the LLM sees the timeout rather than a silent
// success. It is distinct from a setup error (resolve/dial failure), where
// runInFrame returns a wrapped error and a nil result.
var errMCPCommandTimeout = errors.New("command timed out")

// --- The launcher: vshd one-shot exec in a frame ---------------------------

// runInFrame runs `sh -c <command>` in the named frame for the given user,
// starting in workdir (defaulting to "/work"), and returns the collected
// output + exit code. This is the MCP analogue of runContainerSession: it
// resolves the frame via the user's ref store, prepares the rootfs, anchors a
// control server (so `ts` works inside the frame), dials the host vshd, and
// sends a non-PTY one-shot VMX request. The ctx deadline enforces the
// per-call timeout; closing the vshd socket on deadline/cap tears down the
// process group (the e2e cancellation spike, task T2, validates this).
//
// TODO: persistent shell per MCP session — v1 runs a fresh sh -c per call,
// matching Aperture; a persistent shell would carry cd/export across calls.
func runInFrame(ctx context.Context, user, frame, workdir, command, unixUser string) (*mcpexec.ExecResult, error) {
	if user == "" {
		return nil, fmt.Errorf("no MCP user resolved for request")
	}
	if workdir == "" {
		workdir = "/work"
	}
	if unixUser != "user" && unixUser != "root" {
		return nil, fmt.Errorf("user must be either \"user\" or \"root\"")
	}

	rootFS, uuid, err := resolveFrameRootFS(user, frame)
	if err != nil {
		return nil, fmt.Errorf("resolve frame: %w", err)
	}
	if err := prepareContainerRootFS(rootFS, ""); err != nil {
		return nil, fmt.Errorf("prepare frame rootfs: %w", err)
	}

	// Anchor a control server so `ts` subcommands work inside the frame (the
	// control socket lives at /id/thunder.sock in-container). This is the same
	// refcounted getOrCreate/release pair runContainerSession uses.
	if _, err := controlServers.getOrCreateControlServer(rootFS); err != nil {
		return nil, fmt.Errorf("start control socket: %w", err)
	}
	defer controlServers.releaseControlServer(rootFS)

	sockPath, err := hostVshd.ensure()
	if err != nil {
		return nil, fmt.Errorf("start host vshd: %w", err)
	}
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("dial host vshd: %w", err)
	}
	defer conn.Close()

	// VMX header: framePath relative to / (vshd reconstructs it as
	// filepath.Clean("/"+framePath)), target user "root" (matches `ssh
	// root@frame`), non-PTY, args ["sh", "-c", wrapped].
	absRootFS, err := filepath.Abs(rootFS)
	if err != nil {
		return nil, fmt.Errorf("abs rootfs path: %w", err)
	}
	framePathHdr := strings.TrimPrefix(absRootFS, "/")

	// Wrap the command to start in workdir. `cd <workdir> && <command>` matches
	// Aperture's local backend setting cmd.Dir = workdir: if the dir doesn't
	// exist, cd fails and && short-circuits so the command doesn't run.
	wrapped := "cd " + shellQuote(workdir) + " && " + command
	writeVshdRequest(conn, framePathHdr, unixUser, false, []string{"sh", "-c", wrapped}, thundersnapSessionEnv(user, uuid))

	// Collect frames in a goroutine; on ctx cancel (timeout), close the conn to
	// unblock the collector's ReadFrame and tear down vshd's process group.
	type collectResult struct {
		res *mcpexec.ExecResult
		err error
	}
	ch := make(chan collectResult, 1)
	go func() {
		r, err := mcpexec.CollectFrames(conn)
		ch <- collectResult{r, err}
	}()

	select {
	case r := <-ch:
		return r.res, r.err
	case <-ctx.Done():
		// Close the conn to unblock the collector; the deferred Close is a
		// no-op afterwards. Closing the vshd socket mid-stream is identical to
		// an SSH client disconnecting, so vshd's existing reap path fires.
		conn.Close()
		r := <-ch
		// Surface partial output with a timeout marker (unless the cap already
		// produced a marker). The process group is reaped by vshd on disconnect.
		if r.res != nil && !r.res.Truncated {
			marker := "\n\n... command timed out ..."
			if r.res.Output == "" {
				r.res.Output = "... command timed out ..."
			} else if !strings.HasSuffix(r.res.Output, marker) {
				r.res.Output += marker
			}
		}
		return r.res, errMCPCommandTimeout
	}
}

// isWithinRootFS reports whether path is rootFS itself or a descendant of it
// after cleaning. It guards host-side file operations (str_replace) against
// path-traversal escapes from the frame's rootfs: a tool path like
// "/../../etc/shadow" must resolve inside the frame, not the host.
func isWithinRootFS(path, rootFS string) bool {
	absRoot, err := filepath.Abs(rootFS)
	if err != nil {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// openParentUnderRoot walks the parent directories of rel starting from a dir
// fd opened on rootFS, creating any missing parent with mkdirat when
// createMissingDirs is true. Every component is opened with O_NOFOLLOW, so a
// symlink planted at any ancestor by the (unprivileged) frame user cannot
// redirect the host-root daemon's write outside the frame rootfs: the kernel
// returns ELOOP instead of following the link. Newly created parent
// directories are fchown'd to (chownUID, chownGID) when chownNewDirs is true so
// they end up owned by the frame user, matching a user-run `mkdir -p`.
//
// It returns a dir fd for the parent of the final component (caller closes it)
// and the final component name. rel must already be cleaned/contained by the
// caller (isWithinRootFS).
func openParentUnderRoot(rootFS, rel string, createMissingDirs, chownNewDirs bool, chownUID, chownGID int) (parentFd int, name string, err error) {
	parentFd = -1
	comps := strings.Split(rel, string(filepath.Separator))
	// Drop empty components (leading slash from the Clean("/"+path) prefix,
	// or a trailing slash). A fully empty rel means the target is rootFS
	// itself, which is not a writable file.
	filtered := comps[:0]
	for _, c := range comps {
		if c != "" {
			filtered = append(filtered, c)
		}
	}
	if len(filtered) == 0 {
		return -1, "", fmt.Errorf("path is empty")
	}
	name = filtered[len(filtered)-1]
	parents := filtered[:len(filtered)-1]

	cur, err := unix.Open(rootFS, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", fmt.Errorf("open frame rootfs: %w", err)
	}
	for _, c := range parents {
		fd, oerr := unix.Openat(cur, c, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if oerr != nil {
			switch {
			case errors.Is(oerr, unix.ENOENT) && createMissingDirs:
				if mkerr := unix.Mkdirat(cur, c, 0o755); mkerr != nil {
					unix.Close(cur)
					return -1, "", fmt.Errorf("mkdir %s: %w", c, mkerr)
				}
				fd, oerr = unix.Openat(cur, c, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
				if oerr != nil {
					unix.Close(cur)
					return -1, "", fmt.Errorf("open created dir %s: %w", c, oerr)
				}
				if chownNewDirs {
					if cerr := unix.Fchown(fd, chownUID, chownGID); cerr != nil {
						unix.Close(fd)
						unix.Close(cur)
						return -1, "", fmt.Errorf("chown created dir %s: %w", c, cerr)
					}
				}
			case errors.Is(oerr, unix.ELOOP):
				unix.Close(cur)
				return -1, "", fmt.Errorf("refusing to traverse symlink %q in path", c)
			default:
				unix.Close(cur)
				return -1, "", oerr
			}
		}
		unix.Close(cur)
		cur = fd
	}
	return cur, name, nil
}

// --- Command builders (ported from aperture chat/sandbox) -------------------
//
// These are ports of the aperture tool_*.go builders. The fiddly bits (base64+
// heredoc ARG_MAX dodge, surrogateescape Python, awk line-numbering, rune-
// boundary truncation) are proven; re-deriving them risks subtle regressions.

// shellQuote wraps a string in single quotes for safe shell interpolation.
// Ported from aperture chat/sandbox/tools.go.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// buildViewCommand returns the shell program that views path. Ported from
// aperture chat/sandbox/tool_view.go.
func buildViewCommand(path string, viewRange []int) (string, error) {
	const maxEndLine = 99_999_999
	startLine := 1
	endLine := maxEndLine
	if len(viewRange) == 2 {
		startLine = viewRange[0]
		if startLine < 1 {
			startLine = 1
		}
		endLine = viewRange[1]
		if endLine == -1 {
			endLine = maxEndLine
		}
		if endLine < startLine {
			return "", fmt.Errorf("invalid view_range: end (%d) is before start (%d)", endLine, startLine)
		}
	} else if len(viewRange) != 0 {
		return "", fmt.Errorf("view_range must have exactly 2 elements, got %d", len(viewRange))
	}

	qpath := shellQuote(path)
	return fmt.Sprintf(`path=%s
if [ -d "$path" ]; then
  find "$path" -maxdepth 2 -not -path '*/.*' -not -path '*/node_modules/*' | sort | head -200
elif [ -f "$path" ]; then
  case "$path" in
    *.jpg|*.jpeg|*.png|*.gif|*.webp|*.svg|*.bmp|*.ico) echo "[Image: $(basename "$path"), $(stat -c '%%s' "$path") bytes]" ;;
    *) awk 'NR>=%d && NR<=%d {printf "%%6d\t%%s\n", NR, $0}' "$path" ;;
  esac
else
  echo "Error: $path not found" >&2; exit 1
fi`, qpath, startLine, endLine), nil
}

// tailLines returns the final n lines without requiring an in-frame tail
// binary. It preserves a final partial line and raw UTF-8 bytes; truncateUTF8
// applies the model-facing byte cap afterward.
func tailLines(content []byte, n int) string {
	if n <= 0 || len(content) == 0 {
		return ""
	}
	end := len(content)
	searchEnd := end
	if content[end-1] == '\n' {
		searchEnd--
	}
	start := 0
	for i, seen := searchEnd-1, 0; i >= 0; i-- {
		if content[i] == '\n' {
			seen++
			if seen == n {
				start = i + 1
				break
			}
		}
	}
	return strings.ToValidUTF8(string(content[start:end]), "")
}

// tailReadChunk is the maximum number of bytes readTail reads from the end of
// a file into memory at once. It bounds memory for the tail_lines path so a
// multi-gigabyte job log does not get slurped whole; the model-facing output is
// further capped by mcpMaxViewOutput. 1 MiB comfortably exceeds a generous
// line count (e.g. 10k lines of 100 bytes) while staying cheap. It is a var so
// tests can shrink it to exercise the multi-chunk reverse-read path.
var tailReadChunk int64 = 1 << 20

// readTail returns the final n lines of the file at hostPath using a bounded
// reverse read, so the file size (not n) is the memory ceiling. It mirrors
// tailLines' line semantics (final partial line preserved, trailing newline
// preserved, raw bytes then ToValidUTF8) but never holds more than roughly
// tailReadChunk bytes at a time.
func readTail(hostPath string, n int) (string, error) {
	if n <= 0 {
		return "", nil
	}
	f, err := os.Open(hostPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := fi.Size()
	if size == 0 {
		return "", nil
	}
	// Read backwards in chunks, counting newlines, until we have n of them or
	// reach the start of the file. chunks holds data from oldest to newest;
	// it is joined at the end. The trailing newline of the file is kept in the
	// output but not counted as a line separator, matching tailLines' searchEnd.
	var chunks [][]byte
	haveNewlines := 0
	done := false
	pos := size
	for pos > 0 && !done {
		read := tailReadChunk
		if read > pos {
			read = pos
		}
		pos -= read
		buf := make([]byte, read)
		if _, err := f.ReadAt(buf, pos); err != nil {
			return "", err
		}
		// The newest chunk (the first one read) may end with a trailing
		// newline; exclude it from the newline count but keep it in the data.
		scanEnd := len(buf)
		if len(chunks) == 0 && buf[scanEnd-1] == '\n' {
			scanEnd--
		}
		for i := scanEnd - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				haveNewlines++
				if haveNewlines == n {
					// The tail starts right after this newline; buf[i+1:]
					// is the oldest piece (the rest of this chunk stays whole).
					chunks = append([][]byte{buf[i+1:]}, chunks...)
					done = true
					break
				}
			}
		}
		if !done {
			chunks = append([][]byte{buf}, chunks...)
		}
	}
	return strings.ToValidUTF8(string(bytes.Join(chunks, nil)), ""), nil
}

// truncateUTF8 returns s capped at limit bytes, with marker appended if
// truncation occurred. Ported from aperture chat/sandbox/tool_view.go.
func truncateUTF8(s string, limit int, marker string) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 {
		r, size := utf8.DecodeLastRuneInString(s[:cut])
		if r != utf8.RuneError || size > 1 {
			break
		}
		cut--
	}
	return s[:cut] + marker
}

// buildCreateFileCommand is retained for reference and tests, but is no
// longer used by the production create_file handler: that tool is now
// implemented host-side (see mcpCreateFileToolHandler), mirroring
// str_replace, so it works in every frame — including nil:nil:nil frames
// that ship no mkdir/dirname/base64.
func buildCreateFileCommand(path, fileText string) string {
	b64 := base64.StdEncoding.EncodeToString([]byte(fileText))
	qpath := shellQuote(path)
	return fmt.Sprintf(
		"mkdir -p \"$(dirname %s)\" && if [ -L %s ]; then echo 'Error: refusing to overwrite symlink' >&2; exit 1; fi && base64 -d > %s <<'B64EOF'\n%s\nB64EOF\n",
		qpath, qpath, qpath, b64,
	)
}

// str_replace has no command builder: it is implemented host-side (see
// mcpStrReplaceToolHandler) because thundersnap's default nil:nil:nil frames
// ship no python3 and the daemon has direct rootfs access. create_file is
// host-side too (see mcpCreateFileToolHandler) for the same reason: the
// exec-based builder relied on mkdir/dirname/base64, which nil:nil:nil frames
// do not provide. The view tool's builders (shellQuote, buildViewCommand)
// remain exec-based below.

// --- Tool timeout constants (match aperture + design doc) ------------------

const (
	mcpViewTimeout       = 30 * time.Second
	mcpCreateFileTimeout = 30 * time.Second
	mcpStrReplaceTimeout = 30 * time.Second
	mcpMaxViewOutput     = 16_000
)

// --- Tool handlers ---------------------------------------------------------
//
// Each handler is an mcp.ToolHandler: func(ctx, *CallToolRequest) (*CallToolResult, error).
// Setup failures (resolve/dial) are returned as Go errors (protocol errors);
// command non-zero exits and "soft" failures (file not found, string not
// unique) are returned as CallToolResult with IsError=true, matching aperture's
// chatToolToMCPHandler convention (the LLM must see these to self-correct).

// textResult builds a single-TextContent CallToolResult. isError=true marks a
// tool-level failure (non-zero exit, file not found, string not unique,
// timeout, invalid input) so the LLM sees it and can self-correct; the output
// text is always preserved in Content. This mirrors aperture's
// chatToolToMCPHandler convention: protocol/transport errors come back as Go
// errors from the handler, while command/soft failures come back as
// IsError=true results so the LLM can read the output and retry.
func textResult(text string, isError bool) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
		IsError: isError,
	}, nil
}

// mcpViewToolHandler is the ToolHandler for view.
func mcpViewToolHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var params struct {
		Path      string `json:"path"`
		ViewRange []int  `json:"view_range"`
		TailLines int    `json:"tail_lines"`
		Frame     string `json:"frame"`
		User      string `json:"user"`
	}
	if req.Params != nil && len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &params); err != nil {
			return textResult(fmt.Sprintf("invalid input: %v", err), true)
		}
	}
	if params.Path == "" {
		return textResult("path is required", true)
	}
	if params.User != "user" && params.User != "root" {
		return textResult("user is required and must be either \"user\" or \"root\"", true)
	}
	if params.TailLines < 0 {
		return textResult("tail_lines must be positive", true)
	}
	if params.TailLines > 0 && len(params.ViewRange) > 0 {
		return textResult("tail_lines and view_range are mutually exclusive", true)
	}
	var cmd string
	var err error
	if params.TailLines > 0 {
		// Run under the requested Unix identity rather than reading the rootfs
		// host-side as the daemon. The ring-buffer awk avoids requiring tail.
		cmd = fmt.Sprintf(`path=%s
if [ ! -f "$path" ]; then echo "Error: $path not found" >&2; exit 1; fi
awk -v n=%d '{ lines[NR %% n] = $0 } END { start=NR-n+1; if (start < 1) start=1; for (i=start; i<=NR; i++) print lines[i %% n] }' "$path"`, shellQuote(params.Path), params.TailLines)
	} else {
		cmd, err = buildViewCommand(params.Path, params.ViewRange)
	}
	if err != nil {
		return textResult(err.Error(), true)
	}

	ctx, cancel := context.WithTimeout(ctx, mcpViewTimeout)
	defer cancel()

	res, err := runInFrame(ctx, mcpUserFromContext(ctx), params.Frame, "", cmd, params.User)
	if err != nil {
		if errors.Is(err, errMCPCommandTimeout) && res != nil {
			return textResult(truncateUTF8(res.Output, mcpMaxViewOutput, "\n\n... output truncated ..."), true)
		}
		return textResult(fmt.Sprintf("view failed: %v", err), true)
	}
	// A non-zero exit (e.g. path not found) is a tool-level failure. The view
	// command writes its own "Error: <path> not found" to stderr, so the
	// output already carries the message; just mark it IsError.
	return textResult(truncateUTF8(res.Output, mcpMaxViewOutput, "\n\n... output truncated ..."), res.ExitCode != 0)
}

// mcpCreateFileToolHandler is the ToolHandler for create_file.
//
// Like str_replace, create_file does NOT run a command in the frame. The
// exec-based builder (buildCreateFileCommand) relied on mkdir/dirname/base64,
// which thundersnap's default nil:nil:nil frames do not ship — so an omitted
// frame selector (which lands in a fresh minimal frame) produced
// "mkdir: executable file not found in $PATH" instead of a file. The daemon
// has direct host access to the frame's rootfs, so the mkdir/write is done
// in-process. This is binary-safe and works in every frame, minimal or full.
//
// The host-side write chowns the new file (and any freshly created parent
// directories) to the frame's "user" account (UID/GID 7575) when user=="user",
// so the file is owned by the same identity that shell jobs run as; root-owned
// files stay root. An existing file's mode and owner are preserved (matching
// the old `> file` redirect and str_replace behaviour).
func mcpCreateFileToolHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var params struct {
		Path     string `json:"path"`
		FileText string `json:"file_text"`
		Frame    string `json:"frame"`
		User     string `json:"user"`
	}
	if req.Params != nil && len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &params); err != nil {
			return textResult(fmt.Sprintf("invalid input: %v", err), true)
		}
	}
	if params.Path == "" {
		return textResult("path is required", true)
	}
	if params.User != "user" && params.User != "root" {
		return textResult("user is required and must be either \"user\" or \"root\"", true)
	}

	user := mcpUserFromContext(ctx)
	if user == "" {
		return textResult("no MCP user resolved for request", true)
	}

	ctx, cancel := context.WithTimeout(ctx, mcpCreateFileTimeout)
	defer cancel()

	rootFS, _, err := resolveFrameRootFS(user, params.Frame)
	if err != nil {
		return textResult(fmt.Sprintf("resolve frame: %v", err), true)
	}
	if err := prepareContainerRootFS(rootFS, ""); err != nil {
		return textResult(fmt.Sprintf("prepare frame rootfs: %v", err), true)
	}

	// Resolve the in-frame path to a host path under rootFS, refusing to
	// escape the frame (a path like /../../etc/shadow must not reach the host).
	rel := strings.TrimPrefix(filepath.Clean("/"+params.Path), "/")
	hostPath := filepath.Join(rootFS, rel)
	if !isWithinRootFS(hostPath, rootFS) {
		return textResult(fmt.Sprintf("path %q escapes the frame", params.Path), true)
	}

	uid, gid := 0, 0
	if params.User == "user" {
		uid, gid = tsm.ThundersnapUID, tsm.ThundersnapGID
	}

	// Walk the parent directories from a dir fd on rootFS, creating any
	// missing ancestor with mkdirat and refusing to traverse a symlink at any
	// component (O_NOFOLLOW). The frame user is unprivileged and can plant
	// symlinks inside the frame; without this walk a symlinked ancestor could
	// redirect this host-root write outside the frame rootfs.
	parentFd, name, err := openParentUnderRoot(rootFS, rel, true, params.User == "user", uid, gid)
	if err != nil {
		return textResult(fmt.Sprintf("create parent dirs for %s: %v", params.Path, err), true)
	}
	defer unix.Close(parentFd)

	// Open the target with O_NOFOLLOW so a symlink at the final component is
	// refused (ELOOP) rather than followed, and O_TRUNC to overwrite in place
	// (the exec path did the same). O_CREAT only takes effect for a new file;
	// an existing file keeps its mode because open(2) ignores mode on an
	// existing inode. A directory target fails with EISDIR.
	fd, err := unix.Openat(parentFd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_TRUNC, 0o644)
	if err != nil {
		switch {
		case errors.Is(err, unix.ELOOP):
			return textResult(fmt.Sprintf("Error: refusing to overwrite symlink %s", params.Path), true)
		case errors.Is(err, unix.EISDIR):
			return textResult(fmt.Sprintf("Error: %s is a directory", params.Path), true)
		default:
			return textResult(fmt.Sprintf("open %s: %v", params.Path, err), true)
		}
	}
	if _, err := unix.Write(fd, []byte(params.FileText)); err != nil {
		unix.Close(fd)
		return textResult(fmt.Sprintf("write %s: %v", params.Path, err), true)
	}
	if err := unix.Fsync(fd); err != nil {
		// Not fatal; the exec path didn't fsync at all.
		log.Printf("create_file fsync %s: %v", params.Path, err)
	}
	// Match the prior behaviour: a file written as root is chown'd to the
	// frame user so shell jobs running as that user can overwrite it.
	if params.User == "user" {
		if cerr := unix.Fchown(fd, uid, gid); cerr != nil {
			unix.Close(fd)
			return textResult(fmt.Sprintf("chown %s: %v", params.Path, cerr), true)
		}
	}
	unix.Close(fd)

	if ctx.Err() == context.DeadlineExceeded {
		return textResult(fmt.Sprintf("create_file timed out writing %s", params.Path), true)
	}
	return textResult(fmt.Sprintf("Created %s (%d bytes)", params.Path, len(params.FileText)), false)
}

// mcpStrReplaceToolHandler is the ToolHandler for str_replace.
//
// Unlike the other tools, str_replace does NOT run a command in the frame.
// Aperture's tool_str_replace.go pipes an embedded Python program over a
// heredoc because Aperture's sandbox is a remote HTTP backend with no direct
// filesystem access — it must run python3 *inside* the sandbox. Thundersnap's
// daemon, by contrast, has direct host access to the frame's rootfs (a local
// btrfs subvolume), so the read/count/replace/write is done in-process on the
// host. This is a deliberate, documented deviation from the design doc's
// "byte-identical to Aperture" note: thundersnap's default nil:nil:nil frames
// ship no python3 (and no /lib64 for a dynamic one), so the Python approach
// fails there. The host-side implementation is binary-safe (it operates on
// raw []byte, so non-UTF-8 files survive byte-for-byte — the same property
// Aperture's surrogateescape buys) and works in every frame, minimal or full.
// The contract is unchanged: error on 0 or >1 occurrences, replace exactly
// once, preserve the file's existing mode/owner.
func mcpStrReplaceToolHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var params struct {
		Path   string `json:"path"`
		OldStr string `json:"old_str"`
		NewStr string `json:"new_str"`
		Frame  string `json:"frame"`
	}
	if req.Params != nil && len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &params); err != nil {
			return textResult(fmt.Sprintf("invalid input: %v", err), true)
		}
	}
	if params.Path == "" {
		return textResult("path is required", true)
	}
	if params.OldStr == "" {
		return textResult("old_str is required", true)
	}

	user := mcpUserFromContext(ctx)
	if user == "" {
		return textResult("no MCP user resolved for request", true)
	}

	ctx, cancel := context.WithTimeout(ctx, mcpStrReplaceTimeout)
	defer cancel()

	rootFS, _, err := resolveFrameRootFS(user, params.Frame)
	if err != nil {
		return textResult(fmt.Sprintf("resolve frame: %v", err), true)
	}
	if err := prepareContainerRootFS(rootFS, ""); err != nil {
		return textResult(fmt.Sprintf("prepare frame rootfs: %v", err), true)
	}

	// Resolve the in-frame path to a host path under rootFS, refusing to
	// escape the frame (a path like /../../etc/shadow must not reach the host).
	rel := strings.TrimPrefix(filepath.Clean("/"+params.Path), "/")
	hostPath := filepath.Join(rootFS, rel)
	if !isWithinRootFS(hostPath, rootFS) {
		return textResult(fmt.Sprintf("path %q escapes the frame", params.Path), true)
	}

	// Walk the parent dirs from a dir fd on rootFS with O_NOFOLLOW so a
	// symlink planted at an ancestor by the frame user cannot redirect the
	// host-root read/write outside the frame rootfs. str_replace never creates
	// parents (the target must already exist).
	parentFd, name, err := openParentUnderRoot(rootFS, rel, false, false, 0, 0)
	if err != nil {
		return textResult(fmt.Sprintf("resolve %s: %v", params.Path, err), true)
	}
	defer unix.Close(parentFd)

	// O_NOFOLLOW refuses a symlink at the target (ELOOP) instead of following
	// it. O_RDWR without O_CREAT keeps the existing inode (owner/mode
	// preserved). A missing file is ENOENT; a directory is EISDIR.
	fd, err := unix.Openat(parentFd, name, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		switch {
		case errors.Is(err, unix.ENOENT):
			return textResult(fmt.Sprintf("Error: %s not found", params.Path), true)
		case errors.Is(err, unix.ELOOP):
			return textResult(fmt.Sprintf("Error: refusing to replace symlink %s", params.Path), true)
		case errors.Is(err, unix.EISDIR):
			return textResult(fmt.Sprintf("Error: %s is a directory", params.Path), true)
		default:
			return textResult(fmt.Sprintf("open %s: %v", params.Path, err), true)
		}
	}
	defer unix.Close(fd)

	var content []byte
	tmp := make([]byte, 4096)
	for {
		n, rerr := unix.Read(fd, tmp)
		if n > 0 {
			content = append(content, tmp[:n]...)
		}
		if rerr != nil {
			if errors.Is(rerr, unix.EINTR) {
				continue
			}
			unix.Close(fd)
			return textResult(fmt.Sprintf("read %s: %v", params.Path, rerr), true)
		}
		if n == 0 {
			// A raw read(2) on a regular file returns (0, nil) at EOF (the
			// io.EOF sentinel is only an os.File wrapper); n==0 is our EOF signal.
			break
		}
	}

	oldBytes := []byte(params.OldStr)
	count := bytes.Count(content, oldBytes)
	if count == 0 {
		return textResult(fmt.Sprintf("Error: string not found in %s", params.Path), true)
	}
	if count > 1 {
		return textResult(fmt.Sprintf("Error: string appears %d times in %s (must be unique)", count, params.Path), true)
	}

	newContent := bytes.Replace(content, oldBytes, []byte(params.NewStr), 1)
	if _, err := unix.Pwrite(fd, newContent, 0); err != nil {
		return textResult(fmt.Sprintf("write %s: %v", params.Path, err), true)
	}
	// Truncate to the new length in case the replacement shrank the file;
	// ftruncate on the same fd keeps the inode (owner/mode) intact.
	if err := unix.Ftruncate(fd, int64(len(newContent))); err != nil {
		return textResult(fmt.Sprintf("truncate %s: %v", params.Path, err), true)
	}
	unix.Fsync(fd)

	if ctx.Err() == context.DeadlineExceeded {
		return textResult(fmt.Sprintf("str_replace timed out writing %s", params.Path), true)
	}
	return textResult(fmt.Sprintf("Replaced in %s", params.Path), false)
}

// mcpListFramesToolHandler is the ToolHandler for list_frames. It
// does NOT launch a container: it reads the user's frame + ref stores directly,
// so the LLM can pick an existing frame for its first bash call without
// auto-creating a throwaway. Mirrors handleListFrames' logic.
func mcpListFramesToolHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	user := mcpUserFromContext(ctx)
	if user == "" {
		return textResult("no MCP user resolved for request", true)
	}
	frameStore := namespaceFrameStore(user)
	refStore := namespaceRefStore(user)

	uuids, err := frameStore.List()
	if err != nil {
		return textResult(fmt.Sprintf("list frames: %v", err), true)
	}

	refsByUUID := map[frameid.ID][]string{}
	if names, err := refStore.List(); err == nil {
		for _, name := range names {
			if ref, err := refStore.Get(name); err == nil {
				refsByUUID[ref.UUID] = append(refsByUUID[ref.UUID], name)
			}
		}
	}

	type frameInfo struct {
		UUID   string   `json:"uuid"`
		Refs   []string `json:"refs"`
		Status string   `json:"status"`
	}
	var frames []frameInfo
	for _, uuid := range uuids {
		refs := refsByUUID[uuid]
		if refs == nil {
			refs = []string{}
		}
		sort.Strings(refs)
		sessionCount := getActiveFrameCount(framePathForNamespaceUUID(user, uuid))
		status := "stopped"
		if sessionCount > 0 {
			status = fmt.Sprintf("%d", sessionCount)
		}
		frames = append(frames, frameInfo{UUID: uuid.String(), Refs: refs, Status: status})
	}
	if frames == nil {
		frames = []frameInfo{}
	}
	out, err := json.Marshal(map[string]any{"frames": frames})
	if err != nil {
		return textResult(fmt.Sprintf("marshal frames: %v", err), true)
	}
	return textResult(string(out), false)
}

// --- MCP server factory + HTTP mount ---------------------------------------

// newMCPServer builds the MCP server with all tools registered. It is
// called once per handler mount (production main() and test runTestMode each
// build their own). The server name/version are the thundersnap daemon's.

// frameFieldDesc is the shared description for the "frame" parameter on every
// tool that targets a frame. Frame is required: always pass an explicit ref
// name or UUID (from list_frames) so the call lands in the frame you mean. The
// empty string is only valid for thundersnap_jobs, and only when you
// deliberately want a throwaway ephemeral frame for that single call — an
// empty frame is discarded as soon as the MCP call returns, so anything
// written there (e.g. via create_file) is lost. Use a named/ref'd frame for
// any work you want to persist.
const frameFieldDesc = "Frame name or UUID (required). Pass a ref name or UUID from list_frames so the call lands in the frame you mean. " +
	"An empty string selects a throwaway ephemeral frame that is discarded when the MCP call ends; only useful for one-shot jobs, " +
	"and never appropriate for create_file/view/str_replace, which need a persistent frame."

func newMCPServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "thundersnap",
		Title:   "Thundersnap Sandbox",
		Version: mcpDaemonVersion,
	}, &mcp.ServerOptions{
		Instructions: "For background on thundersnap frames, refs, and the ts CLI used inside frames, see the thundersnap " +
			"instructions at https://github.com/tailscale/thundersnap. " +
			"Use jobs for shell execution. Launch entries run concurrently and must be independent. Put dependent " +
			"steps in one multiline shell script, usually beginning with set -ex and one command per line. Every launch and " +
			"file operation requires an explicit user (user or root). Wait using each job's byte offset; output is returned " +
			"only through the last CR/LF while running, and through EOF after exit. A wait timeout never stops a job. " +
			"Use jobs_list to recover status and jobs_kill only for immediate teardown.",
	})

	launchSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command":      map[string]any{"type": "string", "description": "The shell command to run."},
			"frame":        map[string]any{"type": "string", "description": frameFieldDesc},
			"workdir":      map[string]any{"type": "string", "description": "Initial working directory inside the frame (default /work); applies only to this job."},
			"label":        map[string]any{"type": "string", "description": "Optional. Use mainly to distinguish several or long-running jobs; omit for quick commands."},
			"user":         map[string]any{"type": "string", "enum": []string{"user", "root"}, "description": "Required Unix account inside the frame; use root only for administrative operations."},
			"hard_timeout": map[string]any{"type": "integer", "description": "Total job lifetime in seconds (default/max 7200)."},
		},
		"required":             []string{"command", "user", "frame"},
		"additionalProperties": false,
	}
	waitSchema := map[string]any{
		"type":        "object",
		"description": "Optional wait. When launch is present, omit jobs: the wait automatically selects only jobs launched by this call.",
		"properties": map[string]any{
			"jobs": map[string]any{"type": "array", "description": "Optional when waiting for existing jobs; do not provide when launch is present.", "items": map[string]any{"type": "object", "properties": map[string]any{
				"id": map[string]any{"type": "string"}, "offset": map[string]any{"type": "integer", "minimum": 0},
			}, "required": []string{"id", "offset"}, "additionalProperties": false}},
			"until":      map[string]any{"type": "string", "enum": []string{"output", "any_exit", "all_exit"}},
			"timeout":    map[string]any{"type": "integer", "minimum": 1, "maximum": 60, "description": "Optional observation timeout; never stops jobs."},
			"pre_signal": map[string]any{"type": "string", "enum": []string{"HUP", "INT", "TERM", "USR1", "USR2", "STOP", "CONT"}, "description": "Optional. Omit unless you explicitly want to signal jobs before waiting. Sent once up front to each selected running job's process group; never sent after the timeout."},
		}, "additionalProperties": false,
	}
	s.AddTool(&mcp.Tool{
		Name: "jobs",
		Description: "Launch independent shell jobs concurrently and optionally wait. Put dependent steps in one multiline " +
			"shell script (for example set -ex followed by one command per line). Wait jobs use raw combined-log byte offsets " +
			"and return next_offset. While running, output ends after the last CR/LF; after exit, the final partial line is included. " +
			"The optional pre_signal is sent once up front, before waiting, without escalation; omit it for normal observation. A wait timeout only returns a snapshot and never stops jobs. When launch is present with wait, never specify wait.jobs: the wait automatically selects the jobs launched by this call.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"launch": map[string]any{"type": "array", "items": launchSchema, "description": "Jobs to start concurrently. For finite work, strongly prefer including wait in this same call; do not launch and then make a redundant separate wait call."},
				"wait":   waitSchema,
			},
		},
	}, mcpJobsToolHandler)

	s.AddTool(&mcp.Tool{
		Name: "jobs_list",
		Description: "Return current status for background jobs belonging to this Aperture conversation, across every frame " +
			"used by the conversation. Use this to recover forgotten job IDs, log paths, revisions, states, and exit codes. " +
			"Omit job_ids to list all jobs in this conversation; jobs from other conversations are never included. " +
			"State running is non-terminal; exited, timed_out, killed, and lost are terminal. A non-zero exit_code " +
			"means the command failed even though its launch succeeded.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"job_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional filter: status for only these job IDs. Omit or pass [] to list every job in this conversation."},
		}},
	}, mcpJobsListToolHandler)
	s.AddTool(&mcp.Tool{
		Name: "jobs_kill",
		Description: "Stop selected background jobs, including child and grandchild processes, then wait until teardown is " +
			"observed before returning. This is conversation-scoped and does not affect unselected sibling jobs. Killing an " +
			"already-terminal job is harmless. A returned state=killed confirms teardown; use jobs_list afterward if needed.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"job_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Job IDs to stop (required)."},
		}, "required": []string{"job_ids"}},
	}, mcpJobsKillToolHandler)

	// view
	s.AddTool(&mcp.Tool{
		Name: "view",
		Description: "Synchronously view a file or directory. For a file, " +
			"prints lines with line numbers (use view_range [start, end], end=-1 for EOF), or use tail_lines for final " +
			"lines of a running/completed log. For a directory, lists up to 200 entries (maxdepth 2). Output is truncated " +
			"to 16K bytes. A non-zero exit such as file-not-found is an error result.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Absolute path inside the frame to view. Mutually exclusive with job_id.",
				},
				"view_range": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "integer"},
					"description": "Optional [start_line, end_line] to limit file output. end_line=-1 means EOF.",
				},
				"tail_lines": map[string]any{
					"type":        "integer",
					"description": "Optional number of final lines to read; mutually exclusive with view_range.",
				},
				"frame": map[string]any{"type": "string", "description": frameFieldDesc},
				"user":  map[string]any{"type": "string", "enum": []string{"user", "root"}},
			}, "required": []string{"path", "user", "frame"}, "additionalProperties": false,
		},
	}, mcpViewToolHandler)

	// create_file
	s.AddTool(&mcp.Tool{
		Name: "create_file",
		Description: "Synchronously create or overwrite a file as the explicitly selected Unix user; creates parent directories. " +
			"Implemented host-side, so it works in every frame including minimal ones with no POSIX utilities. The frame is required " +
			"and must be a persistent (ref'd) frame — a throwaway empty frame is discarded when this call returns, so a file written " +
			"there would be lost immediately.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Absolute path inside the frame to create.",
				},
				"file_text": map[string]any{
					"type":        "string",
					"description": "The full text content of the file.",
				},
				"frame": map[string]any{"type": "string", "description": frameFieldDesc},
				"user":  map[string]any{"type": "string", "enum": []string{"user", "root"}},
			},
			"required":             []string{"path", "file_text", "user", "frame"},
			"additionalProperties": false,
		},
	}, mcpCreateFileToolHandler)

	// str_replace
	s.AddTool(&mcp.Tool{
		Name: "str_replace",
		Description: "Synchronously replace exactly one occurrence of old_str in a frame file; no background job ID is " +
			"created. The call fails if old_str occurs zero or multiple times, so use view first to capture enough " +
			"unique context. Replacement is performed host-side as raw bytes and preserves the existing file mode and owner.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Absolute path inside the frame to edit.",
				},
				"old_str": map[string]any{
					"type":        "string",
					"description": "The exact string to replace. Must be unique in the file.",
				},
				"new_str": map[string]any{
					"type":        "string",
					"description": "The replacement string.",
				},
				"frame": map[string]any{
					"type":        "string",
					"description": frameFieldDesc,
				},
			},
			"required": []string{"path", "old_str", "new_str", "frame"},
		},
	}, mcpStrReplaceToolHandler)

	// list_frames
	s.AddTool(&mcp.Tool{
		Name: "list_frames",
		Description: "List the caller's thundersnap frames and all refs pointing to each frame (does NOT launch a " +
			"container). Returns JSON {\"frames\":[{\"uuid\",\"refs\",\"status\"}]} where refs may be empty and " +
			"status is \"stopped\" or the active session count. Use this to pick " +
			"a persistent frame (one with refs) for the first jobs/view/create_file/str_replace call. Frames with empty refs " +
			"are unattached throwaways from prior sessions and are discarded when their session ends; prefer a ref'd frame. " +
			"If no useful frames or refs are listed, bootstrap one before doing anything else: call jobs with frame left empty " +
			"(the empty ephemeral frame, which always exists and ships the ts CLI) and user set to root, running " +
			"`ts frame --ref=deb $(ts download-docker debian:latest)::` to download the debian:latest image as a snap and " +
			"create a persistent frame named deb; call list_frames again to confirm, then pass deb as the frame to " +
			"jobs/view/create_file/str_replace.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}, mcpListFramesToolHandler)

	return s
}

// mcpHTTPHandler returns the http.Handler for the /v1/mcp endpoint: the
// streamable MCP handler wrapped in the auth middleware that resolves the
// per-request user. The getServer closure returns the singleton server built
// by newMCPServer(); the SDK calls it once per incoming request.
func mcpHTTPHandler() http.Handler {
	server := newMCPServer()
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, nil)
	return mcpAuthMiddleware(streamable)
}

// mountMCP registers the /v1/mcp endpoint on the given mux. Called by both
// the production httpMux (in main()) and the test-mode mux (in runTestMode).
func mountMCP(mux *http.ServeMux) {
	mux.Handle("/v1/mcp", mcpHTTPHandler())
}
