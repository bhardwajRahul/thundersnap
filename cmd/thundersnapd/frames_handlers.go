// frames_handlers.go provides HTTP handlers for the frame history/log API.
package main

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/tailscale/thundersnap/frameid"
	"github.com/tailscale/thundersnap/frames"
)

// frameStore is the global frame store, initialized in main().
var frameStore *frames.Store

// initFrameStore initializes the frame store with the state directory.
func initFrameStore(stateDir string) {
	frameStore = frames.NewStore(stateDir)
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
func handleLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	uuidStr := r.URL.Query().Get("uuid")
	if uuidStr == "" {
		// TODO: Get current frame UUID from context
		// For now, require uuid parameter
		jsonError(w, "uuid parameter is required", http.StatusBadRequest)
		return
	}

	uuid, err := frameid.Parse(uuidStr)
	if err != nil {
		jsonError(w, "invalid uuid: "+err.Error(), http.StatusBadRequest)
		return
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
			Snap:    entry.Snap.String(),
			Time:    entry.Time.Format("2006-01-02T15:04:05Z07:00"),
			Message: entry.Message,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(LogResponse{
		Status:  "ok",
		UUID:    uuidStr,
		History: entries,
	})
}
