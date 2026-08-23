// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// ts is a client for communicating with thundersnapd via its control socket.
// The protocol uses a vsock-style handshake: after connecting, the client sends
// "CONNECT <port>\n" and waits for "OK <port>\n" before proceeding with HTTP.
//
// In containers, ts connects to /thunder.sock (Unix socket).
// In VMs, ts connects directly via vsock to the host (CID 2) if /dev/vsock exists.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"net"

	"github.com/mdlayher/vsock"
	"github.com/pborman/getopt/v2"
	"github.com/tailscale/thundersnap/frameid"
	"github.com/tailscale/thundersnap/refs"
	"github.com/tailscale/thundersnap/thunderclient"
	"github.com/tailscale/thundersnap/thunderproto"
	"github.com/tailscale/thundersnap/tsm"
	"github.com/tailscale/thundersnap/version"
	"github.com/tailscale/thundersnap/vshdproto"
	"golang.org/x/term"
)

var sockPath = getopt.StringLong("sock", 0, "/id/thunder.sock", "path to control socket")
var help = getopt.BoolLong("help", 'h', "show this help and exit")
var showVersion = getopt.BoolLong("version", 0, "print version and exit")

// usage prints the main help to stderr and exits 1. It is the handler for
// malformed/missing top-level arguments; an explicit --help goes through
// printMainHelp on stdout with exit 0 instead.
func usage() {
	printMainHelp(os.Stderr)
	os.Exit(1)
}

// isShellInvocation reports whether argv0's basename indicates ts is being run
// as the container shell. thundersnapd symlinks /bin/sh -> /bin/ts for
// containers that lack a real shell; a login shell is exec'd with a leading
// dash ("-sh"), which we must also recognize.
func isShellInvocation(base string) bool {
	return base == "sh" || base == "-sh"
}

// isSuInvocation reports whether argv0's basename indicates ts is being run as
// the container 'su' (user-switcher). thundersnapd symlinks /bin/su -> /bin/ts
// for containers that lack a real su, so vshd's `su - user` works in a
// nil:nil:nil frame with no userspace tools installed. A login su is exec'd
// with a leading dash ("-su"), which we also recognize.
func isSuInvocation(base string) bool {
	return base == "su" || base == "-su"
}

func main() {
	base := filepath.Base(os.Args[0])
	// Check if we're being invoked as a shell (argv[0] is "sh" or "-sh").
	if isShellInvocation(base) {
		runAsShell()
		return
	}
	// Check if we're being invoked as su (argv[0] is "su" or "-su"). This lets
	// vshd switch to a non-root user in a minimal container that has no real su
	// binary, mirroring the /bin/sh -> ts symlink trick.
	if isSuInvocation(base) {
		runAsSu(os.Args[1:])
		return
	}

	getopt.SetParameters("<command> [command-options]")
	getopt.SetUsage(usage)
	// Use Getopt (not Parse) so we stop at the first non-option argument
	// (the subcommand). This lets subcommands handle their own flags.
	if err := getopt.Getopt(nil); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	args := getopt.Args()

	if *showVersion {
		fmt.Printf("ts %s\n", version.String())
		os.Exit(0)
	}
	if *help {
		printMainHelp(os.Stdout)
		os.Exit(0)
	}
	if len(args) == 0 {
		usage()
	}

	cmd := args[0]
	cmdArgs := args[1:]

	switch cmd {
	case "ping":
		cmdPing(cmdArgs)
	case "version":
		cmdVersion(cmdArgs)
	case "snap":
		cmdSnap(cmdArgs)
	case "snaps":
		cmdSnaps(cmdArgs)
	case "frame":
		cmdFrame(cmdArgs)
	case "frames":
		cmdFrames(cmdArgs)
	case "taint":
		cmdTaint(cmdArgs)
	case "download-docker":
		cmdDownloadDocker(cmdArgs)
	case "who-has":
		cmdWhoHas(cmdArgs)
	case "download-snap":
		cmdDownloadSnap(cmdArgs)
	case "ref":
		cmdRef(cmdArgs)
	case "refs":
		cmdRefs(cmdArgs)
	case "reflog":
		cmdReflog(cmdArgs)
	case "log":
		cmdLog(cmdArgs)
	case "autorun":
		cmdAutorun(cmdArgs)
	case "autoruns":
		cmdAutoruns(cmdArgs)
	case "go":
		cmdGo(cmdArgs)
	case "undo":
		cmdUndo(cmdArgs)
	case "drop-caps-and-run":
		// Hidden command - not listed in usage. Creates a fresh namespace
		// (must be PID 1 via Cloneflags:CLONE_NEWPID|CLONE_NEWNS), sets up
		// mounts (MS_PRIVATE, pivot_root, setupDev), drops caps, and execs.
		cmdDropCapsAndRun(cmdArgs)
	case "join-and-run":
		// Hidden command - joins an existing container namespace (entered via
		// ts nsenter) and runs a command inside it. Does zero mount operations;
		// the container-init has already set up everything. Replaces the old
		// 'drop-caps-and-run --skip-mount-setup' path.
		cmdJoinAndRun(cmdArgs)
	case "container-init":
		// Hidden command - starts a minimal init process for container namespaces
		cmdContainerInit(cmdArgs)
	case "autoclean":
		// Hidden command - ties a foreground subprocess to a lifecycle fd.
		cmdAutoclean(cmdArgs)
	case "su":
		// Switch user (drop privileges and exec a login shell or -c command).
		// Also reachable via the /bin/su -> ts symlink; this subcommand form is
		// for direct invocation/testing.
		runAsSu(cmdArgs)
	case "session-serve":
		// Hidden command - in-container vshd session endpoint. Runs after chroot
		// so the pty it opens lands in the container's devpts; speaks vshdproto
		// TLV on stdin/stdout, which vshd splices to the client connection.
		cmdSessionServe(cmdArgs)
	case "autorun-run":
		// Hidden command - runs one autorun attempt, retaining a visible process
		// in its session for the retry delay after a failure.
		cmdAutorunRun(cmdArgs)
	case "retry-on-fail":
		// Hidden diagnostic process used by autorun-run during retry backoff.
		cmdRetryOnFail(cmdArgs)
	case "nsenter":
		// Hidden command - CGO-free in-binary nsenter(1) used by vshd to join a
		// shared container namespace identically on the host and inside a VM.
		// The two-stage reexec marks its second stage with --stage2.
		if len(cmdArgs) > 0 && cmdArgs[0] == "--stage2" {
			cmdNsenterStage2(cmdArgs)
		} else {
			cmdNsenter(cmdArgs)
		}
	case "check-dev":
		// Hidden command for e2e testing - outputs /dev state
		cmdCheckDev()
	case "check-isolation":
		// Hidden command for e2e testing - outputs isolation state
		cmdCheckIsolation()
	default:
		fmt.Fprintf(os.Stderr, "error: unknown command: %s\n", cmd)
		os.Exit(1)
	}
}

func cmdPing(args []string) {
	opts, helpFlag := newCmdOpts("ping", "")
	parseCmd(opts, "ping", args)

	if *helpFlag {
		printCommandHelp(os.Stdout, "ping", opts)
		os.Exit(0)
	}

	if opts.NArgs() > 0 {
		fmt.Fprintln(os.Stderr, "error: ping takes no arguments")
		os.Exit(1)
	}

	if err := doPing(*sockPath); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// ControlRequest represents a request to the control socket.
type ControlRequest struct {
	Command string `json:"command"`
}

// ControlResponse represents a response from the control socket.
type ControlResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	// Version is set by the /version endpoint (and only meaningful then): the
	// server's build version string, for the client/server version handshake.
	Version string `json:"version,omitempty"`
}

// progressRenderer renders NDJSON "progress" events to stderr for the four
// streaming subcommands (snap, create, download-docker, download-snap). On a
// TTY it overwrites a single line (truncating to the terminal width and padding
// to erase the previous, longer line); otherwise it prints each message on its
// own line. Finish clears the in-progress TTY line at end of stream.
type progressRenderer struct {
	tty         bool
	width       int
	lastLineLen int
}

// newProgressRenderer probes stderr for a terminal and its width (defaulting to
// 80 columns when the width is unavailable).
func newProgressRenderer() *progressRenderer {
	r := &progressRenderer{width: 80}
	if term.IsTerminal(int(os.Stderr.Fd())) {
		r.tty = true
		if w, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && w > 0 {
			r.width = w
		}
	}
	return r
}

// progress renders a single progress message. On a TTY it overwrites the
// current line; otherwise it prints the message on its own line.
func (r *progressRenderer) progress(msg string) {
	if !r.tty {
		fmt.Fprintln(os.Stderr, msg)
		return
	}
	if len(msg) > r.width {
		msg = msg[:r.width]
	}
	padding := ""
	if len(msg) < r.lastLineLen {
		padding = strings.Repeat(" ", r.lastLineLen-len(msg))
	}
	fmt.Fprintf(os.Stderr, "\r%s%s", msg, padding)
	r.lastLineLen = len(msg)
}

// finish erases the in-progress TTY line (a no-op on non-TTY or when nothing
// was rendered) so subsequent output starts on a clean line.
func (r *progressRenderer) finish() {
	if r.tty && r.lastLineLen > 0 {
		fmt.Fprintf(os.Stderr, "\r%s\r", strings.Repeat(" ", r.lastLineLen))
	}
}

func doPing(sockPath string) error {
	client := thunderclient.NewHTTPClient(sockPath)

	req := ControlRequest{Command: "ping"}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	resp, err := client.Post("http://localhost/ping", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned status %d", resp.StatusCode)
	}

	var result ControlResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	fmt.Println(result.Message)
	return nil
}

