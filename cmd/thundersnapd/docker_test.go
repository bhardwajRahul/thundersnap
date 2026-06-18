package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// TestFlattenDockerTarball tests the docker tarball flattening logic without
// requiring root access or btrfs. It creates a synthetic docker-save-format
// tarball and verifies the extraction produces the correct filesystem.
func TestFlattenDockerTarball(t *testing.T) {
	// Create temp directories
	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "image.tar")
	destDir := filepath.Join(tmpDir, "extracted")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Build a synthetic Docker save tarball with two layers
	if err := buildTestDockerTarball(tarPath); err != nil {
		t.Fatalf("failed to build test tarball: %v", err)
	}

	// Run flattenDockerTarball
	if err := flattenDockerTarball(tarPath, destDir, nil); err != nil {
		t.Fatalf("flattenDockerTarball: %v", err)
	}

	// Verify expected files exist
	checks := []struct {
		path    string
		content string
		isDir   bool
	}{
		{path: "etc", isDir: true},
		{path: "etc/alpine-release", content: "3.14.0\n"},
		{path: "bin", isDir: true},
		{path: "bin/busybox", content: "busybox-binary"},
		{path: "home", isDir: true},
		{path: "home/user", isDir: true},
		{path: "home/user/file.txt", content: "layer2 content\n"},
	}

	for _, c := range checks {
		p := filepath.Join(destDir, c.path)
		fi, err := os.Stat(p)
		if err != nil {
			t.Errorf("expected %s to exist: %v", c.path, err)
			continue
		}
		if c.isDir {
			if !fi.IsDir() {
				t.Errorf("%s should be a directory", c.path)
			}
		} else {
			data, err := os.ReadFile(p)
			if err != nil {
				t.Errorf("failed to read %s: %v", c.path, err)
				continue
			}
			if string(data) != c.content {
				t.Errorf("%s content: got %q, want %q", c.path, data, c.content)
			}
		}
	}
}

// TestFlattenDockerTarballWhiteouts tests that whiteout files are correctly
// handled during layer extraction.
func TestFlattenDockerTarballWhiteouts(t *testing.T) {
	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "image.tar")
	destDir := filepath.Join(tmpDir, "extracted")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Build tarball with whiteout handling
	if err := buildTestDockerTarballWithWhiteouts(tarPath); err != nil {
		t.Fatalf("failed to build test tarball: %v", err)
	}

	if err := flattenDockerTarball(tarPath, destDir, nil); err != nil {
		t.Fatalf("flattenDockerTarball: %v", err)
	}

	// Verify whiteout was applied: /etc/removed-file should not exist
	removedPath := filepath.Join(destDir, "etc", "removed-file")
	if _, err := os.Stat(removedPath); !os.IsNotExist(err) {
		t.Errorf("whiteout file should have been deleted, but exists: %s", removedPath)
	}

	// Verify /etc/kept-file still exists
	keptPath := filepath.Join(destDir, "etc", "kept-file")
	if _, err := os.Stat(keptPath); err != nil {
		t.Errorf("kept-file should exist: %v", err)
	}
}

// TestFlattenDockerTarballOpaqueWhiteout tests opaque directory whiteouts.
func TestFlattenDockerTarballOpaqueWhiteout(t *testing.T) {
	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "image.tar")
	destDir := filepath.Join(tmpDir, "extracted")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		t.Fatal(err)
	}

	if err := buildTestDockerTarballWithOpaqueWhiteout(tarPath); err != nil {
		t.Fatalf("failed to build test tarball: %v", err)
	}

	if err := flattenDockerTarball(tarPath, destDir, nil); err != nil {
		t.Fatalf("flattenDockerTarball: %v", err)
	}

	// After opaque whiteout, /var should only contain new-file, not old-file
	oldPath := filepath.Join(destDir, "var", "old-file")
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("opaque whiteout should have deleted old-file")
	}

	newPath := filepath.Join(destDir, "var", "new-file")
	if _, err := os.Stat(newPath); err != nil {
		t.Errorf("new-file should exist after opaque whiteout: %v", err)
	}
}

