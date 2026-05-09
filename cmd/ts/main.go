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
	"syscall"
	"time"

	"github.com/mdlayher/vsock"
	"github.com/pborman/getopt/v2"
	"github.com/tailscale/thundersnap/bupdate"
	"golang.org/x/sys/unix"
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
	fmt.Fprintln(os.Stderr, "  ping           send a ping to thundersnapd")
	fmt.Fprintln(os.Stderr, "  bupdate        download and reconstruct files from mesh peers")
	fmt.Fprintln(os.Stderr, "  fidx           create a file index (.fidx) for a file or directory")
	fmt.Fprintln(os.Stderr, "  snap           create a snapshot of the current container/VM")
	fmt.Fprintln(os.Stderr, "  create         create a new frame from a snapshot")
	fmt.Fprintln(os.Stderr, "  taint          add a taint to the current frame")
	fmt.Fprintln(os.Stderr, "  download-docker download a Docker image as a snapshot")
	fmt.Fprintln(os.Stderr, "  who-has        query peers to find which ones have a snapshot")
	fmt.Fprintln(os.Stderr, "  download-snap  download a snapshot from mesh peers")
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
	opts.SetParameters("<frame-name> <snapshot-spec>")
	isolation := opts.StringLong("isolation", 0, "", "isolation level: vm, container, none")
	// Parse expects first element to be program name (like os.Args)
	opts.Parse(append([]string{"ts create"}, args...))

	if opts.NArgs() != 2 {
		fmt.Fprintln(os.Stderr, "error: create requires exactly two arguments")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "usage: ts create [--isolation=<level>] <frame-name> <snapshot-spec>")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "snapshot-spec can be:")
		fmt.Fprintln(os.Stderr, "  <snapshot-id>                    single snapshot (legacy)")
		fmt.Fprintln(os.Stderr, "  <rootfs>:<home>:<work>           frame with three components")
		fmt.Fprintln(os.Stderr, "  <rootfs>::                       frame with empty home/work")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "examples:")
		fmt.Fprintln(os.Stderr, "  ts create dev abc123             single snapshot")
		fmt.Fprintln(os.Stderr, "  ts create dev abc123::           rootfs only, empty home/work")
		fmt.Fprintln(os.Stderr, "  ts create dev abc123:def456:     rootfs + home, empty work")
		os.Exit(1)
	}

	frameName := opts.Arg(0)
	snapshotSpec := opts.Arg(1)

	if err := doCreate(*sockPath, frameName, snapshotSpec, *isolation); err != nil {
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

func doWhoHas(sockPath, snapshotID string) ([]bupdate.PeerResult, error) {
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

	// Convert to bupdate.PeerResult for compatibility with existing code
	var peers []bupdate.PeerResult
	for _, p := range result.Peers {
		peers = append(peers, bupdate.PeerResult{
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
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to make mounts private: %v\n", err)
		os.Exit(1)
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
