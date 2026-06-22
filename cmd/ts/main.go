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
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/mdlayher/vsock"
	"github.com/pborman/getopt/v2"
	"github.com/tailscale/thundersnap/tsm"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// thunderPort is the vsock port used for the thunder control protocol.
const thunderPort = 5223

// hostCID is the vsock CID for the host (used in VMs).
const hostCID = 2

var sockPath = getopt.StringLong("sock", 0, "/thunder.sock", "path to control socket")
var help = getopt.BoolLong("help", 'h', "show help")

func usage() {
	getopt.PrintUsage(os.Stderr)
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  ping           send a ping to thundersnapd")
	fmt.Fprintln(os.Stderr, "  snap           create a snapshot of the current container/VM")
	fmt.Fprintln(os.Stderr, "  snaps          list all snapshots with sizes")
	fmt.Fprintln(os.Stderr, "  frame          create a new frame from root:home:work snaps")
	fmt.Fprintln(os.Stderr, "  frames         list all frames with status")
	fmt.Fprintln(os.Stderr, "  taint          add a taint to the current frame")
	fmt.Fprintln(os.Stderr, "  download-docker download a Docker image as a snap")
	fmt.Fprintln(os.Stderr, "  who-has        query peers to find which ones have a snap")
	fmt.Fprintln(os.Stderr, "  download-snap  download a snap from mesh peers")
	os.Exit(1)
}

func main() {
	// Check if we're being invoked as a shell (argv[0] is "sh" or "-sh")
	// This happens when thundersnapd symlinks /bin/sh -> /bin/ts for
	// containers that lack a shell.
	base := filepath.Base(os.Args[0])
	if base == "sh" || base == "-sh" {
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
	case "drop-caps-and-run":
		// Hidden command - not listed in usage
		cmdDropCapsAndRun(cmdArgs)
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

// inVM returns true if we're running inside a VM with vsock support.
func inVM() bool {
	_, err := os.Stat("/dev/vsock")
	return err == nil
}

// dialThunder connects to thundersnapd and performs the vsock handshake.
// In VMs (when /dev/vsock exists), it connects directly via vsock to the host.
// In containers, it connects to the Unix socket at sockPath.
func dialThunder(ctx context.Context, sockPath string) (net.Conn, error) {
	var conn net.Conn
	var err error

	if inVM() {
		// In a VM: connect directly via vsock to host
		conn, err = vsock.Dial(hostCID, thunderPort, nil)
		if err != nil {
			return nil, fmt.Errorf("vsock dial: %w", err)
		}
		// vsock connections don't need the CONNECT handshake - they're already
		// connected to the right port. The host side receives this as a direct
		// connection on the port-specific Unix socket.
		return conn, nil
	}

	// In a container: connect to Unix socket with CONNECT handshake
	conn, err = net.Dial("unix", sockPath)
	if err != nil {
		return nil, err
	}

	// Send vsock-style CONNECT handshake
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", thunderPort); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send CONNECT: %w", err)
	}

	// Read response - should be "OK <port>\n"
	reader := bufio.NewReader(conn)
	response, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read handshake response: %w", err)
	}
	response = strings.TrimSpace(response)
	if !strings.HasPrefix(response, "OK") {
		conn.Close()
		return nil, fmt.Errorf("handshake failed: %s", response)
	}

	// Return a conn that uses the buffered reader (in case there's buffered data)
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

