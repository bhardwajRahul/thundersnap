// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// docker.go handles downloading and flattening Docker images into snaps.
package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/tailscale/thundersnap/btrfsutil"
	"golang.org/x/sys/unix"
)

// DownloadDockerRequest is the request body for /download-docker
type DownloadDockerRequest struct {
	ImageRef string `json:"image_ref"`
}

// DownloadDockerResponse is the response from /download-docker
type DownloadDockerResponse struct {
	Status     string `json:"status"`
	Message    string `json:"message,omitempty"`
	SnapshotID string `json:"snapshot_id,omitempty"`
	Cached     bool   `json:"cached,omitempty"`
}

// DownloadDockerStreamEvent is an event in the streaming download response
type DownloadDockerStreamEvent struct {
	Type       string `json:"type"`
	Message    string `json:"message,omitempty"`
	Status     string `json:"status,omitempty"`
	SnapshotID string `json:"snapshot_id,omitempty"`
	Cached     bool   `json:"cached,omitempty"`
}

// handleDownloadDocker handles POST /download-docker
func handleDownloadDocker(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}

	var req DownloadDockerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.ImageRef == "" {
		writeJSON(w, http.StatusBadRequest, DownloadDockerResponse{
			Status:  "error",
			Message: "image_ref is required",
		})
		return
	}

	// Check if streaming is requested
	stream := r.URL.Query().Get("stream") == "1"
	isTTY := r.URL.Query().Get("tty") == "1"

	if stream {
		handleDownloadDockerStreaming(w, req, isTTY)
		return
	}

	// Non-streaming mode
	snapshotID, cached, err := downloadDockerImage(req.ImageRef, nil)
	if err != nil {
		log.Printf("download-docker failed for %s: %v", req.ImageRef, err)
		writeJSON(w, http.StatusInternalServerError, DownloadDockerResponse{
			Status:  "error",
			Message: err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, DownloadDockerResponse{
		Status:     "ok",
		SnapshotID: snapshotID,
		Cached:     cached,
	})
}

// handleDownloadDockerStreaming handles streaming download-docker
func handleDownloadDockerStreaming(w http.ResponseWriter, req DownloadDockerRequest, isTTY bool) {
	w.Header().Set("Content-Type", "application/x-ndjson")

	// Enable streaming mode immediately
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	pw := &dockerProgressWriter{newProgressEmitter(w)}

	snapshotID, cached, err := downloadDockerImage(req.ImageRef, pw)
	if err != nil {
		log.Printf("download-docker failed for %s: %v", req.ImageRef, err)
		pw.writeResult("error", "", false, err.Error())
		return
	}

	pw.writeResult("ok", snapshotID, cached, "")
}

// dockerProgressWriter wraps ResponseWriter to write progress events
type dockerProgressWriter struct {
	progressEmitter
}

func (pw *dockerProgressWriter) Write(p []byte) (n int, err error) {
	msg := strings.TrimSpace(string(p))
	if msg == "" {
		return len(p), nil
	}
	pw.writeProgress(msg)
	return len(p), nil
}

func (pw *dockerProgressWriter) writeProgress(msg string) {
	pw.emit(DownloadDockerStreamEvent{Type: "progress", Message: msg})
}

func (pw *dockerProgressWriter) writeResult(status, snapshotID string, cached bool, message string) {
	pw.emit(DownloadDockerStreamEvent{
		Type:       "result",
		Status:     status,
		SnapshotID: snapshotID,
		Cached:     cached,
		Message:    message,
	})
}

// downloadDockerImage downloads a Docker image and creates a snap from it.
// Returns the snapshot ID and whether it was cached.
func downloadDockerImage(imageRef string, progress io.Writer) (string, bool, error) {
	// Parse image reference
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return "", false, fmt.Errorf("parse image reference: %w", err)
	}

	if progress != nil {
		fmt.Fprintf(progress, "Resolving %s...\n", ref.Name())
	}

	// Get image descriptor to get the digest
	desc, err := remote.Get(ref, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return "", false, fmt.Errorf("get image descriptor: %w", err)
	}

	// Create canonical ref with digest
	digestRef := ref.Context().Digest(desc.Digest.String())
	canonicalRef := digestRef.String()

	if progress != nil {
		fmt.Fprintf(progress, "Resolved to %s\n", canonicalRef)
	}

	// Check if we already have a snap with this source
	existingID := findSnapByDockerSource(canonicalRef)
	if existingID != "" {
		if progress != nil {
			fmt.Fprintf(progress, "Image already cached as %s\n", existingID)
		}
		return existingID, true, nil
	}

	if progress != nil {
		fmt.Fprintf(progress, "Pulling image...\n")
	}

	// Get the full image
	img, err := desc.Image()
	if err != nil {
		return "", false, fmt.Errorf("get image: %w", err)
	}

	// Create a temporary directory for extraction
	tmpDir, err := os.MkdirTemp(*flagSnapsDir, "docker-extract-")
	if err != nil {
		return "", false, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create a btrfs subvolume for the extracted filesystem
	tmpID, err := generateRandomID()
	if err != nil {
		return "", false, fmt.Errorf("generate temp ID: %w", err)
	}
	extractPath := filepath.Join(*flagSnapsDir, tmpID+".extract")

	if err := btrfsutil.CreateSubvol(extractPath); err != nil {
		return "", false, err
	}

	// Cleanup on error
	cleanup := func() {
		btrfsutil.DeleteSubvol(extractPath) // best effort
	}

	if progress != nil {
		fmt.Fprintf(progress, "Extracting layers...\n")
	}

	// Export the image as a tarball and extract it
	tarPath := filepath.Join(tmpDir, "image.tar")
	tarFile, err := os.Create(tarPath)
	if err != nil {
		cleanup()
		return "", false, fmt.Errorf("create tar file: %w", err)
	}

	if err := tarball.Write(ref, img, tarFile); err != nil {
		tarFile.Close()
		cleanup()
		return "", false, fmt.Errorf("write image tarball: %w", err)
	}
	tarFile.Close()

	// Extract the tarball (it's a Docker save format, needs flattening)
	if err := flattenDockerTarball(tarPath, extractPath, progress); err != nil {
		cleanup()
		return "", false, fmt.Errorf("flatten tarball: %w", err)
	}

	if progress != nil {
		fmt.Fprintf(progress, "Creating snapshot...\n")
	}

	// Create the final snapshot (no progress callback for docker downloads)
	snapshotID, err := createSnapshotWithTaints(extractPath, "", nil, nil)
	if err != nil {
		cleanup()
		return "", false, fmt.Errorf("create snapshot: %w", err)
	}

	// Add source metadata to the snap
	snapMeta, _ := readSnapMeta(*flagSnapsDir, snapshotID)
	if snapMeta == nil {
		snapMeta = &SnapMeta{}
	}
	snapMeta.Source = &SnapSource{
		Type: "docker",
		Ref:  canonicalRef,
	}
	if err := writeSnapMeta(*flagSnapsDir, snapshotID, snapMeta); err != nil {
		log.Printf("Warning: failed to write snap meta with source: %v", err)
	}

	// Cleanup the extraction subvolume
	cleanup()

	log.Printf("Downloaded Docker image %s as snapshot %s", canonicalRef, snapshotID)
	return snapshotID, false, nil
}

// findSnapByDockerSource finds a snap with the given docker source ref.
func findSnapByDockerSource(ref string) string {
	entries, err := os.ReadDir(*flagSnapsDir)
	if err != nil {
		return ""
	}

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".jsonc") {
			continue
		}
		snapID := strings.TrimSuffix(entry.Name(), ".jsonc")
		meta, err := readSnapMeta(*flagSnapsDir, snapID)
		if err != nil || meta == nil {
			continue
		}
		if meta.Source != nil && meta.Source.Type == "docker" && meta.Source.Ref == ref {
			return snapID
		}
	}
	return ""
}

