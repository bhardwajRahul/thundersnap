// metadata.go provides types and helpers for frame and snap metadata files.
// These are stored as .jsonc files alongside the btrfs subvolumes.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/tailscale/hujson"
)

// SnapMeta represents the metadata for a snap (snaps/$snapid.jsonc).
// Snaps are immutable, content-addressed btrfs snapshots.
type SnapMeta struct {
	// Parent is the snap ID this was derived from, if any.
	// Null/empty for base images (e.g., downloaded from docker).
	Parent string `json:"parent,omitempty"`

	// Taints accumulated on this snap. See frames-and-taints.md.
	Taints []string `json:"taints,omitempty"`

	// Source describes where this snap originally came from.
	// Only present on snaps created by ts download-docker.
	// Not inherited by child snaps.
	Source *SnapSource `json:"source,omitempty"`
}

// SnapSource describes the origin of a base snap.
type SnapSource struct {
	Type string `json:"type"` // "docker"
	Ref  string `json:"ref"`  // full image ref with digest, e.g., "docker.io/library/ubuntu:24.04@sha256:abcd..."
}

// FrameMeta represents the metadata for a frame (fs/$user/$frame.jsonc).
// A frame is a named instance that combines rootfs, home, and work components.
type FrameMeta struct {
	// Rootfs is the snap ID for the rootfs component.
	// This is the base OS, packages, /var, /usr, /etc, system state.
	Rootfs string `json:"rootfs"`

	// Home is the snap ID for the home component.
	// Contains user dotfiles, shell config, editor settings.
	// Empty string means an empty subvolume was created.
	Home string `json:"home,omitempty"`

	// Work is the snap ID for the work component.
	// Contains source code, project files, application state.
	// Empty string means an empty subvolume was created.
	Work string `json:"work,omitempty"`

	// Taints on this frame (union of component taints, plus any acquired at runtime).
	Taints []string `json:"taints,omitempty"`

	// Isolation determines the execution environment.
	// "vm" (default): user gets a dedicated VM, containers inside it
	// "container": direct chroot container on the host (no VM)
	// "none": no sub-isolation (single-user thundersnap instance)
	Isolation string `json:"isolation,omitempty"`
}

// readSnapMeta reads and parses a snap's metadata file.
// Returns nil, nil if the file doesn't exist.
func readSnapMeta(snapshotsDir, snapID string) (*SnapMeta, error) {
	path := filepath.Join(snapshotsDir, snapID+".jsonc")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read snap meta %s: %w", snapID, err)
	}

	// Standardize hujson to JSON
	standardized, err := hujson.Standardize(data)
	if err != nil {
		return nil, fmt.Errorf("parse snap meta %s: %w", snapID, err)
	}

	var meta SnapMeta
	if err := json.Unmarshal(standardized, &meta); err != nil {
		return nil, fmt.Errorf("unmarshal snap meta %s: %w", snapID, err)
	}
	return &meta, nil
}

// writeSnapMeta writes a snap's metadata file.
func writeSnapMeta(snapshotsDir, snapID string, meta *SnapMeta) error {
	path := filepath.Join(snapshotsDir, snapID+".jsonc")

	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal snap meta %s: %w", snapID, err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write snap meta %s: %w", snapID, err)
	}
	return nil
}

// readFrameMeta reads and parses a frame's metadata file.
// framePath is the path to the frame directory (e.g., fs/user/dev).
// Returns nil, nil if the file doesn't exist.
func readFrameMeta(framePath string) (*FrameMeta, error) {
	path := framePath + ".jsonc"
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read frame meta %s: %w", framePath, err)
	}

	// Standardize hujson to JSON
	standardized, err := hujson.Standardize(data)
	if err != nil {
		return nil, fmt.Errorf("parse frame meta %s: %w", framePath, err)
	}

	var meta FrameMeta
	if err := json.Unmarshal(standardized, &meta); err != nil {
		return nil, fmt.Errorf("unmarshal frame meta %s: %w", framePath, err)
	}
	return &meta, nil
}

// writeFrameMeta writes a frame's metadata file.
// framePath is the path to the frame directory (e.g., fs/user/dev).
func writeFrameMeta(framePath string, meta *FrameMeta) error {
	path := framePath + ".jsonc"

	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal frame meta %s: %w", framePath, err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write frame meta %s: %w", framePath, err)
	}
	return nil
}

// UnionTaints returns the union of multiple taint sets, sorted.
func UnionTaints(sets ...[]string) []string {
	seen := make(map[string]bool)
	for _, set := range sets {
		for _, t := range set {
			seen[t] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	result := make([]string, 0, len(seen))
	for t := range seen {
		result = append(result, t)
	}
	sort.Strings(result)
	return result
}

// IntersectTaints returns the intersection of two taint sets, sorted.
func IntersectTaints(a, b []string) []string {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	setB := make(map[string]bool)
	for _, t := range b {
		setB[t] = true
	}
	var result []string
	for _, t := range a {
		if setB[t] {
			result = append(result, t)
		}
	}
	if len(result) == 0 {
		return nil
	}
	sort.Strings(result)
	return result
}

// taintsEqual returns true if two taint slices contain the same elements.
func taintsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	// Both are sorted by UnionTaints/IntersectTaints
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// getSnapTaints returns the taints for a snap, or nil if the snap has no metadata.
func getSnapTaints(snapshotsDir, snapID string) []string {
	if snapID == "" {
		return nil
	}
	meta, err := readSnapMeta(snapshotsDir, snapID)
	if err != nil || meta == nil {
		return nil
	}
	return meta.Taints
}
