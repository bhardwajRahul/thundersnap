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
	"sync"
	"time"

	"github.com/mdlayher/vsock"
	"github.com/pborman/getopt/v2"
	"github.com/tailscale/thundersnap/bupdate"
	"golang.org/x/term"
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
	fmt.Fprintln(os.Stderr, "  ping         send a ping to thundersnapd")
	fmt.Fprintln(os.Stderr, "  bupdate      download and reconstruct files from mesh peers")
	fmt.Fprintln(os.Stderr, "  fidx         create a file index (.fidx) for a file or directory")
	fmt.Fprintln(os.Stderr, "  snap         create a snapshot of the current container/VM")
	fmt.Fprintln(os.Stderr, "  create       create a new workspace from a snapshot")
	fmt.Fprintln(os.Stderr, "  who-has      query peers to find which ones have a snapshot")
	os.Exit(1)
}

func main() {
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
	case "bupdate":
		cmdBupdate(cmdArgs)
	case "fidx":
		cmdFidx(cmdArgs)
	case "snap":
		cmdSnap(cmdArgs)
	case "create":
		cmdCreate(cmdArgs)
	case "who-has":
		cmdWhoHas(cmdArgs)
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

func cmdFidx(args []string) {
	opts := getopt.New()
	opts.SetProgram("ts fidx")
	opts.SetParameters("<path>")
	refFile := opts.StringLong("ref", 'r', "", "reference fidx file for incremental indexing")
	// Parse expects first element to be program name (like os.Args)
	opts.Parse(append([]string{"ts fidx"}, args...))

	if opts.NArgs() != 1 {
		fmt.Fprintln(os.Stderr, "error: fidx requires exactly one path argument")
		fmt.Fprintln(os.Stderr, "usage: ts fidx [--ref <ref.fidx>] <path>")
		os.Exit(1)
	}

	path := opts.Arg(0)
	outPath := path + ".fidx"

	indexOpts := bupdate.IndexerOptions{
		RefPath:  *refFile,
		Progress: true,
	}

	if err := bupdate.CreateFidx(path, outPath, indexOpts); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Wrote %s\n", outPath)
}

func cmdSnap(args []string) {
	opts := getopt.New()
	opts.SetProgram("ts snap")
	// Parse expects first element to be program name (like os.Args)
	opts.Parse(append([]string{"ts snap"}, args...))

	if opts.NArgs() > 0 {
		fmt.Fprintln(os.Stderr, "error: snap takes no arguments")
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

func cmdBupdate(args []string) {
	opts := getopt.New()
	opts.SetProgram("ts bupdate")
	opts.SetParameters("<fidx-file>")
	// Parse expects first element to be program name (like os.Args)
	opts.Parse(append([]string{"ts bupdate"}, args...))

	if opts.NArgs() != 1 {
		fmt.Fprintln(os.Stderr, "error: bupdate requires exactly one fidx/mfidx filename argument")
		os.Exit(1)
	}

	fidxName := opts.Arg(0)

	if err := doBupdate(*sockPath, fidxName); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func doBupdate(sockPath, fidxName string) error {
	// Get list of servers from thundersnapd
	servers, err := getServers(sockPath)
	if err != nil {
		return fmt.Errorf("getting servers: %w", err)
	}

	if len(servers) == 0 {
		return fmt.Errorf("no mesh peers available")
	}

	fmt.Printf("Searching %d mesh peers for %s...\n", len(servers), fidxName)

	// Check all servers in parallel for the fidx file
	type serverResult struct {
		peer     meshPeer
		fidx     *bupdate.Fidx
		err      error
		baseURL  string
	}

	results := make(chan serverResult, len(servers))
	var wg sync.WaitGroup

	for _, peer := range servers {
		wg.Add(1)
		go func(p meshPeer) {
			defer wg.Done()

			// Try to fetch the fidx from this peer's /bupdate/ path
			baseURL := strings.TrimSuffix(p.URL, "/")
			fidxURL := baseURL + "/bupdate/" + fidxName

			fidx, err := bupdate.LoadFidxHTTP(fidxURL)
			results <- serverResult{
				peer:    p,
				fidx:    fidx,
				err:     err,
				baseURL: baseURL,
			}
		}(peer)
	}

	// Close results channel when all goroutines are done
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results, pick the first successful one
	var successResults []serverResult
	for r := range results {
		if r.err == nil && r.fidx != nil {
			successResults = append(successResults, r)
		}
	}

	if len(successResults) == 0 {
		return fmt.Errorf("no mesh peer has %s", fidxName)
	}

	// Sort by hostname for determinism, pick first
	sort.Slice(successResults, func(i, j int) bool {
		return successResults[i].peer.Hostname < successResults[j].peer.Hostname
	})

	chosen := successResults[0]
	fmt.Printf("Found %s on %s\n", fidxName, chosen.peer.Hostname)

	// Build local mappings from current directory
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting current directory: %w", err)
	}

	mappings, err := loadLocalMappings(cwd)
	if err != nil {
		return fmt.Errorf("loading local mappings: %w", err)
	}

	// Determine output path based on fidx type
	if chosen.fidx.IsMFIDX {
		// Multi-file index - extract all files
		return bupdateMFIDX(cwd, chosen.baseURL, chosen.fidx, fidxName, mappings)
	}

	// Single file
	outputName := strings.TrimSuffix(fidxName, ".fidx")
	outputPath := filepath.Join(cwd, outputName)
	tmpOutputPath := outputPath + ".tmp"

	if err := reconstructFileHTTP(tmpOutputPath, chosen.fidx, chosen.baseURL, outputName, mappings); err != nil {
		os.Remove(tmpOutputPath)
		return fmt.Errorf("reconstructing file: %w", err)
	}

	if err := os.Rename(tmpOutputPath, outputPath); err != nil {
		return fmt.Errorf("rename: %w", err)
	}

	// Save the fidx locally
	localFidxPath := filepath.Join(cwd, fidxName)
	fidxURL := chosen.baseURL + "/bupdate/" + fidxName
	if err := downloadFile(fidxURL, localFidxPath); err != nil {
		return fmt.Errorf("saving fidx: %w", err)
	}

	fmt.Printf("Downloaded %s\n", outputName)
	return nil
}

// getServers fetches the list of mesh peers via vsock
func getServers(sockPath string) ([]meshPeer, error) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialThunder(ctx, sockPath)
			},
		},
	}

	resp, err := client.Get("http://localhost/ts/servers.json")
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned status %d", resp.StatusCode)
	}

	var servers []meshPeer
	if err := json.NewDecoder(resp.Body).Decode(&servers); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return servers, nil
}