// flattenDockerTarball extracts a Docker save tarball to a directory,
// flattening all layers and applying whiteouts.
func flattenDockerTarball(tarPath, destDir string, progress io.Writer) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	tr := tar.NewReader(f)

	// First pass: find and parse manifest.json to get layer order
	var manifest []struct {
		Layers []string `json:"Layers"`
	}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		if hdr.Name == "manifest.json" {
			if err := json.NewDecoder(tr).Decode(&manifest); err != nil {
				return fmt.Errorf("parse manifest.json: %w", err)
			}
			break
		}
	}

	if len(manifest) == 0 || len(manifest[0].Layers) == 0 {
		return fmt.Errorf("no layers found in image")
	}

	// Reopen the tarball and extract layers in order
	f.Close()
	f, err = os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	// Read all layers into a map for ordered extraction
	layers := make(map[string]string) // layer path -> temp file path
	tmpDir := filepath.Dir(tarPath)

	tr = tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		// Check if this is a layer
		for _, layerPath := range manifest[0].Layers {
			if hdr.Name == layerPath {
				// Save layer to temp file
				tmpPath := filepath.Join(tmpDir, filepath.Base(layerPath))
				layerFile, err := os.Create(tmpPath)
				if err != nil {
					return err
				}
				_, err = io.Copy(layerFile, tr)
				layerFile.Close()
				if err != nil {
					return err
				}
				layers[layerPath] = tmpPath
			}
		}
	}

	// Extract layers in order
	for i, layerPath := range manifest[0].Layers {
		tmpPath, ok := layers[layerPath]
		if !ok {
			return fmt.Errorf("layer %s not found", layerPath)
		}

		if progress != nil {
			fmt.Fprintf(progress, "Extracting layer %d/%d...\n", i+1, len(manifest[0].Layers))
		}

		if err := extractLayer(tmpPath, destDir); err != nil {
			return fmt.Errorf("extract layer %s: %w", layerPath, err)
		}

		os.Remove(tmpPath)
	}

	return nil
}

