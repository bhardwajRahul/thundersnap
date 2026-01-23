// Pipeline implementation for bupdate to maximize HTTP pipelining efficiency.
// Uses bounded channels to flow chunks through: files -> HTTP requests -> writes.
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/tailscale/thundersnap/bupdate"
)

func pipeDebugf(format string, args ...interface{}) {
	if *verbose {
		fmt.Fprintf(os.Stderr, "[PIPE] "+format+"\n", args...)
	}
}

const (
	// Maximum number of HTTP requests to buffer before the fetcher processes them.
	maxInflightRequests = 256

	// Number of requests to batch together in a single HTTP pipeline.
	// Larger batches improve throughput by reducing round-trip overhead.
	httpBatchSize = 64
)

// chunkRequest represents a chunk that needs to be fetched from the remote server.
type chunkRequest struct {
	file       *fileWork
	chunkIdx   int
	ent        bupdate.FidxEntry
	httpPath   string
	httpOffset int64
}

// fileWork tracks the state of a single file being reconstructed.
type fileWork struct {
	outputPath  string
	tmpPath     string
	fidx        *bupdate.Fidx
	fileEntry   bupdate.FileEntry
	chunks      []chunkInfo
	results     chan chunkFetchResult // receives fetched chunks for this file
	localChunks int                   // count of chunks from local sources
	outf        *os.File
	written     int64
	err         error
	done        chan struct{}
}

// chunkFetchResult is sent back to a file's result channel.
type chunkFetchResult struct {
	chunkIdx int
	data     []byte
	err      error
}

// pipeline coordinates the parallel fetching and writing of chunks across files.
type pipeline struct {
	remote   *remoteSource
	mappings *bupdate.FidxMappings
	prog     *progress

	// Channel for HTTP requests
	requests chan chunkRequest

	// Stats
	totalDownloaded atomic.Int64

	// Tracking
	fetcherWg sync.WaitGroup
}

// newPipeline creates a new pipeline for fetching chunks.
func newPipeline(remote *remoteSource, mappings *bupdate.FidxMappings, prog *progress) *pipeline {
	return &pipeline{
		remote:   remote,
		mappings: mappings,
		prog:     prog,
		requests: make(chan chunkRequest, maxInflightRequests),
	}
}

// start begins the pipeline workers.
func (p *pipeline) start() {
	p.fetcherWg.Add(1)
	go p.httpFetcher()
}

// stop signals no more requests and waits for the fetcher to finish.
func (p *pipeline) stop() {
	close(p.requests)
	p.fetcherWg.Wait()
}