// TestExtractLayerSymlinks tests symlink extraction.
func TestExtractLayerSymlinks(t *testing.T) {
	tmpDir := t.TempDir()
	layerPath := filepath.Join(tmpDir, "layer.tar")
	destDir := filepath.Join(tmpDir, "extracted")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Build layer with symlinks
	if err := buildLayerWithSymlinks(layerPath); err != nil {
		t.Fatalf("failed to build layer: %v", err)
	}

	if err := extractLayer(layerPath, destDir); err != nil {
		t.Fatalf("extractLayer: %v", err)
	}

	// Verify symlink
	linkPath := filepath.Join(destDir, "usr", "bin", "python")
	target, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("failed to read symlink: %v", err)
	}
	if target != "python3" {
		t.Errorf("symlink target: got %q, want %q", target, "python3")
	}
}

// TestExtractLayerHardlinks tests hard link extraction.
func TestExtractLayerHardlinks(t *testing.T) {
	tmpDir := t.TempDir()
	layerPath := filepath.Join(tmpDir, "layer.tar")
	destDir := filepath.Join(tmpDir, "extracted")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		t.Fatal(err)
	}

	if err := buildLayerWithHardlinks(layerPath); err != nil {
		t.Fatalf("failed to build layer: %v", err)
	}

	if err := extractLayer(layerPath, destDir); err != nil {
		t.Fatalf("extractLayer: %v", err)
	}

	// Both files should exist and have the same content
	orig := filepath.Join(destDir, "data", "original.txt")
	link := filepath.Join(destDir, "data", "hardlink.txt")

	origData, err := os.ReadFile(orig)
	if err != nil {
		t.Fatalf("failed to read original: %v", err)
	}
	linkData, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("failed to read hardlink: %v", err)
	}

	if string(origData) != string(linkData) {
		t.Errorf("hardlink content mismatch: %q vs %q", origData, linkData)
	}
}

// TestFlattenDockerTarballGzippedLayers tests extraction of gzip-compressed layers.
// go-containerregistry's tarball.Write() produces gzip-compressed layer files,
// so this is essential for real Docker image support.
func TestFlattenDockerTarballGzippedLayers(t *testing.T) {
	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "image.tar")
	destDir := filepath.Join(tmpDir, "extracted")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		t.Fatal(err)
	}

	if err := buildTestDockerTarballWithGzippedLayers(tarPath); err != nil {
		t.Fatalf("failed to build test tarball: %v", err)
	}

	if err := flattenDockerTarball(tarPath, destDir, nil); err != nil {
		t.Fatalf("flattenDockerTarball: %v", err)
	}

	// Verify extraction worked
	testFile := filepath.Join(destDir, "test.txt")
	data, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("failed to read test.txt: %v", err)
	}
	if string(data) != "gzipped content\n" {
		t.Errorf("content: got %q, want %q", data, "gzipped content\n")
	}
}

// TestFlattenDockerTarballMissingManifest tests error handling for invalid tarballs.
func TestFlattenDockerTarballMissingManifest(t *testing.T) {
	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "image.tar")
	destDir := filepath.Join(tmpDir, "extracted")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create an empty tarball (no manifest.json)
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	tw.Close()
	f.Close()

	err = flattenDockerTarball(tarPath, destDir, nil)
	if err == nil {
		t.Error("expected error for missing manifest")
	}
}

// TestExtractLayerInvalidTarHeader tests that invalid tar headers are reported.
func TestExtractLayerInvalidTarHeader(t *testing.T) {
	tmpDir := t.TempDir()
	layerPath := filepath.Join(tmpDir, "layer.tar")
	destDir := filepath.Join(tmpDir, "extracted")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write garbage that looks like a tar but isn't valid
	if err := os.WriteFile(layerPath, []byte("not a valid tar file contents here"), 0644); err != nil {
		t.Fatal(err)
	}

	err := extractLayer(layerPath, destDir)
	if err == nil {
		t.Error("expected error for invalid tar")
	}
}