func doPing(sockPath string) error {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialThunder(ctx, sockPath)
			},
		},
	}

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

	if opts.NArgs() > 0 {
		fmt.Fprintln(os.Stderr, "error: snap takes no arguments (use --delete to delete a snapshot)")
		os.Exit(1)
	}

	snapshotID, err := doSnap(*sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Print just the snapshot ID to stdout
	fmt.Println(snapshotID)
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

func doSnap(sockPath string) (string, error) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialThunder(ctx, sockPath)
			},
		},
	}

	// Detect if stderr is a TTY for progress display
	isTTY := term.IsTerminal(int(os.Stderr.Fd()))

	// Get terminal width for formatting
	termWidth := 80 // default
	if isTTY {
		if w, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && w > 0 {
			termWidth = w
		}
	}

	// Build URL with streaming enabled
	url := "http://localhost/snap?stream=1"
	if isTTY {
		url += "&tty=1"
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
	var lastLineLen int

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var event SnapStreamEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return "", fmt.Errorf("parse stream event: %w (line: %q)", err, string(line))
		}

		if event.Type == "progress" {
			lastProgressMsg = event.Message
			// Write progress to stderr
			if isTTY {
				// Truncate message to terminal width if needed
				msg := event.Message
				if len(msg) > termWidth {
					msg = msg[:termWidth]
				}
				// Pad with spaces to clear previous longer line
				padding := ""
				if len(msg) < lastLineLen {
					padding = strings.Repeat(" ", lastLineLen-len(msg))
				}
				fmt.Fprintf(os.Stderr, "\r%s%s", msg, padding)
				lastLineLen = len(msg)
			}
		} else if event.Type == "result" {
			lastEvent = event
		}
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read stream: %w", err)
	}

	// Clear the progress line if TTY
	if isTTY && lastLineLen > 0 {
		fmt.Fprintf(os.Stderr, "\r%s\r", strings.Repeat(" ", lastLineLen))
	}
	// Print the final summary (works for both TTY and non-TTY since it's the "done" line)
	if lastProgressMsg != "" {
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
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialThunder(ctx, sockPath)
			},
		},
	}

	req := DeleteSnapRequest{SnapshotID: snapshotID}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	resp, err := client.Post("http://localhost/delete-snap", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	var result DeleteSnapResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode response: %w", err)
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
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialThunder(ctx, sockPath)
			},
		},
	}

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
	opts.SetParameters("<frame-name> <snapshot-spec>")
	isolation := opts.StringLong("isolation", 0, "", "isolation level: vm, container, none")
	deleteFlag := opts.BoolLong("delete", 'd', "delete a frame")
	// Parse expects first element to be program name (like os.Args)
	opts.Parse(append([]string{"ts frame"}, args...))

	if *deleteFlag {
		if opts.NArgs() != 1 {
			fmt.Fprintln(os.Stderr, "error: --delete requires exactly one frame name argument")
			fmt.Fprintln(os.Stderr, "usage: ts frame --delete <frame-name>")
			os.Exit(1)
		}
		frameName := opts.Arg(0)
		if err := doDeleteFrame(*sockPath, frameName); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Deleted frame %s\n", frameName)
		return
	}

	if opts.NArgs() != 2 {
		fmt.Fprintln(os.Stderr, "error: frame requires exactly two arguments")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "usage: ts frame [--isolation=<level>] <frame-name> <snapshot-spec>")
		fmt.Fprintln(os.Stderr, "       ts frame --delete <frame-name>")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "snapshot-spec can be:")
		fmt.Fprintln(os.Stderr, "  <snapshot-id>                    single snapshot (legacy)")
		fmt.Fprintln(os.Stderr, "  <rootfs>:<home>:<work>           frame with three components")
		fmt.Fprintln(os.Stderr, "  <rootfs>::                       frame with empty home/work")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "examples:")
		fmt.Fprintln(os.Stderr, "  ts frame dev abc123             single snapshot")
		fmt.Fprintln(os.Stderr, "  ts frame dev abc123::           rootfs only, empty home/work")
		fmt.Fprintln(os.Stderr, "  ts frame dev abc123:def456:     rootfs + home, empty work")
		fmt.Fprintln(os.Stderr, "  ts frame --delete dev           delete a frame")
		os.Exit(1)
	}

	frameName := opts.Arg(0)
	snapshotSpec := opts.Arg(1)

	if err := doCreate(*sockPath, frameName, snapshotSpec, *isolation); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
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
	FrameName  string `json:"frame_name"`
	SnapshotID string `json:"snapshot_id"`
	Isolation  string `json:"isolation,omitempty"`
}

// CreateResponse is the response from /create
type CreateResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	Path    string `json:"path,omitempty"`
}

// CreateStreamEvent is an event in the streaming create response
type CreateStreamEvent struct {
	Type    string `json:"type"`
	Message string `json:"message,omitempty"`
	Status  string `json:"status,omitempty"`
	Path    string `json:"path,omitempty"`
}

