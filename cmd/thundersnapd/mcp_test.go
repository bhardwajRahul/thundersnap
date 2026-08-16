// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestTextResultIsErrorSemantics pins the IsError contract the MCP tools rely
// on: a tool-level failure (non-zero exit, not-found, timeout, bad input) is
// returned as a CallToolResult with IsError=true and the output preserved in
// Content (so the LLM sees it and self-corrects), while a success has
// IsError=false. The handler never returns a Go error for these — that would
// be a protocol error the LLM can't inspect.
func TestTextResultIsErrorSemantics(t *testing.T) {
	res, err := textResult("ok output", false)
	if err != nil {
		t.Fatalf("textResult success: unexpected Go error %v", err)
	}
	if res.IsError {
		t.Errorf("success: IsError=true, want false")
	}
	if len(res.Content) != 1 {
		t.Fatalf("success: want 1 content block, got %d", len(res.Content))
	}
	if tc, ok := res.Content[0].(*mcp.TextContent); !ok || tc.Text != "ok output" {
		t.Errorf("success: content = %+v, want TextContent{ok output}", res.Content[0])
	}

	res, err = textResult("command failed: exit 3", true)
	if err != nil {
		t.Fatalf("textResult error: unexpected Go error %v", err)
	}
	if !res.IsError {
		t.Errorf("error: IsError=false, want true")
	}
	if tc, ok := res.Content[0].(*mcp.TextContent); !ok || tc.Text != "command failed: exit 3" {
		t.Errorf("error: content = %+v, want the failure text preserved", res.Content[0])
	}
}

// TestIsWithinRootFS covers the path-traversal guard that keeps host-side
// str_replace inside the frame's rootfs. A tool path like /../../etc/shadow
// must resolve under the frame, never escape to the host.
func TestIsWithinRootFS(t *testing.T) {
	rootFS := "/data/fs/alice/uuid-123"

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"direct child", "/data/fs/alice/uuid-123/work/poem.txt", true},
		{"rootfs itself", "/data/fs/alice/uuid-123", true},
		{"nested child", "/data/fs/alice/uuid-123/a/b/c", true},
		{"sibling frame escapes", "/data/fs/alice/uuid-456/secret", false},
		{"parent traversal", "/data/fs/alice", false},
		{"deep parent traversal", "/data/fs/alice/uuid-123/../../uuid-456", false},
		{"absolute host path", "/etc/shadow", false},
		{"root", "/", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isWithinRootFS(c.path, rootFS); got != c.want {
				t.Errorf("isWithinRootFS(%q, %q) = %v, want %v", c.path, rootFS, got, c.want)
			}
		})
	}
}

// TestBuildViewCommand asserts the view command builder produces the awk-based
// line-numbered form for files and validates view_range. The exact shell is
// exercised end-to-end in e2e; this pins the structure + range plumbing so a
// regression in the builder is caught without a frame.
func TestBuildViewCommand(t *testing.T) {
	t.Run("default full file", func(t *testing.T) {
		cmd, err := buildViewCommand("/work/poem.txt", nil)
		if err != nil {
			t.Fatalf("buildViewCommand: %v", err)
		}
		if !strings.Contains(cmd, "/work/poem.txt") {
			t.Errorf("command %q missing the path", cmd)
		}
		if !strings.Contains(cmd, "NR>=1 && NR<=99999999") {
			t.Errorf("command %q missing default line range", cmd)
		}
	})

	t.Run("view_range limits", func(t *testing.T) {
		cmd, err := buildViewCommand("/work/poem.txt", []int{5, 10})
		if err != nil {
			t.Fatalf("buildViewCommand: %v", err)
		}
		if !strings.Contains(cmd, "NR>=5 && NR<=10") {
			t.Errorf("command %q missing view_range 5-10", cmd)
		}
	})

	t.Run("view_range end=-1 means EOF", func(t *testing.T) {
		cmd, err := buildViewCommand("/work/poem.txt", []int{5, -1})
		if err != nil {
			t.Fatalf("buildViewCommand: %v", err)
		}
		if !strings.Contains(cmd, "NR>=5 && NR<=99999999") {
			t.Errorf("command %q should map end=-1 to EOF sentinel", cmd)
		}
	})

	t.Run("view_range start clamped to 1", func(t *testing.T) {
		cmd, err := buildViewCommand("/work/poem.txt", []int{-3, 10})
		if err != nil {
			t.Fatalf("buildViewCommand: %v", err)
		}
		if !strings.Contains(cmd, "NR>=1 && NR<=10") {
			t.Errorf("command %q should clamp start to 1", cmd)
		}
	})

	t.Run("view_range end before start errors", func(t *testing.T) {
		if _, err := buildViewCommand("/work/poem.txt", []int{10, 5}); err == nil {
			t.Errorf("expected error for end < start, got nil")
		}
	})

	t.Run("view_range wrong length errors", func(t *testing.T) {
		if _, err := buildViewCommand("/work/poem.txt", []int{1}); err == nil {
			t.Errorf("expected error for 1-element view_range, got nil")
		}
		if _, err := buildViewCommand("/work/poem.txt", []int{1, 2, 3}); err == nil {
			t.Errorf("expected error for 3-element view_range, got nil")
		}
	})

	t.Run("directory branch present", func(t *testing.T) {
		cmd, err := buildViewCommand("/work", nil)
		if err != nil {
			t.Fatalf("buildViewCommand: %v", err)
		}
		if !strings.Contains(cmd, "find \"$path\"") {
			t.Errorf("command %q missing directory find branch", cmd)
		}
	})
}

