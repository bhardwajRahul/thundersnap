// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// snapqueue.go implements background, serialized snap indexing.
//
// ts snap --quick / ts go :: capture a btrfs snapshot (instant, COW) and
// return at once; the slow part — walking the tree and hashing every file to derive the
// content-addressable snap ID — happens in a single daemon-wide background
// worker that drains captured snaps in FIFO order. Serializing indexing gives
// two properties the old synchronous code lacked:
//
//  1. No unbounded parallelism: concurrent ts snap calls capture quickly (the
//     brief btrfs snapshot is serialized under snapQueue.mu) and then queue
//     for indexing; the worker indexes them one at a time.
//  2. Rapid double-ts-snap chains incrementally: each snap records its base
//     (the previous snap of that frame). When the worker reaches snap N, snap
//     N-1 is already finalized, so snap N reuses N-1's chunk hashes for
//     unchanged files instead of re-reading/re-hashing them.
//
// See background-indexing.md for the full design.
package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tailscale/thundersnap/btrfsutil"
	"github.com/tailscale/thundersnap/frameid"
	"github.com/tailscale/thundersnap/frames"
	"github.com/tailscale/thundersnap/snaphash"
	"github.com/tailscale/thundersnap/snapsubdir"
	"github.com/tailscale/thundersnap/tsm"
)

// componentKind identifies which part of a frame a captured subvolume covers.
type componentKind int

const (
	compRoot componentKind = iota
	compHome
	compWork
	compSubdir // single-component snap of a frame subtree
)

// pendingComponent is one captured-but-unindexed btrfs snapshot plus the
// information needed to index it later.
type pendingComponent struct {
	kind    componentKind
	tmpPath string // snaps/<jobid>-<kind>.tmp (captured read-only subvol)
	taints  []string
	// Base for incremental indexing, resolved at index time. If parentJob is
	// non-nil, the base is that (earlier, by FIFO guaranteed-finalized) job's
	// content ID for this component kind; otherwise parentSnap is a finalized
	// snap ID (or "" for a full-index subdir snap).
	parentJob  *snapJob
	parentSnap string
}

// snapJob is a captured snap awaiting (or undergoing) background indexing.
type snapJob struct {
	frame    string // rootFS path of the frame this snap was captured from
	subdir   string // "" for a full three-component snap, else the subtree
	comps    []*pendingComponent
	progress *threeSnapProgress // non-nil only for streaming --wait requests

	// Results, set by the worker before done is closed.
	finalized map[componentKind]string // kind -> content-addressable snap ID
	result    string                   // "root:home:work" (full) or single ID (subdir)
	err       error
	done      chan struct{}
}

// isSubdir reports whether this is a single-subtree snap (vs a full frame).
func (j *snapJob) isSubdir() bool { return j.subdir != "" }

// resultID returns the snap ID to report to the client: the triplet for a full
// snap, or the single content ID for a subdir snap.
func (j *snapJob) resultID() string {
	if j.isSubdir() {
		return j.finalized[compSubdir]
	}
	return j.result
}

// componentTmp returns the captured tmp path for the given component kind,
// or "" if that component was not captured (e.g. an empty home subvolume).
func (j *snapJob) componentTmp(kind componentKind) string {
	for _, c := range j.comps {
		if c.kind == kind {
			return c.tmpPath
		}
	}
	return ""
}

// snapQueue is the daemon-wide snap indexing queue. It owns the single
// indexing worker and the per-frame "last pending snap" map used to chain
// rapid successive snaps incrementally.
type snapQueue struct {
	mu               sync.Mutex
	cond             *sync.Cond
	pending          []*snapJob
	framePendingSnap map[string]*snapJob // frame rootFS -> most recent not-yet-finalized snap job

	// frameMetaMu serializes read-modify-write of frame sidecar files
	// (.jsonc). The background indexing worker updates a frame's sidecar
	// (Rootfs/Home/Work/History) at finalize time, racing /taint and fork
	// which also read-modify-write the same file. Without this lock a taint
	// added while a snap is finalizing can be silently dropped, or the new
	// snap IDs can be clobbered. Keyed by frame rootFS path.
	frameMetaMu sync.Map // map[string]*sync.Mutex
}