// cmdVersion implements `ts version`: it reports the build version of the ts
// client AND of the thundersnapd it is talking to, printing the single agreed
// version on success or an error on mismatch/unreachability. The successful
// output is exactly one line: the version running on both client and server.
func cmdVersion(args []string) {
	opts, helpFlag := newCmdOpts("version", "")
	parseCmd(opts, "version", args)
	if *helpFlag {
		printCommandHelp(os.Stdout, "version", opts)
		os.Exit(0)
	}
	if opts.NArgs() > 0 {
		fmt.Fprintln(os.Stderr, "error: version takes no arguments")
		os.Exit(1)
	}

	res := runVersion(*sockPath)
	switch {
	case res.err != nil:
		fmt.Fprintf(os.Stderr, "error: %v\n", res.err)
		os.Exit(1)
	case !res.matched:
		fmt.Fprintf(os.Stderr, "error: version mismatch: client %q, server %q\n", res.clientVer, res.serverVer)
		os.Exit(1)
	default:
		fmt.Println(res.clientVer)
	}
}

// versionResult is the outcome of `ts version`: the client and server versions
// (server empty on error), whether they matched, and any transport error.
type versionResult struct {
	clientVer string
	serverVer string
	matched   bool
	err       error
}

// runVersion is the testable core of `ts version`: it fetches the server's
// build version and compares it to the client's, without printing or exiting.
func runVersion(sockPath string) versionResult {
	clientVer := version.String()
	serverVer, err := getServerVersion(sockPath)
	if err != nil {
		return versionResult{clientVer: clientVer, err: err}
	}
	return versionResult{
		clientVer: clientVer,
		serverVer: serverVer,
		matched:   serverVer == clientVer,
	}
}

// getServerVersion asks thundersnapd for its build version over the control
// socket. It returns the server's version string, or an error if the daemon is
// unreachable or did not report a version.
func getServerVersion(sockPath string) (string, error) {
	client := thunderclient.NewHTTPClient(sockPath)

	req := ControlRequest{Command: "version"}
	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}
	resp, err := client.Post("http://localhost/version", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("server returned status %d", resp.StatusCode)
	}

	var result ControlResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if result.Status != "ok" {
		return "", fmt.Errorf("server returned status %q", result.Status)
	}
	if result.Version == "" {
		return "", fmt.Errorf("server did not report a version")
	}
	return result.Version, nil
}

// meshPort is the HTTP port for mesh discovery (TSTS in leetspeak = 7575)
const meshPort = 7575

// meshPeer represents a peer from /ts/servers.json
type meshPeer struct {
	URL      string    `json:"url"`
	Hostname string    `json:"hostname"`
	LastSeen time.Time `json:"last_seen"`
}

func cmdSnap(args []string) {
	opts, helpFlag := newCmdOpts("snap", "[<path>]")
	deleteFlag := opts.BoolLong("delete", 'd', "delete a snapshot")
	waitFlag := opts.BoolLong("wait", 'w', "wait for indexing to complete and print the snap ID (default; retained for compatibility and overrides --quick)")
	quickFlag := opts.BoolLong("quick", 'q', "capture and return quietly while indexing continues in the background")
	parseCmd(opts, "snap", args)

	if *helpFlag {
		printCommandHelp(os.Stdout, "snap", opts)
		os.Exit(0)
	}

	if *deleteFlag {
		if opts.NArgs() != 1 {
			fmt.Fprintln(os.Stderr, "error: --delete requires exactly one snapshot ID argument")
			fmt.Fprintln(os.Stderr, "usage: ts snap --delete <snapshot-id>")
			os.Exit(1)
		}
		snapshotID := opts.Arg(0)
		if err := doDeleteSnap(*sockPath, snapshotID); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Deleted snapshot %s\n", snapshotID)
		return
	}

	if opts.NArgs() > 1 {
		fmt.Fprintln(os.Stderr, "error: snap takes at most one path argument")
		fmt.Fprintln(os.Stderr, "usage: ts snap [<path>]    snapshot the whole frame, or just <path>'s subtree")
		os.Exit(1)
	}

	// Optional subdir argument: snapshot just that subtree of the frame.
	// Resolve it to a path that is absolute within the container so the
	// daemon can map it onto the frame's rootfs.
	subdir := ""
	if opts.NArgs() == 1 {
		resolved, err := resolveSnapSubdir(opts.Arg(0))
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		subdir = resolved
	}

	// Waiting is the default. --quick selects fire-and-forget mode, while an
	// explicit --wait wins if both flags are supplied for compatibility with
	// callers that append -w to a shared option list.
	wait := !*quickFlag || *waitFlag
	snapshotID, err := doSnap(*sockPath, subdir, wait)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if wait {
		fmt.Println(snapshotID)
	}
	// Quick success is deliberately silent on both stdout and stderr. The ID
	// is not known yet, and Unix commands generally need not announce success.
}

// resolveSnapSubdir turns a user-supplied path (absolute or relative to the
// current working directory inside the container) into a clean container-
// absolute path with the leading slash stripped, suitable for the daemon's
// "subdir" parameter. It rejects paths that don't exist or aren't directories.
func resolveSnapSubdir(arg string) (string, error) {
	abs := arg
	if !filepath.IsAbs(abs) {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get working directory: %w", err)
		}
		abs = filepath.Join(cwd, arg)
	}
	abs = filepath.Clean(abs)

	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("path %q: %w", arg, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path %q is not a directory", arg)
	}

	rel := strings.TrimPrefix(abs, "/")
	if rel == "" {
		return "", fmt.Errorf("cannot snap the container root as a subdir; run 'ts snap' with no argument")
	}
	return rel, nil
}

func cmdSnaps(args []string) {
	opts, helpFlag := newCmdOpts("snaps", "")
	parseCmd(opts, "snaps", args)

	if *helpFlag {
		printCommandHelp(os.Stdout, "snaps", opts)
		os.Exit(0)
	}

	if opts.NArgs() > 0 {
		fmt.Fprintln(os.Stderr, "error: snaps takes no arguments")
		fmt.Fprintln(os.Stderr, "usage: ts snaps    list all snapshots")
		os.Exit(1)
	}

	if err := doListSnaps(*sockPath); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// SnapResponse is the response from the /snap endpoint (non-streaming)
type SnapResponse struct {
	Status     string `json:"status"`
	SnapshotID string `json:"snapshot_id,omitempty"`
	Message    string `json:"message,omitempty"`
}

// SnapStreamEvent is a single event in the streaming snap response (NDJSON format).
type SnapStreamEvent struct {
	Type       string `json:"type"`                  // "progress" or "result"
	Message    string `json:"message,omitempty"`     // progress message
	Status     string `json:"status,omitempty"`      // "ok" or "error" (for result)
	SnapshotID string `json:"snapshot_id,omitempty"` // snapshot ID (for result)
}

func doSnap(sockPath, subdir string, wait bool) (string, error) {
	client := thunderclient.NewHTTPClient(sockPath)
	render := newProgressRenderer()

	// Build URL with streaming enabled. wait=1 makes the server block until
	// indexing completes and stream back progress + the snap ID; without it
	// the server captures and returns immediately, indexing in the background.
	url := "http://localhost/snap?stream=1"
	if wait {
		url += "&wait=1"
	}
	if render.tty {
		url += "&tty=1"
	}
	if subdir != "" {
		url += "&subdir=" + neturl.QueryEscape(subdir)
	}

	resp, err := client.Post(url, "application/json", nil)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Parse NDJSON stream
	scanner := bufio.NewScanner(resp.Body)
	var lastEvent SnapStreamEvent
	var lastProgressMsg string

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var event SnapStreamEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return "", fmt.Errorf("parse stream event: %w (line: %q)", err, string(line))
		}

		switch {
		case event.Type == "progress":
			lastProgressMsg = event.Message
			render.progress(event.Message)
		case event.Type == "result":
			lastEvent = event
		case event.Type == "" && event.Status != "":
			// Non-streaming error response (e.g., emitted before the stream
			// started). Treat it as the result so the status check below fires.
			lastEvent = event
			lastEvent.Type = "result"
		}
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read stream: %w", err)
	}

	render.finish()
	// On a TTY the progress line was overwritten in place, so re-print the final
	// "done" message; on non-TTY it was already printed on its own line.
	if render.tty && lastProgressMsg != "" {
		fmt.Fprintln(os.Stderr, lastProgressMsg)
	}

	// Check result
	if lastEvent.Type != "result" {
		return "", fmt.Errorf("no result received from server")
	}

	if lastEvent.Status != "ok" {
		return "", fmt.Errorf("snap failed: %s", lastEvent.Message)
	}

	// In fire-and-forget mode the server returns no snapshot ID (indexing is
	// still running in the background); return an empty string to signal that.
	return lastEvent.SnapshotID, nil
}

// DeleteSnapRequest is the request body for /delete-snap
type DeleteSnapRequest struct {
	SnapshotID string `json:"snapshot_id"`
}

// DeleteSnapResponse is the response from /delete-snap
type DeleteSnapResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

func doDeleteSnap(sockPath, snapshotID string) error {
	result, err := thunderclient.PostJSON[DeleteSnapRequest, DeleteSnapResponse](sockPath, "/delete-snap",
		DeleteSnapRequest{SnapshotID: snapshotID})
	if err != nil {
		return err
	}
	if result.Status != "ok" {
		return fmt.Errorf("%s", result.Message)
	}
	return nil
}

// ListSnapsResponse is the response from /list-snaps
type ListSnapsResponse struct {
	Status string     `json:"status"`
	Snaps  []SnapInfo `json:"snaps,omitempty"`
	Error  string     `json:"error,omitempty"`
}

// SnapInfo contains info about a single snapshot
type SnapInfo struct {
	ID   string `json:"id"`
	Size uint64 `json:"size"` // Total size in bytes from TSM header
}

func doListSnaps(sockPath string) error {
	client := thunderclient.NewHTTPClient(sockPath)

	resp, err := client.Get("http://localhost/list-snaps")
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	var result ListSnapsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	if result.Status != "ok" {
		return fmt.Errorf("%s", result.Error)
	}

	// Sort by ID for consistent output
	sort.Slice(result.Snaps, func(i, j int) bool {
		return result.Snaps[i].ID < result.Snaps[j].ID
	})

	// Print in du-like format: size first, then ID
	for _, snap := range result.Snaps {
		sizeGB := float64(snap.Size) / (1024 * 1024 * 1024)
		fmt.Printf("%8.3fG  %s\n", sizeGB, snap.ID)
	}

	return nil
}