// TestBuildCreateFileCommand asserts the base64+heredoc round-trips the content
// exactly (including newlines and shell-special bytes), since the whole point
// of the base64 dodge is binary safety beyond ARG_MAX.
func TestBuildCreateFileCommand(t *testing.T) {
	content := "line one\nline two with 'quotes' and $vars\n"
	cmd := buildCreateFileCommand("/work/file.txt", content)
	if !strings.Contains(cmd, "/work/file.txt") {
		t.Errorf("command %q missing path", cmd)
	}
	// The content must be base64-encoded in the heredoc, not raw, so shell
	// metacharacters in the content can't break the command.
	if strings.Contains(cmd, "$vars") {
		t.Errorf("command %q leaked raw content (base64 encode missing)", cmd)
	}
	// Decode the base64 body and confirm it round-trips. The heredoc opener is
	// <<'B64EOF' (quoted) and the closer is a bare B64EOF line; the body lies
	// between them.
	const opener = "<<'B64EOF'\n"
	start := strings.Index(cmd, opener)
	if start < 0 {
		t.Fatalf("could not locate heredoc opener in %q", cmd)
	}
	start += len(opener)
	end := strings.Index(cmd[start:], "\nB64EOF\n")
	if end < 0 {
		t.Fatalf("could not locate heredoc closer in %q", cmd)
	}
	end += start // relative to absolute offset
	body := strings.TrimRight(cmd[start:end], "\n")
	decoded, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if string(decoded) != content {
		t.Errorf("round-trip mismatch: got %q, want %q", decoded, content)
	}
}

// TestReadTail verifies readTail matches tailLines on small files and does not
// slurp a whole large file into memory. It exercises the chunk-boundary path
// by forcing a tiny effective chunk via a temp file that spans multiple
// chunks of a deliberately reduced ceiling.
func TestReadTail(t *testing.T) {
	// Small-file parity: readTail and tailLines must agree.
	for _, tt := range []struct {
		name    string
		content string
		n       int
		want    string
	}{
		{"empty", "", 3, ""},
		{"zero n", "a\nb\n", 0, ""},
		{"fewer than requested", "a\nb\n", 5, "a\nb\n"},
		{"final newline", "a\nb\nc\n", 2, "b\nc\n"},
		{"partial final line", "a\nb\nc", 2, "b\nc"},
		{"one line", "a\nb\nc\n", 1, "c\n"},
		{"utf8", "α\nβ\nγ", 2, "β\nγ"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "f")
			if err := os.WriteFile(p, []byte(tt.content), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := readTail(p, tt.n)
			if err != nil {
				t.Fatalf("readTail: %v", err)
			}
			if got != tt.want {
				t.Errorf("readTail(%q, %d) = %q, want %q", tt.content, tt.n, got, tt.want)
			}
			if want := tailLines([]byte(tt.content), tt.n); got != want {
				t.Errorf("readTail != tailLines: got %q want %q", got, want)
			}
		})
	}

	// A file larger than one chunk must still return the correct tail. Temporarily
	// shrink the chunk to keep the test fast and exercise the multi-chunk path.
	prev := tailReadChunk
	tailReadChunk = 8 // tiny so a modest file spans several chunks
	t.Cleanup(func() { tailReadChunk = prev })
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("readTail panicked: %v", r)
		}
	}()
	var lines []string
	for i := 1; i <= 50; i++ {
		lines = append(lines, fmt.Sprintf("line-%02d", i))
	}
	big := []byte(strings.Join(lines, "\n") + "\n")
	p := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(p, big, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readTail(p, 3)
	if err != nil {
		t.Fatalf("readTail big: %v", err)
	}
	if want := "line-48\nline-49\nline-50\n"; got != want {
		t.Errorf("readTail big = %q, want %q", got, want)
	}
}