// TestFlattenDockerTarballGoContainerRegistry tests the docker tarball flattening
// with a tarball created using go-containerregistry, which is what the actual
// downloadDockerImage function uses. This ensures we handle the exact format
// that tarball.Write() produces.
func TestFlattenDockerTarballGoContainerRegistry(t *testing.T) {
	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "image.tar")
	destDir := filepath.Join(tmpDir, "extracted")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Build a layer tarball with test content
	layerContent := buildLayer([]testFile{
		{name: "etc/", mode: 0755, isDir: true},
		{name: "etc/os-release", content: "NAME=\"Test\"\nVERSION=\"1.0\"\n", mode: 0644},
		{name: "bin/", mode: 0755, isDir: true},
		{name: "bin/sh", content: "#!/bin/sh\necho hello\n", mode: 0755},
	})

	// Create an image using go-containerregistry
	layer, err := tarball.LayerFromReader(bytes.NewReader(layerContent))
	if err != nil {
		t.Fatalf("failed to create layer: %v", err)
	}

	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatalf("failed to append layer: %v", err)
	}

	// Write the image as a Docker save tarball
	ref, err := name.ParseReference("test:latest")
	if err != nil {
		t.Fatalf("failed to parse reference: %v", err)
	}

	tarFile, err := os.Create(tarPath)
	if err != nil {
		t.Fatalf("failed to create tar file: %v", err)
	}

	if err := tarball.Write(ref, img, tarFile); err != nil {
		tarFile.Close()
		t.Fatalf("failed to write tarball: %v", err)
	}
	tarFile.Close()

	// Now test flattenDockerTarball
	if err := flattenDockerTarball(tarPath, destDir, nil); err != nil {
		t.Fatalf("flattenDockerTarball: %v", err)
	}

	// Verify expected files exist
	checks := []struct {
		path    string
		content string
	}{
		{path: "etc/os-release", content: "NAME=\"Test\"\nVERSION=\"1.0\"\n"},
		{path: "bin/sh", content: "#!/bin/sh\necho hello\n"},
	}

	for _, c := range checks {
		p := filepath.Join(destDir, c.path)
		data, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("failed to read %s: %v", c.path, err)
			continue
		}
		if string(data) != c.content {
			t.Errorf("%s content: got %q, want %q", c.path, data, c.content)
		}
	}
}

// TestFlattenDockerTarballGoContainerRegistryMultipleLayers tests extraction
// with multiple layers to ensure layer ordering is correct.
func TestFlattenDockerTarballGoContainerRegistryMultipleLayers(t *testing.T) {
	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "image.tar")
	destDir := filepath.Join(tmpDir, "extracted")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Layer 1: base files
	layer1Content := buildLayer([]testFile{
		{name: "data/", mode: 0755, isDir: true},
		{name: "data/base.txt", content: "base layer\n", mode: 0644},
		{name: "data/override.txt", content: "from base\n", mode: 0644},
	})

	// Layer 2: overrides and adds files
	layer2Content := buildLayer([]testFile{
		{name: "data/override.txt", content: "from overlay\n", mode: 0644},
		{name: "data/new.txt", content: "new file\n", mode: 0644},
	})

	layer1, err := tarball.LayerFromReader(bytes.NewReader(layer1Content))
	if err != nil {
		t.Fatalf("failed to create layer1: %v", err)
	}
	layer2, err := tarball.LayerFromReader(bytes.NewReader(layer2Content))
	if err != nil {
		t.Fatalf("failed to create layer2: %v", err)
	}

	img, err := mutate.AppendLayers(empty.Image, layer1, layer2)
	if err != nil {
		t.Fatalf("failed to append layers: %v", err)
	}

	ref, _ := name.ParseReference("test:latest")
	tarFile, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := tarball.Write(ref, img, tarFile); err != nil {
		tarFile.Close()
		t.Fatalf("failed to write tarball: %v", err)
	}
	tarFile.Close()

	if err := flattenDockerTarball(tarPath, destDir, nil); err != nil {
		t.Fatalf("flattenDockerTarball: %v", err)
	}

	// Verify layer ordering - override.txt should have content from layer 2
	checks := []struct {
		path    string
		content string
	}{
		{path: "data/base.txt", content: "base layer\n"},
		{path: "data/override.txt", content: "from overlay\n"}, // layer2 overrides layer1
		{path: "data/new.txt", content: "new file\n"},
	}

	for _, c := range checks {
		p := filepath.Join(destDir, c.path)
		data, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("failed to read %s: %v", c.path, err)
			continue
		}
		if string(data) != c.content {
			t.Errorf("%s content: got %q, want %q", c.path, data, c.content)
		}
	}
}

