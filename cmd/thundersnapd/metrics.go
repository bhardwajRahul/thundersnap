// metrics.go wires the importable metrics package into the daemon's :7575
// tsnet HTTP server, supplying the live session/VM counts from in-memory state.
package main

import (
	"log"
	"net/http"

	"github.com/tailscale/thundersnap/metrics"
)

// countRunningSessions returns the total number of active container sessions
// across all frames.
func countRunningSessions() int {
	activeFrames.Lock()
	defer activeFrames.Unlock()
	total := 0
	for _, c := range activeFrames.count {
		total += c
	}
	return total
}

// countRunningVMs returns the number of running outer VMX VMs.
func countRunningVMs() int {
	vmxSessions.mu.Lock()
	defer vmxSessions.mu.Unlock()
	return len(vmxSessions.sessions)
}

// registerMetrics installs the /metrics handler on mux, exporting OS-level
// metrics plus thundersnap counts (frames, snaps, refs, running sessions,
// running VMs). Live session/VM counts come from in-memory daemon state.
func registerMetrics(mux *http.ServeMux, fsDir, snapsDir string) {
	handler, err := metrics.NewHandler(metrics.Sources{
		FsDir:           fsDir,
		SnapsDir:        snapsDir,
		Refs:            refStore,
		RunningSessions: countRunningSessions,
		RunningVMs:      countRunningVMs,
	})
	if err != nil {
		log.Printf("Failed to set up metrics handler: %v", err)
		return
	}
	mux.Handle("/metrics", handler)
}
