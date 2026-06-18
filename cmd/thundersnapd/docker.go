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
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
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
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req DownloadDockerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.ImageRef == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(DownloadDockerResponse{
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(DownloadDockerResponse{
			Status:  "error",
			Message: err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(DownloadDockerResponse{
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

	pw := &dockerProgressWriter{w: w, encoder: json.NewEncoder(w)}
	if f, ok := w.(http.Flusher); ok {
		pw.flusher = f
	}

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
	w       http.ResponseWriter
	flusher http.Flusher
	encoder *json.Encoder
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
	event := DownloadDockerStreamEvent{
		Type:    "progress",
		Message: msg,
	}
	pw.encoder.Encode(event)
	if pw.flusher != nil {
		pw.flusher.Flush()
	}
}

func (pw *dockerProgressWriter) writeResult(status, snapshotID string, cached bool, message string) {
	event := DownloadDockerStreamEvent{
		Type:       "result",
		Status:     status,
		SnapshotID: snapshotID,
		Cached:     cached,
		Message:    message,
	}
	pw.encoder.Encode(event)
	if pw.flusher != nil {
		pw.flusher.Flush()
	}
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
	tmpDir, err := os.MkdirTemp(*flagSnapshotsDir, "docker-extract-")
	if err != nil {
		return "", false, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create a btrfs subvolume for the extracted filesystem
	tmpID, err := generateRandomID()
	if err != nil {
		return "", false, fmt.Errorf("generate temp ID: %w", err)
	}
	extractPath := filepath.Join(*flagSnapshotsDir, tmpID+".extract")

	cmd := exec.Command("btrfs", "subvolume", "create", extractPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", false, fmt.Errorf("btrfs subvolume create: %w\noutput: %s", err, string(output))
	}

	// Cleanup on error
	cleanup := func() {
		exec.Command("btrfs", "subvolume", "delete", extractPath).Run()
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

	// Create the final snapshot
	snapshotID, err := createSnapshotWithTaints(extractPath, "", nil, progress, false)
	if err != nil {
		cleanup()
		return "", false, fmt.Errorf("create snapshot: %w", err)
	}

	// Add source metadata to the snap
	snapMeta, _ := readSnapMeta(*flagSnapshotsDir, snapshotID)
	if snapMeta == nil {
		snapMeta = &SnapMeta{}
	}
	snapMeta.Source = &SnapSource{
		Type: "docker",
		Ref:  canonicalRef,
	}
	if err := writeSnapMeta(*flagSnapshotsDir, snapshotID, snapMeta); err != nil {
		log.Printf("Warning: failed to write snap meta with source: %v", err)
	}

	// Cleanup the extraction subvolume
	cleanup()

	log.Printf("Downloaded Docker image %s as snapshot %s", canonicalRef, snapshotID)
	return snapshotID, false, nil
}

// findSnapByDockerSource finds a snap with the given docker source ref.
func findSnapByDockerSource(ref string) string {
	entries, err := os.ReadDir(*flagSnapshotsDir)
	if err != nil {
		return ""
	}

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".jsonc") {
			continue
		}
		snapID := strings.TrimSuffix(entry.Name(), ".jsonc")
		meta, err := readSnapMeta(*flagSnapshotsDir, snapID)
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
		case tar.TypeReg:
			// Ensure parent directory exists
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			// Remove existing file if any
			os.Remove(target)
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			f.Close()
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
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

	return nil
}
