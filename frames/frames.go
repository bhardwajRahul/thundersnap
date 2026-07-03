// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// Package frames provides frame metadata management for thundersnap.
//
// A frame is identified by a UUID and represents a filesystem with history.
// Each `ts snap` adds a new snapshot entry to that frame's history. The UUID is
// the "content lineage" - the same filesystem evolving over time.
//
// Frame filesystems are stored at fs/<uuid>/ with metadata at fs/<uuid>.jsonc
// for a flat (legacy) Store created with NewStore. A per-user Store created with
// NewUserStore(stateDir, user) namespaces frames under the owning user at
// fs/<user>/<uuid>/ with metadata at fs/<user>/<uuid>.jsonc, so a user only ever
// sees their own frames.
package frames

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tailscale/hujson"
	"github.com/tailscale/thundersnap/frameid"
)

var (
	// ErrFrameExists is returned when creating a frame that already exists.
	ErrFrameExists = errors.New("frame already exists")
	// ErrFrameNotFound is returned when operating on a frame that doesn't exist.
	ErrFrameNotFound = errors.New("frame not found")
)

// HistoryEntry records when a snapshot was taken.
type HistoryEntry struct {
	// Snap is the snap ID of this snapshot.
	Snap string `json:"snap"`
	// Time is when this snapshot was created.
	Time time.Time `json:"time"`
	// Message is an optional description of this snapshot.
	Message string `json:"message,omitempty"`
}

// Frame represents the metadata for a frame (fs/<user>/<uuid>.jsonc).
//
// Rootfs/Home/Work are snap IDs (the on-disk snap directory names under
// snaps/), so the daemon can rebuild each component subvolume from the snap
// store. An empty Home/Work means an empty subvolume was created.
type Frame struct {
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

	// History is the snapshot history of this frame.
	// Most recent entries are first.
	History []HistoryEntry `json:"history,omitempty"`

	// CreatedAt is when this frame was created.
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// Store manages frames in a state directory. When user is non-empty, frames are
// namespaced under that user (fs/<user>/<uuid>) so a user only ever sees their
// own frames; when user is empty the flat legacy layout (fs/<uuid>) is used.
type Store struct {
	stateDir string
	user     string
}

// NewStore creates a new frame store rooted at stateDir using the flat layout.
func NewStore(stateDir string) *Store {
	return &Store{stateDir: stateDir}
}

// NewUserStore creates a frame store namespaced under user (fs/<user>/<uuid>).
// A user only ever sees their own frames.
func NewUserStore(stateDir, user string) *Store {
	return &Store{stateDir: stateDir, user: user}
}

// fsDir returns the path to the fs directory.
func (s *Store) fsDir() string {
	if s.user != "" {
		return filepath.Join(s.stateDir, "fs", s.user)
	}
	return filepath.Join(s.stateDir, "fs")
}

// framePath returns the path to a frame's directory.
func (s *Store) framePath(uuid frameid.ID) string {
	return filepath.Join(s.fsDir(), uuid.String())
}

// metaPath returns the path to a frame's metadata file.
func (s *Store) metaPath(uuid frameid.ID) string {
	return filepath.Join(s.fsDir(), uuid.String()+".jsonc")
}

// Create creates a new frame with the given UUID and initial metadata.
// The caller is responsible for creating the actual btrfs subvolume at framePath(uuid).
func (s *Store) Create(uuid frameid.ID, frame *Frame) error {
	if frameid.IsZero(uuid) {
		return errors.New("cannot create frame with nil UUID")
	}

	// Ensure fs directory exists.
	if err := os.MkdirAll(s.fsDir(), 0755); err != nil {
		return fmt.Errorf("create fs dir: %w", err)
	}

	path := s.metaPath(uuid)

	// Check if frame already exists.
	if _, err := os.Stat(path); err == nil {
		return ErrFrameExists
	}

	// Set creation time if not already set.
	if frame.CreatedAt.IsZero() {
		frame.CreatedAt = time.Now()
	}

	return s.write(uuid, frame)
}

// notFoundOr maps a not-exist error to ErrFrameNotFound and wraps any other
// error with context. It returns nil when err is nil.
func notFoundOr(uuid frameid.ID, what string, err error) error {
	if err == nil {
		return nil
	}
	if os.IsNotExist(err) {
		return ErrFrameNotFound
	}
	return fmt.Errorf("%s frame %s: %w", what, uuid, err)
}

// Get retrieves a frame by UUID.
// Returns ErrFrameNotFound if the frame doesn't exist.
func (s *Store) Get(uuid frameid.ID) (*Frame, error) {
	path := s.metaPath(uuid)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, notFoundOr(uuid, "read", err)
	}

	// Standardize hujson to JSON.
	standardized, err := hujson.Standardize(data)
	if err != nil {
		return nil, fmt.Errorf("parse frame %s: %w", uuid, err)
	}

	var frame Frame
	if err := json.Unmarshal(standardized, &frame); err != nil {
		return nil, fmt.Errorf("unmarshal frame %s: %w", uuid, err)
	}

	return &frame, nil
}

