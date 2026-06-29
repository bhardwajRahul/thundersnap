// Package cgroup implements the cgroup v2 / OOM resource-control plumbing used
// to confine thundersnap container processes. It is pure Linux resource-control
// glue with no dependency on the rest of the daemon: a Manager owns one parent
// cgroup (named per daemon instance) under which each container gets a leaf
// cgroup with memory, pids (fork-bomb), and CPU-fairness limits, plus an OOM
// score bias so containers are killed before the host or the daemon itself.
//
// All operations are best-effort: failures are logged and never fatal, because
// the daemon must keep serving sessions even on a kernel without full cgroup v2
// controller support.
package cgroup

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Resource-limit tuning constants. These provide defense against runaway
// processes while allowing efficient memory sharing between containers.
const (
	// containerOOMScore is the OOM score adjustment applied to container
	// processes. Range is -1000..+1000 (default 0); +500 makes containers far
	// more likely to be OOM-killed than the host OS or the daemon itself.
	containerOOMScore = 500

	// parentMemoryMaxPercent is the percentage of system RAM that all
	// thundersnap containers combined may use — a hard limit protecting the host.
	parentMemoryMaxPercent = 80

	// parentCPUWeight is the CPU weight for all thundersnap containers relative
	// to other system work (default 100); 50 lets non-thundersnap work win when
	// CPU is contested, while idle CPU is still fully available to containers.
	parentCPUWeight = 50

	// containerMemoryHighPercent is the per-container soft memory limit as a
	// percentage of system RAM. Above it the kernel reclaims aggressively
	// (swap, drop caches) but does not OOM-kill, letting a container burst above
	// its fair share when memory is available.
	containerMemoryHighPercent = 10

	// containerPidsMax limits the process count per container — the primary
	// fork-bomb defense.
	containerPidsMax = 2000

	// containerCPUWeight is the CPU weight for each container relative to other
	// containers (100 = default, i.e. equal weight).
	containerCPUWeight = 100
)

const cgroupRoot = "/sys/fs/cgroup"

// Manager owns one parent cgroup (one per daemon instance) and the leaf cgroups
// of the containers beneath it.
type Manager struct {
	parentName  string
	initialized bool
}

// New returns a Manager rooted at the given parent cgroup name. The name should
// be unique per daemon instance (e.g. "thundersnap-<pid>") so multiple or
// nested daemons do not collide.
func New(parentName string) *Manager {
	return &Manager{parentName: parentName}
}

// ParentName returns the parent cgroup name, used by callers to build the
// per-container leaf name (e.g. "<parent>/<user>/<session>").
func (m *Manager) ParentName() string {
	return m.parentName
}

// ConfigureContainer applies resource limits to a freshly-started container
// process: it biases the OOM score and creates+joins a leaf cgroup with memory,
// pids, and CPU limits. cgroupName is the leaf path relative to the cgroup root
// (typically "<ParentName()>/<user>/<session>"). Best-effort; errors are logged.
func (m *Manager) ConfigureContainer(pid int, cgroupName string) {
	setProcessOOMScore(pid, containerOOMScore)
	m.setupContainerCgroup(pid, cgroupName)
}

// getSystemMemoryBytes returns the total system memory in bytes.
func getSystemMemoryBytes() (uint64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, err := strconv.ParseUint(fields[1], 10, 64)
				if err != nil {
					return 0, err
				}
				return kb * 1024, nil // Convert KB to bytes
			}
		}
	}
	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}

// initParent creates the parent cgroup with system-wide limits, once. Errors
// are logged but not fatal.
func (m *Manager) initParent() {
	if m.initialized {
		return
	}

	cgroupPath := filepath.Join(cgroupRoot, m.parentName)

	// Create parent cgroup directory
	if err := os.MkdirAll(cgroupPath, 0755); err != nil {
		log.Printf("warning: failed to create parent cgroup %s: %v", cgroupPath, err)
		return
	}

	// Enable controllers for child cgroups. We need to enable controllers in
	// the parent so children can use them.
	subtreeControl := filepath.Join(cgroupPath, "cgroup.subtree_control")
	if err := os.WriteFile(subtreeControl, []byte("+memory +pids +cpu"), 0644); err != nil {
		log.Printf("warning: failed to enable cgroup controllers: %v", err)
		// Continue anyway - some controllers might already be enabled
	}

	// Set CPU weight (lower priority than default)
	cpuWeight := filepath.Join(cgroupPath, "cpu.weight")
	if err := os.WriteFile(cpuWeight, []byte(strconv.Itoa(parentCPUWeight)), 0644); err != nil {
		log.Printf("warning: failed to set parent cpu.weight: %v", err)
	}

	// Set memory.max as hard backstop (percentage of system RAM)
	totalMem, err := getSystemMemoryBytes()
	if err != nil {
		log.Printf("warning: failed to get system memory: %v", err)
	} else {
		memMax := totalMem * parentMemoryMaxPercent / 100
		memMaxPath := filepath.Join(cgroupPath, "memory.max")
		if err := os.WriteFile(memMaxPath, []byte(strconv.FormatUint(memMax, 10)), 0644); err != nil {
			log.Printf("warning: failed to set parent memory.max: %v", err)
		} else {
			log.Printf("Configured parent cgroup %s: memory.max=%dMB, cpu.weight=%d",
				m.parentName, memMax/(1024*1024), parentCPUWeight)
		}
	}

	m.initialized = true
}