// httpFetcher reads from requests channel and sends pipelined HTTP requests.
// It groups requests by path and sends them in batches for efficiency.
// Requests for different paths are sent in separate batches to maintain HTTP correctness.
func (p *pipeline) httpFetcher() {
	defer p.fetcherWg.Done()

	const batchSize = httpBatchSize
	pipeDebugf("httpFetcher started, batchSize=%d", batchSize)

	// Group requests by path for batching
	type pathBatch struct {
		path     string
		requests []chunkRequest
	}
	var batches []pathBatch

	// Find or create batch for a path
	getBatch := func(path string) *pathBatch {
		for i := range batches {
			if batches[i].path == path {
				return &batches[i]
			}
		}
		batches = append(batches, pathBatch{path: path})
		return &batches[len(batches)-1]
	}

	// Flush a single path batch
	flushBatch := func(batch *pathBatch) {
		if len(batch.requests) == 0 {
			return
		}

		pipeDebugf("flushBatch: path=%s requests=%d", batch.path, len(batch.requests))

		// Build range requests
		rangeReqs := make([]bupdate.RangeRequest, len(batch.requests))
		for i, req := range batch.requests {
			rangeReqs[i] = bupdate.RangeRequest{
				Offset: req.httpOffset,
				Size:   int64(req.ent.Size),
			}
		}

		// Send pipelined requests
		pipeDebugf("  Sending %d HTTP range requests", len(rangeReqs))
		results, err := p.remote.httpReader.ReadRangesFromPath(batch.path, rangeReqs)
		if err != nil {
			pipeDebugf("  HTTP error: %v", err)
			for _, req := range batch.requests {
				req.file.results <- chunkFetchResult{
					chunkIdx: req.chunkIdx,
					err:      err,
				}
			}
			batch.requests = batch.requests[:0]
			return
		}

		pipeDebugf("  Received %d results, sending to file writers", len(results))
		// Send successful results
		for i, req := range batch.requests {
			data := results[i]

			// Verify SHA
			computedSHA := bupdate.BlobSHA(data)
			if !bytes.Equal(computedSHA[:], req.ent.SHA[:]) {
				req.file.results <- chunkFetchResult{
					chunkIdx: req.chunkIdx,
					err:      fmt.Errorf("checksum mismatch for chunk at offset %d", req.httpOffset),
				}
				continue
			}

			p.totalDownloaded.Add(int64(len(data)))
			req.file.results <- chunkFetchResult{
				chunkIdx: req.chunkIdx,
				data:     data,
			}
		}

		batch.requests = batch.requests[:0]
	}

	// Flush all batches that have reached batchSize
	flushFullBatches := func() {
		for i := range batches {
			if len(batches[i].requests) >= batchSize {
				pipeDebugf("flushFullBatches: batch %s has %d requests (>= %d), flushing", batches[i].path, len(batches[i].requests), batchSize)
				flushBatch(&batches[i])
			}
		}
	}

	// Flush all batches regardless of size
	flushAll := func() {
		pipeDebugf("flushAll: flushing %d batches", len(batches))
		for i := range batches {
			flushBatch(&batches[i])
		}
	}

	for {
		// Use a select with a default case to check for pending requests
		// This allows us to flush incomplete batches when the channel is temporarily empty
		select {
		case req, ok := <-p.requests:
			if !ok {
				// Channel closed, flush remaining
				pipeDebugf("httpFetcher: channel closed, flushing remaining batches")
				flushAll()
				pipeDebugf("httpFetcher: done")
				return
			}
			pipeDebugf("httpFetcher: received request for chunk %d of %s", req.chunkIdx, req.httpPath)
			batch := getBatch(req.httpPath)
			batch.requests = append(batch.requests, req)

			// Flush any batches that are full
			flushFullBatches()

		default:
			// No requests immediately available - flush any pending batches
			// This prevents deadlock when file writers are waiting for incomplete batches
			hasPending := false
			for i := range batches {
				if len(batches[i].requests) > 0 {
					hasPending = true
					break
				}
			}
			if hasPending {
				pipeDebugf("httpFetcher: no requests pending, flushing incomplete batches")
				flushAll()
			}

			// Now block waiting for the next request (or channel close)
			req, ok := <-p.requests
			if !ok {
				pipeDebugf("httpFetcher: channel closed")
				pipeDebugf("httpFetcher: done")
				return
			}
			pipeDebugf("httpFetcher: received request for chunk %d of %s", req.chunkIdx, req.httpPath)
			batch := getBatch(req.httpPath)
			batch.requests = append(batch.requests, req)

			// Flush any batches that are full
			flushFullBatches()
		}
	}
}

// processFile queues a file for processing through the pipeline.
// It submits HTTP requests for remote chunks and starts a writer goroutine.
// Returns a fileWork that can be waited on.
func (p *pipeline) processFile(
	outputPath, tmpPath string,
	fidx *bupdate.Fidx,
	fileEntry bupdate.FileEntry,
	remoteFilePath string,
) (*fileWork, error) {
	pipeDebugf("processFile: %s (size=%d, entries=%d)", fileEntry.Filename, fidx.FileSize, len(fidx.Entries))

	// Create output file
	outf, err := os.Create(tmpPath)
	if err != nil {
		return nil, err
	}

	// Pre-allocate
	if err := outf.Truncate(fidx.FileSize); err != nil {
		outf.Close()
		return nil, fmt.Errorf("truncating file: %w", err)
	}

	fw := &fileWork{
		outputPath: outputPath,
		tmpPath:    tmpPath,
		fidx:       fidx,
		fileEntry:  fileEntry,
		outf:       outf,
		done:       make(chan struct{}),
	}

	// Build chunk list
	httpPath := p.remote.httpPath(remoteFilePath)
	var remoteOffset int64
	remoteCount := 0
	zeroBlocks := 0

	for i, ent := range fidx.Entries {
		mapping := p.mappings.FindMapping(ent.SHA)
		ci := chunkInfo{
			ent:          ent,
			localMapping: mapping,
			remoteOffset: remoteOffset,
			outputIdx:    i,
		}
		fw.chunks = append(fw.chunks, ci)
		remoteOffset += int64(ent.Size)

		if mapping == nil {
			// Not a zero block?
			if !(ent.Size == bupdate.BLOB_MAX && ent.SHA == bupdate.ZeroBlockSHA) {
				remoteCount++
			} else {
				zeroBlocks++
			}
		} else {
			fw.localChunks++
		}
	}

	pipeDebugf("  chunks: total=%d local=%d remote=%d zeroBlocks=%d", len(fw.chunks), fw.localChunks, remoteCount, zeroBlocks)

	// Create results channel sized for expected remote chunks
	fw.results = make(chan chunkFetchResult, remoteCount+1)

	// Queue HTTP requests for remote chunks
	pipeDebugf("  Queuing %d HTTP requests for remote chunks", remoteCount)
	for i, ci := range fw.chunks {
		if ci.localMapping == nil {
			if ci.ent.Size == bupdate.BLOB_MAX && ci.ent.SHA == bupdate.ZeroBlockSHA {
				continue
			}
			pipeDebugf("    Queuing chunk %d (offset=%d, size=%d)", i, ci.remoteOffset, ci.ent.Size)
			p.requests <- chunkRequest{
				file:       fw,
				chunkIdx:   i,
				ent:        ci.ent,
				httpPath:   httpPath,
				httpOffset: ci.remoteOffset,
			}
		}
	}

	// Start writer goroutine
	pipeDebugf("  Starting fileWriter goroutine, expecting %d remote results", remoteCount)
	go p.fileWriter(fw, remoteCount)

	return fw, nil
}