func TestMCPJobScopeFromRequest(t *testing.T) {
	ctx := context.WithValue(context.Background(), mcpUserKey{}, "alice")
	for name, req := range map[string]*mcp.CallToolRequest{
		"metadata": {
			Params: &mcp.CallToolParamsRaw{Meta: mcp.Meta{apertureConversationIDMetaKey: "conv-meta"}},
		},
		"header": {
			Params: &mcp.CallToolParamsRaw{},
			Extra:  &mcp.RequestExtra{Header: http.Header{apertureConversationIDHeaderName: {"conv-header"}}},
		},
		"header takes precedence": {
			Params: &mcp.CallToolParamsRaw{Meta: mcp.Meta{apertureConversationIDMetaKey: "conv-meta"}},
			Extra:  &mcp.RequestExtra{Header: http.Header{apertureConversationIDHeaderName: {"conv-header"}}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := mcpJobScopeFromRequest(ctx, req)
			if err != nil {
				t.Fatalf("mcpJobScopeFromRequest: %v", err)
			}
			wantConversation := "conv-header"
			if name == "metadata" {
				wantConversation = "conv-meta"
			}
			if got.user != "alice" || got.conversation != wantConversation {
				t.Errorf("scope = %+v, want alice/%s", got, wantConversation)
			}
		})
	}

	for name, req := range map[string]*mcp.CallToolRequest{
		"nil request": nil,
		"no params":   {},
		"no metadata": {Params: &mcp.CallToolParamsRaw{}},
		"empty metadata and header": {
			Params: &mcp.CallToolParamsRaw{Meta: mcp.Meta{apertureConversationIDMetaKey: ""}},
			Extra:  &mcp.RequestExtra{Header: http.Header{apertureConversationIDHeaderName: {" "}}},
		},
		"non-string metadata": {Params: &mcp.CallToolParamsRaw{Meta: mcp.Meta{
			apertureConversationIDMetaKey: 123,
		}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := mcpJobScopeFromRequest(ctx, req); err == nil {
				t.Fatal("expected missing conversation identity error")
			}
		})
	}
}

func TestNestedCallToolRequestPreservesConversationHeader(t *testing.T) {
	ctx := context.WithValue(context.Background(), mcpUserKey{}, "alice")
	original := &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Meta: mcp.Meta{}},
		Extra: &mcp.RequestExtra{Header: http.Header{
			apertureConversationIDHeaderName: {"conv-header"},
		}},
	}

	nested := nestedCallToolRequest(original, json.RawMessage(`{"command":"true"}`))
	got, err := mcpJobScopeFromRequest(ctx, nested)
	if err != nil {
		t.Fatalf("mcpJobScopeFromRequest(nested): %v", err)
	}
	if got.user != "alice" || got.conversation != "conv-header" {
		t.Fatalf("nested scope = %+v, want alice/conv-header", got)
	}
}

