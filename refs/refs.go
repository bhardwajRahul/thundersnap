// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// Package refs provides the ref system for thundersnap.
//
// A ref is a mutable pointer from a name to a frame UUID, analogous to a git branch.
// Refs can be created, moved to point at different UUIDs, and deleted. Each ref
// maintains a reflog of which UUIDs it has pointed to over time.
//
// State directory structure (flat, legacy: a Store created with NewStore):
//
//	<state-dir>/
//	  fs/<uuid>/             # frame filesystems
//	  snaps/                 # snap storage
//	  refs/<refname>.jsonc   # ref config, autorun, reflog
//	  id/<refname>/          # private state per ref (keys, tsnet, etc.)
//
// Namespaced structure (a Store created with NewNamespaceStore): refs and
// identity state are grouped by workspace namespace:
//
//	<state-dir>/
//	  refs/<namespace>/<refname>.jsonc
//	  id/<namespace>/<refname>/
package refs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tailscale/hujson"
	"github.com/tailscale/thundersnap/frameid"
)

var (
	// ErrRefExists is returned when creating a ref that already exists.
	ErrRefExists = errors.New("ref already exists")
	// ErrRefNotFound is returned when operating on a ref that doesn't exist.
	ErrRefNotFound = errors.New("ref not found")
	// ErrInvalidRefName is returned when a ref name is invalid.
	ErrInvalidRefName = errors.New("invalid ref name")
)

// validRefName matches valid ref names. The charset is deliberately
// restrictive so syntax is reserved for the future: only ASCII letters,
// digits, dash, and underscore are allowed, and the name must start with a
// letter. In particular dots ('.') and slashes ('/') are NOT allowed — dots
// are reserved for future namespace/version separators and would also be a
// path-traversal hazard (ref names become filenames via refPath), and slashes
// would create nested paths. A UUID is never a valid ref name under this rule
// (UUIDs start with a hex digit), which keeps the UUID and ref namespaces
// disjoint (see ValidateFrameName).
var validRefName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// ReflogEntry records when a ref pointed to a particular UUID.
type ReflogEntry struct {
	// UUID is the frame this ref pointed to at Time.
	UUID frameid.ID `json:"uuid"`
	// Time is when the ref started pointing at UUID.
	Time time.Time `json:"time"`
}

// Ref represents a named pointer to a frame UUID.
type Ref struct {
	// UUID is the frame UUID this ref currently points to.
	UUID frameid.ID `json:"uuid"`

	// Autorun is the argv array for a program thundersnapd should keep running.
	// Empty means no autorun configured.
	Autorun []string `json:"autorun,omitempty"`

	// Reflog is the history of which UUIDs this ref has pointed to.
	// Most recent entries are first.
	Reflog []ReflogEntry `json:"reflog,omitempty"`
}

// Store manages refs in a state directory. When namespace is non-empty, refs
// and identity state live under refs/<namespace>/ and id/<namespace>/; an empty
// namespace selects the flat legacy layout.
type Store struct {
	stateDir  string
	namespace string
}

// NewStore creates a new ref store rooted at stateDir using the flat layout.
func NewStore(stateDir string) *Store {
	return &Store{stateDir: stateDir}
}

// NewNamespaceStore creates a ref store under refs/<namespace>/ and
// id/<namespace>/.
func NewNamespaceStore(stateDir, namespace string) *Store {
	return &Store{stateDir: stateDir, namespace: namespace}
}

// refsDir returns the path to the refs directory.
func (s *Store) refsDir() string {
	if s.namespace != "" {
		return filepath.Join(s.stateDir, "refs", s.namespace)
	}
	return filepath.Join(s.stateDir, "refs")
}

// refPath returns the path to a ref's config file.
func (s *Store) refPath(name string) string {
	return filepath.Join(s.refsDir(), name+".jsonc")
}

// idDir returns the path to a ref's private identity directory.
func (s *Store) idDir(name string) string {
	if s.namespace != "" {
		return filepath.Join(s.stateDir, "id", s.namespace, name)
	}
	return filepath.Join(s.stateDir, "id", name)
}

// notFoundOr maps a not-exist error to ErrRefNotFound and wraps any other
// error with context. It returns nil when err is nil.
func notFoundOr(name, what string, err error) error {
	if err == nil {
		return nil
	}
	if os.IsNotExist(err) {
		return ErrRefNotFound
	}
	return fmt.Errorf("%s ref %s: %w", what, name, err)
}

// ValidateName checks if a ref name is valid: non-empty, at most 128 bytes,
// starting with an ASCII letter, and containing only ASCII letters, digits,
// dash, and underscore (no dots, no slashes — see validRefName).
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty name", ErrInvalidRefName)
	}
	if len(name) > 128 {
		return fmt.Errorf("%w: name too long (max 128)", ErrInvalidRefName)
	}
	if !validRefName.MatchString(name) {
		return fmt.Errorf("%w: must start with a letter and contain only letters/digits/dash/underscore", ErrInvalidRefName)
	}
	return nil
}