func doCreate(sockPath, frameName, snapshotID, isolation string) error {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialThunder(ctx, sockPath)
			},
		},
	}

	// Detect if stderr is a TTY for progress display
	isTTY := term.IsTerminal(int(os.Stderr.Fd()))

	// Get terminal width for formatting
	termWidth := 80 // default
	if isTTY {
		if w, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && w > 0 {
			termWidth = w
		}
	}

	// Build URL with streaming enabled
	url := "http://localhost/create?stream=1"
	if isTTY {
		url += "&tty=1"
	}

	req := CreateRequest{
		FrameName:  frameName,
		SnapshotID: snapshotID,
		Isolation:  isolation,
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
	var lastEvent CreateStreamEvent
	var lastLineLen int

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var event CreateStreamEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return fmt.Errorf("parse stream event: %w (line: %q)", err, string(line))
		}

		if event.Type == "progress" {
			// Write progress to stderr
			if isTTY {
				// Truncate message to terminal width if needed
				msg := event.Message
				if len(msg) > termWidth {
					msg = msg[:termWidth]
				}
				// Pad with spaces to clear previous longer line
				padding := ""
				if len(msg) < lastLineLen {
					padding = strings.Repeat(" ", lastLineLen-len(msg))
				}
				fmt.Fprintf(os.Stderr, "\r%s%s", msg, padding)
				lastLineLen = len(msg)
			} else {
				// Non-TTY: print each progress message on its own line
				fmt.Fprintln(os.Stderr, event.Message)
			}
		} else if event.Type == "result" {
			lastEvent = event
		} else if event.Type == "" && event.Status != "" {
			// Non-streaming error response (e.g., frame already exists)
			// Convert to a result event for consistent handling
			lastEvent = event
			lastEvent.Type = "result"
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read stream: %w", err)
	}

	// Clear the progress line if TTY
	if isTTY && lastLineLen > 0 {
		fmt.Fprintf(os.Stderr, "\r%s\r", strings.Repeat(" ", lastLineLen))
	}

	// Check result
	if lastEvent.Type != "result" {
		return fmt.Errorf("no result received from server")
	}

	if lastEvent.Status != "ok" {
		return fmt.Errorf("%s", lastEvent.Message)
	}

	fmt.Printf("Created frame at %s\n", lastEvent.Path)
	return nil
}

// DeleteFrameRequest is the request body for /delete-frame
type DeleteFrameRequest struct {
	FrameName string `json:"frame_name"`
}

// DeleteFrameResponse is the response from /delete-frame
type DeleteFrameResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

