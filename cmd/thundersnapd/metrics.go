// metrics.go wires the importable metrics package into the daemon's :7575
// tsnet HTTP server, supplying the live session/VM counts from in-memory state.
package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/tailscale/thundersnap/metrics"
)

// countRunningSessions returns the total number of active container sessions
// across all frames.
func countRunningSessions() int {
	return controlServers.countAllSessions()
}

// countRunningVMs returns the number of running outer VMX VMs.
func countRunningVMs() int {
	vmxSessions.mu.Lock()
	defer vmxSessions.mu.Unlock()
	return len(vmxSessions.sessions)
}

// countRefs sums the number of refs across all per-user ref stores. Refs live
// under <data-dir>/refs/<user>/; the user directories are enumerated from the
// live-filesystem dir (fsDir/<user>) so a freshly-seen user with refs but no
// frame on disk is still counted via its own store. A scrape never fails: any
// read error is treated as zero for that user.
func countRefs(fsDir string) int {
	userEntries, err := os.ReadDir(filepath.Join(refsStateDir, "refs"))
	if err != nil {
		return 0
	}
	n := 0
	for _, ue := range userEntries {
		if !ue.IsDir() {
			continue
		}
		names, err := userRefStore(ue.Name()).List()
		if err != nil {
			continue
		}
		n += len(names)
	}
	return n
}

// registerMetrics installs the /metrics handler on mux, exporting OS-level
// metrics plus thundersnap counts (frames, snaps, refs, running sessions,
// running VMs). Live session/VM counts come from in-memory daemon state.
func registerMetrics(mux *http.ServeMux, fsDir, snapsDir string) {
	handler, err := metrics.NewHandler(metrics.Sources{
		FsDir:           fsDir,
		SnapsDir:        snapsDir,
		Refs:            func() int { return countRefs(fsDir) },
		RunningSessions: countRunningSessions,
		RunningVMs:      countRunningVMs,
	})
	if err != nil {
		log.Printf("Failed to set up metrics handler: %v", err)
		return
	}
	mux.Handle("/metrics", handler)
}