func cmdFrame(args []string) {
	opts, helpFlag := newCmdOpts("frame", "[<spec>]")
	isolation := opts.StringLong("isolation", 0, "", "isolation level: vm, container, none")
	refName := opts.StringLong("ref", 0, "", "create a ref with this name pointing at the new frame")
	deleteFlag := opts.BoolLong("delete", 'd', "delete a frame by UUID")
	parseCmd(opts, "frame", args)

	if *helpFlag {
		printCommandHelp(os.Stdout, "frame", opts)
		os.Exit(0)
	}

	if *deleteFlag {
		if opts.NArgs() != 1 {
			fmt.Fprintln(os.Stderr, "error: --delete requires exactly one frame UUID argument")
			fmt.Fprintln(os.Stderr, "usage: ts frame --delete <uuid>")
			os.Exit(1)
		}
		uuid := opts.Arg(0)
		// `ts frame --delete` addresses a frame by UUID; reject anything that is
		// not a valid UUID so a bogus string is never sent to the daemon. (To
		// delete a frame you reached via a ref, delete the ref first, then delete
		// the now-unreferenced frame by UUID — the daemon refuses to delete a
		// frame that still has refs.)
		if _, err := frameid.Parse(uuid); err != nil {
			fmt.Fprintf(os.Stderr, "error: %q is not a valid frame UUID: %v\n", uuid, err)
			fmt.Fprintln(os.Stderr, "usage: ts frame --delete <uuid>")
			os.Exit(1)
		}
		if err := doDeleteFrame(*sockPath, uuid); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Deleted frame %s\n", uuid)
		return
	}

	// No argument: print current frame UUID
	if opts.NArgs() == 0 {
		uuid, err := doGetCurrentFrame(*sockPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(uuid)
		return
	}

	if opts.NArgs() != 1 {
		frameUsage()
		os.Exit(1)
	}

	spec := opts.Arg(0)

	// Validate snap triplet syntax: exactly two colons required for creation
	colonCount := strings.Count(spec, ":")
	if colonCount == 1 {
		fmt.Fprintln(os.Stderr, "error: invalid spec - one colon is invalid")
		fmt.Fprintln(os.Stderr, "       use two colons for snap triplet: root:home:work")
		os.Exit(1)
	}
	if colonCount > 2 {
		fmt.Fprintln(os.Stderr, "error: invalid spec - too many colons")
		fmt.Fprintln(os.Stderr, "       snap triplet format: root:home:work (exactly two colons)")
		os.Exit(1)
	}

	// No colons: this is a UUID or ref resolution (not creation). Validate it
	// is a valid frame name (UUID or ref) client-side so a bogus string gives
	// a clean error here rather than reaching the daemon.
	if colonCount == 0 {
		if err := refs.ValidateFrameName(spec); err != nil {
			fmt.Fprintf(os.Stderr, "error: invalid frame name %q: %v\n", spec, err)
			fmt.Fprintln(os.Stderr, "       frame names must be a valid UUID or a ref (start with a letter; letters/digits/dash/underscore only)")
			os.Exit(1)
		}
		uuid, err := doResolveFrame(*sockPath, spec)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(uuid)
		return
	}

	// Two colons: snap triplet. If a ref name is requested, validate it
	// client-side (the daemon validates too, but a clean local error is better
	// than a round-trip rejection).
	if *refName != "" {
		if err := refs.ValidateName(*refName); err != nil {
			fmt.Fprintf(os.Stderr, "error: invalid ref name %q: %v\n", *refName, err)
			os.Exit(1)
		}
	}
	// Special case: :: snaps the current frame and creates a new frame from it.
	// Fork does this without blocking on indexing: it captures the current
	// frame as a background snap and clones a new frame from the live
	// filesystem. (ts frame :: is otherwise identical to ts go :: minus the
	// session entry.)
	if spec == "::" {
		uuid, err := doFork(*sockPath, *refName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error forking current frame: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(uuid)
		return
	}

	// Resolve snap/frame/ref components, then create.
	snapshotSpec, sourceFrames, err := resolveFrameTriplet(*sockPath, spec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	uuid, err := doCreate(*sockPath, snapshotSpec, *isolation, *refName, sourceFrames[:]...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	// Output just the UUID for scripting
	fmt.Println(uuid)
}

func frameUsage() {
	fmt.Fprintln(os.Stderr, "usage: ts frame                            print current frame UUID")
	fmt.Fprintln(os.Stderr, "       ts frame <uuid>                     validate UUID exists, print it")
	fmt.Fprintln(os.Stderr, "       ts frame <ref>                      resolve ref to UUID")
	fmt.Fprintln(os.Stderr, "       ts frame <root:home:work>           create frame from snap triplet")
	fmt.Fprintln(os.Stderr, "       ts frame --delete <uuid>            delete a frame")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "triplet syntax (exactly two colons):")
	fmt.Fprintln(os.Stderr, "  - each component may be a snap, frame UUID, or ref")
	fmt.Fprintln(os.Stderr, "  - a frame/ref contributes its root, home, or work component by position")
	fmt.Fprintln(os.Stderr, "  - empty components use the corresponding component of the current frame")
	fmt.Fprintln(os.Stderr, "  - nil explicitly requests an empty component")
	fmt.Fprintln(os.Stderr, "  - ts frame nil:deb:nil     empty root/work, /home from ref deb")
	fmt.Fprintln(os.Stderr, "  - ts frame a:b:c           root from a, home from b, work from c")
	fmt.Fprintln(os.Stderr, "  - ts frame ::              fork the current frame")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "options:")
	fmt.Fprintln(os.Stderr, "  --ref <name>         create a ref pointing at the new frame")
	fmt.Fprintln(os.Stderr, "  --isolation <level>  vm, container, or none")
}

func cmdFrames(args []string) {
	opts, helpFlag := newCmdOpts("frames", "")
	parseCmd(opts, "frames", args)

	if *helpFlag {
		printCommandHelp(os.Stdout, "frames", opts)
		os.Exit(0)
	}

	if opts.NArgs() > 0 {
		fmt.Fprintln(os.Stderr, "error: frames takes no arguments")
		fmt.Fprintln(os.Stderr, "usage: ts frames    list all frames")
		os.Exit(1)
	}

	if err := doListFrames(*sockPath); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// CreateRequest is the request body for /create
type CreateRequest struct {
	SnapshotSpec string `json:"snapshot_spec"` // <root>:<home>:<work>
	Isolation    string `json:"isolation,omitempty"`
	RefName      string `json:"ref_name,omitempty"` // optional ref to create
	RootfsFrame  string `json:"rootfs_frame,omitempty"`
	HomeFrame    string `json:"home_frame,omitempty"`
	WorkFrame    string `json:"work_frame,omitempty"`
}

// CreateResponse is the response from /create
type CreateResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	UUID    string `json:"uuid,omitempty"` // the new frame's UUID
	Path    string `json:"path,omitempty"`
}

// CreateStreamEvent is an event in the streaming create response
type CreateStreamEvent struct {
	Type    string `json:"type"`
	Message string `json:"message,omitempty"`
	Status  string `json:"status,omitempty"`
	UUID    string `json:"uuid,omitempty"`
	Path    string `json:"path,omitempty"`
}

func doCreate(sockPath, snapshotSpec, isolation, refName string, sourceFrames ...string) (string, error) {
	client := thunderclient.NewHTTPClient(sockPath)
	render := newProgressRenderer()
	quick := false
	for _, source := range sourceFrames {
		quick = quick || source != ""
	}

	// Build URL with streaming enabled
	url := "http://localhost/create?stream=1"
	if render.tty {
		url += "&tty=1"
	}

	req := CreateRequest{
		SnapshotSpec: snapshotSpec,
		Isolation:    isolation,
		RefName:      refName,
	}
	if len(sourceFrames) == 3 {
		req.RootfsFrame, req.HomeFrame, req.WorkFrame = sourceFrames[0], sourceFrames[1], sourceFrames[2]
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Parse NDJSON stream
	scanner := bufio.NewScanner(resp.Body)
	var lastEvent CreateStreamEvent

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var event CreateStreamEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return "", fmt.Errorf("parse stream event: %w (line: %q)", err, string(line))
		}

		switch {
		case event.Type == "progress":
			if !quick {
				render.progress(event.Message)
			}
		case event.Type == "result":
			lastEvent = event
		case event.Type == "" && event.Status != "":
			// Non-streaming error response (e.g., frame already exists).
			// Convert to a result event for consistent handling.
			lastEvent = event
			lastEvent.Type = "result"
		}
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read stream: %w", err)
	}

	if !quick {
		render.finish()
	}

	// Check result
	if lastEvent.Type != "result" {
		return "", fmt.Errorf("no result received from server")
	}

	if lastEvent.Status != "ok" {
		return "", fmt.Errorf("%s", lastEvent.Message)
	}

	return lastEvent.UUID, nil
}

// ForkRequest is the request body for /fork.
type ForkRequest struct {
	RefName string `json:"ref_name,omitempty"`
}

// ForkResponse is the response from /fork.
type ForkResponse struct {
	Status  string `json:"status"`
	UUID    string `json:"uuid,omitempty"`
	Message string `json:"message,omitempty"`
}

// doFork captures the current frame as a background snap and creates a new
// frame cloned from the current frame's live filesystem, returning the new
// frame's UUID. This is the fast path for `ts go ::` / `ts frame ::`: it does
// not block on content-addressable indexing (the current frame's snap is
// recorded in the background). See background-indexing.md.
func doFork(sockPath, refName string) (string, error) {
	result, err := thunderclient.PostJSON[ForkRequest, ForkResponse](sockPath, "/fork", ForkRequest{RefName: refName})
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	if result.Status != "ok" {
		return "", fmt.Errorf("%s", result.Message)
	}
	return result.UUID, nil
}

// DeleteFrameRequest is the request body for /delete-frame
type DeleteFrameRequest struct {
	UUID string `json:"uuid"`
}

// DeleteFrameResponse is the response from /delete-frame
type DeleteFrameResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

func doDeleteFrame(sockPath, uuid string) error {
	client := thunderclient.NewHTTPClient(sockPath)

	req := DeleteFrameRequest{UUID: uuid}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	resp, err := client.Post("http://localhost/delete-frame", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	var result DeleteFrameResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	if result.Status != "ok" {
		return fmt.Errorf("%s", result.Message)
	}

	return nil
}

// GetFrameResponse is the response from GET /frame
type GetFrameResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	UUID    string `json:"uuid"`
	Rootfs  string `json:"rootfs,omitempty"`
	Home    string `json:"home,omitempty"`
	Work    string `json:"work,omitempty"`
}

// doGetCurrentFrame returns the current frame's UUID.
func doGetCurrentFrame(sockPath string) (string, error) {
	client := thunderclient.NewHTTPClient(sockPath)

	resp, err := client.Get("http://localhost/frame")
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	var result GetFrameResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	if result.Status != "ok" {
		return "", fmt.Errorf("%s", result.Message)
	}

	return result.UUID, nil
}

// doGetCurrentFrameInfo returns the current frame's full metadata.
func doGetCurrentFrameInfo(sockPath string) (*GetFrameResponse, error) {
	client := thunderclient.NewHTTPClient(sockPath)

	resp, err := client.Get("http://localhost/frame")
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	var result GetFrameResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if result.Status != "ok" {
		return nil, fmt.Errorf("%s", result.Message)
	}

	return &result, nil
}

// ResolveFrameRequest is the request body for /resolve-frame
type ResolveFrameRequest struct {
	Spec string `json:"spec"`
}

// ResolveFrameResponse is the response from /resolve-frame
type ResolveFrameResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	UUID    string `json:"uuid"`
	Exists  bool   `json:"exists"`
	IsRef   bool   `json:"is_ref,omitempty"`
	RefName string `json:"ref_name,omitempty"`
	Rootfs  string `json:"rootfs,omitempty"`
	Home    string `json:"home,omitempty"`
	Work    string `json:"work,omitempty"`
}

