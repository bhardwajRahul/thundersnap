// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// refs_handlers.go provides HTTP handlers for the ref API.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/tailscale/thundersnap/frameid"
	"github.com/tailscale/thundersnap/refid"
	"github.com/tailscale/thundersnap/refs"
)

// framePathForUserUUID returns the on-disk frame path for a user's frame UUID.
// Frames live at <fs-dir>/<user>/<uuid>/. It returns "" when the fs dir is not
// configured (e.g. in unit tests that exercise the ref store without a running
// daemon).
func framePathForUserUUID(user string, uuid frameid.ID) string {
	if flagFsDir == nil || *flagFsDir == "" {
		return ""
	}
	return filepath.Join(*flagFsDir, user, uuid.String())
}

// refsStateDir is the data directory used to construct per-user ref stores.
// It is set in initRefStore from --data-dir, NOT the fs dir: a per-user
// refs.Store appends "refs/<user>", so its root must be the data dir.
var refsStateDir string

// initRefStore records the data directory used for per-user ref stores.
func initRefStore(dataDir string) {
	refsStateDir = dataDir
}

// userRefStore returns a ref store scoped to the given tailscale user.
func userRefStore(user string) *refs.Store {
	return refs.NewUserStore(refsStateDir, user)
}

// userRefStore returns a ref store scoped to the tailscale user that owns this
// control server's frame. The user is derived from the frame's rootFS path
// (<fs-dir>/<user>/<uuid>).
func (c *controlServer) userRefStore() (*refs.Store, string, error) {
	user, err := tailscaleUserFromRootFS(c.rootFS)
	if err != nil {
		return nil, "", err
	}
	return userRefStore(user), user, nil
}

// defaultRefName is the reserved ref name used for a user's "default" frame:
// the one reached by a bare login (`ssh host`, which tsnet turns into
// `<tailscale-user>@host`) or an explicitly empty frame name (`root@@host`).
const defaultRefName = "default"

// resolveFrameForUser maps an SSH frame name to a concrete frame for the given
// tailscale user, using that user's ref store. The returned uuid identifies the
// frame; framePath is its on-disk location (<fs-dir>/<user>/<uuid>).
//
// Resolution rules:
//   - If the name is a valid UUID and a frame with that UUID exists for the
//     user, return it directly (unattached, since there's no ref binding).
//   - An empty name, or a name equal to the tailscale username (a bare login,
//     where tsnet inserts the username as the SSH user), resolves the reserved
//     "default" ref.
//   - Any other name is looked up verbatim as a ref.
//   - If the "default" ref does not exist, the caller is told to use a fresh
//     unattached frame (attached==false, a freshly minted uuid, no ref bound).
//   - Any OTHER unknown name is an error ("no such frame ...") — no implicit
//     create.
//
// attached reports whether the returned uuid is bound to an existing ref. When
// false (only possible for the default case or UUID lookups), the caller should
// create/connect to an empty frame without binding a ref.
func resolveFrameForUser(tailscaleUser, name string) (uuid frameid.ID, framePath string, attached bool, err error) {
	// First, check if the name is a valid UUID that exists as a frame. This
	// allows SSH directly into a frame by UUID even if it has no refs.
	if parsed, perr := frameid.Parse(name); perr == nil {
		frameStore := userFrameStore(tailscaleUser)
		if frameStore.Exists(parsed) {
			return parsed, framePathForUserUUID(tailscaleUser, parsed), false, nil
		}
		// UUID parses but frame doesn't exist - fall through to ref lookup
		// in case a ref happens to be named like a UUID (unlikely but allowed).
	}

	store := userRefStore(tailscaleUser)

	isDefault := name == "" || name == tailscaleUser
	lookup := name
	if isDefault {
		lookup = defaultRefName
	}

	ref, gerr := store.Get(lookup)
	if gerr == nil {
		return ref.UUID, framePathForUserUUID(tailscaleUser, ref.UUID), true, nil
	}
	if gerr != refs.ErrRefNotFound {
		return frameid.Nil, "", false, gerr
	}

	if isDefault {
		// No bound default yet: hand back a fresh, unattached frame. The user
		// can later run `ts frame --ref=default ...` to bind it.
		fresh := frameid.MustNew()
		return fresh, framePathForUserUUID(tailscaleUser, fresh), false, nil
	}

	return frameid.Nil, "", false, fmt.Errorf("no such frame %q", name)
}

// RefRequest is the request body for ref operations.
type RefRequest struct {
	Name  string `json:"name"`
	UUID  string `json:"uuid,omitempty"`
	Force bool   `json:"force,omitempty"`
}

