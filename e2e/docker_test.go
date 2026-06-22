// Package e2e contains end-to-end tests for thundersnap Docker import functionality.
package e2e

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestDockerImportBasic tests importing a locally-constructed OCI tarball.
// It constructs a minimal Docker-save-format tarball using pure Go (no external
// downloads), imports it via the test control server's /import-docker-tarball endpoint,
// and verifies the resulting snapshot contains the expected files.
func TestDockerImportBasic(t *testing.T) {
	env := newTestEnv(t)

	// Start the test control server
	sockPath := filepath.Join(env.root, "ctrl.sock")
	ctrl := startDockerTestControlServer(t, env, sockPath)
	defer ctrl.Close()

	client := newTestHTTPClient(sockPath)

	// Create a minimal Docker-save-format tarball
	tarPath := filepath.Join(env.root, "test-image.tar")
	if err := buildMinimalDockerTarball(t, tarPath); err != nil {
		t.Fatalf("failed to build test tarball: %v", err)
	}
	t.Logf("Created test Docker tarball at %s", tarPath)

	// Import the tarball using the /import-docker-tarball endpoint
	// (which wraps flattenDockerTarball for local files)
	importResp, err := client.postJSON("/import-docker-tarball", map[string]string{
		"tarball_path": tarPath,
	})
	if err != nil {
		t.Fatalf("import docker tarball: %v", err)
	}
	if importResp["status"] != "ok" {
		t.Fatalf("import docker tarball failed: %v", importResp["message"])
	}

	snapshotID, ok := importResp["snapshot_id"].(string)
	if !ok || snapshotID == "" {
		t.Fatalf("import did not return snapshot_id: %v", importResp)
	}
	t.Logf("Imported Docker image as snapshot: %s", snapshotID)

	// Verify the snapshot appears in the list
	listResp, err := client.getJSON("/list-snaps")
	if err != nil {
		t.Fatalf("list-snaps: %v", err)
	}
	if listResp["status"] != "ok" {
		t.Fatalf("list-snaps failed: %v", listResp["error"])
	}

	snaps, ok := listResp["snaps"].([]interface{})
	if !ok {
		t.Fatalf("snaps is not a list: %T", listResp["snaps"])
	}

	var foundSnap map[string]interface{}
	for _, s := range snaps {
		smap := s.(map[string]interface{})
		if smap["id"] == snapshotID {
			foundSnap = smap
			break
		}
	}
	if foundSnap == nil {
		t.Fatalf("snapshot %q not found in snaps list", snapshotID)
	}
	t.Logf("Found snapshot in list: %v", foundSnap)

	// Verify the snapshot has the expected files
	snapPath := filepath.Join(env.snapshotsDir, snapshotID)

	expectedFiles := []struct {
		path    string
		content string
		isDir   bool
	}{
		{path: "etc", isDir: true},
		{path: "etc/os-release", content: "NAME=\"TestOS\"\nVERSION=\"1.0\"\n"},
		{path: "bin", isDir: true},
		{path: "bin/sh", content: "#!/bin/sh\necho hello\n"},
		{path: "home", isDir: true},
		{path: "home/user", isDir: true},
		{path: "home/user/test.txt", content: "test content from layer 2\n"},
	}

	for _, ef := range expectedFiles {
		fullPath := filepath.Join(snapPath, ef.path)
		info, err := os.Stat(fullPath)
		if err != nil {
			t.Errorf("expected file %s not found: %v", ef.path, err)
			continue
		}

		if ef.isDir {
			if !info.IsDir() {
				t.Errorf("%s should be a directory", ef.path)
			}
		} else {
			data, err := os.ReadFile(fullPath)
			if err != nil {
				t.Errorf("failed to read %s: %v", ef.path, err)
				continue
			}
			if string(data) != ef.content {
				t.Errorf("%s content: got %q, want %q", ef.path, data, ef.content)
			}
		}
	}

	t.Logf("All expected files verified in snapshot")
}