// downloadFile downloads a file from an HTTP URL to a local path
func downloadFile(rawURL, localPath string) error {
	data, err := bupdate.FetchFullFile(rawURL)
	if err != nil {
		return err
	}
	return os.WriteFile(localPath, data, 0644)
}

// loadLocalMappings scans the local directory for .fidx and .mfidx files
func loadLocalMappings(dir string) (*bupdate.FidxMappings, error) {
	var allMappings []bupdate.FidxMapping

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return &bupdate.FidxMappings{}, nil
		}
		return nil, err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		if !strings.HasSuffix(entry.Name(), ".fidx") && !strings.HasSuffix(entry.Name(), ".mfidx") {
			continue
		}

		fidxPath := filepath.Join(dir, entry.Name())
		fidx, err := bupdate.LoadFidx(fidxPath)
		if err != nil {
			continue // skip invalid fidx files
		}

		if fidx.IsMFIDX {
			// Multi-file index
			for _, fileEntry := range fidx.Files {
				filePath := filepath.Join(dir, fileEntry.Filename)
				if _, err := os.Lstat(filePath); err != nil {
					continue
				}

				var offset int64
				for _, ent := range fileEntry.Entries {
					allMappings = append(allMappings, bupdate.FidxMapping{
						SHA:      ent.SHA,
						Filename: filePath,
						Offset:   offset,
						Size:     ent.Size,
					})
					offset += int64(ent.Size)
				}
			}
		} else {
			// Single-file index
			filename := strings.TrimSuffix(entry.Name(), ".fidx")
			filePath := filepath.Join(dir, filename)

			if _, err := os.Stat(filePath); err != nil {
				continue
			}

			var offset int64
			for _, ent := range fidx.Entries {
				allMappings = append(allMappings, bupdate.FidxMapping{
					SHA:      ent.SHA,
					Filename: filePath,
					Offset:   offset,
					Size:     ent.Size,
				})
				offset += int64(ent.Size)
			}
		}
	}

	// Sort by SHA for binary search
	sort.Slice(allMappings, func(i, j int) bool {
		return bytes.Compare(allMappings[i].SHA[:], allMappings[j].SHA[:]) < 0
	})

	return &bupdate.FidxMappings{Mappings: allMappings}, nil
}