// dirTimeInfo holds directory path and mtime for deferred timestamp setting.
// We can't set directory mtimes until after all their contents are extracted,
// because extracting files inside updates the directory mtime.
type dirTimeInfo struct {
	path  string
	mtime time.Time
}

// extractLayer extracts a layer tarball to the destination, handling whiteouts.
// The layer may be gzip-compressed (as produced by go-containerregistry's tarball.Write).
func extractLayer(layerPath, destDir string) error {
	f, err := os.Open(layerPath)
	if err != nil {
		return err
	}
	defer f.Close()

	// Check for gzip magic bytes (1f 8b)
	var magic [2]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}

	var r io.Reader = f
	if magic[0] == 0x1f && magic[1] == 0x8b {
		gr, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("gzip reader: %w", err)
		}
		defer gr.Close()
		r = gr
	}

	tr := tar.NewReader(r)

	// Collect directories so we can set their timestamps after extraction.
	// Directory mtimes get updated when files are extracted inside them,
	// so we need to set them at the end.
	var dirs []dirTimeInfo

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		// Handle whiteouts
		name := hdr.Name
		base := filepath.Base(name)
		dir := filepath.Dir(name)

		// .wh.* files mark deletions
		if strings.HasPrefix(base, ".wh.") {
			// Delete the corresponding file/directory
			target := filepath.Join(destDir, dir, strings.TrimPrefix(base, ".wh."))
			if base == ".wh..wh..opq" {
				// Opaque directory: delete all contents of the directory
				target = filepath.Join(destDir, dir)
				entries, _ := os.ReadDir(target)
				for _, entry := range entries {
					os.RemoveAll(filepath.Join(target, entry.Name()))
				}
			} else {
				os.RemoveAll(target)
			}
			continue
		}

		target := filepath.Join(destDir, name)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
			// MkdirAll doesn't update mode if dir already exists (e.g., created
			// by extracting a file inside it first), so explicitly set the mode.
			// This is important for directories like /tmp that need 1777.
			if err := os.Chmod(target, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
			// Defer setting directory timestamps until after all contents are extracted
			dirs = append(dirs, dirTimeInfo{path: target, mtime: hdr.ModTime})
		case tar.TypeReg:
			// Ensure parent directory exists
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			// Remove existing file if any
			os.Remove(target)
			outf, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			_, err = io.Copy(outf, tr)
			outf.Close()
			if err != nil {
				return err
			}
			// Preserve mtime from the tar header for reproducible snapshots
			os.Chtimes(target, hdr.ModTime, hdr.ModTime)
		case tar.TypeSymlink:
			os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
			// Preserve mtime on symlinks using Lutimes (Linux-specific)
			setSymlinkTimes(target, hdr.ModTime)
		case tar.TypeLink:
			// Hard link
			linkTarget := filepath.Join(destDir, hdr.Linkname)
			os.Remove(target)
			if err := os.Link(linkTarget, target); err != nil {
				// Fall back to copy if link fails
				srcF, err := os.Open(linkTarget)
				if err != nil {
					continue // Skip if source doesn't exist
				}
				dstF, err := os.Create(target)
				if err != nil {
					srcF.Close()
					return err
				}
				_, err = io.Copy(dstF, srcF)
				srcF.Close()
				dstF.Close()
				if err != nil {
					return err
				}
			}
		case tar.TypeChar, tar.TypeBlock:
			// Skip device nodes for now (would need root)
			continue
		case tar.TypeFifo:
			// Skip fifos
			continue
		}

		// Set ownership (best effort, may need root)
		os.Lchown(target, hdr.Uid, hdr.Gid)
	}

	// Set directory timestamps in reverse order (deepest first) so that
	// setting a child directory's mtime doesn't update the parent's mtime.
	sort.Slice(dirs, func(i, j int) bool {
		return len(dirs[i].path) > len(dirs[j].path)
	})
	for _, d := range dirs {
		os.Chtimes(d.path, d.mtime, d.mtime)
	}

	return nil
}

// setSymlinkTimes sets the mtime on a symlink using Lutimes.
// This is Linux-specific but necessary for reproducible Docker image extraction.
func setSymlinkTimes(path string, mtime time.Time) {
	// Convert time.Time to unix.Timeval
	sec := mtime.Unix()
	usec := mtime.Nanosecond() / 1000 // Lutimes uses microseconds
	tv := []unix.Timeval{
		{Sec: sec, Usec: int64(usec)}, // atime
		{Sec: sec, Usec: int64(usec)}, // mtime
	}
	// Ignore errors - this is best effort for reproducibility
	unix.Lutimes(path, tv)
}