func doDeleteFrame(sockPath, frameName string) error {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialThunder(ctx, sockPath)
			},
		},
	}

	req := DeleteFrameRequest{FrameName: frameName}
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
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialThunder(ctx, sockPath)
			},
		},
	}

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
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialThunder(ctx, sockPath)
			},
		},
	}

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
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialThunder(ctx, sockPath)
			},
		},
	}

	req := TaintRequest{
		TaintName: taintName,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	resp, err := client.Post("http://localhost/taint", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	var result TaintResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("parse response: %w", err)
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
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialThunder(ctx, sockPath)
			},
		},
		// Docker downloads can be slow
		Timeout: 30 * time.Minute,
	}

	// Detect if stderr is a TTY for progress display
	isTTY := term.IsTerminal(int(os.Stderr.Fd()))

	// Get terminal width for formatting
	termWidth := 80 // default
	if isTTY {
		if w, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && w > 0 {
			termWidth = w
		}
	}

	// Build URL with streaming enabled
	url := "http://localhost/download-docker?stream=1"
	if isTTY {
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
	var lastLineLen int

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

		if event.Type == "progress" && isTTY {
			// Clear line and show progress
			msg := event.Message
			if len(msg) > termWidth-2 {
				msg = msg[:termWidth-5] + "..."
			}
			// Pad to clear previous line
			padding := ""
			if len(msg) < lastLineLen {
				padding = strings.Repeat(" ", lastLineLen-len(msg))
			}
			fmt.Fprintf(os.Stderr, "\r%s%s", msg, padding)
			lastLineLen = len(msg)
		} else if event.Type == "progress" {
			fmt.Fprintf(os.Stderr, "%s\n", event.Message)
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read stream: %w", err)
	}

	// Clear progress line
	if isTTY && lastLineLen > 0 {
		fmt.Fprintf(os.Stderr, "\r%s\r", strings.Repeat(" ", lastLineLen))
	}

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
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialThunder(ctx, sockPath)
			},
		},
	}

	// Detect if stderr is a TTY for progress display
	isTTY := term.IsTerminal(int(os.Stderr.Fd()))

	// Get terminal width for formatting
	termWidth := 80 // default
	if isTTY {
		if w, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && w > 0 {
			termWidth = w
		}
	}

	// Build URL with streaming enabled
	url := "http://localhost/download-snap?stream=1"
	if isTTY {
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
	var lastLineLen int

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
			// Write progress to stderr
			if isTTY {
				// Truncate message to terminal width if needed
				msg := event.Message
				if len(msg) > termWidth {
					msg = msg[:termWidth]
				}
				// Pad with spaces to clear previous longer line
				padding := ""
				if len(msg) < lastLineLen {
					padding = strings.Repeat(" ", lastLineLen-len(msg))
				}
				fmt.Fprintf(os.Stderr, "\r%s%s", msg, padding)
				lastLineLen = len(msg)
			} else {
				// Non-TTY: print each progress message on its own line
				fmt.Fprintln(os.Stderr, event.Message)
			}
		} else if event.Type == "result" {
			lastEvent = event
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read stream: %w", err)
	}

	// Clear the progress line if TTY
	if isTTY && lastLineLen > 0 {
		fmt.Fprintf(os.Stderr, "\r%s\r", strings.Repeat(" ", lastLineLen))
	}
	// Print the final progress message (the "done" line)
	if lastProgressMsg != "" && !isTTY {
		// Already printed for non-TTY
	} else if lastProgressMsg != "" {
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

// cmdDropCapsAndRun sets up container isolation and then execs the command
// specified in the remaining arguments. This is used by thundersnapd to
// initialize and restrict container processes.
//
// Setup performed:
//   - Makes all mounts private (prevents mount propagation to host)
//   - Mounts /proc filesystem
//   - Sets hostname and domainname (if --hostname/--domainname provided)
//   - Drops dangerous capabilities from the bounding set
//
// Capabilities dropped:
//   - CAP_NET_ADMIN: prevents iptables, routing, interface config changes
//   - CAP_SYS_MODULE: prevents loading kernel modules
//   - CAP_SYS_BOOT: prevents reboot
//   - CAP_SYS_TIME: prevents changing system clock
//   - CAP_MKNOD: prevents creating device nodes
//   - CAP_AUDIT_WRITE: prevents writing to audit log
//   - CAP_SETFCAP: prevents setting file capabilities
func cmdDropCapsAndRun(args []string) {
	// Parse our flags manually since we need to pass remaining args to exec
	var hostname, domainname, chrootPath string
	var cmdArgs []string

	for i := 0; i < len(args); i++ {
		if args[i] == "--hostname" && i+1 < len(args) {
			hostname = args[i+1]
			i++
		} else if strings.HasPrefix(args[i], "--hostname=") {
			hostname = strings.TrimPrefix(args[i], "--hostname=")
		} else if args[i] == "--domainname" && i+1 < len(args) {
			domainname = args[i+1]
			i++
		} else if strings.HasPrefix(args[i], "--domainname=") {
			domainname = strings.TrimPrefix(args[i], "--domainname=")
		} else if args[i] == "--chroot" && i+1 < len(args) {
			chrootPath = args[i+1]
			i++
		} else if strings.HasPrefix(args[i], "--chroot=") {
			chrootPath = strings.TrimPrefix(args[i], "--chroot=")
		} else if args[i] == "--" {
			cmdArgs = args[i+1:]
			break
		} else {
			// First non-flag argument starts the command
			cmdArgs = args[i:]
			break
		}
	}

	if len(cmdArgs) == 0 {
		fmt.Fprintln(os.Stderr, "error: drop-caps-and-run requires a command to execute")
		os.Exit(1)
	}

	// Make all mounts private so mounts inside the container don't propagate
	// to the host. This must be done BEFORE chroot while "/" is still a real
	// mount point. After CLONE_NEWNS, we have our own copy of the mount table
	// but it still has "shared" propagation. Making it private here only
	// affects our namespace, not the parent.
	//
	// In VM mode (running as init), this may fail because the root filesystem
	// (virtiofs) doesn't support propagation changes. That's fine - VMs don't
	// have mount propagation concerns anyway.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		// Only log, don't exit - this is expected to fail in VM mode
		fmt.Fprintf(os.Stderr, "warning: failed to make mounts private: %v (ok in VM mode)\n", err)
	}

	// Chroot into the container rootfs if specified
	if chrootPath != "" {
		if err := unix.Chroot(chrootPath); err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to chroot to %s: %v\n", chrootPath, err)
			os.Exit(1)
		}
		if err := unix.Chdir("/"); err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to chdir to /: %v\n", err)
			os.Exit(1)
		}
	}

	// Mount /proc filesystem
	if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		// Ignore errors - /proc might already be mounted or not exist
		_ = err
	}

	// Mount /sys filesystem
	if err := unix.Mount("sysfs", "/sys", "sysfs", 0, ""); err != nil {
		// Ignore errors - /sys might already be mounted or not exist
		_ = err
	}

	// Set up /dev like Docker/containerd do:
	// - tmpfs at /dev
	// - Essential device nodes (null, zero, full, random, urandom, tty)
	// - Symlinks for stdin/stdout/stderr and /dev/fd
	// - /dev/pts for pseudoterminals
	// - /dev/shm for shared memory
	setupDev()

	// Set hostname if provided
	if hostname != "" {
		if err := unix.Sethostname([]byte(hostname)); err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to set hostname: %v\n", err)
			os.Exit(1)
		}
	}

	// Set domainname if provided
	if domainname != "" {
		if err := unix.Setdomainname([]byte(domainname)); err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to set domainname: %v\n", err)
			os.Exit(1)
		}
	}

	// Capabilities to drop from the bounding set
	capsToDrop := []uintptr{
		unix.CAP_NET_ADMIN,
		unix.CAP_SYS_MODULE,
		unix.CAP_SYS_BOOT,
		unix.CAP_SYS_TIME,
		unix.CAP_MKNOD,
		unix.CAP_AUDIT_WRITE,
		unix.CAP_SETFCAP,
	}

	// Drop each capability from the bounding set
	for _, cap := range capsToDrop {
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, cap, 0, 0, 0); err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to drop capability %d: %v\n", cap, err)
			os.Exit(1)
		}
	}

	// Ensure PATH is set - the kernel doesn't set it when starting init,
	// and child processes (like vshd calling "su") need it.
	if os.Getenv("PATH") == "" {
		os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}

	// Find the executable in PATH
	executable, err := findExecutable(cmdArgs[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Exec the command, replacing this process
	if err := syscall.Exec(executable, cmdArgs, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "error: exec %s: %v\n", cmdArgs[0], err)
		os.Exit(1)
	}
}