// fileWriter handles writing chunks to a file as they arrive.
func (p *pipeline) fileWriter(fw *fileWork, expectedRemote int) {
	defer close(fw.done)
	defer fw.outf.Close()

	pipeDebugf("fileWriter[%s]: started, expecting %d remote chunks", fw.fileEntry.Filename, expectedRemote)

	// Collect all remote results first (they arrive out of order)
	remoteData := make(map[int][]byte)
	for i := 0; i < expectedRemote; i++ {
		pipeDebugf("fileWriter[%s]: waiting for result %d/%d", fw.fileEntry.Filename, i+1, expectedRemote)
		result := <-fw.results
		if result.err != nil {
			pipeDebugf("fileWriter[%s]: error receiving chunk: %v", fw.fileEntry.Filename, result.err)
			fw.err = result.err
			// Drain remaining results to unblock sender
			for j := i + 1; j < expectedRemote; j++ {
				<-fw.results
			}
			return
		}
		pipeDebugf("fileWriter[%s]: received chunk %d (%d bytes)", fw.fileEntry.Filename, result.chunkIdx, len(result.data))
		remoteData[result.chunkIdx] = result.data
	}
	pipeDebugf("fileWriter[%s]: all remote chunks received, writing file", fw.fileEntry.Filename)

	// Write chunks in order
	for i, ci := range fw.chunks {
		chunkSize := int64(ci.ent.Size)

		// Zero block - leave a hole
		if ci.ent.Size == bupdate.BLOB_MAX && ci.ent.SHA == bupdate.ZeroBlockSHA {
			if _, err := fw.outf.Seek(chunkSize, io.SeekCurrent); err != nil {
				fw.err = fmt.Errorf("seeking past zero block: %w", err)
				return
			}
			fw.written += chunkSize
			p.prog.status("", fw.written, fw.fidx.FileSize, fw.fileEntry.Filename)
			continue
		}

		var data []byte

		if ci.localMapping != nil {
			// Read from local file
			var err error
			data, err = bupdate.ReadChunk(ci.localMapping.Filename, ci.localMapping.Offset, int64(ci.localMapping.Size))
			if err != nil {
				fw.err = fmt.Errorf("reading local chunk: %w", err)
				return
			}

			// Verify SHA
			computedSHA := bupdate.BlobSHA(data)
			if !bytes.Equal(computedSHA[:], ci.ent.SHA[:]) {
				// Try remote fallback if available
				if rd, ok := remoteData[i]; ok {
					data = rd
				} else {
					fw.err = fmt.Errorf("local chunk checksum mismatch")
					return
				}
			}
		} else {
			data = remoteData[i]
		}

		if _, err := fw.outf.Write(data); err != nil {
			fw.err = fmt.Errorf("writing chunk: %w", err)
			return
		}
		fw.written += int64(len(data))
		p.prog.status("", fw.written, fw.fidx.FileSize, fw.fileEntry.Filename)
	}
	pipeDebugf("fileWriter[%s]: done, wrote %d bytes", fw.fileEntry.Filename, fw.written)
}

// wait waits for the file to complete and returns any error.
func (fw *fileWork) wait() error {
	pipeDebugf("wait[%s]: waiting for file to complete", fw.fileEntry.Filename)
	<-fw.done
	pipeDebugf("wait[%s]: file completed, err=%v", fw.fileEntry.Filename, fw.err)
	return fw.err
}