// globalSnapQueue is the singleton snap queue, started in initSnapQueue.
var globalSnapQueue = newSnapQueue()

func newSnapQueue() *snapQueue {
	q := &snapQueue{framePendingSnap: make(map[string]*snapJob)}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// frameMetaLock returns the per-frame mutex used to serialize sidecar
// read-modify-write. Callers must hold the returned mutex while doing the
// read-modify-write and must unlock it once the write is complete.
func (q *snapQueue) frameMetaLock(frame string) sync.Locker {
	v, _ := q.frameMetaMu.LoadOrStore(frame, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// updateFrameMetaLocked performs a sidecar read-modify-write under the
// per-frame sidecar lock, so it doesn't race the snap-indexing worker or
// /taint. mutator may modify the *frames.Frame in place; the result is
// written atomically (temp+rename) by writeFrameSidecar. Returns the frame
// metadata as written.
func (q *snapQueue) updateFrameMetaLocked(frame string, mutator func(*frames.Frame)) (*frames.Frame, error) {
	mu := q.frameMetaLock(frame)
	mu.Lock()
	defer mu.Unlock()

	meta, _ := readFrameSidecar(frame)
	if meta == nil {
		meta = &frames.Frame{}
	}
	mutator(meta)
	if err := writeFrameSidecar(frame, meta); err != nil {
		return nil, err
	}
	return meta, nil
}

// initSnapQueue starts the background indexing worker. Called from main once
// --data-dir is resolved.
func initSnapQueue() {
	go globalSnapQueue.run()
}

// run is the single indexing worker. It processes jobs in FIFO order, which
// guarantees that a job's parentJob (always enqueued earlier for the same
// frame) is finalized before the job is indexed.
func (q *snapQueue) run() {
	for {
		q.mu.Lock()
		for len(q.pending) == 0 {
			q.cond.Wait()
		}
		job := q.pending[0]
		q.pending = q.pending[1:]
		q.mu.Unlock()
		q.processJob(job)
	}
}

func (q *snapQueue) enqueueLocked(job *snapJob) {
	q.pending = append(q.pending, job)
	q.cond.Signal()
}

// captureSubvol does the instant part of a snap: a btrfs snapshot of source
// into a fresh tmp path in snaps-dir. It does NOT index or write the snap
// stamp (the stamp is written at finalize time, alongside the rename, so it
// records the resolved parent). Returns the tmp path.
func captureSubvol(source, subdir string) (string, error) {
	tmpID, err := generateRandomID()
	if err != nil {
		return "", fmt.Errorf("generating temporary ID: %w", err)
	}
	tmpPath := filepath.Join(*flagSnapsDir, tmpID+".tmp")
	if subdir == "" {
		// Read-only snapshot of the whole subvolume (btrfs excludes nested
		// subvolumes like /home and /work automatically).
		if err := btrfsSnapshot(source, tmpPath, true); err != nil {
			return "", err
		}
	} else {
		// Subtree snap: writable snapshot that snapsubdir prunes down to just
		// the requested subtree before indexing.
		if err := snapsubdir.Snapshot(source, subdir, tmpPath); err != nil {
			btrfsutil.DeleteSubvol(tmpPath) // best effort: remove partial subvol
			return "", err
		}
	}
	return tmpPath, nil
}

// indexAndFinalizeSubvol does the slow part of a snap: index the captured tmp
// subvolume (tsm/tsc), compute its SHA-256 content ID, dedup against any
// existing snap with that ID (with taint intersection), and rename everything
// to the final content-addressed names. Returns the content ID.
//
// parentStampID is the resolved parent snap ID (a finalized content ID, or ""
// / "1" for a full-index snap with no usable parent manifest).
func indexAndFinalizeSubvol(tmpPath, subdir, parentStampID string, taints []string, progressCallback func(tsm.IndexerStats)) (string, error) {
	tmpTSMPath := tmpPath + ".tsm"
	tmpTSCPath := tmpPath + ".tsc"

	cleanupTmp := func() {
		btrfsutil.DeleteSubvol(tmpPath) // best effort
		os.Remove(tmpPath + ".stamp")
		os.Remove(tmpTSMPath)
		os.Remove(tmpTSCPath)
	}

	// Write the snap's own stamp (parent reference) alongside the subvol; it
	// is renamed with the subvol at finalize. Written here, not at capture, so
	// it records the parent resolved at index time (which may be a previously
	// pending snap's now-known content ID).
	if err := writeStampFile(tmpPath, parentStampID); err != nil {
		cleanupTmp()
		return "", fmt.Errorf("write stamp file: %w", err)
	}

	// Index. Incremental reuse only applies to a full-root snap (subdir==""),
	// where the parent manifest's paths line up with this tree; a subdir snap
	// re-roots the tree so the parent's paths no longer match.
	tsmOpts := tsm.IndexerOptions{ProgressCallback: progressCallback}
	if subdir == "" {
		if parentTSM, parentTSC := loadParentManifest(parentStampID); parentTSM != nil && parentTSC != nil {
			tsmOpts.ParentTSM = parentTSM
			tsmOpts.ParentTSC = parentTSC
		}
	}
	if err := tsm.Create(tmpPath, tmpPath, tsmOpts); err != nil {
		cleanupTmp()
		return "", fmt.Errorf("create tsm/tsc: %w", err)
	}

	// The TSM's SHA-256 is the content-addressable snap ID.
	tsmReader, err := tsm.ReadTSM(tmpTSMPath)
	if err != nil {
		cleanupTmp()
		return "", fmt.Errorf("read tsm for checksum: %w", err)
	}
	snapshotID := snaphash.Encode(snaphash.Hash(tsmReader.SHA256))

	finalPath := filepath.Join(*flagSnapsDir, snapshotID)
	finalTSMPath := finalPath + ".tsm"
	finalTSCPath := finalPath + ".tsc"

	if taints == nil {
		taints = getSnapTaints(*flagSnapsDir, parentStampID)
	}

	// Dedup: if a snap with this content ID already exists, intersect taints
	// and discard the new capture.
	if _, err := os.Stat(finalPath); err == nil {
		log.Printf("Snapshot %s already exists, checking taints", snapshotID)
		existingMeta, _ := readSnapMeta(*flagSnapsDir, snapshotID)
		if existingMeta != nil && len(taints) > 0 {
			intersected := IntersectTaints(existingMeta.Taints, taints)
			if !taintsEqual(existingMeta.Taints, intersected) {
				existingMeta.Taints = intersected
				if err := writeSnapMeta(*flagSnapsDir, snapshotID, existingMeta); err != nil {
					log.Printf("Warning: failed to update snap meta for taint intersection: %v", err)
				} else {
					log.Printf("Taint intersection for %s: %v", snapshotID, intersected)
				}
			}
		}
		// Report the indexed content as "unmodified" since nothing changed
		// from the content's perspective and we're discarding the duplicate.
		if progressCallback != nil {
			totalEntries := len(tsmReader.Entries)
			var totalBytes int64
			for _, e := range tsmReader.Entries {
				totalBytes += int64(e.Size)
			}
			progressCallback(tsm.IndexerStats{
				UnmodifiedEntries: totalEntries,
				ModifiedEntries:   0,
				ChunkCount:        uint64(len(tsmReader.Entries)),
				TotalBytes:        totalBytes,
			})
		}
		cleanupTmp()
		return snapshotID, nil
	}

	// Rename to final content-addressed names.
	if err := os.Rename(tmpPath, finalPath); err != nil {
		cleanupTmp()
		return "", fmt.Errorf("rename snapshot: %w", err)
	}
	os.Rename(tmpPath+".stamp", finalPath+".stamp")
	if err := os.Rename(tmpTSMPath, finalTSMPath); err != nil {
		log.Printf("Warning: failed to rename tsm: %v", err)
	}
	if err := os.Rename(tmpTSCPath, finalTSCPath); err != nil {
		log.Printf("Warning: failed to rename tsc: %v", err)
	}

	snapMeta := &SnapMeta{Parent: parentStampID, Taints: taints}
	if err := writeSnapMeta(*flagSnapsDir, snapshotID, snapMeta); err != nil {
		log.Printf("Warning: failed to write snap.jsonc for %s: %v", snapshotID, err)
	}
	log.Printf("Created snapshot %s (SHA-256) with tsm/tsc", snapshotID)
	return snapshotID, nil
}

// captureSnapJob captures a frame (or a subtree) synchronously and enqueues
// the job for background indexing. It returns immediately with the job; the
// caller may block on job.done to get the finalized ID (--wait behavior).
//
// progress is non-nil for streaming --wait requests; the worker uses it to
// emit progress and the final result event.
func captureSnapJob(rootFS, subdir string, progress *threeSnapProgress) (*snapJob, error) {
	q := globalSnapQueue
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.captureSnapJobLocked(rootFS, subdir, progress)
}

// captureSnapJobLocked is captureSnapJob assuming snapQueue.mu is held. The
// btrfs snapshots are taken under the lock so captures serialize (the user
// wants snaps fully serialized inside the daemon); indexing happens later in
// the worker without holding the lock.
func (q *snapQueue) captureSnapJobLocked(rootFS, subdir string, progress *threeSnapProgress) (*snapJob, error) {
	if subdir != "" {
		clean, err := snapsubdir.Validate(subdir)
		if err != nil {
			return nil, err
		}
		// Inherit the frame's taints; subdir snaps have no parent (full index).
		frameMeta, _ := readFrameSidecar(rootFS)
		var taints []string
		if frameMeta != nil {
			taints = frameMeta.Taints
		}
		tmpPath, err := captureSubvol(rootFS, clean)
		if err != nil {
			return nil, err
		}
		job := &snapJob{
			frame:    rootFS,
			subdir:   clean,
			comps:    []*pendingComponent{{kind: compSubdir, tmpPath: tmpPath, taints: taints}},
			progress: progress,
			done:     make(chan struct{}),
		}
		// Subdir snaps don't update the frame's stamp/sidecar/pending chain.
		q.enqueueLocked(job)
		return job, nil
	}

	// Full three-component snap.
	homePath := filepath.Join(rootFS, "home")
	workPath := filepath.Join(rootFS, "work")

	frameMeta, _ := readFrameSidecar(rootFS)
	var frameTaints []string
	if frameMeta != nil {
		frameTaints = frameMeta.Taints
	}

	baseStampID := readStampFile(rootFS)
	if baseStampID == "" {
		baseStampID = "1"
	}
	var homeParentSnap, workParentSnap string
	if frameMeta != nil {
		homeParentSnap = frameMeta.Home
		workParentSnap = frameMeta.Work
	}

	// Chain to the most recent not-yet-finalized snap of this frame, if any.
	// At index time (FIFO) it will be finalized, so we can use its content IDs.
	var baseJob *snapJob
	if pj, ok := q.framePendingSnap[rootFS]; ok {
		baseJob = pj
	}

	var comps []*pendingComponent

	// Root.
	rootTmp, err := captureSubvol(rootFS, "")
	if err != nil {
		return nil, err
	}
	comps = append(comps, &pendingComponent{
		kind:       compRoot,
		tmpPath:    rootTmp,
		taints:     frameTaints,
		parentJob:  baseJob,
		parentSnap: baseStampID,
	})

	// Home.
	if !isSubvolume(homePath) {
		cleanupCaptured(comps)
		return nil, fmt.Errorf("home is not a subvolume: %s", homePath)
	}
	if !isDirEmpty(homePath) {
		htmp, err := captureSubvol(homePath, "")
		if err != nil {
			cleanupCaptured(comps)
			return nil, err
		}
		comps = append(comps, &pendingComponent{
			kind:       compHome,
			tmpPath:    htmp,
			taints:     frameTaints,
			parentJob:  baseJob,
			parentSnap: homeParentSnap,
		})
	}

	// Work.
	if !isSubvolume(workPath) {
		cleanupCaptured(comps)
		return nil, fmt.Errorf("work is not a subvolume: %s", workPath)
	}
	if !isDirEmpty(workPath) {
		wtmp, err := captureSubvol(workPath, "")
		if err != nil {
			cleanupCaptured(comps)
			return nil, err
		}
		comps = append(comps, &pendingComponent{
			kind:       compWork,
			tmpPath:    wtmp,
			taints:     frameTaints,
			parentJob:  baseJob,
			parentSnap: workParentSnap,
		})
	}

	job := &snapJob{
		frame:    rootFS,
		comps:    comps,
		progress: progress,
		done:     make(chan struct{}),
	}
	q.framePendingSnap[rootFS] = job
	q.enqueueLocked(job)
	return job, nil
}

// cleanupCaptured deletes captured tmp subvolumes (on partial-capture
// failure) so we don't leak them.
func cleanupCaptured(comps []*pendingComponent) {
	for _, c := range comps {
		btrfsutil.DeleteSubvol(c.tmpPath)
		os.Remove(c.tmpPath + ".stamp")
	}
}

// processJob indexes and finalizes a single captured snap, then updates the
// frame's stamp/sidecar/history. Runs on the worker goroutine.
func (q *snapQueue) processJob(job *snapJob) {
	finalized := make(map[componentKind]string, len(job.comps))
	var firstErr error

	for _, comp := range job.comps {
		// Resolve the parent: prefer the chained pending job's now-finalized
		// content ID (FIFO guarantees it's done); fall back to the finalized
		// snap ID recorded at capture time.
		parent := comp.parentSnap
		if comp.parentJob != nil {
			if pid := comp.parentJob.finalized[comp.kind]; pid != "" {
				parent = pid
			}
		}

		var cb func(tsm.IndexerStats)
		if job.progress != nil {
			switch comp.kind {
			case compHome:
				cb = job.progress.HomeCallback()
			case compWork:
				cb = job.progress.WorkCallback()
			default: // compRoot, compSubdir
				cb = job.progress.RootCallback()
			}
		}

		subdir := ""
		if job.isSubdir() {
			subdir = job.subdir
		}
		id, err := indexAndFinalizeSubvol(comp.tmpPath, subdir, parent, comp.taints, cb)
		if err != nil {
			firstErr = err
			break
		}
		finalized[comp.kind] = id
	}

	if firstErr != nil {
		// Clean up any components we didn't get to index.
		for _, comp := range job.comps {
			if _, ok := finalized[comp.kind]; !ok {
				btrfsutil.DeleteSubvol(comp.tmpPath)
				os.Remove(comp.tmpPath + ".stamp")
			}
		}
		job.err = firstErr
		q.finishJob(job, false)
		log.Printf("background snap failed for %s: %v", job.frame, firstErr)
		return
	}

	job.finalized = finalized

	// Build the history entry once; it is prepended to the source frame's
	// history below and to any forked frame's history after that.
	var historyEntry frames.HistoryEntry
	if job.isSubdir() {
		historyEntry = frames.HistoryEntry{Snap: finalized[compSubdir], Time: time.Now()}
	} else {
		rootID := finalized[compRoot]
		homeID := finalized[compHome]
		workID := finalized[compWork]
		homeStr, workStr := homeID, workID
		if homeStr == "" {
			homeStr = "nil"
		}
		if workStr == "" {
			workStr = "nil"
		}
		triplet := fmt.Sprintf("%s:%s:%s", rootID, homeStr, workStr)
		job.result = triplet
		historyEntry = frames.HistoryEntry{Snap: triplet, Time: time.Now()}
	}

	// Update the frame's stamp/sidecar/history with the finalized IDs. The
	// sidecar RMW is serialized against /taint and fork via the per-frame
	// sidecar lock (updateFrameMetaLocked) so a concurrent taint add isn't
	// silently dropped and the new snap IDs aren't clobbered.
	if job.isSubdir() {
		sid := finalized[compSubdir]
		if _, err := q.updateFrameMetaLocked(job.frame, func(m *frames.Frame) {
			m.History = append([]frames.HistoryEntry{historyEntry}, m.History...)
		}); err != nil {
			log.Printf("Warning: failed to update frame sidecar for %s: %v", job.frame, err)
		}
		log.Printf("Background snap (subdir) finalized: %s", sid)
	} else {
		rootID := finalized[compRoot]
		homeID := finalized[compHome]
		workID := finalized[compWork]

		if err := writeStampFile(job.frame, rootID); err != nil {
			log.Printf("Warning: failed to update stamp file for %s: %v", job.frame, err)
		}
		if _, err := q.updateFrameMetaLocked(job.frame, func(m *frames.Frame) {
			m.Rootfs = rootID
			if homeID != "" {
				m.Home = homeID
			}
			if workID != "" {
				m.Work = workID
			}
			m.History = append([]frames.HistoryEntry{historyEntry}, m.History...)
		}); err != nil {
			log.Printf("Warning: failed to update frame sidecar for %s: %v", job.frame, err)
		}
		log.Printf("Background snap finalized: %s", job.result)

		// A fork (forkFrame) registers this same job as the pending snap for
		// both the source frame (job.frame, updated above) and the new forked
		// frame. The forked frame's sidecar/stamp were cloned from the
		// source's *pre-fork* metadata, so without the update below its
		// history would omit the fork-point snap (ts undo in the new frame
		// would roll back past the fork point) and its stamp/Rootfs/Home/Work
		// would stay stale — so the forked frame's next incremental snap, once
		// finishJob clears the pending entry, would chain against the wrong
		// parent. Bring the forked frame's metadata up to the fork-point snap
		// now, under the same per-frame sidecar lock discipline. Forks are
		// always full snaps (forkFrame captures a full snap), so this only
		// runs in the full-snap branch.
		for _, ff := range q.forkedFramesForJob(job) {
			if err := writeStampFile(ff, rootID); err != nil {
				log.Printf("Warning: failed to update stamp file for forked frame %s: %v", ff, err)
			}
			if _, err := q.updateFrameMetaLocked(ff, func(m *frames.Frame) {
				m.Rootfs = rootID
				if homeID != "" {
					m.Home = homeID
				}
				if workID != "" {
					m.Work = workID
				}
				m.History = append([]frames.HistoryEntry{historyEntry}, m.History...)
			}); err != nil {
				log.Printf("Warning: failed to update sidecar for forked frame %s: %v", ff, err)
			}
			log.Printf("Forked frame %s metadata updated to fork-point snap %s", filepath.Base(ff), job.result)
		}
	}

	q.finishJob(job, true)
}

// forkedFramesForJob returns the frames (other than the snap's own source
// frame) whose pending snap is this job — i.e. the frames forked from the
// source while this snap was still indexing. Must be called from the worker
// (processJob), which is the only goroutine that calls finishJob for this job,
// so the returned set is stable across the finalize that follows.
func (q *snapQueue) forkedFramesForJob(job *snapJob) []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []string
	for fr, j := range q.framePendingSnap {
		if j == job && fr != job.frame {
			out = append(out, fr)
		}
	}
	return out
}

// finishJob emits the final result event (for streaming --wait), clears any
// per-frame pending entries that still point at this job (a fork wires the
// same job as the pending snap for both the source frame and the new forked
// frame), and signals done.
func (q *snapQueue) finishJob(job *snapJob, ok bool) {
	if job.progress != nil {
		if ok {
			job.progress.Final()
			_ = job.progress.emitter.emit(SnapStreamEvent{Type: "result", Status: "ok", SnapshotID: job.resultID()})
		} else {
			_ = job.progress.emitter.emit(SnapStreamEvent{Type: "result", Status: "error", Message: job.err.Error()})
		}
	}
	q.mu.Lock()
	for frame, j := range q.framePendingSnap {
		if j == job {
			delete(q.framePendingSnap, frame)
		}
	}
	q.mu.Unlock()
	close(job.done)
}

// forkFrame captures the current frame as a background snap and creates a new
// frame that is a clone of the current frame's live filesystem. The new frame
// is wired so its first ts snap chains against the captured job (indexing
// incrementally against the fork-point snap once it finalizes). Returns the
// new frame's UUID.
func forkFrame(rootFS string) (frameid.ID, error) {
	q := globalSnapQueue

	user, err := namespaceFromRootFS(rootFS)
	if err != nil {
		return frameid.Nil, fmt.Errorf("fork: %w", err)
	}

	srcMeta, _ := readFrameSidecar(rootFS)

	uuid, err := frameid.New()
	if err != nil {
		return frameid.Nil, fmt.Errorf("generate frame UUID: %w", err)
	}
	framePath := framePathForNamespaceUUID(user, uuid)

	// Build the new frame's sidecar from the source frame's metadata (same
	// base snaps, taints, isolation). History is cloned too. The new frame's
	// first snap will chain against the just-captured job, so the sidecar's
	// (stale) snap IDs are only a fallback.
	meta := &frames.Frame{}
	if srcMeta != nil {
		*meta = *srcMeta
		meta.History = append([]frames.HistoryEntry(nil), srcMeta.History...)
	}

	// Capture the current frame as a background snap (so the current state is
	// recorded for history/sharing) AND snapshot the captured tmp subvolumes
	// into the new frame, all under the queue lock. Holding the lock across
	// only the btrfs snapshots is load-bearing: it blocks the indexing worker
	// from popping the just-enqueued job and renaming its tmp subvolumes to
	// their final content-addressed names before we've snapshotted them into
	// the new frame. (The new frame's subvols are independent COW snapshots,
	// so the later rename doesn't affect them, but the snapshot must observe
	// the tmp paths before they move.) The rest of frame setup (subvol
	// creation for empties, symlink, /id, sidecar/stamp, finalizeFrameRootfs)
	// is done unlocked so one fork doesn't stall every other frame's snaps.
	q.mu.Lock()
	job, err := q.captureSnapJobLocked(rootFS, "", nil)
	if err != nil {
		q.mu.Unlock()
		return frameid.Nil, fmt.Errorf("fork: capture current frame: %w", err)
	}
	if err := cloneFrameSubvolsLocked(framePath, job); err != nil {
		q.mu.Unlock()
		return frameid.Nil, fmt.Errorf("clone frame subvols: %w", err)
	}
	// Wire the new frame's first snap to chain against the captured job.
	q.framePendingSnap[framePath] = job
	q.mu.Unlock()

	if err := cloneFrameSetup(framePath, job, meta); err != nil {
		return frameid.Nil, fmt.Errorf("clone frame: %w", err)
	}
	if err := copyTsBinary(framePath); err != nil {
		log.Printf("Warning: failed to copy ts binary to %s: %v", framePath, err)
	}

	log.Printf("Forked frame %s from %s (background snap pending)", framePath, rootFS)
	return uuid, nil
}

// cloneFrameSubvolsLocked snapshots the captured job's root/home/work tmp
// subvolumes into the new frame path. Must run under snapQueue.mu (see
// forkFrame) so the worker can't rename the tmp sources away mid-clone.
func cloneFrameSubvolsLocked(framePath string, job *snapJob) error {
	parentDir := filepath.Dir(framePath)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		return fmt.Errorf("creating parent directory: %w", err)
	}

	rootTmp := job.componentTmp(compRoot)
	if rootTmp == "" {
		return fmt.Errorf("no root component in captured job")
	}
	if err := btrfsSnapshot(rootTmp, framePath, false); err != nil {
		return fmt.Errorf("clone rootfs: %w", err)
	}

	// Home/work: the cloned rootfs subvol may already contain a /home or /work
	// directory (not a subvol) from the source rootfs snap; btrfs subvolume
	// snapshot refuses to overwrite it, so remove the stale directory first,
	// then snapshot the captured component into place. If the job has no such
	// component (empty source /home or /work), leave the directory in place —
	// cloneFrameSetup will replace it with an empty subvol.
	homePath := filepath.Join(framePath, "home")
	if homeTmp := job.componentTmp(compHome); homeTmp != "" {
		if fi, err := os.Stat(homePath); err == nil && fi.IsDir() && !isSubvolume(homePath) {
			os.RemoveAll(homePath)
		}
		if err := btrfsSnapshot(homeTmp, homePath, false); err != nil {
			return fmt.Errorf("clone home: %w", err)
		}
	}
	workPath := filepath.Join(framePath, "work")
	if workTmp := job.componentTmp(compWork); workTmp != "" {
		if fi, err := os.Stat(workPath); err == nil && fi.IsDir() && !isSubvolume(workPath) {
			os.RemoveAll(workPath)
		}
		if err := btrfsSnapshot(workTmp, workPath, false); err != nil {
			return fmt.Errorf("clone work: %w", err)
		}
	}
	return nil
}

// cloneFrameSetup finishes assembling a new forked frame: creates empty
// home/work subvols for any component the captured job lacked, adds the
// /home/work symlink and /id subvol, writes the sidecar/stamp, and runs the
// standard frame rootfs finalization. Runs unlocked (see forkFrame).
func cloneFrameSetup(framePath string, job *snapJob, meta *frames.Frame) error {
	// Home: if the job had no home component (empty source /home), create an
	// empty subvol now (the locked phase only cloned present components).
	homePath := filepath.Join(framePath, "home")
	if fi, err := os.Stat(homePath); err == nil && fi.IsDir() && !isSubvolume(homePath) {
		os.RemoveAll(homePath)
	}
	if !isSubvolume(homePath) {
		if err := btrfsCreateSubvol(homePath); err != nil {
			return fmt.Errorf("create home subvol: %w", err)
		}
		if err := os.Chown(homePath, tsm.ThundersnapUID, tsm.ThundersnapGID); err != nil {
			log.Printf("Warning: failed to chown home subvolume: %v", err)
		}
	}

	// Work: same as home.
	workPath := filepath.Join(framePath, "work")
	if fi, err := os.Stat(workPath); err == nil && fi.IsDir() && !isSubvolume(workPath) {
		os.RemoveAll(workPath)
	}
	if !isSubvolume(workPath) {
		if err := btrfsCreateSubvol(workPath); err != nil {
			return fmt.Errorf("create work subvol: %w", err)
		}
		if err := os.Chown(workPath, tsm.ThundersnapUID, tsm.ThundersnapGID); err != nil {
			log.Printf("Warning: failed to chown work subvolume: %v", err)
		}
	}

	// /home/work convenience symlink.
	homeWorkPath := filepath.Join(homePath, "work")
	if _, err := os.Lstat(homeWorkPath); errors.Is(err, os.ErrNotExist) {
		if err := os.Symlink("/work", homeWorkPath); err != nil {
			log.Printf("Warning: failed to create /home/work symlink: %v", err)
		}
	}

	// /id subvolume for frame-local secrets (never persisted in snaps); always
	// created fresh and empty.
	idPath := filepath.Join(framePath, "id")
	if fi, err := os.Stat(idPath); err == nil && fi.IsDir() && !isSubvolume(idPath) {
		os.RemoveAll(idPath)
	}
	if !isSubvolume(idPath) {
		if err := btrfsCreateSubvol(idPath); err != nil {
			return fmt.Errorf("create id subvol: %w", err)
		}
	}
	configureIDDir(idPath)

	// Sidecar + stamp. The stamp reflects the source frame's last finalized
	// root snap; the new frame's first snap chains past it via the pending map.
	meta.CreatedAt = time.Now()
	if err := writeFrameSidecar(framePath, meta); err != nil {
		log.Printf("Warning: failed to write frame sidecar for %s: %v", framePath, err)
	}
	if err := writeStampFile(framePath, meta.Rootfs); err != nil {
		log.Printf("Warning: failed to write stamp file for %s: %v", framePath, err)
	}

	finalizeFrameRootfs(framePath)
	log.Printf("Created forked frame %s (rootfs:%s home:%s work:%s)",
		framePath, meta.Rootfs, meta.Home, meta.Work)
	return nil
}