// setupDev creates a minimal /dev filesystem like Docker/containerd.
// This creates a tmpfs at /dev with essential device nodes, symlinks,
// /dev/pts for pseudoterminals, and /dev/shm for shared memory.
func setupDev() {
	// Check if /dev/vsock exists on devtmpfs before we mount over it.
	// In VMs, /dev/vsock is a misc device that only works when backed by devtmpfs -
	// creating it via mknod on a different filesystem doesn't work. We'll bind-mount
	// it from devtmpfs after setting up our tmpfs.
	//
	// To preserve access to the original devtmpfs vsock, we mount devtmpfs at a
	// temporary location first.
	var vsockSource string
	if _, err := os.Stat("/dev/vsock"); err == nil {
		// vsock exists - mount devtmpfs at a temp location so we can bind-mount it later
		os.MkdirAll("/tmp/.devtmpfs", 0755)
		if err := unix.Mount("devtmpfs", "/tmp/.devtmpfs", "devtmpfs", 0, ""); err == nil {
			if _, err := os.Stat("/tmp/.devtmpfs/vsock"); err == nil {
				vsockSource = "/tmp/.devtmpfs/vsock"
			}
		}
	}

	// Mount tmpfs at /dev
	if err := unix.Mount("tmpfs", "/dev", "tmpfs", unix.MS_NOSUID|unix.MS_STRICTATIME, "mode=755,size=65536k"); err != nil {
		// /dev might not exist or we might not have permissions
		return
	}

	// Create essential device nodes
	// Format: name, mode, major, minor
	// Note: vsock is NOT included here - it's a misc device that only works via
	// devtmpfs, so we bind-mount it separately if it existed before.
	devices := []struct {
		name  string
		mode  uint32
		major uint32
		minor uint32
	}{
		{"null", unix.S_IFCHR | 0666, 1, 3},
		{"zero", unix.S_IFCHR | 0666, 1, 5},
		{"full", unix.S_IFCHR | 0666, 1, 7},
		{"random", unix.S_IFCHR | 0666, 1, 8},
		{"urandom", unix.S_IFCHR | 0666, 1, 9},
		{"tty", unix.S_IFCHR | 0666, 5, 0},
	}

	for _, dev := range devices {
		path := "/dev/" + dev.name
		devNum := unix.Mkdev(dev.major, dev.minor)
		// Ignore errors - we're best-effort here
		if err := unix.Mknod(path, dev.mode, int(devNum)); err == nil {
			// Mknod doesn't respect mode bits for permissions (affected by umask),
			// so explicitly set the permissions after creating the device.
			unix.Chmod(path, dev.mode&0777)
		}
	}

	// Create symlinks for stdin/stdout/stderr
	os.Symlink("/proc/self/fd/0", "/dev/stdin")
	os.Symlink("/proc/self/fd/1", "/dev/stdout")
	os.Symlink("/proc/self/fd/2", "/dev/stderr")

	// Create /dev/fd -> /proc/self/fd
	os.Symlink("/proc/self/fd", "/dev/fd")

	// Create /dev/pts directory and mount devpts
	os.MkdirAll("/dev/pts", 0755)
	unix.Mount("devpts", "/dev/pts", "devpts", unix.MS_NOSUID|unix.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=620")

	// Create /dev/ptmx symlink to /dev/pts/ptmx for the newinstance mount
	os.Symlink("pts/ptmx", "/dev/ptmx")

	// Create /dev/shm for shared memory
	os.MkdirAll("/dev/shm", 1777)
	unix.Mount("tmpfs", "/dev/shm", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=1777,size=65536k")

	// Create /dev/mqueue for POSIX message queues (optional but some programs expect it)
	os.MkdirAll("/dev/mqueue", 0755)
	unix.Mount("mqueue", "/dev/mqueue", "mqueue", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "")

	// Bind-mount /dev/vsock from devtmpfs if it was available.
	// This is necessary because vsock is a misc device that only works via devtmpfs.
	if vsockSource != "" {
		// Create the mount point
		f, err := os.OpenFile("/dev/vsock", os.O_CREATE|os.O_WRONLY, 0666)
		if err == nil {
			f.Close()
			unix.Mount(vsockSource, "/dev/vsock", "", unix.MS_BIND, "")
		}
		// Clean up the temporary devtmpfs mount
		unix.Unmount("/tmp/.devtmpfs", 0)
		os.Remove("/tmp/.devtmpfs")
	}
}

// cmdCheckDev outputs the state of /dev for e2e testing.
// Output format is one item per line:
//
//	DEV:<name>:<exists|missing>:<perms>
//	LINK:<name>:<exists|missing>:<target>
//	DIR:<name>:<exists|missing>
//	DONE
func cmdCheckDev() {
	// Check device nodes (vsock is optional - only works in VMs with vsock support)
	devices := []string{"null", "zero", "full", "random", "urandom", "tty", "vsock"}
	for _, dev := range devices {
		path := "/dev/" + dev
		info, err := os.Lstat(path)
		if err != nil {
			fmt.Printf("DEV:%s:missing:0\n", dev)
			continue
		}
		if info.Mode()&os.ModeCharDevice == 0 {
			fmt.Printf("DEV:%s:not-chardev:%o\n", dev, info.Mode().Perm())
			continue
		}
		fmt.Printf("DEV:%s:exists:%o\n", dev, info.Mode().Perm())
	}

	// Check symlinks
	links := []string{"stdin", "stdout", "stderr", "fd"}
	for _, link := range links {
		path := "/dev/" + link
		target, err := os.Readlink(path)
		if err != nil {
			fmt.Printf("LINK:%s:missing:\n", link)
			continue
		}
		fmt.Printf("LINK:%s:exists:%s\n", link, target)
	}

	// Check directories
	dirs := []string{"pts", "shm", "mqueue"}
	for _, dir := range dirs {
		path := "/dev/" + dir
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			fmt.Printf("DIR:%s:missing\n", dir)
			continue
		}
		fmt.Printf("DIR:%s:exists\n", dir)
	}

	// List all entries in /dev for completeness checking
	// This allows tests to verify that unwanted devtmpfs entries are not present
	entries, err := os.ReadDir("/dev")
	if err == nil {
		for _, entry := range entries {
			fmt.Printf("ENTRY:%s\n", entry.Name())
		}
	}

	fmt.Println("DONE")
}

// cmdCheckIsolation outputs the container isolation state for e2e testing.
// Output format is one item per line:
//
//	HOSTNAME:<hostname>
//	DOMAINNAME:<domainname>
//	PID1:<pid-is-1>
//	PROC:<mounted|not-mounted>
//	SYS:<mounted|not-mounted>
//	CAP:<name>:<has|dropped>
//	NS:<name>:<inode>
//	DONE
func cmdCheckIsolation() {
	// Check hostname
	hostname, _ := os.Hostname()
	fmt.Printf("HOSTNAME:%s\n", hostname)

	// Check domainname via syscall
	var uts unix.Utsname
	if err := unix.Uname(&uts); err == nil {
		domainname := string(uts.Domainname[:])
		if idx := strings.IndexByte(domainname, 0); idx >= 0 {
			domainname = domainname[:idx]
		}
		fmt.Printf("DOMAINNAME:%s\n", domainname)
	}

	// Check if we're PID 1 (indicates PID namespace isolation)
	if os.Getpid() == 1 {
		fmt.Println("PID1:yes")
	} else {
		fmt.Printf("PID1:no:%d\n", os.Getpid())
	}

	// Check /proc mount
	if _, err := os.Stat("/proc/self"); err == nil {
		fmt.Println("PROC:mounted")
	} else {
		fmt.Println("PROC:not-mounted")
	}

	// Check /sys mount
	if _, err := os.Stat("/sys/class"); err == nil {
		fmt.Println("SYS:mounted")
	} else {
		fmt.Println("SYS:not-mounted")
	}

	// Check capabilities in bounding set
	// These are the caps that cmdDropCapsAndRun drops
	capsToCheck := []struct {
		name string
		cap  uintptr
	}{
		{"NET_ADMIN", unix.CAP_NET_ADMIN},
		{"SYS_MODULE", unix.CAP_SYS_MODULE},
		{"SYS_BOOT", unix.CAP_SYS_BOOT},
		{"SYS_TIME", unix.CAP_SYS_TIME},
		{"MKNOD", unix.CAP_MKNOD},
		{"AUDIT_WRITE", unix.CAP_AUDIT_WRITE},
		{"SETFCAP", unix.CAP_SETFCAP},
	}

	for _, c := range capsToCheck {
		// Use prctl to check if capability is in bounding set
		ret, _, _ := unix.Syscall(unix.SYS_PRCTL, unix.PR_CAPBSET_READ, c.cap, 0)
		if ret == 1 {
			fmt.Printf("CAP:%s:has\n", c.name)
		} else {
			fmt.Printf("CAP:%s:dropped\n", c.name)
		}
	}

	// Check namespace inodes (to verify we're in new namespaces)
	namespaces := []string{"pid", "mnt", "uts", "net"}
	for _, ns := range namespaces {
		path := fmt.Sprintf("/proc/self/ns/%s", ns)
		info, err := os.Stat(path)
		if err != nil {
			fmt.Printf("NS:%s:error\n", ns)
			continue
		}
		stat := info.Sys().(*syscall.Stat_t)
		fmt.Printf("NS:%s:%d\n", ns, stat.Ino)
	}

	// Check mount propagation for root mount
	// Read /proc/self/mountinfo to determine propagation type
	mountinfo, err := os.ReadFile("/proc/self/mountinfo")
	foundRoot := false
	if err == nil {
		// Look for root mount (target = /) and check propagation flags
		// Format: id parent major:minor root target options opt:value - fstype source super-options
		for _, line := range strings.Split(string(mountinfo), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 5 {
				target := fields[4]
				if target == "/" {
					foundRoot = true
					// Options are in fields[5] onwards until "-"
					options := ""
					for i := 5; i < len(fields) && fields[i] != "-"; i++ {
						options += fields[i] + " "
					}
					// Propagation types: shared, private, slave, unbindable
					if strings.Contains(options, "shared:") {
						fmt.Println("MOUNT_PROPAGATION:shared")
					} else if strings.Contains(options, "master:") {
						fmt.Println("MOUNT_PROPAGATION:slave")
					} else if strings.Contains(options, "unbindable") {
						fmt.Println("MOUNT_PROPAGATION:unbindable")
					} else {
						// Default is private (no propagation marker)
						fmt.Println("MOUNT_PROPAGATION:private")
					}
					break
				}
			}
		}
		if !foundRoot {
			// In a container with a fresh mount namespace, there might not be a "/" entry
			// if the root is the pivot_root target. Default to private in this case.
			fmt.Println("MOUNT_PROPAGATION:private")
		}
	} else {
		fmt.Println("MOUNT_PROPAGATION:error")
	}

	fmt.Println("DONE")
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

// runAsShell implements a POSIX-compatible shell using mvdan.cc/sh.
//
// When ts is symlinked to /bin/sh, it acts as a real shell supporting:
//   - sh -c 'command' - run a command string
//   - sh script.sh - run a script file
//   - sh (no args) - interactive shell
//
// This uses the mvdan.cc/sh/v3 interpreter which provides proper POSIX shell
// semantics including pipes, redirects, variable expansion, and control flow.
func runAsShell() {
	err := runShell(os.Stdin, os.Stdout, os.Stderr, os.Args[1:]...)
	if status, ok := interp.IsExitStatus(err); ok {
		os.Exit(int(status))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ts: %v\n", err)
		os.Exit(1)
	}
}

// runShell is the core shell implementation.
func runShell(stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	runner, err := interp.New(interp.StdIO(stdin, stdout, stderr))
	if err != nil {
		return err
	}

	parser := syntax.NewParser()

	// Parse arguments to find -c command or script file
	var commandStr string
	var scriptFile string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-c":
			if i+1 >= len(args) {
				return fmt.Errorf("-c requires an argument")
			}
			commandStr = args[i+1]
			i++
		case "-i", "-l", "--login", "-e", "-x", "-v":
			// Flags we recognize but ignore for now
		default:
			if strings.HasPrefix(args[i], "-") {
				// Unknown flag - ignore
				continue
			}
			// First non-flag argument is a script file
			scriptFile = args[i]
		}
	}

	// Execute based on what we found
	if commandStr != "" {
		// sh -c 'command'
		return runShellCommand(runner, parser, commandStr)
	}

	if scriptFile != "" {
		// sh script.sh
		return runShellScript(runner, parser, scriptFile)
	}

	// Interactive shell (or reading from stdin if not a TTY)
	if r, ok := stdin.(*os.File); ok && term.IsTerminal(int(r.Fd())) {
		return runShellInteractive(runner, parser, stdin, stdout)
	}

	// Reading commands from stdin (non-interactive)
	return runShellCommand(runner, parser, "")
}

