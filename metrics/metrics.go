// Package metrics exports Prometheus metrics for thundersnapd: the standard
// OS-level Go runtime and process collectors, plus thundersnap-specific gauges
// (frames, snaps, refs, running sessions, running VMs). The counting logic is
// kept here, separate from the daemon's main package, so it can be exercised
// directly by tests against real directories and a real ref store.
package metrics

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/tailscale/thundersnap/frameid"
)

// CountSnaps returns the number of snapshots in snapsDir, identified by their
// .tsm manifest files. A missing or unreadable directory is reported as zero
// rather than an error so a metrics scrape never fails.
func CountSnaps(snapsDir string) int {
	entries, err := os.ReadDir(snapsDir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tsm") {
			n++
		}
	}
	return n
}

// CountFrames returns the number of frames stored under fsDir. Frames live at
// fsDir/<user>/<uuid>/ alongside a <uuid>.jsonc metadata sidecar; that sidecar,
// whose stem is a valid frame UUID, is what identifies a frame. Non-UUID stems
// (legacy-layout leftovers, the "id"/"refs" dirs, ".vmx-*" files) are ignored.
// A missing or unreadable directory is reported as zero so a scrape never fails.
func CountFrames(fsDir string) int {
	userEntries, err := os.ReadDir(fsDir)
	if err != nil {
		return 0
	}
	n := 0
	for _, ue := range userEntries {
		if !ue.IsDir() {
			continue
		}
		frameEntries, err := os.ReadDir(filepath.Join(fsDir, ue.Name()))
		if err != nil {
			continue
		}
		for _, fe := range frameEntries {
			if fe.IsDir() {
				continue
			}
			name := fe.Name()
			stem, ok := strings.CutSuffix(name, ".jsonc")
			if !ok {
				continue
			}
			if _, err := frameid.Parse(stem); err != nil {
				continue // skip non-UUID stems (legacy layout)
			}
			n++
		}
	}
	return n
}

// Sources supplies the live data the collector reads on each scrape. FsDir and
// SnapsDir are scanned on disk; Refs, RunningSessions and RunningVMs are
// closures over the daemon's per-user ref stores and in-memory session/VM
// state (nil closures report zero).
type Sources struct {
	FsDir           string
	SnapsDir        string
	Refs            func() int
	RunningSessions func() int
	RunningVMs      func() int
}

// collector implements prometheus.Collector, computing thundersnap-specific
// gauges on each scrape so there is no shared mutable gauge state.
type collector struct {
	src Sources

	framesDesc   *prometheus.Desc
	snapsDesc    *prometheus.Desc
	refsDesc     *prometheus.Desc
	sessionsDesc *prometheus.Desc
	vmsDesc      *prometheus.Desc
}

func newCollector(src Sources) *collector {
	return &collector{
		src: src,
		framesDesc: prometheus.NewDesc(
			"thundersnap_frames_total",
			"Number of frames (live workspaces) on this node.",
			nil, nil,
		),
		snapsDesc: prometheus.NewDesc(
			"thundersnap_snaps_total",
			"Number of content-addressed snapshots stored on this node.",
			nil, nil,
		),
		refsDesc: prometheus.NewDesc(
			"thundersnap_refs_total",
			"Number of refs pointing at frames on this node.",
			nil, nil,
		),
		sessionsDesc: prometheus.NewDesc(
			"thundersnap_running_sessions",
			"Number of active container sessions across all frames.",
			nil, nil,
		),
		vmsDesc: prometheus.NewDesc(
			"thundersnap_running_vms",
			"Number of running VMs on this node.",
			nil, nil,
		),
	}
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.framesDesc
	ch <- c.snapsDesc
	ch <- c.refsDesc
	ch <- c.sessionsDesc
	ch <- c.vmsDesc
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	sessions := 0
	if c.src.RunningSessions != nil {
		sessions = c.src.RunningSessions()
	}
	vms := 0
	if c.src.RunningVMs != nil {
		vms = c.src.RunningVMs()
	}
	refs := 0
	if c.src.Refs != nil {
		refs = c.src.Refs()
	}
	ch <- prometheus.MustNewConstMetric(c.framesDesc, prometheus.GaugeValue, float64(CountFrames(c.src.FsDir)))
	ch <- prometheus.MustNewConstMetric(c.snapsDesc, prometheus.GaugeValue, float64(CountSnaps(c.src.SnapsDir)))
	ch <- prometheus.MustNewConstMetric(c.refsDesc, prometheus.GaugeValue, float64(refs))
	ch <- prometheus.MustNewConstMetric(c.sessionsDesc, prometheus.GaugeValue, float64(sessions))
	ch <- prometheus.MustNewConstMetric(c.vmsDesc, prometheus.GaugeValue, float64(vms))
}

// NewRegistry builds a Prometheus registry with the standard Go and process
// collectors (OS-level metrics) plus thundersnap's own collector.
func NewRegistry(src Sources) (*prometheus.Registry, error) {
	reg := prometheus.NewRegistry()
	if err := reg.Register(collectors.NewGoCollector()); err != nil {
		return nil, err
	}
	if err := reg.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})); err != nil {
		return nil, err
	}
	if err := reg.Register(newCollector(src)); err != nil {
		return nil, err
	}
	return reg, nil
}

// NewHandler returns an http.Handler for the /metrics endpoint backed by a
// registry built from src.
func NewHandler(src Sources) (http.Handler, error) {
	reg, err := NewRegistry(src)
	if err != nil {
		return nil, err
	}
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{}), nil
}
