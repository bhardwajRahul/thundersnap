// frames_handlers.go provides HTTP handlers for the frame history/log API.
package main

import (
	"log"
	"net/http"

	"github.com/tailscale/thundersnap/frameid"
	"github.com/tailscale/thundersnap/frames"
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