// TestFlattenDockerTarballGoContainerRegistryWithWhiteout tests whiteout
// handling with layers created by go-containerregistry.
func TestFlattenDockerTarballGoContainerRegistryWithWhiteout(t *testing.T) {
	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "image.tar")
	destDir := filepath.Join(tmpDir, "extracted")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Layer 1: create files
	layer1Content := buildLayer([]testFile{
		{name: "config/", mode: 0755, isDir: true},
		{name: "config/keep.conf", content: "keep this\n", mode: 0644},
		{name: "config/delete.conf", content: "delete this\n", mode: 0644},
	})

	// Layer 2: whiteout delete.conf
	layer2Content := buildLayer([]testFile{
		{name: "config/.wh.delete.conf", content: "", mode: 0644},
	})

	layer1, _ := tarball.LayerFromReader(bytes.NewReader(layer1Content))
	layer2, _ := tarball.LayerFromReader(bytes.NewReader(layer2Content))

	img, _ := mutate.AppendLayers(empty.Image, layer1, layer2)
	ref, _ := name.ParseReference("test:latest")
	tarFile, _ := os.Create(tarPath)
	tarball.Write(ref, img, tarFile)
	tarFile.Close()

	if err := flattenDockerTarball(tarPath, destDir, nil); err != nil {
		t.Fatalf("flattenDockerTarball: %v", err)
	}

	// keep.conf should exist
	if _, err := os.Stat(filepath.Join(destDir, "config", "keep.conf")); err != nil {
		t.Errorf("keep.conf should exist: %v", err)
	}

	// delete.conf should NOT exist (whiteout)
	if _, err := os.Stat(filepath.Join(destDir, "config", "delete.conf")); !os.IsNotExist(err) {
		t.Errorf("delete.conf should have been deleted by whiteout")
	}

	// .wh.delete.conf should NOT exist in the output
	if _, err := os.Stat(filepath.Join(destDir, "config", ".wh.delete.conf")); !os.IsNotExist(err) {
		t.Errorf(".wh.delete.conf marker should not be extracted")
	}
}