// runShellCommand executes a command string.
func runShellCommand(runner *interp.Runner, parser *syntax.Parser, command string) error {
	var reader io.Reader
	if command != "" {
		reader = strings.NewReader(command)
	} else {
		reader = os.Stdin
	}

	prog, err := parser.Parse(reader, "")
	if err != nil {
		return err
	}

	runner.Reset()
	return runner.Run(context.Background(), prog)
}

// runShellScript executes a script file.
func runShellScript(runner *interp.Runner, parser *syntax.Parser, filename string) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	prog, err := parser.Parse(f, filename)
	if err != nil {
		return err
	}

	runner.Reset()
	return runner.Run(context.Background(), prog)
}

// runShellInteractive runs a simple interactive shell.
func runShellInteractive(runner *interp.Runner, parser *syntax.Parser, stdin io.Reader, stdout io.Writer) error {
	fmt.Fprintf(stdout, "$ ")

	scanner := bufio.NewScanner(stdin)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			fmt.Fprintf(stdout, "$ ")
			continue
		}

		prog, err := parser.Parse(strings.NewReader(line), "")
		if err != nil {
			fmt.Fprintf(stdout, "error: %v\n$ ", err)
			continue
		}

		if err := runner.Run(context.Background(), prog); err != nil {
			if _, ok := interp.IsExitStatus(err); !ok {
				fmt.Fprintf(stdout, "error: %v\n", err)
			}
		}

		if runner.Exited() {
			return nil
		}

		fmt.Fprintf(stdout, "$ ")
	}

	return scanner.Err()
}