// bupdateMFIDX handles reconstruction of all files from a multi-file index
func bupdateMFIDX(localDir, baseURL string, fidx *bupdate.Fidx, fidxName string, mappings *bupdate.FidxMappings) error {
	fmt.Printf("Reconstructing %d files from %s\n", len(fidx.Files), fidxName)

	for i, fileEntry := range fidx.Files {
		outputPath := filepath.Join(localDir, fileEntry.Filename)
		tmpOutputPath := outputPath + ".tmp"

		// Ensure parent directory exists
		if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
			return fmt.Errorf("creating directory for %s: %w", fileEntry.Filename, err)
		}

		// Create a temporary Fidx for this file
		fileFidx := &bupdate.Fidx{
			Entries:  fileEntry.Entries,
			FileSize: int64(fileEntry.FileSize),
		}

		if err := reconstructFileHTTP(tmpOutputPath, fileFidx, baseURL, fileEntry.Filename, mappings); err != nil {
			os.Remove(tmpOutputPath)
			return fmt.Errorf("reconstructing %s: %w", fileEntry.Filename, err)
		}

		if err := os.Rename(tmpOutputPath, outputPath); err != nil {
			return fmt.Errorf("rename %s: %w", fileEntry.Filename, err)
		}

		fmt.Printf("  [%d/%d] %s\n", i+1, len(fidx.Files), fileEntry.Filename)
	}

	// Save the mfidx locally
	localFidxPath := filepath.Join(localDir, fidxName)
	fidxURL := baseURL + "/bupdate/" + fidxName
	if err := downloadFile(fidxURL, localFidxPath); err != nil {
		return fmt.Errorf("saving mfidx: %w", err)
	}

	return nil
}

// reconstructFileHTTP rebuilds a file using local chunks and remote HTTP for missing ones
func reconstructFileHTTP(outputPath string, fidx *bupdate.Fidx, baseURL, remoteFileName string, mappings *bupdate.FidxMappings) error {
	outf, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer outf.Close()

	// Pre-allocate file
	if err := outf.Truncate(fidx.FileSize); err != nil {
		return fmt.Errorf("truncating file: %w", err)
	}

	// Build list of remote chunks we need
	type chunkInfo struct {
		ent          bupdate.FidxEntry
		localMapping *bupdate.FidxMapping
		remoteOffset int64
		outputIdx    int
	}

	var chunks []chunkInfo
	var remoteOffset int64
	var remoteChunks []chunkInfo

	for i, ent := range fidx.Entries {
		mapping := mappings.FindMapping(ent.SHA)
		ci := chunkInfo{
			ent:          ent,
			localMapping: mapping,
			remoteOffset: remoteOffset,
			outputIdx:    i,
		}
		chunks = append(chunks, ci)
		remoteOffset += int64(ent.Size)

		if mapping == nil {
			// Skip zero blocks
			if ent.Size == bupdate.BLOB_MAX && ent.SHA == bupdate.ZeroBlockSHA {
				continue
			}
			remoteChunks = append(remoteChunks, ci)
		}
	}

	// Fetch remote chunks via HTTP
	var remoteData map[int][]byte
	if len(remoteChunks) > 0 {
		fileURL := baseURL + "/bupdate/" + remoteFileName
		reader, err := bupdate.NewHTTPReader(fileURL)
		if err != nil {
			return fmt.Errorf("creating HTTP reader: %w", err)
		}
		defer reader.Close()

		remoteData = make(map[int][]byte)

		// Batch requests for pipelining (16 at a time)
		const batchSize = 16
		for i := 0; i < len(remoteChunks); i += batchSize {
			end := i + batchSize
			if end > len(remoteChunks) {
				end = len(remoteChunks)
			}
			batch := remoteChunks[i:end]

			requests := make([]bupdate.RangeRequest, len(batch))
			for j, ci := range batch {
				requests[j] = bupdate.RangeRequest{
					Offset: ci.remoteOffset,
					Size:   int64(ci.ent.Size),
				}
			}

			results, err := reader.ReadRanges(requests)
			if err != nil {
				return fmt.Errorf("reading ranges: %w", err)
			}

			for j, data := range results {
				ci := batch[j]
				// Verify SHA
				computedSHA := bupdate.BlobSHA(data)
				if computedSHA != ci.ent.SHA {
					return fmt.Errorf("checksum mismatch at offset %d", ci.remoteOffset)
				}
				remoteData[ci.outputIdx] = data
			}
		}
	}

	// Write all chunks in order
	for i, ci := range chunks {
		chunkSize := int64(ci.ent.Size)

		// Zero block - leave a hole
		if ci.ent.Size == bupdate.BLOB_MAX && ci.ent.SHA == bupdate.ZeroBlockSHA {
			if _, err := outf.Seek(chunkSize, io.SeekCurrent); err != nil {
				return fmt.Errorf("seeking past zero block: %w", err)
			}
			continue
		}

		var data []byte

		if ci.localMapping != nil {
			// Read from local
			data, err = bupdate.ReadChunk(ci.localMapping.Filename, ci.localMapping.Offset, int64(ci.localMapping.Size))
			if err != nil {
				// Fall back to remote
				if rd, ok := remoteData[i]; ok {
					data = rd
				} else {
					return fmt.Errorf("reading local chunk: %w", err)
				}
			} else {
				// Verify SHA
				computedSHA := bupdate.BlobSHA(data)
				if computedSHA != ci.ent.SHA {
					if rd, ok := remoteData[i]; ok {
						data = rd
					} else {
						return fmt.Errorf("local chunk checksum mismatch")
					}
				}
			}
		} else {
			// Get from remote
			var ok bool
			data, ok = remoteData[i]
			if !ok {
				return fmt.Errorf("remote chunk not available for chunk %d", i)
			}
		}

		if _, err := outf.Write(data); err != nil {
			return fmt.Errorf("writing chunk: %w", err)
		}
	}

	return nil
}

