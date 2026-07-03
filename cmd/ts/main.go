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
	"github.com/tailscale/thundersnap/thunderclient"
	"github.com/tailscale/thundersnap/thunderproto"
	"github.com/tailscale/thundersnap/tsm"
	"github.com/tailscale/thundersnap/vshdproto"
	"golang.org/x/term"
)

var sockPath = getopt.StringLong("sock", 0, "/thunder.sock", "path to control socket")
var help = getopt.BoolLong("help", 'h', "show help")

func usage() {
	getopt.PrintUsage(os.Stderr)
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  ping           send a ping to thundersnapd")
	fmt.Fprintln(os.Stderr, "  snap           create a snapshot of the current container/VM")
	fmt.Fprintln(os.Stderr, "  snaps          list all snapshots with sizes")
	fmt.Fprintln(os.Stderr, "  frame          resolve or create frames")
	fmt.Fprintln(os.Stderr, "  frames         list all frames with status")
	fmt.Fprintln(os.Stderr, "  go             enter a frame (create/resolve + start session)")
	fmt.Fprintln(os.Stderr, "  undo           jump backward in time by one snap")
	fmt.Fprintln(os.Stderr, "  ref            manage refs (named pointers to frames)")
	fmt.Fprintln(os.Stderr, "  refs           list all refs")
	fmt.Fprintln(os.Stderr, "  reflog         show ref history")
	fmt.Fprintln(os.Stderr, "  log            show frame snapshot history")
	fmt.Fprintln(os.Stderr, "  taint          add a taint to the current frame")
	fmt.Fprintln(os.Stderr, "  autorun        configure a program to run automatically")
	fmt.Fprintln(os.Stderr, "  download-docker download a Docker image as a snap")
	fmt.Fprintln(os.Stderr, "  who-has        query peers to find which ones have a snap")
	fmt.Fprintln(os.Stderr, "  download-snap  download a snap from mesh peers")
	os.Exit(1)
}

// isShellInvocation reports whether argv0's basename indicates ts is being run
// as the container shell. thundersnapd symlinks /bin/sh -> /bin/ts for
// containers that lack a real shell; a login shell is exec'd with a leading
// dash ("-sh"), which we must also recognize.
func isShellInvocation(base string) bool {
	return base == "sh" || base == "-sh"
}