// ValidateFrameName reports whether name is a valid frame address: either a
// valid frame UUID (parsed by frameid.Parse) or a valid ref name (ValidateName).
// A frame is addressed by UUID (e.g. `ts frame --delete <uuid>`,
// `ssh user@<uuid>`) or by ref name (e.g. `ts frame <ref>`, `ssh user@<ref>`);
// any other string is rejected so a bogus name never becomes a filesystem path.
// The two branches are disjoint: a UUID starts with a hex digit and so is not a
// valid ref name (refs must start with a letter), which means a name never
// matches both and the UUID/ref namespaces stay unambiguous.
func ValidateFrameName(name string) error {
	if _, err := frameid.Parse(name); err == nil {
		return nil
	}
	return ValidateName(name)
}

// Create creates a new ref pointing at the given UUID.
// Returns ErrRefExists if the ref already exists.
func (s *Store) Create(name string, uuid frameid.ID) error {
	if err := ValidateName(name); err != nil {
		return err
	}

	// Ensure refs directory exists.
	if err := os.MkdirAll(s.refsDir(), 0755); err != nil {
		return fmt.Errorf("create refs dir: %w", err)
	}

	path := s.refPath(name)

	// Check if ref already exists.
	if _, err := os.Stat(path); err == nil {
		return ErrRefExists
	}

	ref := &Ref{
		UUID: uuid,
		Reflog: []ReflogEntry{
			{UUID: uuid, Time: time.Now()},
		},
	}

	return s.write(name, ref)
}

// Get retrieves a ref by name.
// Returns ErrRefNotFound if the ref doesn't exist.
func (s *Store) Get(name string) (*Ref, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}

	path := s.refPath(name)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, notFoundOr(name, "read", err)
	}

	// Standardize hujson to JSON.
	standardized, err := hujson.Standardize(data)
	if err != nil {
		return nil, fmt.Errorf("parse ref %s: %w", name, err)
	}

	var ref Ref
	if err := json.Unmarshal(standardized, &ref); err != nil {
		return nil, fmt.Errorf("unmarshal ref %s: %w", name, err)
	}

	return &ref, nil
}

// Move moves a ref to point at a new UUID.
// Returns ErrRefNotFound if the ref doesn't exist.
func (s *Store) Move(name string, newUUID frameid.ID) error {
	ref, err := s.Get(name)
	if err != nil {
		return err
	}

	// Prepend so Reflog stays newest-first (mirrors frames' History).
	ref.Reflog = append([]ReflogEntry{{UUID: newUUID, Time: time.Now()}}, ref.Reflog...)
	ref.UUID = newUUID

	return s.write(name, ref)
}

// Delete deletes a ref.
// Returns ErrRefNotFound if the ref doesn't exist.
func (s *Store) Delete(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}

	if err := os.Remove(s.refPath(name)); err != nil {
		return notFoundOr(name, "delete", err)
	}

	return nil
}

// SetAutorun sets the autorun configuration for a ref.
// Pass nil or empty slice to clear autorun.
func (s *Store) SetAutorun(name string, argv []string) error {
	ref, err := s.Get(name)
	if err != nil {
		return err
	}

	ref.Autorun = argv
	return s.write(name, ref)
}

// List returns the names of all refs.
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.refsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list refs: %w", err)
	}

	var names []string
	for _, e := range entries {
		// refs/ holds one <name>.jsonc per ref; ignore any directories or
		// non-.jsonc files that may appear alongside them.
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if filepath.Ext(name) == ".jsonc" {
			names = append(names, strings.TrimSuffix(name, ".jsonc"))
		}
	}
	return names, nil
}

// Exists returns true if a ref exists.
func (s *Store) Exists(name string) bool {
	if err := ValidateName(name); err != nil {
		return false
	}
	_, err := os.Stat(s.refPath(name))
	return err == nil
}

// IDDirExists returns true if a ref's identity directory exists and is non-empty.
func (s *Store) IDDirExists(name string) (bool, error) {
	if err := ValidateName(name); err != nil {
		return false, err
	}

	dir := s.idDir(name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	// "Exists" means exists AND is non-empty: an empty id dir carries no
	// identity state, so callers treat it the same as absent.
	return len(entries) > 0, nil
}

// EnsureIDDir creates the identity directory for a ref if it doesn't exist.
func (s *Store) EnsureIDDir(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	return os.MkdirAll(s.idDir(name), 0700)
}

// RemoveIDDir removes the identity directory for a ref.
func (s *Store) RemoveIDDir(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	dir := s.idDir(name)
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove id dir: %w", err)
	}
	return nil
}

// write writes a ref to disk atomically (temp file + rename) so a reader never
// observes a partially written ref.
func (s *Store) write(name string, ref *Ref) error {
	path := s.refPath(name)

	data, err := json.MarshalIndent(ref, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal ref %s: %w", name, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+name+".jsonc.*")
	if err != nil {
		return fmt.Errorf("write ref %s: %w", name, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write ref %s: %w", name, err)
	}
	if err := tmp.Chmod(0644); err != nil {
		tmp.Close()
		return fmt.Errorf("write ref %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write ref %s: %w", name, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("write ref %s: %w", name, err)
	}
	return nil
}