// setProcessOOMScore sets the OOM score adjustment for a process. Higher scores
// make the process more likely to be killed during memory pressure. Best-effort.
func setProcessOOMScore(pid int, score int) {
	path := fmt.Sprintf("/proc/%d/oom_score_adj", pid)
	if err := os.WriteFile(path, []byte(strconv.Itoa(score)), 0644); err != nil {
		log.Printf("warning: failed to set OOM score for pid %d: %v", pid, err)
	}
}

// setupContainerCgroup creates a leaf cgroup for the container process with
// resource limits and moves the process into it. Limits applied:
//   - memory.high: soft memory limit (kernel reclaims aggressively above this)
//   - memory.oom.group: kill entire container on OOM, not just one process
//   - pids.max: limit process count (fork bomb protection)
//   - cpu.weight: fair sharing among containers
//
// Best-effort; errors are logged.
func (m *Manager) setupContainerCgroup(pid int, cgroupName string) {
	// Ensure parent cgroup exists with system-wide limits
	m.initParent()

	// Use cgroup v2 unified hierarchy
	cgroupPath := filepath.Join(cgroupRoot, cgroupName)

	// Create cgroup directory
	if err := os.MkdirAll(cgroupPath, 0755); err != nil {
		log.Printf("warning: failed to create cgroup %s: %v", cgroupPath, err)
		return
	}

	// Enable subtree_control on all intermediate directories between parent and
	// leaf. In cgroup v2, each intermediate directory must have controllers
	// enabled for children to use them. The cgroupName is like
	// "thundersnap-123/user/container", so we need to enable controllers on
	// "thundersnap-123/user" as well.
	parts := strings.Split(cgroupName, "/")
	for i := 1; i < len(parts); i++ {
		intermediateDir := filepath.Join(cgroupRoot, filepath.Join(parts[:i]...))
		subtreeControl := filepath.Join(intermediateDir, "cgroup.subtree_control")
		// Ignore errors - the parent's initParent already set the top level,
		// and some systems may not support all controllers
		os.WriteFile(subtreeControl, []byte("+memory +pids +cpu"), 0644)
	}

	// Set memory.high (soft limit) - kernel reclaims aggressively above this
	totalMem, err := getSystemMemoryBytes()
	if err == nil {
		memHigh := totalMem * containerMemoryHighPercent / 100
		memHighPath := filepath.Join(cgroupPath, "memory.high")
		if err := os.WriteFile(memHighPath, []byte(strconv.FormatUint(memHigh, 10)), 0644); err != nil {
			log.Printf("warning: failed to set memory.high for %s: %v", cgroupName, err)
		}
	}

	// Enable memory.oom.group=1 so OOM kills the entire cgroup
	oomGroupPath := filepath.Join(cgroupPath, "memory.oom.group")
	if err := os.WriteFile(oomGroupPath, []byte("1"), 0644); err != nil {
		log.Printf("warning: failed to set memory.oom.group for %s: %v", cgroupName, err)
	}

	// Set pids.max (fork bomb protection)
	pidsMaxPath := filepath.Join(cgroupPath, "pids.max")
	if err := os.WriteFile(pidsMaxPath, []byte(strconv.Itoa(containerPidsMax)), 0644); err != nil {
		log.Printf("warning: failed to set pids.max for %s: %v", cgroupName, err)
	}

	// Set cpu.weight for fair sharing among containers
	cpuWeightPath := filepath.Join(cgroupPath, "cpu.weight")
	if err := os.WriteFile(cpuWeightPath, []byte(strconv.Itoa(containerCPUWeight)), 0644); err != nil {
		log.Printf("warning: failed to set cpu.weight for %s: %v", cgroupName, err)
	}

	// Move the process into the cgroup
	procsPath := filepath.Join(cgroupPath, "cgroup.procs")
	if err := os.WriteFile(procsPath, []byte(strconv.Itoa(pid)), 0644); err != nil {
		log.Printf("warning: failed to add pid %d to cgroup %s: %v", pid, cgroupName, err)
		return
	}
}