// doResolveFrame resolves a UUID or ref name to a frame UUID.
func doResolveFrame(sockPath, spec string) (string, error) {
	result, err := thunderclient.PostJSON[ResolveFrameRequest, ResolveFrameResponse](
		sockPath, "/resolve-frame", ResolveFrameRequest{Spec: spec})
	if err != nil {
		return "", err
	}
	if result.Status != "ok" {
		return "", fmt.Errorf("%s", result.Message)
	}
	if !result.Exists {
		return "", fmt.Errorf("frame or ref %q not found", spec)
	}
	return result.UUID, nil
}

// resolveSnapTriplet resolves each component as either a snap ID, a frame UUID,
// or a ref. A frame/ref contributes its corresponding root, home, or work snap;
// an empty component names the current frame, and "nil" remains explicitly
// empty. Returns a snap-only spec like "abc:def:ghi".
func resolveSnapTriplet(sockPath, spec string) (string, error) {
	resolved, _, err := resolveFrameTriplet(sockPath, spec)
	return resolved, err
}

// resolveFrameTriplet also returns the source frame UUID for each component.
// The daemon uses these to clone the named frame's live component rather than
// merely cloning the snapshot from which that frame was originally created.
func resolveFrameTriplet(sockPath, spec string) (string, [3]string, error) {
	parts := strings.Split(spec, ":")
	if len(parts) != 3 {
		return "", [3]string{}, fmt.Errorf("invalid snap triplet: expected exactly 2 colons")
	}

	var sources [3]string
	var current *GetFrameResponse
	for i, part := range parts {
		if part == "nil" {
			continue
		}

		var frame *GetFrameResponse
		if part == "" {
			if current == nil {
				var err error
				current, err = doGetCurrentFrameInfo(sockPath)
				if err != nil {
					return "", sources, fmt.Errorf("get current frame: %w", err)
				}
			}
			frame = current
			sources[i] = current.UUID
		} else {
			resolved, err := doResolveFrameInfo(sockPath, part)
			if err != nil {
				return "", sources, err
			}
			if !resolved.Exists {
				continue // Not a frame or ref: leave this component as a snap ID.
			}
			frame = &GetFrameResponse{Rootfs: resolved.Rootfs, Home: resolved.Home, Work: resolved.Work}
			sources[i] = resolved.UUID
		}

		switch i {
		case 0:
			parts[i] = frame.Rootfs
		case 1:
			parts[i] = frame.Home
		case 2:
			parts[i] = frame.Work
		}
		if parts[i] == "" {
			parts[i] = "nil"
		}
	}
	return strings.Join(parts, ":"), sources, nil
}

func doResolveFrameInfo(sockPath, spec string) (*ResolveFrameResponse, error) {
	result, err := thunderclient.PostJSON[ResolveFrameRequest, ResolveFrameResponse](
		sockPath, "/resolve-frame", ResolveFrameRequest{Spec: spec})
	if err != nil {
		return nil, err
	}
	if result.Status != "ok" {
		return nil, fmt.Errorf("%s", result.Message)
	}
	return &result, nil
}

// ListFramesResponse is the response from /list-frames
type ListFramesResponse struct {
	Status string      `json:"status"`
	Frames []FrameInfo `json:"frames,omitempty"`
	Error  string      `json:"error,omitempty"`
}

// FrameInfo contains info about a single frame
type FrameInfo struct {
	UUID   string   `json:"uuid"`
	Status string   `json:"status"` // "stopped" or number of sessions
	Refs   []string `json:"refs,omitempty"`
}

func doListFrames(sockPath string) error {
	client := thunderclient.NewHTTPClient(sockPath)

	resp, err := client.Get("http://localhost/list-frames")
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	var result ListFramesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	if result.Status != "ok" {
		return fmt.Errorf("%s", result.Error)
	}

	printFrames(result.Frames, false)

	return nil
}

// printFrames emits the common `frames`/`refs` format. Frames are sorted by
// UUID and refs within a frame are sorted alphabetically.
func printFrames(frames []FrameInfo, refsOnly bool) {
	sort.Slice(frames, func(i, j int) bool {
		return frames[i].UUID < frames[j].UUID
	})
	for _, frame := range frames {
		if refsOnly && len(frame.Refs) == 0 {
			continue
		}
		sort.Strings(frame.Refs)
		fmt.Printf("%-7s  %s", frame.Status, frame.UUID)
		for _, ref := range frame.Refs {
			fmt.Printf(" %s", ref)
		}
		fmt.Println()
	}
}