// TestExtractLayerGzipCompressed tests that gzip-compressed layers are
// properly detected and decompressed. go-containerregistry's tarball.Write()
// produces gzip-compressed layer files with .tar.gz extension.
func TestExtractLayerGzipCompressed(t *testing.T) {
	tmpDir := t.TempDir()
	layerPath := filepath.Join(tmpDir, "layer.tar.gz")
	destDir := filepath.Join(tmpDir, "extracted")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create gzip-compressed layer
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	content := []byte("compressed file content\n")
	tw.WriteHeader(&tar.Header{
		Name: "test.txt",
		Mode: 0644,
		Size: int64(len(content)),
	})
	tw.Write(content)
	tw.Close()
	gw.Close()

	if err := os.WriteFile(layerPath, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	if err := extractLayer(layerPath, destDir); err != nil {
		t.Fatalf("extractLayer: %v", err)
	}

	// Verify content was extracted correctly
	data, err := os.ReadFile(filepath.Join(destDir, "test.txt"))
	if err != nil {
		t.Fatalf("failed to read extracted file: %v", err)
	}
	if string(data) != "compressed file content\n" {
		t.Errorf("content mismatch: got %q, want %q", data, "compressed file content\n")
	}
}

// Helper functions to build test tarballs

// buildTestDockerTarball creates a Docker-save-format tarball with two layers.
// Uses unique layer file names to match real Docker save format (hash-based names).
func buildTestDockerTarball(tarPath string) error {
	f, err := os.Create(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	tw := tar.NewWriter(f)
	defer tw.Close()

	// Build layer 1 (base layer)
	layer1 := buildLayer([]testFile{
		{name: "etc/", mode: 0755, isDir: true},
		{name: "etc/alpine-release", content: "3.14.0\n", mode: 0644},
		{name: "bin/", mode: 0755, isDir: true},
		{name: "bin/busybox", content: "busybox-binary", mode: 0755},
	})

	// Build layer 2 (adds files)
	layer2 := buildLayer([]testFile{
		{name: "home/", mode: 0755, isDir: true},
		{name: "home/user/", mode: 0755, isDir: true},
		{name: "home/user/file.txt", content: "layer2 content\n", mode: 0644},
	})

	// Use unique filenames (simulating Docker's sha256-based naming)
	layer1Name := "abc123layer1.tar"
	layer2Name := "def456layer2.tar"

	// Write manifest.json
	manifest := []map[string]interface{}{
		{
			"Layers": []string{layer1Name, layer2Name},
		},
	}
	manifestJSON, _ := json.Marshal(manifest)
	if err := addTarEntry(tw, "manifest.json", manifestJSON, 0644); err != nil {
		return err
	}

	// Write layers
	if err := addTarEntry(tw, layer1Name, layer1, 0644); err != nil {
		return err
	}
	if err := addTarEntry(tw, layer2Name, layer2, 0644); err != nil {
		return err
	}

	return nil
}

// buildTestDockerTarballWithWhiteouts creates a tarball testing whiteout handling.
func buildTestDockerTarballWithWhiteouts(tarPath string) error {
	f, err := os.Create(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	tw := tar.NewWriter(f)
	defer tw.Close()

	// Layer 1: create files
	layer1 := buildLayer([]testFile{
		{name: "etc/", mode: 0755, isDir: true},
		{name: "etc/removed-file", content: "to be deleted\n", mode: 0644},
		{name: "etc/kept-file", content: "should remain\n", mode: 0644},
	})

	// Layer 2: whiteout etc/removed-file
	layer2 := buildLayer([]testFile{
		{name: "etc/.wh.removed-file", content: "", mode: 0644},
	})

	layer1Name := "whiteout_layer1.tar"
	layer2Name := "whiteout_layer2.tar"

	manifest := []map[string]interface{}{
		{"Layers": []string{layer1Name, layer2Name}},
	}
	manifestJSON, _ := json.Marshal(manifest)

	addTarEntry(tw, "manifest.json", manifestJSON, 0644)
	addTarEntry(tw, layer1Name, layer1, 0644)
	addTarEntry(tw, layer2Name, layer2, 0644)

	return nil
}

// buildTestDockerTarballWithOpaqueWhiteout tests opaque directory whiteouts.
func buildTestDockerTarballWithOpaqueWhiteout(tarPath string) error {
	f, err := os.Create(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	tw := tar.NewWriter(f)
	defer tw.Close()

	// Layer 1: create /var with old-file
	layer1 := buildLayer([]testFile{
		{name: "var/", mode: 0755, isDir: true},
		{name: "var/old-file", content: "old content\n", mode: 0644},
	})

	// Layer 2: opaque whiteout clears /var, then adds new-file
	layer2 := buildLayer([]testFile{
		{name: "var/", mode: 0755, isDir: true},
		{name: "var/.wh..wh..opq", content: "", mode: 0644},
		{name: "var/new-file", content: "new content\n", mode: 0644},
	})

	layer1Name := "opaque_layer1.tar"
	layer2Name := "opaque_layer2.tar"

	manifest := []map[string]interface{}{
		{"Layers": []string{layer1Name, layer2Name}},
	}
	manifestJSON, _ := json.Marshal(manifest)

	addTarEntry(tw, "manifest.json", manifestJSON, 0644)
	addTarEntry(tw, layer1Name, layer1, 0644)
	addTarEntry(tw, layer2Name, layer2, 0644)

	return nil
}

// buildTestDockerTarballWithGzippedLayers tests gzip-compressed layers.
func buildTestDockerTarballWithGzippedLayers(tarPath string) error {
	f, err := os.Create(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	tw := tar.NewWriter(f)
	defer tw.Close()

	// Build a gzipped layer
	var layerBuf bytes.Buffer
	gw := gzip.NewWriter(&layerBuf)
	layerTw := tar.NewWriter(gw)

	hdr := &tar.Header{
		Name: "test.txt",
		Mode: 0644,
		Size: int64(len("gzipped content\n")),
	}
	layerTw.WriteHeader(hdr)
	layerTw.Write([]byte("gzipped content\n"))
	layerTw.Close()
	gw.Close()

	layerName := "gzipped_layer1.tar.gz"

	manifest := []map[string]interface{}{
		{"Layers": []string{layerName}},
	}
	manifestJSON, _ := json.Marshal(manifest)

	addTarEntry(tw, "manifest.json", manifestJSON, 0644)
	addTarEntry(tw, layerName, layerBuf.Bytes(), 0644)

	return nil
}

// buildLayerWithSymlinks creates a layer tar with symlinks.
func buildLayerWithSymlinks(layerPath string) error {
	f, err := os.Create(layerPath)
	if err != nil {
		return err
	}
	defer f.Close()

	tw := tar.NewWriter(f)
	defer tw.Close()

	// Create directory structure
	tw.WriteHeader(&tar.Header{Name: "usr/", Mode: 0755, Typeflag: tar.TypeDir})
	tw.WriteHeader(&tar.Header{Name: "usr/bin/", Mode: 0755, Typeflag: tar.TypeDir})

	// Create target file
	content := []byte("#!/usr/bin/env python3\n")
	tw.WriteHeader(&tar.Header{Name: "usr/bin/python3", Mode: 0755, Size: int64(len(content))})
	tw.Write(content)

	// Create symlink
	tw.WriteHeader(&tar.Header{
		Name:     "usr/bin/python",
		Mode:     0777,
		Typeflag: tar.TypeSymlink,
		Linkname: "python3",
	})

	return nil
}

// buildLayerWithHardlinks creates a layer tar with hard links.
func buildLayerWithHardlinks(layerPath string) error {
	f, err := os.Create(layerPath)
	if err != nil {
		return err
	}
	defer f.Close()

	tw := tar.NewWriter(f)
	defer tw.Close()

	// Create directory
	tw.WriteHeader(&tar.Header{Name: "data/", Mode: 0755, Typeflag: tar.TypeDir})

	// Create original file
	content := []byte("shared content\n")
	tw.WriteHeader(&tar.Header{Name: "data/original.txt", Mode: 0644, Size: int64(len(content))})
	tw.Write(content)

	// Create hard link
	tw.WriteHeader(&tar.Header{
		Name:     "data/hardlink.txt",
		Mode:     0644,
		Typeflag: tar.TypeLink,
		Linkname: "data/original.txt",
	})

	return nil
}

type testFile struct {
	name    string
	content string
	mode    int64
	isDir   bool
}

// buildLayer creates an in-memory layer tarball.
func buildLayer(files []testFile) []byte {
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

// addTarEntry adds a file entry to a tar archive.
func addTarEntry(tw *tar.Writer, name string, content []byte, mode int64) error {
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