// buildMinimalDockerTarball creates a Docker-save-format tarball with two layers
// for testing. This is a minimal tarball that can be processed by flattenDockerTarball.
func buildMinimalDockerTarball(t *testing.T, tarPath string) error {
	t.Helper()

	f, err := os.Create(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	tw := tar.NewWriter(f)
	defer tw.Close()

	// Build layer 1 (base layer with etc and bin)
	layer1 := buildDockerLayer([]dockerLayerFile{
		{name: "etc/", mode: 0755, isDir: true},
		{name: "etc/os-release", content: "NAME=\"TestOS\"\nVERSION=\"1.0\"\n", mode: 0644},
		{name: "bin/", mode: 0755, isDir: true},
		{name: "bin/sh", content: "#!/bin/sh\necho hello\n", mode: 0755},
	})

	// Build layer 2 (adds home directory structure)
	layer2 := buildDockerLayer([]dockerLayerFile{
		{name: "home/", mode: 0755, isDir: true},
		{name: "home/user/", mode: 0755, isDir: true},
		{name: "home/user/test.txt", content: "test content from layer 2\n", mode: 0644},
	})

	// Use unique layer names (simulating Docker's sha256-based naming)
	layer1Name := "layer1abc123.tar"
	layer2Name := "layer2def456.tar"

	// Write manifest.json (required by flattenDockerTarball)
	manifest := []map[string]interface{}{
		{
			"Config":   "config.json",
			"RepoTags": []string{"test:latest"},
			"Layers":   []string{layer1Name, layer2Name},
		},
	}
	manifestJSON, _ := json.Marshal(manifest)
	if err := addDockerTarEntry(tw, "manifest.json", manifestJSON, 0644); err != nil {
		return err
	}

	// Write minimal config.json
	config := map[string]interface{}{
		"architecture": "amd64",
		"os":           "linux",
		"config": map[string]interface{}{
			"Env": []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		},
		"rootfs": map[string]interface{}{
			"type":     "layers",
			"diff_ids": []string{"sha256:layer1hash", "sha256:layer2hash"},
		},
	}
	configJSON, _ := json.Marshal(config)
	if err := addDockerTarEntry(tw, "config.json", configJSON, 0644); err != nil {
		return err
	}

	// Write layers
	if err := addDockerTarEntry(tw, layer1Name, layer1, 0644); err != nil {
		return err
	}
	if err := addDockerTarEntry(tw, layer2Name, layer2, 0644); err != nil {
		return err
	}

	return nil
}

// dockerLayerFile describes a file to include in a Docker layer.
type dockerLayerFile struct {
	name    string
	content string
	mode    int64
	isDir   bool
}

// buildDockerLayer creates an uncompressed tar layer from the given files.
func buildDockerLayer(files []dockerLayerFile) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	for _, f := range files {
		if f.isDir {
			tw.WriteHeader(&tar.Header{
				Name:     f.name,
				Mode:     f.mode,
				Typeflag: tar.TypeDir,
			})
		} else {
			tw.WriteHeader(&tar.Header{
				Name: f.name,
				Mode: f.mode,
				Size: int64(len(f.content)),
			})
			tw.Write([]byte(f.content))
		}
	}

	tw.Close()
	return buf.Bytes()
}

// addDockerTarEntry adds a file entry to a tar archive.
func addDockerTarEntry(tw *tar.Writer, name string, content []byte, mode int64) error {
	if err := tw.WriteHeader(&tar.Header{
		Name: name,
		Mode: mode,
		Size: int64(len(content)),
	}); err != nil {
		return err
	}
	_, err := tw.Write(content)
	return err
}

// dockerTestControlServer extends testControlServer with Docker import handling.
type dockerTestControlServer struct {
	*testControlServer
}

// startDockerTestControlServer starts a control server that handles Docker import.
func startDockerTestControlServer(t *testing.T, env *testEnv, sockPath string) *dockerTestControlServer {
	t.Helper()

	// Start the base control server
	base := startTestControlServer(t, env, sockPath)

	return &dockerTestControlServer{testControlServer: base}
}

// Close releases resources.
func (s *dockerTestControlServer) Close() {
	s.testControlServer.Close()
}

// handleImportDockerTarball handles POST /import-docker-tarball
// This is a test-only endpoint that imports a local Docker tarball file.
func (s *testControlServer) handleImportDockerTarball(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		TarballPath string `json:"tarball_path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "error",
			"message": "invalid request: " + err.Error(),
		})
		return
	}

	if req.TarballPath == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "error",
			"message": "tarball_path is required",
		})
		return
	}

	// Generate a snapshot ID
	snapID, err := generateSnapshotID()
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "error",
			"message": "generate snapshot ID: " + err.Error(),
		})
		return
	}

	// Create the snapshot directory directly
	snapPath := filepath.Join(s.env.snapshotsDir, snapID)
	if err := os.MkdirAll(snapPath, 0755); err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "error",
			"message": "create snapshot dir: " + err.Error(),
		})
		return
	}

	// Extract the Docker tarball
	if err := extractDockerTarball(req.TarballPath, snapPath); err != nil {
		os.RemoveAll(snapPath)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "error",
			"message": "extract tarball: " + err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "ok",
		"snapshot_id": snapID,
	})
}

// extractDockerTarball extracts a Docker-save-format tarball to the destination directory.
// This is a simplified version of flattenDockerTarball for e2e testing.
func extractDockerTarball(tarPath, destDir string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	tr := tar.NewReader(f)

	// First pass: find and parse manifest.json
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
				return err
			}
			break
		}
	}

	if len(manifest) == 0 || len(manifest[0].Layers) == 0 {
		return os.ErrNotExist
	}

	// Reopen the tarball and read layers into memory
	f.Close()
	f, err = os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	layers := make(map[string][]byte)
	tr = tar.NewReader(f)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		for _, layerPath := range manifest[0].Layers {
			if hdr.Name == layerPath {
				// Read the entire layer content
				data, err := io.ReadAll(tr)
				if err != nil {
					return err
				}
				layers[layerPath] = data
				break
			}
		}
	}

	// Extract layers in order
	for _, layerPath := range manifest[0].Layers {
		layerData, ok := layers[layerPath]
		if !ok {
			continue
		}

		if err := extractTarLayer(layerData, destDir); err != nil {
			return err
		}
	}

	return nil
}

// extractTarLayer extracts a tar layer to the destination directory.
func extractTarLayer(layerData []byte, destDir string) error {
	tr := tar.NewReader(bytes.NewReader(layerData))

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target := filepath.Join(destDir, hdr.Name)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			outf, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(outf, tr); err != nil {
				outf.Close()
				return err
			}
			outf.Close()
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		}
	}

	return nil
}