func cmdCreate(args []string) {
	opts := getopt.New()
	opts.SetProgram("ts create")
	opts.SetParameters("<workspace-name> <snapshot-id>")
	// Parse expects first element to be program name (like os.Args)
	opts.Parse(append([]string{"ts create"}, args...))

	if opts.NArgs() != 2 {
		fmt.Fprintln(os.Stderr, "error: create requires exactly two arguments: workspace-name and snapshot-id")
		fmt.Fprintln(os.Stderr, "usage: ts create <workspace-name> <snapshot-id>")
		os.Exit(1)
	}

	workspaceName := opts.Arg(0)
	snapshotID := opts.Arg(1)

	if err := doCreate(*sockPath, workspaceName, snapshotID); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// CreateRequest is the request body for /create
type CreateRequest struct {
	TailscaleUser string `json:"tailscale_user"`
	WorkspaceName string `json:"workspace_name"`
	SnapshotID    string `json:"snapshot_id"`
}

// CreateResponse is the response from /create
type CreateResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	Path    string `json:"path,omitempty"`
}

func doCreate(sockPath, workspaceName, snapshotID string) error {
	// Get the tailscale user from the environment
	tailscaleUser := os.Getenv("TAILSCALE_USER")
	if tailscaleUser == "" {
		return fmt.Errorf("TAILSCALE_USER environment variable not set")
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialThunder(ctx, sockPath)
			},
		},
	}

	req := CreateRequest{
		TailscaleUser: tailscaleUser,
		WorkspaceName: workspaceName,
		SnapshotID:    snapshotID,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	resp, err := client.Post("http://localhost/create", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	var result CreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	if result.Status != "ok" {
		return fmt.Errorf("%s", result.Message)
	}

	fmt.Printf("Created workspace at %s\n", result.Path)
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

	peers, err := doWhoHas(*sockPath, snapshotID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if len(peers) == 0 {
		fmt.Printf("No peers have snapshot %s\n", snapshotID)
		os.Exit(1)
	}

	fmt.Printf("Peers with snapshot %s:\n", snapshotID)
	for _, peer := range peers {
		fmt.Printf("  %s (%s)\n", peer.Hostname, peer.PeerURL)
	}
}

func doWhoHas(sockPath, snapshotID string) ([]bupdate.PeerResult, error) {
	// Get list of servers from thundersnapd
	servers, err := getServers(sockPath)
	if err != nil {
		return nil, fmt.Errorf("getting servers: %w", err)
	}

	if len(servers) == 0 {
		return nil, fmt.Errorf("no mesh peers available")
	}

	// Convert meshPeer to bupdate.PeerInfo
	peers := make([]bupdate.PeerInfo, len(servers))
	for i, s := range servers {
		peers[i] = bupdate.PeerInfo{
			URL:      s.URL,
			Hostname: s.Hostname,
		}
	}

	// Query all peers in parallel
	results := bupdate.CheckPeersForSnapshot(peers, snapshotID)

	// Filter to only peers that have the snapshot
	var hasSnap []bupdate.PeerResult
	for _, r := range results {
		if r.HasSnap {
			hasSnap = append(hasSnap, r)
		}
	}

	// Sort by hostname for determinism
	sort.Slice(hasSnap, func(i, j int) bool {
		return hasSnap[i].Hostname < hasSnap[j].Hostname
	})

	return hasSnap, nil
}
