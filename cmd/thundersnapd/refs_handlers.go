// refs_handlers.go provides HTTP handlers for the ref API.
package main

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/tailscale/thundersnap/frameid"
	"github.com/tailscale/thundersnap/refs"
)

// refStore is the global ref store, initialized in main().
var refStore *refs.Store

// initRefStore initializes the ref store with the state directory.
func initRefStore(stateDir string) {
	refStore = refs.NewStore(stateDir)
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
func handleRefCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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

	log.Printf("Created ref %s -> %s", req.Name, req.UUID)
	jsonResponse(w, RefResponse{Status: "ok"})
}

// handleRefMove handles POST /ref/move
func handleRefMove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
		_ = currentRef
	}

	// Move the ref
	if err := refStore.Move(req.Name, uuid); err != nil {
		log.Printf("ref move failed: %v", err)
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("Moved ref %s -> %s", req.Name, req.UUID)
	jsonResponse(w, RefResponse{Status: "ok"})
}

// handleRefDelete handles POST /ref/delete
func handleRefDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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

	// Also remove id dir if force
	if req.Force {
		refStore.RemoveIDDir(req.Name)
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
func handleListRefs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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

// ReflogEntry is a single entry in the reflog response.
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
func handleReflog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name := r.URL.Query().Get("name")
	if name == "" {
		jsonError(w, "name parameter is required", http.StatusBadRequest)
		return
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
func handleAutorun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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

	if err := refStore.SetAutorun(req.RefName, req.Argv); err != nil {
		if err == refs.ErrRefNotFound {
			jsonError(w, "ref not found", http.StatusNotFound)
			return
		}
		log.Printf("set autorun failed: %v", err)
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if len(req.Argv) > 0 {
		log.Printf("Set autorun for ref %s: %v", req.RefName, req.Argv)
	} else {
		log.Printf("Cleared autorun for ref %s", req.RefName)
	}
	jsonResponse(w, AutorunResponse{Status: "ok"})
}

// jsonError sends a JSON error response.
func jsonError(w http.ResponseWriter, message string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(RefResponse{Status: "error", Message: message})
}

// jsonResponse sends a JSON response.
func jsonResponse(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