func TestCreateMCPJobLogSkipsPersistentIDs(t *testing.T) {
	rootFS := t.TempDir()
	key := mcpJobScopeKey{user: "alice", conversation: "conversation"}
	logRoot := filepath.Join(rootFS, "tmp", ".ts", "jobs", mcpJobScopeDir(key))
	if err := os.MkdirAll(filepath.Join(logRoot, "j1"), 0700); err != nil {
		t.Fatal(err)
	}
	oldLog := filepath.Join(logRoot, "j1", "combined.log")
	if err := os.WriteFile(oldLog, []byte("old output\n"), 0600); err != nil {
		t.Fatal(err)
	}

	list := newMCPJobList() // Models a fresh daemon whose nextID reset to zero.
	id, logDir, log, err := createMCPJobLog(list, key, rootFS)
	if err != nil {
		t.Fatalf("createMCPJobLog: %v", err)
	}
	log.Close()
	if id != "j2" {
		t.Errorf("id = %q, want j2", id)
	}
	if want := filepath.Join("/tmp/.ts/jobs", mcpJobScopeDir(key), "j2"); logDir != want {
		t.Errorf("log dir = %q, want %q", logDir, want)
	}
	if got, err := os.ReadFile(oldLog); err != nil || string(got) != "old output\n" {
		t.Errorf("old log changed: content=%q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(logRoot, "j2", "combined.log")); err != nil {
		t.Errorf("new combined log: %v", err)
	}
}

func TestTruncateUTF8(t *testing.T) {
	t.Run("under limit unchanged", func(t *testing.T) {
		if got := truncateUTF8("hello", 100, "..."); got != "hello" {
			t.Errorf("got %q, want %q", got, "hello")
		}
	})

	t.Run("ascii truncate appends marker", func(t *testing.T) {
		got := truncateUTF8("abcdefgh", 5, "...")
		if got != "abcde..." {
			t.Errorf("got %q, want %q", got, "abcde...")
		}
	})

	t.Run("exact limit no marker", func(t *testing.T) {
		if got := truncateUTF8("abc", 3, "..."); got != "abc" {
			t.Errorf("got %q, want %q (no marker at exact limit)", got, "abc")
		}
	})

	t.Run("rune boundary not split", func(t *testing.T) {
		// € is 3 bytes (0xE2 0x82 0xAC). With a 2-byte limit, the trim must
		// drop the whole rune (cut back to 0 bytes) rather than emit a partial
		// 2-byte prefix, so the result is just the marker.
		got := truncateUTF8("€", 2, "...")
		if got != "..." {
			t.Errorf("got %q, want %q (partial rune must be dropped)", got, "...")
		}
	})

	t.Run("rune boundary preserves whole runes", func(t *testing.T) {
		// "a€" = 1 + 3 = 4 bytes. Limit 3: 'a' (1) fits, the € (3) would make 4
		// > 3, and only 2 bytes of headroom remain so the rune is dropped.
		got := truncateUTF8("a€b", 3, "…")
		if got != "a…" {
			t.Errorf("got %q, want %q", got, "a…")
		}
	})
}

// TestShellQuote covers the single-quote escaping used to interpolate paths and
// arguments safely into the generated shell commands.
func TestShellQuote(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"plain", "'plain'"},
		{"with space", "'with space'"},
		{"with'quote", "'with'\\''quote'"},
		{"", "''"},
	}
	for _, c := range cases {
		if got := shellQuote(c.in); got != c.want {
			t.Errorf("shellQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestErrMCPCommandTimeoutSentinel confirms the timeout sentinel is a distinct,
// errors.Is-matchable value, so handlers can branch on "timed out (has partial
// output)" vs "setup failed (no output)" without string-matching.
func TestErrMCPCommandTimeoutSentinel(t *testing.T) {
	if errMCPCommandTimeout == nil {
		t.Fatal("errMCPCommandTimeout is nil")
	}
	if errMCPCommandTimeout.Error() == "" {
		t.Errorf("errMCPCommandTimeout has empty message")
	}
	// Wrap it and confirm errors.Is still matches (handlers use errors.Is).
	wrapped := errors.Join(errMCPCommandTimeout, errors.New("ctx detail"))
	if !errors.Is(wrapped, errMCPCommandTimeout) {
		t.Errorf("errors.Is(wrapped, errMCPCommandTimeout) = false, want true")
	}
}
