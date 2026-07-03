// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// frames_handlers.go provides HTTP handlers for the frame history/log API.
package main

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/tailscale/thundersnap/frameid"
	"github.com/tailscale/thundersnap/frames"
	"github.com/tailscale/thundersnap/refs"
)

// framesStateDir is the data directory used to construct per-user frame stores.
// It is set in initFrameStore from --data-dir, NOT the fs dir: a per-user
// frames.Store appends "fs/<user>", so its root must be the data dir.
var framesStateDir string

// initFrameStore records the data directory used for per-user frame stores.
func initFrameStore(dataDir string) {
	framesStateDir = dataDir
}

// userFrameStore returns a frame store scoped to the given tailscale user.
func userFrameStore(user string) *frames.Store {
	return frames.NewUserStore(framesStateDir, user)
}

// LogEntry is a single entry in the frame history response.
type LogEntry struct {
	Snap    string `json:"snap"`
	Time    string `json:"time"`
	Message string `json:"message,omitempty"`
}

// LogResponse is the response from /log.
type LogResponse struct {
	Status  string     `json:"status"`
	Message string     `json:"message,omitempty"`
	UUID    string     `json:"uuid"`
	History []LogEntry `json:"history"`
}

// handleLog handles GET /log?uuid=<uuid>
// If uuid is not provided, it returns the log for the current frame.
func (c *controlServer) handleLog(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}

	user, err := tailscaleUserFromRootFS(c.rootFS)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	frameStore := userFrameStore(user)

	uuidStr := r.URL.Query().Get("uuid")
	var uuid frameid.ID
	if uuidStr == "" {
		// No uuid provided: use the current frame's UUID from the control
		// server's rootFS path (<fs-dir>/<user>/<uuid>).
		uuid, err = frameUUIDFromRootFS(c.rootFS)
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		uuidStr = uuid.String()
	} else {
		uuid, err = frameid.Parse(uuidStr)
		if err != nil {
			jsonError(w, "invalid uuid: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	frame, err := frameStore.Get(uuid)
	if err != nil {
		if err == frames.ErrFrameNotFound {
			jsonError(w, "frame not found", http.StatusNotFound)
			return
		}
		log.Printf("get frame failed: %v", err)
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var entries []LogEntry
	for _, entry := range frame.History {
		entries = append(entries, LogEntry{
			Snap:    entry.Snap,
			Time:    entry.Time.Format("2006-01-02T15:04:05Z07:00"),
			Message: entry.Message,
		})
	}

	writeJSON(w, http.StatusOK, LogResponse{
		Status:  "ok",
		UUID:    uuidStr,
		History: entries,
	})
}

// FrameResponse is the response from /frame (current frame info).
type FrameResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	UUID    string `json:"uuid"`
	Rootfs  string `json:"rootfs,omitempty"`
	Home    string `json:"home,omitempty"`
	Work    string `json:"work,omitempty"`
}

// handleFrame handles GET /frame - returns current frame UUID and metadata.
func (c *controlServer) handleFrame(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}

	user, err := tailscaleUserFromRootFS(c.rootFS)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	uuid, err := frameUUIDFromRootFS(c.rootFS)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	frameStore := userFrameStore(user)
	frame, err := frameStore.Get(uuid)
	if err != nil {
		if err == frames.ErrFrameNotFound {
			// Frame exists on disk but no metadata - return just the UUID
			writeJSON(w, http.StatusOK, FrameResponse{
				Status: "ok",
				UUID:   uuid.String(),
			})
			return
		}
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, FrameResponse{
		Status: "ok",
		UUID:   uuid.String(),
		Rootfs: frame.Rootfs,
		Home:   frame.Home,
		Work:   frame.Work,
	})
}

// ResolveFrameRequest is the request body for /resolve-frame.
type ResolveFrameRequest struct {
	// Spec can be a UUID, ref name, or snap triplet (root:home:work)
	Spec string `json:"spec"`
}

// ResolveFrameResponse is the response from /resolve-frame.
type ResolveFrameResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	UUID    string `json:"uuid"`
	Exists  bool   `json:"exists"`           // true if frame already exists
	IsRef   bool   `json:"is_ref,omitempty"` // true if spec was resolved as a ref
	RefName string `json:"ref_name,omitempty"`
	Rootfs  string `json:"rootfs,omitempty"`
	Home    string `json:"home,omitempty"`
	Work    string `json:"work,omitempty"`
}