// Update updates a frame's metadata.
// Returns ErrFrameNotFound if the frame doesn't exist.
func (s *Store) Update(uuid frameid.ID, frame *Frame) error {
	// The stat is advisory: it maps a missing frame to ErrFrameNotFound for
	// callers. It is inherently racy with the write below (TOCTOU), but the
	// store is single-writer in practice, and a concurrent delete simply
	// recreates the metadata file.
	if _, err := os.Stat(s.metaPath(uuid)); err != nil {
		return notFoundOr(uuid, "stat", err)
	}
	return s.write(uuid, frame)
}

// Delete deletes a frame's metadata.
// The caller is responsible for deleting the actual btrfs subvolume.
// Returns ErrFrameNotFound if the frame doesn't exist.
func (s *Store) Delete(uuid frameid.ID) error {
	if err := os.Remove(s.metaPath(uuid)); err != nil {
		return notFoundOr(uuid, "delete", err)
	}
	return nil
}

// AddHistoryEntry adds a snapshot to a frame's history.
func (s *Store) AddHistoryEntry(uuid frameid.ID, snap string, message string) error {
	frame, err := s.Get(uuid)
	if err != nil {
		return err
	}

	entry := HistoryEntry{
		Snap:    snap,
		Time:    time.Now(),
		Message: message,
	}
	// Prepend so History stays newest-first. This allocates a fresh slice each
	// call, which is fine for the small histories frames accumulate.
	frame.History = append([]HistoryEntry{entry}, frame.History...)

	return s.write(uuid, frame)
}

// AddTaint adds a taint to a frame.
func (s *Store) AddTaint(uuid frameid.ID, taint string) error {
	frame, err := s.Get(uuid)
	if err != nil {
		return err
	}

	// Check if already present.
	for _, t := range frame.Taints {
		if t == taint {
			return nil // already tainted
		}
	}

	frame.Taints = append(frame.Taints, taint)
	sort.Strings(frame.Taints)

	return s.write(uuid, frame)
}

// List returns the UUIDs of all frames.
func (s *Store) List() ([]frameid.ID, error) {
	entries, err := os.ReadDir(s.fsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list frames: %w", err)
	}

	var uuids []frameid.ID
	for _, e := range entries {
		// Each frame is a metadata file fs/<uuid>.jsonc sitting next to the
		// frame's own fs/<uuid>/ subvolume directory. Skip directories (the
		// subvolume) and any non-.jsonc file so we count each frame once.
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if filepath.Ext(name) != ".jsonc" {
			continue
		}
		uuidStr := strings.TrimSuffix(name, ".jsonc")
		uuid, err := frameid.Parse(uuidStr)
		if err != nil {
			continue // a .jsonc whose stem isn't a UUID is not a frame
		}
		uuids = append(uuids, uuid)
	}
	return uuids, nil
}

// Exists returns true if a frame exists.
func (s *Store) Exists(uuid frameid.ID) bool {
	_, err := os.Stat(s.metaPath(uuid))
	return err == nil
}

// Path returns the filesystem path for a frame.
func (s *Store) Path(uuid frameid.ID) string {
	return s.framePath(uuid)
}

// write writes frame metadata to disk atomically (write to a temp file in the
// same directory, then rename over the target) so a reader never observes a
// partially written or truncated metadata file.
func (s *Store) write(uuid frameid.ID, frame *Frame) error {
	path := s.metaPath(uuid)

	data, err := json.MarshalIndent(frame, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal frame %s: %w", uuid, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+uuid.String()+".jsonc.*")
	if err != nil {
		return fmt.Errorf("write frame %s: %w", uuid, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write frame %s: %w", uuid, err)
	}
	if err := tmp.Chmod(0644); err != nil {
		tmp.Close()
		return fmt.Errorf("write frame %s: %w", uuid, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write frame %s: %w", uuid, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("write frame %s: %w", uuid, err)
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