func cmdWhoHas(args []string) {
	opts, helpFlag := newCmdOpts("who-has", "<snapshot-id>")
	parseCmd(opts, "who-has", args)

	if *helpFlag {
		printCommandHelp(os.Stdout, "who-has", opts)
		os.Exit(0)
	}

	if opts.NArgs() != 1 {
		fmt.Fprintln(os.Stderr, "error: who-has requires exactly one argument: snapshot-id")
		fmt.Fprintln(os.Stderr, "usage: ts who-has <snapshot-id>")
		os.Exit(1)
	}

	snapshotID := opts.Arg(0)

	// Detect frame specs (colon-separated) and give helpful error
	if strings.Contains(snapshotID, ":") {
		parts := strings.Split(snapshotID, ":")
		var nonEmpty []string
		for _, p := range parts {
			if p != "" && p != "nil" {
				nonEmpty = append(nonEmpty, p)
			}
		}
		fmt.Fprintln(os.Stderr, "error: who-has can only query one snap at a time")
		fmt.Fprintln(os.Stderr, "")
		if len(nonEmpty) == 0 {
			fmt.Fprintln(os.Stderr, "The frame spec contains no non-empty snaps.")
		} else {
			fmt.Fprintf(os.Stderr, "Try querying each snap separately (%d commands):\n", len(nonEmpty))
			for _, snap := range nonEmpty {
				fmt.Fprintf(os.Stderr, "  ts who-has %s\n", snap)
			}
		}
		os.Exit(1)
	}

	peers, err := doWhoHas(*sockPath, snapshotID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if len(peers) == 0 {
		fmt.Fprintf(os.Stderr, "No peers have snapshot %s\n", snapshotID)
		os.Exit(1)
	}

	// Print machine-readable list of bupdate URLs (one per line)
	for _, peer := range peers {
		fmt.Printf("%s/bupdate/\n", strings.TrimSuffix(peer.PeerURL, "/"))
	}
}

// WhoHasRequest is the request body for /who-has
type WhoHasRequest struct {
	SnapshotID string `json:"snapshot_id"`
}

// WhoHasResponse is the response from /who-has
type WhoHasResponse struct {
	Status string           `json:"status"`
	Peers  []WhoHasPeerInfo `json:"peers,omitempty"`
	Error  string           `json:"error,omitempty"`
}

// WhoHasPeerInfo represents a peer that has the snapshot
type WhoHasPeerInfo struct {
	Hostname string `json:"hostname"`
	URL      string `json:"url"`
}

func doWhoHas(sockPath, snapshotID string) ([]tsm.PeerResult, error) {
	client := thunderclient.NewHTTPClient(sockPath)

	req := WhoHasRequest{SnapshotID: snapshotID}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	resp, err := client.Post("http://localhost/who-has", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	var result WhoHasResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if result.Status != "ok" {
		return nil, fmt.Errorf("%s", result.Error)
	}

	// Convert to tsm.PeerResult for compatibility with existing code
	var peers []tsm.PeerResult
	for _, p := range result.Peers {
		peers = append(peers, tsm.PeerResult{
			Hostname: p.Hostname,
			PeerURL:  p.URL,
			HasSnap:  true,
		})
	}

	return peers, nil
}

func cmdTaint(args []string) {
	opts, helpFlag := newCmdOpts("taint", "[<taint-name>]")
	parseCmd(opts, "taint", args)

	if *helpFlag {
		printCommandHelp(os.Stdout, "taint", opts)
		os.Exit(0)
	}

	// No argument: list the current frame's taints.
	if opts.NArgs() == 0 {
		if err := doListTaints(*sockPath); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if opts.NArgs() != 1 {
		fmt.Fprintln(os.Stderr, "error: taint takes at most one argument: taint-name")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "usage: ts taint [<taint-name>]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "examples:")
		fmt.Fprintln(os.Stderr, "  ts taint                  list current frame's taints")
		fmt.Fprintln(os.Stderr, "  ts taint pii:customers    add a taint")
		fmt.Fprintln(os.Stderr, "  ts taint unsafe-permissions")
		os.Exit(1)
	}

	taintName := opts.Arg(0)

	if err := doTaint(*sockPath, taintName); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// TaintRequest is the request body for /taint
type TaintRequest struct {
	TaintName string `json:"taint_name"`
}

// TaintResponse is the response from /taint
type TaintResponse struct {
	Status  string   `json:"status"`
	Message string   `json:"message,omitempty"`
	Taints  []string `json:"taints,omitempty"`
}

func doTaint(sockPath, taintName string) error {
	result, err := thunderclient.PostJSON[TaintRequest, TaintResponse](sockPath, "/taint",
		TaintRequest{TaintName: taintName})
	if err != nil {
		return err
	}

	if result.Status != "ok" {
		return fmt.Errorf("server error: %s", result.Message)
	}

	fmt.Printf("Added taint: %s\n", taintName)
	if len(result.Taints) > 0 {
		fmt.Printf("Current taints: %v\n", result.Taints)
	}
	return nil
}

// doListTaints lists the current frame's taints via GET /taint.
func doListTaints(sockPath string) error {
	client := thunderclient.NewHTTPClient(sockPath)
	resp, err := client.Get("http://localhost/taint")
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	var result TaintResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if result.Status != "ok" {
		return fmt.Errorf("server error: %s", result.Message)
	}
	if len(result.Taints) == 0 {
		fmt.Println("(no taints)")
		return nil
	}
	for _, t := range result.Taints {
		fmt.Println(t)
	}
	return nil
}

func cmdDownloadDocker(args []string) {
	opts, helpFlag := newCmdOpts("download-docker", "<image-reference>")
	parseCmd(opts, "download-docker", args)

	if *helpFlag {
		printCommandHelp(os.Stdout, "download-docker", opts)
		os.Exit(0)
	}

	if opts.NArgs() != 1 {
		fmt.Fprintln(os.Stderr, "error: download-docker requires exactly one argument: image-reference")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "usage: ts download-docker <image-reference>")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "examples:")
		fmt.Fprintln(os.Stderr, "  ts download-docker ubuntu:24.04")
		fmt.Fprintln(os.Stderr, "  ts download-docker docker.io/library/golang:1.22")
		os.Exit(1)
	}

	imageRef := opts.Arg(0)

	if err := doDownloadDocker(*sockPath, imageRef); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// DownloadDockerRequest is the request body for /download-docker
type DownloadDockerRequest struct {
	ImageRef string `json:"image_ref"`
}

// DownloadDockerResponse is the response from /download-docker
type DownloadDockerResponse struct {
	Status     string `json:"status"`
	Message    string `json:"message,omitempty"`
	SnapshotID string `json:"snapshot_id,omitempty"`
	Cached     bool   `json:"cached,omitempty"`
}

// DownloadDockerStreamEvent is an event in the streaming download response
type DownloadDockerStreamEvent struct {
	Type       string `json:"type"`
	Message    string `json:"message,omitempty"`
	Status     string `json:"status,omitempty"`
	SnapshotID string `json:"snapshot_id,omitempty"`
	Cached     bool   `json:"cached,omitempty"`
}

func doDownloadDocker(sockPath, imageRef string) error {
	client := thunderclient.NewHTTPClient(sockPath)
	client.Timeout = 30 * time.Minute // Docker downloads can be slow
	render := newProgressRenderer()

	// Build URL with streaming enabled
	url := "http://localhost/download-docker?stream=1"
	if render.tty {
		url += "&tty=1"
	}

	req := DownloadDockerRequest{
		ImageRef: imageRef,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Parse NDJSON stream
	scanner := bufio.NewScanner(resp.Body)
	var lastEvent DownloadDockerStreamEvent
	var lastProgressMsg string

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var event DownloadDockerStreamEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return fmt.Errorf("parse stream event: %w (line: %q)", err, string(line))
		}

		lastEvent = event

		if event.Type == "progress" {
			lastProgressMsg = event.Message
			render.progress(event.Message)
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read stream: %w", err)
	}

	render.finish()
	if render.tty && lastProgressMsg != "" {
		fmt.Fprintln(os.Stderr, lastProgressMsg)
	}

	if lastEvent.Status == "error" {
		return fmt.Errorf("server error: %s", lastEvent.Message)
	}

	writeDownloadDockerResult(os.Stdout, os.Stderr, lastEvent)
	return nil
}

func writeDownloadDockerResult(stdout, stderr io.Writer, result DownloadDockerStreamEvent) {
	if result.Cached {
		fmt.Fprintf(stderr, "Using cached image snapshot %s\n", result.SnapshotID)
	}
	fmt.Fprintln(stdout, result.SnapshotID)
}

func cmdDownloadSnap(args []string) {
	opts, helpFlag := newCmdOpts("download-snap", "<snapshot-id>")
	parseCmd(opts, "download-snap", args)

	if *helpFlag {
		printCommandHelp(os.Stdout, "download-snap", opts)
		os.Exit(0)
	}

	if opts.NArgs() != 1 {
		fmt.Fprintln(os.Stderr, "error: download-snap requires exactly one argument: snapshot-id")
		fmt.Fprintln(os.Stderr, "usage: ts download-snap <snapshot-id>")
		os.Exit(1)
	}

	snapshotID := opts.Arg(0)

	// Handle frame specs (colon-separated) by downloading all non-empty snaps
	if strings.Contains(snapshotID, ":") {
		parts := strings.Split(snapshotID, ":")
		var snapsToDownload []string
		for _, p := range parts {
			if p != "" && p != "nil" {
				snapsToDownload = append(snapsToDownload, p)
			}
		}

		if len(snapsToDownload) == 0 {
			// All empty - nothing to download
			return
		}

		// Download all snaps in parallel
		type downloadResult struct {
			snap string
			err  error
		}
		results := make(chan downloadResult, len(snapsToDownload))

		for _, snap := range snapsToDownload {
			go func(s string) {
				err := doDownloadSnap(*sockPath, s)
				results <- downloadResult{snap: s, err: err}
			}(snap)
		}

		// Collect results
		var failed []string
		for range snapsToDownload {
			r := <-results
			if r.err != nil {
				fmt.Fprintf(os.Stderr, "error downloading %s: %v\n", r.snap, r.err)
				failed = append(failed, r.snap)
			}
		}

		if len(failed) > 0 {
			os.Exit(1)
		}
		return
	}

	if err := doDownloadSnap(*sockPath, snapshotID); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// DownloadSnapRequest is the request body for /download-snap
type DownloadSnapRequest struct {
	SnapshotID string `json:"snapshot_id"`
}

// DownloadSnapResponse is the response from /download-snap
type DownloadSnapResponse struct {
	Status       string `json:"status"`
	Message      string `json:"message,omitempty"`
	SnapshotPath string `json:"snapshot_path,omitempty"`
	AlreadyHad   bool   `json:"already_had,omitempty"`
}

// DownloadSnapStreamEvent is an event in the streaming download response
type DownloadSnapStreamEvent struct {
	Type         string `json:"type"`
	Message      string `json:"message,omitempty"`
	Status       string `json:"status,omitempty"`
	SnapshotPath string `json:"snapshot_path,omitempty"`
	AlreadyHad   bool   `json:"already_had,omitempty"`
}

func doDownloadSnap(sockPath, snapshotID string) error {
	client := thunderclient.NewHTTPClient(sockPath)
	render := newProgressRenderer()

	// Build URL with streaming enabled
	url := "http://localhost/download-snap?stream=1"
	if render.tty {
		url += "&tty=1"
	}

	req := DownloadSnapRequest{
		SnapshotID: snapshotID,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Parse NDJSON stream
	scanner := bufio.NewScanner(resp.Body)
	var lastEvent DownloadSnapStreamEvent
	var lastProgressMsg string

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var event DownloadSnapStreamEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return fmt.Errorf("parse stream event: %w (line: %q)", err, string(line))
		}

		if event.Type == "progress" {
			lastProgressMsg = event.Message
			render.progress(event.Message)
		} else if event.Type == "result" {
			lastEvent = event
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read stream: %w", err)
	}

	render.finish()
	// On a TTY the progress line was overwritten in place, so re-print the final
	// "done" message; on non-TTY it was already printed on its own line.
	if render.tty && lastProgressMsg != "" {
		fmt.Fprintln(os.Stderr, lastProgressMsg)
	}

	// Check result
	if lastEvent.Type != "result" {
		return fmt.Errorf("no result received from server")
	}

	if lastEvent.Status != "ok" {
		return fmt.Errorf("%s", lastEvent.Message)
	}

	// Success - print nothing if we already had the snapshot (per requirements)
	// "Return success and no message if we already had the snapshot since it means we're fine."
	if !lastEvent.AlreadyHad {
		fmt.Printf("Downloaded snapshot to %s\n", lastEvent.SnapshotPath)
	}

	return nil
}

// findExecutable looks up the executable path, searching PATH if necessary.
func findExecutable(name string) (string, error) {
	// If it contains a slash, use it directly
	if strings.Contains(name, "/") {
		if _, err := os.Stat(name); err != nil {
			return "", fmt.Errorf("executable not found: %s", name)
		}
		return name, nil
	}

	// Search PATH
	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		pathEnv = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}

	for _, dir := range strings.Split(pathEnv, ":") {
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil {
			if info.Mode()&0111 != 0 { // executable bit set
				return path, nil
			}
		}
	}

	return "", fmt.Errorf("executable not found in PATH: %s", name)
}

// =====================================
// Ref commands
// =====================================

func cmdRef(args []string) {
	// "ts ref --help" / "ts ref -h" prints the group help to stdout (exit 0);
	// "ts ref" with no subcommand prints it to stderr (exit 1).
	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
		printRefGroupHelp(os.Stdout)
		os.Exit(0)
	}
	if len(args) == 0 {
		printRefGroupHelp(os.Stderr)
		os.Exit(1)
	}

	subcmd := args[0]
	subargs := args[1:]

	switch subcmd {
	case "create":
		cmdRefCreate(subargs)
	case "move":
		cmdRefMove(subargs)
	case "delete":
		cmdRefDelete(subargs)
	default:
		fmt.Fprintf(os.Stderr, "error: unknown ref subcommand: %s\n", subcmd)
		fmt.Fprintln(os.Stderr, "run 'ts ref --help' for the list of ref subcommands")
		os.Exit(1)
	}
}

func cmdRefCreate(args []string) {
	opts, helpFlag := newCmdOpts("ref create", "<name> <uuid-or-ref>")
	parseCmd(opts, "ref create", args)

	if *helpFlag {
		printCommandHelp(os.Stdout, "ref create", opts)
		os.Exit(0)
	}

	if opts.NArgs() != 2 {
		fmt.Fprintln(os.Stderr, "error: ref create requires name and target")
		fmt.Fprintln(os.Stderr, "usage: ts ref create <name> <uuid-or-ref>")
		os.Exit(1)
	}

	name := opts.Arg(0)
	target := opts.Arg(1)

	if err := doRefCreate(*sockPath, name, target); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func cmdRefMove(args []string) {
	opts, helpFlag := newCmdOpts("ref move", "<name> <uuid>")
	force := opts.BoolLong("force", 'f', "force move even if frame has running processes")
	parseCmd(opts, "ref move", args)

	if *helpFlag {
		printCommandHelp(os.Stdout, "ref move", opts)
		os.Exit(0)
	}

	if opts.NArgs() != 2 {
		fmt.Fprintln(os.Stderr, "error: ref move requires name and uuid")
		fmt.Fprintln(os.Stderr, "usage: ts ref move <name> <uuid> [-f]")
		os.Exit(1)
	}

	name := opts.Arg(0)
	uuid := opts.Arg(1)

	if err := doRefMove(*sockPath, name, uuid, *force); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Moved ref %s -> %s\n", name, uuid)
}

func cmdRefDelete(args []string) {
	opts, helpFlag := newCmdOpts("ref delete", "<name>")
	force := opts.BoolLong("force", 'f', "force delete even if frame has running processes or id dir is non-empty")
	parseCmd(opts, "ref delete", args)

	if *helpFlag {
		printCommandHelp(os.Stdout, "ref delete", opts)
		os.Exit(0)
	}

	if opts.NArgs() != 1 {
		fmt.Fprintln(os.Stderr, "error: ref delete requires name")
		fmt.Fprintln(os.Stderr, "usage: ts ref delete <name> [-f]")
		os.Exit(1)
	}

	name := opts.Arg(0)

	if err := doRefDelete(*sockPath, name, *force); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Deleted ref %s\n", name)
}

// RefRequest is the request body for ref operations
type RefRequest struct {
	Name  string `json:"name"`
	UUID  string `json:"uuid,omitempty"`
	Force bool   `json:"force,omitempty"`
}

// RefResponse is the response from ref operations
type RefResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// doRefRequest POSTs a RefRequest to one of the /ref/* endpoints and checks the
// standard {status,message} response. It backs doRefCreate/Move/Delete, which
// differ only in the endpoint path and which RefRequest fields they populate.
func doRefRequest(sockPath, path string, req RefRequest) error {
	result, err := thunderclient.PostJSON[RefRequest, RefResponse](sockPath, path, req)
	if err != nil {
		return err
	}
	if result.Status != "ok" {
		return fmt.Errorf("server error: %s", result.Message)
	}
	return nil
}

func doRefCreate(sockPath, name, target string) error {
	uuid, err := doResolveFrame(sockPath, target)
	if err != nil {
		return fmt.Errorf("resolve target %q: %w", target, err)
	}
	return doRefRequest(sockPath, "/ref/create", RefRequest{Name: name, UUID: uuid})
}

func doRefMove(sockPath, name, uuid string, force bool) error {
	return doRefRequest(sockPath, "/ref/move", RefRequest{Name: name, UUID: uuid, Force: force})
}

func doRefDelete(sockPath, name string, force bool) error {
	return doRefRequest(sockPath, "/ref/delete", RefRequest{Name: name, Force: force})
}

func cmdRefs(args []string) {
	opts, helpFlag := newCmdOpts("refs", "")
	parseCmd(opts, "refs", args)

	if *helpFlag {
		printCommandHelp(os.Stdout, "refs", opts)
		os.Exit(0)
	}

	if opts.NArgs() > 0 {
		fmt.Fprintln(os.Stderr, "error: refs takes no arguments")
		fmt.Fprintln(os.Stderr, "usage: ts refs    list all refs")
		os.Exit(1)
	}

	if err := doListRefs(*sockPath); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// RefListEntry is a single ref in the list response
type RefListEntry struct {
	Name    string   `json:"name"`
	UUID    string   `json:"uuid"`
	Autorun []string `json:"autorun,omitempty"`
}

// RefListResponse is the response from /refs
type RefListResponse struct {
	Status string         `json:"status"`
	Refs   []RefListEntry `json:"refs"`
}

func getRefs(sockPath string) ([]RefListEntry, error) {
	client := thunderclient.NewHTTPClient(sockPath)

	resp, err := client.Get("http://localhost/refs")
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	var result RefListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if result.Status != "ok" {
		return nil, fmt.Errorf("server error")
	}
	return result.Refs, nil
}

func doListRefs(sockPath string) error {
	client := thunderclient.NewHTTPClient(sockPath)
	resp, err := client.Get("http://localhost/list-frames")
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	var result ListFramesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if result.Status != "ok" {
		return fmt.Errorf("%s", result.Error)
	}
	printFrames(result.Frames, true)
	return nil
}

func cmdReflog(args []string) {
	opts, helpFlag := newCmdOpts("reflog", "[ref-name]")
	parseCmd(opts, "reflog", args)

	if *helpFlag {
		printCommandHelp(os.Stdout, "reflog", opts)
		os.Exit(0)
	}

	var name string
	if opts.NArgs() > 0 {
		name = opts.Arg(0)
	}
	// If name is empty, the server will default to the unique ref for the
	// current frame (if exactly one exists) or return an error with suggestions.

	if err := doReflog(*sockPath, name); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// ReflogEntry is a single entry in the reflog
type ReflogEntry struct {
	UUID string `json:"uuid"`
	Time string `json:"time"`
}

// ReflogResponse is the response from /reflog
type ReflogResponse struct {
	Status  string        `json:"status"`
	Message string        `json:"message,omitempty"`
	Name    string        `json:"name"`
	Reflog  []ReflogEntry `json:"reflog"`
}

func doReflog(sockPath, name string) error {
	client := thunderclient.NewHTTPClient(sockPath)

	url := "http://localhost/reflog"
	if name != "" {
		url += "?name=" + name
	}

	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	var result ReflogResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	if result.Status != "ok" {
		if result.Message != "" {
			return fmt.Errorf("%s", result.Message)
		}
		return fmt.Errorf("server error")
	}

	if len(result.Reflog) == 0 {
		fmt.Println("(empty reflog)")
		return nil
	}

	for _, entry := range result.Reflog {
		fmt.Printf("%s  %s\n", entry.UUID, entry.Time)
	}
	return nil
}

func cmdLog(args []string) {
	opts, helpFlag := newCmdOpts("log", "[uuid]")
	parseCmd(opts, "log", args)

	if *helpFlag {
		printCommandHelp(os.Stdout, "log", opts)
		os.Exit(0)
	}

	var uuid string
	if opts.NArgs() > 0 {
		uuid = opts.Arg(0)
	}

	if err := doLog(*sockPath, uuid); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// LogEntry is a single entry in the frame history
type LogEntry struct {
	Snap    string `json:"snap"`
	Time    string `json:"time"`
	Message string `json:"message,omitempty"`
}

// LogResponse is the response from /log
type LogResponse struct {
	Status  string     `json:"status"`
	UUID    string     `json:"uuid"`
	History []LogEntry `json:"history"`
}

func doLog(sockPath, uuid string) error {
	client := thunderclient.NewHTTPClient(sockPath)

	url := "http://localhost/log"
	if uuid != "" {
		url += "?uuid=" + uuid
	}

	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	var result LogResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	if result.Status != "ok" {
		return fmt.Errorf("server error")
	}

	if len(result.History) == 0 {
		fmt.Println("(no snapshots)")
		return nil
	}

	for _, entry := range result.History {
		if entry.Message != "" {
			fmt.Printf("%s  %s  %s\n", entry.Time, entry.Snap, entry.Message)
		} else {
			fmt.Printf("%s  %s\n", entry.Time, entry.Snap)
		}
	}
	return nil
}

func cmdAutorun(args []string) {
	opts, helpFlag := newCmdOpts("autorun", "<program> [args...]")
	refName := opts.StringLong("ref", 0, "", "ref name (required)")
	stop := opts.BoolLong("stop", 0, "clear autorun configuration")
	parseCmd(opts, "autorun", args)

	if *helpFlag {
		printCommandHelp(os.Stdout, "autorun", opts)
		os.Exit(0)
	}

	if *refName == "" {
		fmt.Fprintln(os.Stderr, "error: --ref is required")
		fmt.Fprintln(os.Stderr, "usage: ts autorun --ref <ref> <program> [args...]")
		fmt.Fprintln(os.Stderr, "       ts autorun --ref <ref> --stop")
		os.Exit(1)
	}

	if *stop {
		if err := doAutorunStop(*sockPath, *refName); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if opts.NArgs() == 0 {
		fmt.Fprintln(os.Stderr, "error: autorun requires a program")
		fmt.Fprintln(os.Stderr, "usage: ts autorun --ref <ref> <program> [args...]")
		os.Exit(1)
	}

	argv := opts.Args()
	if err := doAutorunSet(*sockPath, *refName, argv); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func cmdAutoruns(args []string) {
	opts, helpFlag := newCmdOpts("autoruns", "")
	parseCmd(opts, "autoruns", args)
	if *helpFlag {
		printCommandHelp(os.Stdout, "autoruns", opts)
		return
	}
	if opts.NArgs() != 0 {
		fmt.Fprintln(os.Stderr, "error: autoruns takes no arguments")
		os.Exit(1)
	}

	refs, err := getRefs(*sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	for _, ref := range refs {
		if len(ref.Autorun) > 0 {
			// One JSON argv array per line makes every argument unambiguous,
			// including whitespace, quotes, control characters, and newlines.
			if err := enc.Encode(ref.Autorun); err != nil {
				fmt.Fprintf(os.Stderr, "error: encode autorun: %v\n", err)
				os.Exit(1)
			}
		}
	}
}

// AutorunRequest is the request body for /autorun
type AutorunRequest struct {
	RefName string   `json:"ref_name"`
	Argv    []string `json:"argv,omitempty"`
}

// AutorunResponse is the response from /autorun
type AutorunResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// doAutorun POSTs an AutorunRequest to /autorun and checks the standard
// {status,message} response. A nil argv clears the ref's autorun.
func doAutorun(sockPath, refName string, argv []string) error {
	result, err := thunderclient.PostJSON[AutorunRequest, AutorunResponse](sockPath, "/autorun",
		AutorunRequest{RefName: refName, Argv: argv})
	if err != nil {
		return err
	}
	if result.Status != "ok" {
		return fmt.Errorf("server error: %s", result.Message)
	}
	return nil
}

func doAutorunSet(sockPath, refName string, argv []string) error {
	return doAutorun(sockPath, refName, argv)
}

func doAutorunStop(sockPath, refName string) error {
	return doAutorun(sockPath, refName, nil)
}

// =====================================
// ts go command
// =====================================

// goArgs holds the parsed arguments for the "ts go" command.
type goArgs struct {
	spec      string // frame spec (uuid, ref, ::, or snap triplet), with any <user>@ stripped
	user      string // optional <user>@ prefix; empty means auto-detect on the host
	isolation string // isolation level
	command   string // command to run (if -c specified)
	help      bool   // --help/-h was requested
}

// parseGoArgs parses the arguments for "ts go" and returns the parsed values
// along with the getopt set (so the caller can render --help). Returns an error
// if the arguments are invalid.
func parseGoArgs(args []string) (*goArgs, *getopt.Set, error) {
	opts, helpFlag := newCmdOpts("go", "[<spec>]")
	isolation := opts.StringLong("isolation", 0, "", "isolation level for new frames: vm, container, none")
	cmdFlag := opts.StringLong("command", 'c', "", "run shell command instead of interactive session")

	// Parse stops at the first non-option (the spec). Call Parse again on
	// remaining args to support GNU-style "ts go :: -c cmd" ordering.
	// The first element of Args() is the spec itself, which becomes the
	// "program name" for the second parse - that's fine, we just need to
	// extract the spec before the second parse.
	if err := opts.Getopt(append([]string{"ts go"}, args...), nil); err != nil {
		return nil, opts, err
	}
	var spec string
	if opts.NArgs() > 0 {
		spec = opts.Arg(0)
		// Parse any flags that appear after the positional argument.
		// Args()[0] (the spec) becomes the program name for this parse.
		if err := opts.Getopt(opts.Args(), nil); err != nil {
			return nil, opts, err
		}
	}

	if opts.NArgs() > 0 {
		return nil, opts, fmt.Errorf("unexpected argument: %s", opts.Arg(0))
	}

	// Split an optional "<user>@" prefix from the spec. The frame spec
	// itself (UUID, ref, "::", or snap triplet) never contains '@': UUIDs are
	// hex+hyphens, refs are letters/digits/dash/underscore (refs.ValidateName),
	// and snap ids are hex. So the first '@' unambiguously separates the
	// requested Unix user from the frame identifier, for every spec form.
	var user string
	if idx := strings.Index(spec, "@"); idx != -1 {
		user = spec[:idx]
		spec = spec[idx+1:]
		if user == "" {
			return nil, opts, fmt.Errorf("empty user before '@'")
		}
	}

	return &goArgs{
		spec:      spec,
		user:      user,
		isolation: *isolation,
		command:   *cmdFlag,
		help:      *helpFlag,
	}, opts, nil
}

// cmdGo creates/resolves a frame and starts a new session inside it.
func cmdGo(args []string) {
	parsed, opts, err := parseGoArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ts go: %v\n", err)
		fmt.Fprintln(os.Stderr, "usage: ts go                         enter current frame (no-op)")
		fmt.Fprintln(os.Stderr, "       ts go <uuid>                  enter existing frame by UUID")
		fmt.Fprintln(os.Stderr, "       ts go <ref>                   enter frame by ref name")
		fmt.Fprintln(os.Stderr, "       ts go <root:home:work>        create and enter new frame")
		fmt.Fprintln(os.Stderr, "       ts go :: -c 'cmd'             create new frame, run cmd, exit")
		fmt.Fprintln(os.Stderr, "       ts go <user>@<spec>           enter/create frame as <user>")
		fmt.Fprintln(os.Stderr, "run 'ts go --help' for full help")
		os.Exit(1)
	}
	if parsed.help {
		printCommandHelp(os.Stdout, "go", opts)
		os.Exit(0)
	}
	spec := parsed.spec

	// Get current frame UUID for history cloning
	currentUUID, err := doGetCurrentFrame(*sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	var targetUUID string
	var createdNewFrame bool
	var forkedFromCurrent bool // :: forks the current frame; history is already copied by /fork

	if spec == "" {
		// No args: stay in current frame (identity operation, but still enters session)
		targetUUID = currentUUID
	} else {
		colonCount := strings.Count(spec, ":")

		if colonCount == 1 {
			fmt.Fprintln(os.Stderr, "error: invalid spec - one colon is invalid")
			fmt.Fprintln(os.Stderr, "       use two colons for snap triplet: root:home:work")
			os.Exit(1)
		}
		if colonCount > 2 {
			fmt.Fprintln(os.Stderr, "error: invalid spec - too many colons")
			os.Exit(1)
		}

		if colonCount == 0 {
			// UUID or ref - resolve it
			targetUUID, err = doResolveFrame(*sockPath, spec)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
		} else if spec == "::" {
			// :: = save current state and branch from it. Fork captures the
			// current frame as a background snap and clones a new frame from
			// the live filesystem, without blocking on indexing. /fork already
			// copies the source frame's history into the new frame, so we must
			// NOT call doCloneHistory below (that would re-read the source's
			// history — which may or may not yet include the fork-point snap,
			// depending on background-indexing timing — and overwrite the new
			// frame's history nondeterministically).
			forkedUUID, err := doFork(*sockPath, "")
			if err != nil {
				fmt.Fprintf(os.Stderr, "error forking current frame: %v\n", err)
				os.Exit(1)
			}
			targetUUID = forkedUUID
			createdNewFrame = true
			forkedFromCurrent = true
		} else {
			// Snap/frame/ref triplet - create new frame.
			snapshotSpec, sourceFrames, err := resolveFrameTriplet(*sockPath, spec)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
			targetUUID, err = doCreate(*sockPath, snapshotSpec, parsed.isolation, "", sourceFrames[:]...)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
			createdNewFrame = true
		}
	}

	// If we created a new frame from a snap triplet, clone the parent's
	// history. Skip this for :: (fork), which already copied history in the
	// daemon to avoid racing the background indexer.
	if createdNewFrame && !forkedFromCurrent && currentUUID != "" {
		if err := doCloneHistory(*sockPath, currentUUID, targetUUID); err != nil {
			// Log but don't fail - history cloning is best-effort
			fmt.Fprintf(os.Stderr, "warning: failed to clone history: %v\n", err)
		}
	}

	// Build command args if -c was provided
	var cmdArgs []string
	if parsed.command != "" {
		cmdArgs = []string{"sh", "-c", parsed.command}
	}

	// Connect to the target frame via vsock and start session
	exitCode, err := runVsockSession(targetUUID, parsed.user, cmdArgs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	os.Exit(exitCode)
}

// hostCID is the vsock context ID of the host.
const hostCID = 2

// inVM reports whether we are running inside a VM with vsock support.
func inVM() bool {
	_, err := os.Stat("/dev/vsock")
	return err == nil
}

// dialEnter connects to the /enter endpoint on port 5224. In VMs (when
// /dev/vsock exists) it connects directly via vsock to the host; in containers
// it connects to the Unix socket at sockPath and performs the CONNECT 5224
// handshake.
func dialEnter() (net.Conn, *bufio.Reader, error) {
	if inVM() {
		// In a VM: connect directly via vsock to the host.
		conn, err := vsock.Dial(hostCID, thunderproto.EnterPort, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("vsock dial: %w", err)
		}
		reader := bufio.NewReader(conn)
		return conn, reader, nil
	}

	// In a container: connect to the Unix socket with CONNECT 5224 handshake.
	conn, err := net.Dial("unix", *sockPath)
	if err != nil {
		return nil, nil, fmt.Errorf("dial control socket: %w", err)
	}

	reader := bufio.NewReader(conn)
	if err := thunderproto.WriteClientHandshakePort(conn, reader, thunderproto.EnterPort); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("enter handshake: %w", err)
	}

	return conn, reader, nil
}

// runVsockSession connects to the /enter endpoint and runs a session with optional command.
// If cmdArgs is empty, runs an interactive login shell.
// It returns the exit code from the remote session.
func runVsockSession(frameUUID, user string, cmdArgs []string) (int, error) {
	// Connect to /enter endpoint via vsock (VM) or control socket (container)
	conn, reader, err := dialEnter()
	if err != nil {
		return 1, err
	}
	defer conn.Close()

	// Determine if we have a TTY - but if running a command, don't request PTY
	isPTY := term.IsTerminal(int(os.Stdin.Fd())) && len(cmdArgs) == 0

	// Write the /enter protocol header: uuid\0user\0pty\0argc\0arg0\0arg1\0...
	ptyFlag := "0"
	if isPTY {
		ptyFlag = "1"
	}
	// Empty user means auto-detect on the host.
	fmt.Fprintf(conn, "%s\x00%s\x00%s\x00%d\x00", frameUUID, user, ptyFlag, len(cmdArgs))
	for _, arg := range cmdArgs {
		fmt.Fprintf(conn, "%s\x00", arg)
	}

	// Use the buffered reader for subsequent reads (important: the handshake
	// response may have left bytes in the reader's buffer)

	// Set up terminal raw mode if PTY
	var oldState *term.State
	if isPTY {
		oldState, err = term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			return 1, fmt.Errorf("make raw: %w", err)
		}
		defer term.Restore(int(os.Stdin.Fd()), oldState)
	}

	// Set up signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// Mutex for writing frames (stdin and winsize both write)
	var writeMu sync.Mutex
	writeFrame := func(typ uint8, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return vshdproto.WriteFrame(conn, typ, payload)
	}

	// Send initial window size for PTY sessions
	if isPTY {
		width, height, err := term.GetSize(int(os.Stdin.Fd()))
		if err == nil {
			writeFrame(vshdproto.FrameWinsize, vshdproto.EncodeWinsize(vshdproto.Winsize{
				Rows: uint16(height),
				Cols: uint16(width),
			}))
		}
	}

	// Done channel signals when the remote session ends
	done := make(chan struct{})
	exitCode := 0

	// Handle signals (window resize, interrupt)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case sig := <-sigCh:
				switch sig {
				case syscall.SIGWINCH:
					if isPTY {
						width, height, err := term.GetSize(int(os.Stdin.Fd()))
						if err == nil {
							writeFrame(vshdproto.FrameWinsize, vshdproto.EncodeWinsize(vshdproto.Winsize{
								Rows: uint16(height),
								Cols: uint16(width),
							}))
						}
					}
				case syscall.SIGINT:
					// Send Ctrl-C to remote
					writeFrame(vshdproto.FrameStdin, []byte{3})
				}
			}
		}
	}()

	// Host -> guest: send stdin as FrameStdin.
	// Skip stdin forwarding when running a command (-c) since there's no
	// interactive input expected and stdin may not close properly in some
	// environments (e.g., SSH exec sessions).
	if len(cmdArgs) == 0 {
		go func() {
			buf := make([]byte, 32*1024)
			for {
				n, err := os.Stdin.Read(buf)
				if n > 0 {
					if werr := writeFrame(vshdproto.FrameStdin, buf[:n]); werr != nil {
						break
					}
				}
				if err != nil {
					break
				}
			}
		}()
	}

	// Guest -> host: decode TLV frames (using buffered reader to not lose
	// any bytes that may have been buffered during the handshake)
	go func() {
		defer close(done)
		for {
			typ, payload, err := vshdproto.ReadFrame(reader)
			if err != nil {
				break
			}
			switch typ {
			case vshdproto.FrameStdout:
				os.Stdout.Write(payload)
			case vshdproto.FrameStderr:
				os.Stderr.Write(payload)
			case vshdproto.FrameExit:
				if code, derr := vshdproto.DecodeExit(payload); derr == nil {
					exitCode = int(code)
				}
			}
		}
	}()

	// Wait for session end
	<-done

	return exitCode, nil
}

// CloneHistoryRequest is the request body for /clone-history
type CloneHistoryRequest struct {
	SourceUUID string `json:"source_uuid"`
	TargetUUID string `json:"target_uuid"`
}

// CloneHistoryResponse is the response from /clone-history
type CloneHistoryResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

func doCloneHistory(sockPath, sourceUUID, targetUUID string) error {
	result, err := thunderclient.PostJSON[CloneHistoryRequest, CloneHistoryResponse](
		sockPath, "/clone-history", CloneHistoryRequest{
			SourceUUID: sourceUUID,
			TargetUUID: targetUUID,
		})
	if err != nil {
		return err
	}
	if result.Status != "ok" {
		return fmt.Errorf("%s", result.Message)
	}
	return nil
}

// PruneHistoryRequest is the request body for /prune-history
type PruneHistoryRequest struct {
	UUID  string   `json:"uuid"`
	Snaps []string `json:"snaps"`
}

// PruneHistoryResponse is the response from /prune-history
type PruneHistoryResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	Pruned  int    `json:"pruned"`
}

func doPruneHistory(sockPath, uuid string, snaps []string) error {
	result, err := thunderclient.PostJSON[PruneHistoryRequest, PruneHistoryResponse](
		sockPath, "/prune-history", PruneHistoryRequest{
			UUID:  uuid,
			Snaps: snaps,
		})
	if err != nil {
		return err
	}
	if result.Status != "ok" {
		return fmt.Errorf("%s", result.Message)
	}
	return nil
}

// doGetLog retrieves the frame's history log.
func doGetLog(sockPath, uuid string) ([]LogEntry, error) {
	client := thunderclient.NewHTTPClient(sockPath)

	url := "http://localhost/log"
	if uuid != "" {
		url += "?uuid=" + uuid
	}

	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	var result LogResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if result.Status != "ok" {
		return nil, fmt.Errorf("server error")
	}

	return result.History, nil
}

// =====================================
// ts undo command
// =====================================

// undoArgs holds the parsed arguments for the "ts undo" command.
type undoArgs struct {
	command string // command to run (if -c specified)
	help    bool   // --help/-h was requested
}

// parseUndoArgs parses the arguments for "ts undo" and returns the parsed
// values along with the getopt set (so the caller can render --help). Returns
// an error if the arguments are invalid. Unlike "ts go", undo takes no
// positional arguments, so a single Getopt pass suffices: parsing stops at the
// first non-option, which the NArgs check rejects.
func parseUndoArgs(args []string) (*undoArgs, *getopt.Set, error) {
	opts, helpFlag := newCmdOpts("undo", "")
	cmdFlag := opts.StringLong("command", 'c', "", "run shell command instead of interactive session")

	if err := opts.Getopt(append([]string{"ts undo"}, args...), nil); err != nil {
		return nil, opts, err
	}
	if opts.NArgs() > 0 {
		return nil, opts, fmt.Errorf("unexpected argument: %s", opts.Arg(0))
	}

	return &undoArgs{
		command: *cmdFlag,
		help:    *helpFlag,
	}, opts, nil
}

// cmdUndo jumps backward in time by one snap.
func cmdUndo(args []string) {
	parsed, opts, err := parseUndoArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ts undo: %v\n", err)
		fmt.Fprintln(os.Stderr, "usage: ts undo                jump back one snap, enter new frame")
		fmt.Fprintln(os.Stderr, "       ts undo -c 'cmd'       jump back one snap, run cmd, exit")
		fmt.Fprintln(os.Stderr, "run 'ts undo --help' for full help")
		os.Exit(1)
	}
	if parsed.help {
		printCommandHelp(os.Stdout, "undo", opts)
		os.Exit(0)
	}

	// 1. Get current frame info and history
	currentUUID, err := doGetCurrentFrame(*sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	history, err := doGetLog(*sockPath, currentUUID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if len(history) == 0 {
		fmt.Fprintln(os.Stderr, "error: no snapshots in history to undo")
		os.Exit(1)
	}

	// The most recent snap in the log is history[0] (newest first)
	prevSnap := history[0].Snap

	// 2. Run ts snap to record current state (wait so the ID is known for
	// history pruning below).
	currentSnap, err := doSnap(*sockPath, "", true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error taking snapshot: %v\n", err)
		os.Exit(1)
	}

	// 3. Get current frame metadata for home/work inheritance
	currentFrame, err := doGetCurrentFrameInfo(*sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// 4. Create new frame based on prev snap for rootfs, keep home/work
	// Format: <prevSnap>:<currentHome>:<currentWork>
	homeSnap := currentFrame.Home
	workSnap := currentFrame.Work
	if homeSnap == "" {
		homeSnap = "nil"
	}
	if workSnap == "" {
		workSnap = "nil"
	}
	snapshotSpec := fmt.Sprintf("%s:%s:%s", prevSnap, homeSnap, workSnap)

	newUUID, err := doCreate(*sockPath, snapshotSpec, "", "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating frame: %v\n", err)
		os.Exit(1)
	}

	// 5. Clone history from current frame to new frame
	if err := doCloneHistory(*sockPath, currentUUID, newUUID); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to clone history: %v\n", err)
	}

	// 6. Prune both currentSnap and prevSnap from new frame's history
	if err := doPruneHistory(*sockPath, newUUID, []string{currentSnap, prevSnap}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to prune history: %v\n", err)
	}

	// 7. Enter the new frame. With -c, run the command instead of an
	// interactive session, mirroring ts go -c.
	var cmdArgs []string
	if parsed.command != "" {
		cmdArgs = []string{"sh", "-c", parsed.command}
	}
	fmt.Fprintf(os.Stderr, "Undoing to snap %s...\n", prevSnap)
	exitCode, err := runVsockSession(newUUID, "", cmdArgs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	os.Exit(exitCode)
}