// RefResponse is the response from ref operations.
type RefResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// handleRefCreate handles POST /ref/create
func (c *controlServer) handleRefCreate(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}

	refStore, user, err := c.userRefStore()
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var req RefRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Name == "" || req.UUID == "" {
		jsonError(w, "name and uuid are required", http.StatusBadRequest)
		return
	}

	// Validate ref name
	if err := refs.ValidateName(req.Name); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Parse UUID
	uuid, err := frameid.Parse(req.UUID)
	if err != nil {
		jsonError(w, "invalid uuid: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Create the ref
	if err := refStore.Create(req.Name, uuid); err != nil {
		if err == refs.ErrRefExists {
			jsonError(w, "ref already exists", http.StatusConflict)
			return
		}
		log.Printf("ref create failed: %v", err)
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Initialize the ref's identity subvolume inside the target frame's /id.
	if framePath := framePathForUserUUID(user, uuid); framePath != "" {
		if err := refid.Ensure(framePath, req.Name); err != nil {
			log.Printf("Warning: ensure id subvolume for ref %s in frame %s: %v", req.Name, uuid, err)
		}
	}

	log.Printf("Created ref %s -> %s for user %s", req.Name, req.UUID, user)
	jsonResponse(w, RefResponse{Status: "ok"})
}

// handleRefMove handles POST /ref/move
func (c *controlServer) handleRefMove(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}

	refStore, user, err := c.userRefStore()
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var req RefRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Name == "" || req.UUID == "" {
		jsonError(w, "name and uuid are required", http.StatusBadRequest)
		return
	}

	// Parse UUID
	uuid, err := frameid.Parse(req.UUID)
	if err != nil {
		jsonError(w, "invalid uuid: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Get current ref to check for running processes
	currentRef, err := refStore.Get(req.Name)
	if err != nil {
		if err == refs.ErrRefNotFound {
			jsonError(w, "ref not found", http.StatusNotFound)
			return
		}
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Check for running processes in current frame (unless force)
	if !req.Force {
		// TODO: Check activeFrames for the current UUID
		// For now, we'll allow the move
	}

	oldUUID := currentRef.UUID

	// Move the ref
	if err := refStore.Move(req.Name, uuid); err != nil {
		log.Printf("ref move failed: %v", err)
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Move the ref's identity subvolume from the old frame's /id to the new
	// frame's /id so its private state follows the ref.
	srcFrame, dstFrame := framePathForUserUUID(user, oldUUID), framePathForUserUUID(user, uuid)
	if oldUUID != uuid && srcFrame != "" && dstFrame != "" {
		if err := refid.Move(srcFrame, dstFrame, req.Name); err != nil {
			log.Printf("Warning: move id subvolume for ref %s (%s -> %s): %v", req.Name, oldUUID, uuid, err)
		}
	}

	// If the ref has autorun configured and we're moving to a different frame,
	// restart the autorun process in the new frame.
	if globalAutorun != nil && len(currentRef.Autorun) > 0 && oldUUID != uuid {
		globalAutorun.restartProcess(user, req.Name, uuid, currentRef.Autorun)
	}

	log.Printf("Moved ref %s -> %s", req.Name, req.UUID)
	jsonResponse(w, RefResponse{Status: "ok"})
}

// handleRefDelete handles POST /ref/delete
func (c *controlServer) handleRefDelete(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}

	refStore, user, err := c.userRefStore()
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var req RefRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Name == "" {
		jsonError(w, "name is required", http.StatusBadRequest)
		return
	}

	// Check if id dir is non-empty (unless force)
	if !req.Force {
		hasID, err := refStore.IDDirExists(req.Name)
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if hasID {
			jsonError(w, "ref has non-empty identity directory (use -f to force)", http.StatusConflict)
			return
		}
	}

	// Resolve the ref's current frame UUID before deleting the ref config: on
	// a force delete we must scrub the ref's per-frame identity subvolume
	// (<fs-dir>/<uuid>/id/<ref>) too, and once the config is gone we can no
	// longer find which frame held it. A missing ref here is reported below.
	ref, getErr := refStore.Get(req.Name)

	// Delete the ref
	if err := refStore.Delete(req.Name); err != nil {
		if err == refs.ErrRefNotFound {
			jsonError(w, "ref not found", http.StatusNotFound)
			return
		}
		log.Printf("ref delete failed: %v", err)
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Stop any autorun process for this ref
	if globalAutorun != nil {
		globalAutorun.stopProcess(user, req.Name)
	}

	// Also remove identity state if force. RemoveIDDir clears the state-dir id
	// dir; refid.Remove scrubs the per-frame identity subvolume that actually
	// holds the ref's private key material, so a force delete does not leave it
	// orphaned on the frame for a later snapshot or frame reuse to expose.
	if req.Force {
		refStore.RemoveIDDir(req.Name)
		if getErr == nil {
			if framePath := framePathForUserUUID(user, ref.UUID); framePath != "" {
				if err := refid.Remove(framePath, req.Name); err != nil {
					log.Printf("Warning: remove id subvolume for ref %s in frame %s: %v", req.Name, ref.UUID, err)
				}
			}
		}
	}

	log.Printf("Deleted ref %s", req.Name)
	jsonResponse(w, RefResponse{Status: "ok"})
}

// RefListEntry is a single ref in the list response.
type RefListEntry struct {
	Name    string   `json:"name"`
	UUID    string   `json:"uuid"`
	Autorun []string `json:"autorun,omitempty"`
}

// RefListResponse is the response from /refs.
type RefListResponse struct {
	Status string         `json:"status"`
	Refs   []RefListEntry `json:"refs"`
}

// handleListRefs handles GET /refs
func (c *controlServer) handleListRefs(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}

	refStore, _, err := c.userRefStore()
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	names, err := refStore.List()
	if err != nil {
		log.Printf("list refs failed: %v", err)
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var entries []RefListEntry
	for _, name := range names {
		ref, err := refStore.Get(name)
		if err != nil {
			continue // skip refs that can't be read
		}
		entries = append(entries, RefListEntry{
			Name:    name,
			UUID:    ref.UUID.String(),
			Autorun: ref.Autorun,
		})
	}

	jsonResponse(w, RefListResponse{Status: "ok", Refs: entries})
}

// ReflogEntryResponse is a single entry in the reflog response.
type ReflogEntryResponse struct {
	UUID string `json:"uuid"`
	Time string `json:"time"`
}

// ReflogResponse is the response from /reflog.
type ReflogResponse struct {
	Status  string                `json:"status"`
	Message string                `json:"message,omitempty"`
	Name    string                `json:"name"`
	Reflog  []ReflogEntryResponse `json:"reflog"`
}

// handleReflog handles GET /reflog?name=<name>
// If name is not provided, it defaults to the unique ref for the current frame
// (if exactly one ref points to it), or returns an error suggesting available refs.
func (c *controlServer) handleReflog(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}

	refStore, _, err := c.userRefStore()
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	name := r.URL.Query().Get("name")
	if name == "" {
		// No ref name provided: find refs pointing to the current frame and
		// default to the unique one (if any), or suggest available refs.
		frameUUID, err := frameUUIDFromRootFS(c.rootFS)
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}

		names, err := refStore.List()
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Find refs pointing to this frame
		var matchingRefs []string
		for _, n := range names {
			ref, err := refStore.Get(n)
			if err != nil {
				continue
			}
			if ref.UUID == frameUUID {
				matchingRefs = append(matchingRefs, n)
			}
		}

		switch len(matchingRefs) {
		case 0:
			jsonError(w, "no refs point to this frame", http.StatusBadRequest)
			return
		case 1:
			name = matchingRefs[0]
		default:
			// Multiple refs - suggest them
			msg := fmt.Sprintf("multiple refs point to this frame, specify one: %s",
				strings.Join(matchingRefs, ", "))
			jsonError(w, msg, http.StatusBadRequest)
			return
		}
	}

	ref, err := refStore.Get(name)
	if err != nil {
		if err == refs.ErrRefNotFound {
			jsonError(w, "ref not found", http.StatusNotFound)
			return
		}
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var entries []ReflogEntryResponse
	for _, entry := range ref.Reflog {
		entries = append(entries, ReflogEntryResponse{
			UUID: entry.UUID.String(),
			Time: entry.Time.Format("2006-01-02T15:04:05Z07:00"),
		})
	}

	jsonResponse(w, ReflogResponse{
		Status: "ok",
		Name:   name,
		Reflog: entries,
	})
}

// AutorunRequest is the request body for /autorun.
type AutorunRequest struct {
	RefName string   `json:"ref_name"`
	Argv    []string `json:"argv,omitempty"`
}

// AutorunResponse is the response from /autorun.
type AutorunResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// handleAutorun handles POST /autorun
func (c *controlServer) handleAutorun(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}

	refStore, user, err := c.userRefStore()
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var req AutorunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.RefName == "" {
		jsonError(w, "ref_name is required", http.StatusBadRequest)
		return
	}

	// Get the ref to find its current UUID (needed to start the process in the right frame)
	ref, err := refStore.Get(req.RefName)
	if err != nil {
		if err == refs.ErrRefNotFound {
			jsonError(w, "ref not found", http.StatusNotFound)
			return
		}
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := refStore.SetAutorun(req.RefName, req.Argv); err != nil {
		log.Printf("set autorun failed: %v", err)
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Start or stop the autorun process
	if globalAutorun != nil {
		if len(req.Argv) > 0 {
			globalAutorun.startProcess(user, req.RefName, ref.UUID, req.Argv)
			log.Printf("Set autorun for ref %s: %v", req.RefName, req.Argv)
		} else {
			globalAutorun.stopProcess(user, req.RefName)
			log.Printf("Cleared autorun for ref %s", req.RefName)
		}
	}
	jsonResponse(w, AutorunResponse{Status: "ok"})
}

// jsonError sends a JSON error response.
func jsonError(w http.ResponseWriter, message string, code int) {
	writeJSON(w, code, RefResponse{Status: "error", Message: message})
}

// jsonResponse sends a JSON response.
func jsonResponse(w http.ResponseWriter, v interface{}) {
	writeJSON(w, http.StatusOK, v)
}
