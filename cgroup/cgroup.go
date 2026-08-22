// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// Package cgroup implements fail-closed cgroup v2 resource control for
// thundersnap containers and sessions.
package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	containerOOMScore          = 500
	parentMemoryMaxPercent     = 80
	parentCPUWeight            = 50
	containerMemoryHighPercent = 10
	containerPidsMax           = 2000
	containerCPUWeight         = 100
	cgroupRoot                 = "/sys/fs/cgroup"
	requiredControllers        = "memory pids cpu"
)

// Manager owns one parent cgroup and the container/session hierarchy below it.
type Manager struct {
	parentName  string
	initialized bool
}

func New(parentName string) *Manager  { return &Manager{parentName: parentName} }
func (m *Manager) ParentName() string { return m.parentName }

// CanCloneInto reports whether cgroup paths visible through /proc are rooted in
// the same hierarchy as the cgroup2 mount. Some service containers expose a
// relative cgroup namespace path containing ".."; clone3 then rejects an
// otherwise valid CLONE_INTO_CGROUP fd with ENOENT. Such processes are already
// confined by an outer cgroup and must use the post-start move fallback.
func CanCloneInto() bool {
	self, err := os.ReadFile("/proc/self/cgroup")
	return err == nil && !strings.Contains(string(self), "..")
}

// ContainerName returns the cgroup path, relative to the current cgroup2 mount,
// reserved for a container. The caller-provided key is reduced to one safe path
// component while retaining enough entropy to avoid collisions.
func (m *Manager) ContainerName(key string) string {
	base := filepath.Base(filepath.Clean(key))
	base = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '_'
	}, base)
	return filepath.Join(m.parentName, "containers", base)
}

// PrepareContainer creates a delegated container subtree and opens it for use
// with clone3(CLONE_INTO_CGROUP). The caller must close the returned file.
func (m *Manager) PrepareContainer(name string) (*os.File, error) {
	if err := m.initParent(); err != nil {
		return nil, err
	}
	path := filepath.Join(cgroupRoot, name)
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return nil, fmt.Errorf("create container cgroup parent %s: %w", parent, err)
	}
	if err := enableControllers(parent); err != nil {
		return nil, err
	}
	if err := os.Mkdir(path, 0755); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("create container cgroup %s: %w", path, err)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open container cgroup %s: %w", path, err)
	}
	return f, nil
}

// Move moves pid into the named cgroup.
func (m *Manager) Move(pid int, name string) error {
	path := filepath.Join(cgroupRoot, name, "cgroup.procs")
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)), 0644); err != nil {
		return fmt.Errorf("move pid %d to cgroup %s: %w", pid, name, err)
	}
	return nil
}

// KillSession terminates every process remaining in a session cgroup.
func (m *Manager) KillSession(name string) error {
	path := filepath.Join(cgroupRoot, name, "cgroup.kill")
	if err := os.WriteFile(path, []byte("1"), 0644); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("kill session cgroup %s: %w", path, err)
	}
	return nil
}

// KillContainer writes cgroup.kill at the root of a delegated container cgroup,
// which (per cgroup2 semantics) terminates every process in that subtree
// *including* detached descendants that escaped the container-init's PID-1
// shutdown (setsid/nohup jobs, nested-thundersnap children living under the
// same cgroup hierarchy). It is a one-shot: writing 1 kills current members and
// the file does not persist as "armed". The container-init itself is normally
// already dead by the time this is called (stopEntry SIGKILLs it), but a
// detached leftover could otherwise keep the cgroup non-empty and make the
// subsequent RemoveContainer fail with EBUSY, leaving a stale cgroup that
// poisons the next incarnation (reused name, stale limits/procs). Best-effort:
// the caller logs and proceeds regardless.
func (m *Manager) KillContainer(name string) error {
	path := filepath.Join(cgroupRoot, name, "cgroup.kill")
	if err := os.WriteFile(path, []byte("1"), 0644); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("kill container cgroup %s: %w", path, err)
	}
	return nil
}