func main() {
	// Check if we're being invoked as a shell (argv[0] is "sh" or "-sh").
	if isShellInvocation(filepath.Base(os.Args[0])) {
		runAsShell()
		return
	}

	getopt.SetParameters("<command> [command-options]")
	getopt.SetUsage(usage)
	getopt.Parse()
	args := getopt.Args()

	if *help || len(args) == 0 {
		usage()
	}

	cmd := args[0]
	cmdArgs := args[1:]

	switch cmd {
	case "ping":
		cmdPing(cmdArgs)
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
	case "go":
		cmdGo(cmdArgs)
	case "undo":
		cmdUndo(cmdArgs)
	case "drop-caps-and-run":
		// Hidden command - not listed in usage
		cmdDropCapsAndRun(cmdArgs)
	case "container-init":
		// Hidden command - starts a minimal init process for container namespaces
		cmdContainerInit(cmdArgs)
	case "session-serve":
		// Hidden command - in-container vshd session endpoint. Runs after chroot
		// so the pty it opens lands in the container's devpts; speaks vshdproto
		// TLV on stdin/stdout, which vshd splices to the client connection.
		cmdSessionServe(cmdArgs)
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
	opts := getopt.New()
	opts.SetProgram("ts ping")
	opts.Parse(args)

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

// meshPort is the HTTP port for mesh discovery (TSTS in leetspeak = 7575)
const meshPort = 7575

// meshPeer represents a peer from /ts/servers.json
type meshPeer struct {
	URL      string    `json:"url"`
	Hostname string    `json:"hostname"`
	LastSeen time.Time `json:"last_seen"`
}

func cmdSnap(args []string) {
	opts := getopt.New()
	opts.SetProgram("ts snap")
	deleteFlag := opts.BoolLong("delete", 'd', "delete a snapshot")
	// Parse expects first element to be program name (like os.Args)
	opts.Parse(append([]string{"ts snap"}, args...))

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

	snapshotID, err := doSnap(*sockPath, subdir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Print just the snapshot ID to stdout
	fmt.Println(snapshotID)
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
	opts := getopt.New()
	opts.SetProgram("ts snaps")
	// Parse expects first element to be program name (like os.Args)
	opts.Parse(append([]string{"ts snaps"}, args...))

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

func doSnap(sockPath, subdir string) (string, error) {
	client := thunderclient.NewHTTPClient(sockPath)
	render := newProgressRenderer()

	// Build URL with streaming enabled
	url := "http://localhost/snap?stream=1"
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
	opts := getopt.New()
	opts.SetProgram("ts frame")
	opts.SetParameters("[<spec>]")
	isolation := opts.StringLong("isolation", 0, "", "isolation level: vm, container, none")
	refName := opts.StringLong("ref", 0, "", "create a ref with this name pointing at the new frame")
	deleteFlag := opts.BoolLong("delete", 'd', "delete a frame by UUID")
	// Parse expects first element to be program name (like os.Args)
	opts.Parse(append([]string{"ts frame"}, args...))

	if *deleteFlag {
		if opts.NArgs() != 1 {
			fmt.Fprintln(os.Stderr, "error: --delete requires exactly one frame UUID argument")
			fmt.Fprintln(os.Stderr, "usage: ts frame --delete <uuid>")
			os.Exit(1)
		}
		uuid := opts.Arg(0)
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

	// No colons: this is a UUID or ref resolution (not creation)
	if colonCount == 0 {
		uuid, err := doResolveFrame(*sockPath, spec)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(uuid)
		return
	}

	// Two colons: snap triplet
	// Special case: :: snaps the current frame and creates a new frame from it
	if spec == "::" {
		// First snap the current state
		snapTriplet, err := doSnap(*sockPath, "")
		if err != nil {
			fmt.Fprintf(os.Stderr, "error snapping current frame: %v\n", err)
			os.Exit(1)
		}
		// The snapshot triplet is already a valid spec (root:home:work)
		spec = snapTriplet
	}

	// Handle empty components by inheriting from current frame, then create
	snapshotSpec, err := resolveSnapTriplet(*sockPath, spec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	uuid, err := doCreate(*sockPath, snapshotSpec, *isolation, *refName)
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
	fmt.Fprintln(os.Stderr, "snap triplet syntax (exactly two colons):")
	fmt.Fprintln(os.Stderr, "  - empty components inherit from current frame")
	fmt.Fprintln(os.Stderr, "  - ts frame <snap>::        replace root, keep /home and /work")
	fmt.Fprintln(os.Stderr, "  - ts frame :<snap>:        replace /home only")
	fmt.Fprintln(os.Stderr, "  - ts frame ::              current frame (identity)")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "options:")
	fmt.Fprintln(os.Stderr, "  --ref <name>         create a ref pointing at the new frame")
	fmt.Fprintln(os.Stderr, "  --isolation <level>  vm, container, or none")
}

func cmdFrames(args []string) {
	opts := getopt.New()
	opts.SetProgram("ts frames")
	// Parse expects first element to be program name (like os.Args)
	opts.Parse(append([]string{"ts frames"}, args...))

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

func doCreate(sockPath, snapshotSpec, isolation, refName string) (string, error) {
	client := thunderclient.NewHTTPClient(sockPath)
	render := newProgressRenderer()

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
			render.progress(event.Message)
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

	render.finish()

	// Check result
	if lastEvent.Type != "result" {
		return "", fmt.Errorf("no result received from server")
	}

	if lastEvent.Status != "ok" {
		return "", fmt.Errorf("%s", lastEvent.Message)
	}

	return lastEvent.UUID, nil
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

// resolveSnapTriplet resolves a snap triplet spec by filling in empty components
// from the current frame. Returns a fully resolved spec like "abc:def:ghi".
func resolveSnapTriplet(sockPath, spec string) (string, error) {
	parts := strings.Split(spec, ":")
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid snap triplet: expected exactly 2 colons")
	}

	root, home, work := parts[0], parts[1], parts[2]

	// If all three are specified, no need to look up current frame
	if root != "" && home != "" && work != "" {
		return spec, nil
	}

	// Get current frame info for inheritance
	current, err := doGetCurrentFrameInfo(sockPath)
	if err != nil {
		return "", fmt.Errorf("get current frame for inheritance: %w", err)
	}

	// Fill in empty components from current frame
	if root == "" {
		root = current.Rootfs
	}
	if home == "" {
		home = current.Home
	}
	if work == "" {
		work = current.Work
	}

	// Format for the create API (empty strings become "nil" for that API)
	formatSnap := func(s string) string {
		if s == "" {
			return "nil"
		}
		return s
	}

	return fmt.Sprintf("%s:%s:%s", formatSnap(root), formatSnap(home), formatSnap(work)), nil
}

// ListFramesResponse is the response from /list-frames
type ListFramesResponse struct {
	Status string      `json:"status"`
	Frames []FrameInfo `json:"frames,omitempty"`
	Error  string      `json:"error,omitempty"`
}

// FrameInfo contains info about a single frame
type FrameInfo struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "stopped" or "running"
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

	// Sort by name for consistent output
	sort.Slice(result.Frames, func(i, j int) bool {
		return result.Frames[i].Name < result.Frames[j].Name
	})

	// Print with fixed-width status column
	for _, frame := range result.Frames {
		fmt.Printf("%-7s  %s\n", frame.Status, frame.Name)
	}

	return nil
}

func cmdWhoHas(args []string) {
	opts := getopt.New()
	opts.SetProgram("ts who-has")
	opts.SetParameters("<snapshot-id>")
	// Parse expects first element to be program name (like os.Args)
	opts.Parse(append([]string{"ts who-has"}, args...))

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
	opts := getopt.New()
	opts.SetProgram("ts taint")
	opts.SetParameters("<taint-name>")
	// Parse expects first element to be program name (like os.Args)
	opts.Parse(append([]string{"ts taint"}, args...))

	if opts.NArgs() != 1 {
		fmt.Fprintln(os.Stderr, "error: taint requires exactly one argument: taint-name")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "usage: ts taint <taint-name>")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "examples:")
		fmt.Fprintln(os.Stderr, "  ts taint pii:customers")
		fmt.Fprintln(os.Stderr, "  ts taint unsafe-permissions")
		fmt.Fprintln(os.Stderr, "  ts taint untrusted-code")
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

func cmdDownloadDocker(args []string) {
	opts := getopt.New()
	opts.SetProgram("ts download-docker")
	opts.SetParameters("<image-reference>")
	// Parse expects first element to be program name (like os.Args)
	opts.Parse(append([]string{"ts download-docker"}, args...))

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
			render.progress(event.Message)
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read stream: %w", err)
	}

	render.finish()

	if lastEvent.Status == "error" {
		return fmt.Errorf("server error: %s", lastEvent.Message)
	}

	if lastEvent.Cached {
		fmt.Printf("%s (cached)\n", lastEvent.SnapshotID)
	} else {
		fmt.Printf("%s\n", lastEvent.SnapshotID)
	}

	return nil
}

func cmdDownloadSnap(args []string) {
	opts := getopt.New()
	opts.SetProgram("ts download-snap")
	opts.SetParameters("<snapshot-id>")
	// Parse expects first element to be program name (like os.Args)
	opts.Parse(append([]string{"ts download-snap"}, args...))

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
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ts ref <subcommand> [options]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "subcommands:")
		fmt.Fprintln(os.Stderr, "  create <name> <uuid>      create a new ref pointing at a frame UUID")
		fmt.Fprintln(os.Stderr, "  move <name> <uuid> [-f]   move a ref to point at a different UUID")
		fmt.Fprintln(os.Stderr, "  delete <name> [-f]        delete a ref")
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
		os.Exit(1)
	}
}

func cmdRefCreate(args []string) {
	opts := getopt.New()
	opts.SetProgram("ts ref create")
	opts.SetParameters("<name> <uuid>")
	opts.Parse(append([]string{"ts ref create"}, args...))

	if opts.NArgs() != 2 {
		fmt.Fprintln(os.Stderr, "error: ref create requires name and uuid")
		fmt.Fprintln(os.Stderr, "usage: ts ref create <name> <uuid>")
		os.Exit(1)
	}

	name := opts.Arg(0)
	uuid := opts.Arg(1)

	if err := doRefCreate(*sockPath, name, uuid); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Created ref %s -> %s\n", name, uuid)
}

func cmdRefMove(args []string) {
	opts := getopt.New()
	opts.SetProgram("ts ref move")
	opts.SetParameters("<name> <uuid>")
	force := opts.BoolLong("force", 'f', "force move even if frame has running processes")
	opts.Parse(append([]string{"ts ref move"}, args...))

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
	opts := getopt.New()
	opts.SetProgram("ts ref delete")
	opts.SetParameters("<name>")
	force := opts.BoolLong("force", 'f', "force delete even if frame has running processes or id dir is non-empty")
	opts.Parse(append([]string{"ts ref delete"}, args...))

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

func doRefCreate(sockPath, name, uuid string) error {
	return doRefRequest(sockPath, "/ref/create", RefRequest{Name: name, UUID: uuid})
}

func doRefMove(sockPath, name, uuid string, force bool) error {
	return doRefRequest(sockPath, "/ref/move", RefRequest{Name: name, UUID: uuid, Force: force})
}

func doRefDelete(sockPath, name string, force bool) error {
	return doRefRequest(sockPath, "/ref/delete", RefRequest{Name: name, Force: force})
}

func cmdRefs(args []string) {
	opts := getopt.New()
	opts.SetProgram("ts refs")
	opts.Parse(append([]string{"ts refs"}, args...))

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

func doListRefs(sockPath string) error {
	client := thunderclient.NewHTTPClient(sockPath)

	resp, err := client.Get("http://localhost/refs")
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	var result RefListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	if result.Status != "ok" {
		return fmt.Errorf("server error")
	}

	if len(result.Refs) == 0 {
		fmt.Println("(no refs)")
		return nil
	}

	for _, ref := range result.Refs {
		if len(ref.Autorun) > 0 {
			fmt.Printf("%s -> %s [autorun: %s]\n", ref.Name, ref.UUID, strings.Join(ref.Autorun, " "))
		} else {
			fmt.Printf("%s -> %s\n", ref.Name, ref.UUID)
		}
	}
	return nil
}

func cmdReflog(args []string) {
	opts := getopt.New()
	opts.SetProgram("ts reflog")
	opts.SetParameters("[ref-name]")
	opts.Parse(append([]string{"ts reflog"}, args...))

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
	opts := getopt.New()
	opts.SetProgram("ts log")
	opts.SetParameters("[uuid]")
	opts.Parse(append([]string{"ts log"}, args...))

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
	opts := getopt.New()
	opts.SetProgram("ts autorun")
	opts.SetParameters("<program> [args...]")
	refName := opts.StringLong("ref", 0, "", "ref name (required)")
	stop := opts.BoolLong("stop", 0, "clear autorun configuration")
	opts.Parse(append([]string{"ts autorun"}, args...))

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
		fmt.Printf("Cleared autorun for ref %s\n", *refName)
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
	fmt.Printf("Set autorun for ref %s: %s\n", *refName, strings.Join(argv, " "))
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

// cmdGo creates/resolves a frame and starts a new session inside it.
func cmdGo(args []string) {
	opts := getopt.New()
	opts.SetProgram("ts go")
	opts.SetParameters("[<spec>]")
	isolation := opts.StringLong("isolation", 0, "", "isolation level for new frames: vm, container, none")
	// Parse expects first element to be program name (like os.Args)
	opts.Parse(append([]string{"ts go"}, args...))

	if opts.NArgs() > 1 {
		fmt.Fprintln(os.Stderr, "usage: ts go                         enter current frame (no-op)")
		fmt.Fprintln(os.Stderr, "       ts go <uuid>                  enter existing frame by UUID")
		fmt.Fprintln(os.Stderr, "       ts go <ref>                   enter frame by ref name")
		fmt.Fprintln(os.Stderr, "       ts go <root:home:work>        create and enter new frame")
		os.Exit(1)
	}

	// Get current frame UUID for history cloning
	currentUUID, err := doGetCurrentFrame(*sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	var targetUUID string
	var createdNewFrame bool

	if opts.NArgs() == 0 {
		// No args: stay in current frame (identity operation, but still enters session)
		targetUUID = currentUUID
	} else {
		spec := opts.Arg(0)
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
			// :: snaps current frame and creates a new frame from it
			snapTriplet, err := doSnap(*sockPath, "")
			if err != nil {
				fmt.Fprintf(os.Stderr, "error snapping current frame: %v\n", err)
				os.Exit(1)
			}
			targetUUID, err = doCreate(*sockPath, snapTriplet, *isolation, "")
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
			createdNewFrame = true
		} else {
			// Snap triplet - create new frame
			snapshotSpec, err := resolveSnapTriplet(*sockPath, spec)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
			targetUUID, err = doCreate(*sockPath, snapshotSpec, *isolation, "")
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
			createdNewFrame = true
		}
	}

	// If we created a new frame, clone the parent's history
	if createdNewFrame && currentUUID != "" {
		if err := doCloneHistory(*sockPath, currentUUID, targetUUID); err != nil {
			// Log but don't fail - history cloning is best-effort
			fmt.Fprintf(os.Stderr, "warning: failed to clone history: %v\n", err)
		}
	}

	// Connect to the target frame via vsock and start session
	exitCode, err := runVsockSession(targetUUID)
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

// dialVshd connects to vshd using the appropriate transport. In VMs (when
// /dev/vsock exists) it connects directly via vsock to the host; in containers
// it connects to the Unix socket at sockPath and performs the CONNECT 5222
// handshake to be proxied to vshd.
func dialVshd() (net.Conn, error) {
	if inVM() {
		// In a VM: connect directly via vsock to the host.
		conn, err := vsock.Dial(hostCID, thunderproto.VshPort, nil)
		if err != nil {
			return nil, fmt.Errorf("vsock dial: %w", err)
		}
		return conn, nil
	}

	// In a container: connect to the Unix socket with CONNECT 5222 handshake.
	conn, err := net.Dial("unix", *sockPath)
	if err != nil {
		return nil, fmt.Errorf("dial control socket: %w", err)
	}

	reader := bufio.NewReader(conn)
	if err := thunderproto.WriteClientHandshakePort(conn, reader, thunderproto.VshPort); err != nil {
		conn.Close()
		return nil, fmt.Errorf("vshd handshake: %w", err)
	}

	return &bufferedConn{Conn: conn, reader: reader}, nil
}

// bufferedConn wraps a net.Conn with a buffered reader for the handshake.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

// runVsockSession connects to vshd and runs an interactive session.
// It returns the exit code from the remote session.
func runVsockSession(frameUUID string) (int, error) {
	// Connect to vshd via vsock (VM) or control socket proxy (container)
	conn, err := dialVshd()
	if err != nil {
		return 1, err
	}
	defer conn.Close()

	// Determine if we have a TTY
	isPTY := term.IsTerminal(int(os.Stdin.Fd()))

	// Write the VMX protocol header: VMX\0framePath\0user\0pty\0argc\0
	// For ts go, we always want a login shell (no command)
	ptyFlag := "0"
	if isPTY {
		ptyFlag = "1"
	}
	// Empty user means auto-detect, empty command means login shell
	fmt.Fprintf(conn, "VMX\x00%s\x00\x00%s\x000\x00", frameUUID, ptyFlag)

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

	// Host -> guest: send stdin as FrameStdin
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

	// Guest -> host: decode TLV frames
	go func() {
		defer close(done)
		for {
			typ, payload, err := vshdproto.ReadFrame(conn)
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

// cmdUndo jumps backward in time by one snap.
func cmdUndo(args []string) {
	opts := getopt.New()
	opts.SetProgram("ts undo")
	opts.Parse(append([]string{"ts undo"}, args...))

	if opts.NArgs() > 0 {
		fmt.Fprintln(os.Stderr, "usage: ts undo")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Jumps backward in time by one snap:")
		fmt.Fprintln(os.Stderr, "1. Takes a snapshot of the current state")
		fmt.Fprintln(os.Stderr, "2. Creates a new frame based on the previous snap")
		fmt.Fprintln(os.Stderr, "3. Enters the new frame with pruned history")
		os.Exit(1)
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

	// 2. Run ts snap to record current state
	currentSnap, err := doSnap(*sockPath, "")
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

	// 7. Enter the new frame
	fmt.Fprintf(os.Stderr, "Undoing to snap %s...\n", prevSnap)
	exitCode, err := runVsockSession(newUUID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	os.Exit(exitCode)
}