// handleResolveFrame handles POST /resolve-frame.
// It resolves a spec (UUID, ref, or snap triplet) to frame info without creating anything.
func (c *controlServer) handleResolveFrame(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}

	var req ResolveFrameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Spec == "" {
		jsonError(w, "spec is required", http.StatusBadRequest)
		return
	}

	user, err := tailscaleUserFromRootFS(c.rootFS)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	frameStore := userFrameStore(user)
	refStore := userRefStore(user)

	// Try parsing as UUID first
	if uuid, err := frameid.Parse(req.Spec); err == nil {
		if frameStore.Exists(uuid) {
			frame, _ := frameStore.Get(uuid)
			resp := ResolveFrameResponse{
				Status: "ok",
				UUID:   uuid.String(),
				Exists: true,
			}
			if frame != nil {
				resp.Rootfs = frame.Rootfs
				resp.Home = frame.Home
				resp.Work = frame.Work
			}
			writeJSON(w, http.StatusOK, resp)
			return
		}
		// UUID doesn't exist as a frame
		writeJSON(w, http.StatusOK, ResolveFrameResponse{
			Status: "ok",
			UUID:   uuid.String(),
			Exists: false,
		})
		return
	}

	// Try as a ref name
	ref, err := refStore.Get(req.Spec)
	if err == nil {
		frame, _ := frameStore.Get(ref.UUID)
		resp := ResolveFrameResponse{
			Status:  "ok",
			UUID:    ref.UUID.String(),
			Exists:  frameStore.Exists(ref.UUID),
			IsRef:   true,
			RefName: req.Spec,
		}
		if frame != nil {
			resp.Rootfs = frame.Rootfs
			resp.Home = frame.Home
			resp.Work = frame.Work
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if err != refs.ErrRefNotFound {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Not a UUID or ref - treat as a snap triplet spec
	// Return what the frame WOULD be created with, but don't create it
	writeJSON(w, http.StatusOK, ResolveFrameResponse{
		Status: "ok",
		Exists: false,
	})
}

// CloneHistoryRequest is the request body for /clone-history.
type CloneHistoryRequest struct {
	SourceUUID string `json:"source_uuid"`
	TargetUUID string `json:"target_uuid"`
}

// CloneHistoryResponse is the response from /clone-history.
type CloneHistoryResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// handleCloneHistory handles POST /clone-history.
// Copies history from one frame to another.
func (c *controlServer) handleCloneHistory(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}

	var req CloneHistoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.SourceUUID == "" || req.TargetUUID == "" {
		jsonError(w, "source_uuid and target_uuid are required", http.StatusBadRequest)
		return
	}

	sourceUUID, err := frameid.Parse(req.SourceUUID)
	if err != nil {
		jsonError(w, "invalid source_uuid: "+err.Error(), http.StatusBadRequest)
		return
	}

	targetUUID, err := frameid.Parse(req.TargetUUID)
	if err != nil {
		jsonError(w, "invalid target_uuid: "+err.Error(), http.StatusBadRequest)
		return
	}

	user, err := tailscaleUserFromRootFS(c.rootFS)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	frameStore := userFrameStore(user)

	sourceFrame, err := frameStore.Get(sourceUUID)
	if err != nil {
		if err == frames.ErrFrameNotFound {
			jsonError(w, "source frame not found", http.StatusNotFound)
			return
		}
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	targetFrame, err := frameStore.Get(targetUUID)
	if err != nil {
		if err == frames.ErrFrameNotFound {
			jsonError(w, "target frame not found", http.StatusNotFound)
			return
		}
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Copy history from source to target
	targetFrame.History = make([]frames.HistoryEntry, len(sourceFrame.History))
	copy(targetFrame.History, sourceFrame.History)

	if err := frameStore.Update(targetUUID, targetFrame); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("Cloned history from frame %s to %s (%d entries)", sourceUUID, targetUUID, len(targetFrame.History))
	writeJSON(w, http.StatusOK, CloneHistoryResponse{Status: "ok"})
}

// PruneHistoryRequest is the request body for /prune-history.
type PruneHistoryRequest struct {
	UUID  string   `json:"uuid"`
	Snaps []string `json:"snaps"` // snap IDs to remove from history
}

// PruneHistoryResponse is the response from /prune-history.
type PruneHistoryResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	Pruned  int    `json:"pruned"` // number of entries removed
}

// handlePruneHistory handles POST /prune-history.
// Removes specific snap entries from a frame's history.
func (c *controlServer) handlePruneHistory(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}

	var req PruneHistoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.UUID == "" {
		jsonError(w, "uuid is required", http.StatusBadRequest)
		return
	}

	uuid, err := frameid.Parse(req.UUID)
	if err != nil {
		jsonError(w, "invalid uuid: "+err.Error(), http.StatusBadRequest)
		return
	}

	user, err := tailscaleUserFromRootFS(c.rootFS)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	frameStore := userFrameStore(user)

	frame, err := frameStore.Get(uuid)
	if err != nil {
		if err == frames.ErrFrameNotFound {
			jsonError(w, "frame not found", http.StatusNotFound)
			return
		}
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Build a set of snaps to prune
	pruneSet := make(map[string]bool)
	for _, s := range req.Snaps {
		pruneSet[s] = true
	}

	// Filter out pruned entries
	var newHistory []frames.HistoryEntry
	pruned := 0
	for _, entry := range frame.History {
		if pruneSet[entry.Snap] {
			pruned++
		} else {
			newHistory = append(newHistory, entry)
		}
	}

	frame.History = newHistory

	if err := frameStore.Update(uuid, frame); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("Pruned %d entries from frame %s history", pruned, uuid)
	writeJSON(w, http.StatusOK, PruneHistoryResponse{
		Status: "ok",
		Pruned: pruned,
	})
}