// RemoveSession removes a session leaf after its process has exited.
func (m *Manager) RemoveSession(name string) error {
	path := filepath.Join(cgroupRoot, name)
	deadline := time.Now().Add(time.Second)
	for {
		err := os.Remove(path)
		if err == nil || os.IsNotExist(err) {
			return nil
		}
		if !errors.Is(err, syscall.EBUSY) || time.Now().After(deadline) {
			return fmt.Errorf("remove session cgroup %s: %w", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// DelegateContainer moves the namespace anchor out of the container root and
// enables all required controllers there. The cgroup namespace remains rooted
// at the container cgroup even after its creating process moves to .init.
func (m *Manager) DelegateContainer(pid int, name string) error {
	initName := filepath.Join(name, ".init")
	if err := os.MkdirAll(filepath.Join(cgroupRoot, initName), 0755); err != nil {
		return fmt.Errorf("create container init cgroup: %w", err)
	}
	if err := m.Move(pid, initName); err != nil {
		return err
	}
	return enableControllers(filepath.Join(cgroupRoot, name))
}

// RemoveContainer removes an empty delegated container hierarchy after its
// namespace anchor and all sessions have exited.
func (m *Manager) RemoveContainer(name string) error {
	root := filepath.Join(cgroupRoot, name)
	var dirs []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk container cgroup %s: %w", root, err)
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Remove(dirs[i]); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove container cgroup %s: %w", dirs[i], err)
		}
	}
	return nil
}

// ConfigureContainer creates a session leaf, applies every configured limit,
// and moves pid into it. Any failure is fatal: running without one requested
// controller or limit would make confinement depend silently on host details.
func (m *Manager) ConfigureContainer(pid int, cgroupName string) error {
	if err := setProcessOOMScore(pid, containerOOMScore); err != nil {
		return err
	}
	if err := m.initParent(); err != nil {
		return err
	}
	path := filepath.Join(cgroupRoot, cgroupName)
	if err := os.MkdirAll(path, 0755); err != nil {
		return fmt.Errorf("create session cgroup %s: %w", path, err)
	}

	parts := strings.Split(filepath.Clean(cgroupName), string(filepath.Separator))
	for i := 1; i < len(parts); i++ {
		dir := filepath.Join(cgroupRoot, filepath.Join(parts[:i]...))
		if err := enableControllers(dir); err != nil {
			return err
		}
	}
	totalMem, err := getSystemMemoryBytes()
	if err != nil {
		return err
	}
	settings := map[string]string{
		"memory.high":      strconv.FormatUint(totalMem*containerMemoryHighPercent/100, 10),
		"memory.oom.group": "1",
		"pids.max":         strconv.Itoa(containerPidsMax),
		"cpu.weight":       strconv.Itoa(containerCPUWeight),
	}
	for file, value := range settings {
		if err := os.WriteFile(filepath.Join(path, file), []byte(value), 0644); err != nil {
			return fmt.Errorf("set %s for cgroup %s: %w", file, cgroupName, err)
		}
	}
	return m.Move(pid, cgroupName)
}

func (m *Manager) initParent() error {
	if m.initialized {
		return nil
	}
	if err := requireCgroup2(cgroupRoot); err != nil {
		return err
	}
	path := filepath.Join(cgroupRoot, m.parentName)
	if err := os.MkdirAll(path, 0755); err != nil {
		return fmt.Errorf("create parent cgroup %s: %w", path, err)
	}
	if err := enableControllers(cgroupRoot); err != nil {
		return err
	}
	if err := enableControllers(path); err != nil {
		return err
	}
	totalMem, err := getSystemMemoryBytes()
	if err != nil {
		return err
	}
	settings := map[string]string{
		"cpu.weight": strconv.Itoa(parentCPUWeight),
		"memory.max": strconv.FormatUint(totalMem*parentMemoryMaxPercent/100, 10),
	}
	for file, value := range settings {
		if err := os.WriteFile(filepath.Join(path, file), []byte(value), 0644); err != nil {
			return fmt.Errorf("set parent %s: %w", file, err)
		}
	}
	m.initialized = true
	return nil
}

func requireCgroup2(path string) error {
	data, err := os.ReadFile(filepath.Join(path, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("cgroup2 is not mounted at %s: %w", path, err)
	}
	have := map[string]bool{}
	for _, controller := range strings.Fields(string(data)) {
		have[controller] = true
	}
	for _, controller := range strings.Fields(requiredControllers) {
		if !have[controller] {
			return fmt.Errorf("required cgroup2 controller %q is unavailable at %s", controller, path)
		}
	}
	return nil
}

func enableControllers(path string) error {
	if err := requireCgroup2(path); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(path, "cgroup.subtree_control"), []byte("+memory +pids +cpu"), 0644); err != nil {
		return fmt.Errorf("enable cgroup2 controllers under %s: %w", path, err)
	}
	return nil
}

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
				return kb * 1024, nil
			}
		}
	}
	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}

func setProcessOOMScore(pid, score int) error {
	path := fmt.Sprintf("/proc/%d/oom_score_adj", pid)
	if err := os.WriteFile(path, []byte(strconv.Itoa(score)), 0644); err != nil {
		return fmt.Errorf("set OOM score for pid %d: %w", pid, err)
	}
	return nil
}
