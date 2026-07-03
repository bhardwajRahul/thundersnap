// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// TestControlServerManagerRefCounting tests that controlServerManager correctly
// shares control servers across multiple sessions to the same rootFS and only
// removes the socket when all sessions have disconnected.
//
// This tests the fix for the bug where one SSH session exiting would delete
// thunder.sock, breaking other concurrent sessions to the same container.
func TestControlServerManagerRefCounting(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a fake rootFS directory
	rootFS := filepath.Join(tmpDir, "fs", "testuser", "testframe")
	if err := os.MkdirAll(rootFS, 0755); err != nil {
		t.Fatalf("mkdir rootFS: %v", err)
	}

	// Create a fresh manager for this test (don't use the global one)
	manager := &controlServerManager{
		servers: make(map[string]*managedControlServer),
	}

	sockPath := filepath.Join(rootFS, "thunder.sock")

	// Session 1 connects
	cs1, err := manager.getOrCreateControlServer(rootFS)
	if err != nil {
		t.Fatalf("session 1 getOrCreateControlServer: %v", err)
	}
	if cs1 == nil {
		t.Fatal("session 1 got nil control server")
	}

	// Verify socket was created
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("thunder.sock not created after session 1: %v", err)
	}
	t.Log("Session 1 connected, thunder.sock exists")

	// Verify we can connect to the socket
	conn1, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("cannot connect to thunder.sock after session 1: %v", err)
	}
	conn1.Close()

	// Session 2 connects to the same rootFS
	cs2, err := manager.getOrCreateControlServer(rootFS)
	if err != nil {
		t.Fatalf("session 2 getOrCreateControlServer: %v", err)
	}

	// Should get the same control server (shared)
	if cs1 != cs2 {
		t.Error("session 2 should get the same control server instance as session 1")
	}
	t.Log("Session 2 connected, sharing control server with session 1")

	// Verify refCount is 2
	manager.mu.Lock()
	ms := manager.servers[rootFS]
	if ms == nil {
		manager.mu.Unlock()
		t.Fatal("managedControlServer not found in manager")
	}
	if ms.refCount != 2 {
		t.Errorf("refCount should be 2, got %d", ms.refCount)
	}
	manager.mu.Unlock()

	// Session 2 disconnects
	manager.releaseControlServer(rootFS)
	t.Log("Session 2 disconnected")

	// Socket should still exist (session 1 is still connected)
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("thunder.sock should still exist after session 2 disconnects: %v", err)
	}

	// Verify we can still connect
	conn2, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("cannot connect to thunder.sock after session 2 disconnects: %v", err)
	}
	conn2.Close()
	t.Log("thunder.sock still works after session 2 disconnected")

	// Session 1 disconnects
	manager.releaseControlServer(rootFS)
	t.Log("Session 1 disconnected")

	// Now the socket should be removed
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Errorf("thunder.sock should be removed after all sessions disconnect, but stat returned: %v", err)
	}
	t.Log("thunder.sock removed after all sessions disconnected")

	// Verify the manager no longer tracks this rootFS
	manager.mu.Lock()
	if _, ok := manager.servers[rootFS]; ok {
		t.Error("manager should not track rootFS after all sessions disconnected")
	}
	manager.mu.Unlock()
}

// TestControlServerManagerMultipleRootFS tests that different rootFS paths
// get independent control servers.
func TestControlServerManagerMultipleRootFS(t *testing.T) {
	tmpDir := t.TempDir()

	// Create two fake rootFS directories
	rootFS1 := filepath.Join(tmpDir, "fs", "user1", "frame1")
	rootFS2 := filepath.Join(tmpDir, "fs", "user1", "frame2")
	for _, dir := range []string{rootFS1, rootFS2} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	manager := &controlServerManager{
		servers: make(map[string]*managedControlServer),
	}

	// Connect to rootFS1
	cs1, err := manager.getOrCreateControlServer(rootFS1)
	if err != nil {
		t.Fatalf("getOrCreateControlServer rootFS1: %v", err)
	}

	// Connect to rootFS2
	cs2, err := manager.getOrCreateControlServer(rootFS2)
	if err != nil {
		t.Fatalf("getOrCreateControlServer rootFS2: %v", err)
	}

	// Should be different control servers
	if cs1 == cs2 {
		t.Error("different rootFS should get different control servers")
	}

	// Both sockets should exist
	sock1 := filepath.Join(rootFS1, "thunder.sock")
	sock2 := filepath.Join(rootFS2, "thunder.sock")

	if _, err := os.Stat(sock1); err != nil {
		t.Errorf("sock1 should exist: %v", err)
	}
	if _, err := os.Stat(sock2); err != nil {
		t.Errorf("sock2 should exist: %v", err)
	}

	// Release rootFS1
	manager.releaseControlServer(rootFS1)

	// sock1 should be gone, sock2 should still exist
	if _, err := os.Stat(sock1); !os.IsNotExist(err) {
		t.Errorf("sock1 should be removed after release")
	}
	if _, err := os.Stat(sock2); err != nil {
		t.Errorf("sock2 should still exist: %v", err)
	}

	// Release rootFS2
	manager.releaseControlServer(rootFS2)

	if _, err := os.Stat(sock2); !os.IsNotExist(err) {
		t.Errorf("sock2 should be removed after release")
	}
}

// TestControlServerManagerConcurrentAccess tests that the manager handles
// concurrent getOrCreate and release calls safely.
func TestControlServerManagerConcurrentAccess(t *testing.T) {
	tmpDir := t.TempDir()
	rootFS := filepath.Join(tmpDir, "fs", "concurrent", "frame")
	if err := os.MkdirAll(rootFS, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	manager := &controlServerManager{
		servers: make(map[string]*managedControlServer),
	}

	// Simulate 10 concurrent sessions connecting
	numSessions := 10
	done := make(chan error, numSessions)

	for i := 0; i < numSessions; i++ {
		go func() {
			_, err := manager.getOrCreateControlServer(rootFS)
			done <- err
		}()
	}

	// Wait for all to connect
	for i := 0; i < numSessions; i++ {
		if err := <-done; err != nil {
			t.Errorf("concurrent connect error: %v", err)
		}
	}

	// Verify refCount
	manager.mu.Lock()
	ms := manager.servers[rootFS]
	if ms == nil {
		manager.mu.Unlock()
		t.Fatal("server not found")
	}
	if ms.refCount != numSessions {
		t.Errorf("refCount should be %d, got %d", numSessions, ms.refCount)
	}
	manager.mu.Unlock()

	// Socket should exist
	sockPath := filepath.Join(rootFS, "thunder.sock")
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("socket should exist: %v", err)
	}

	// All sessions disconnect concurrently
	releaseDone := make(chan struct{}, numSessions)
	for i := 0; i < numSessions; i++ {
		go func() {
			manager.releaseControlServer(rootFS)
			releaseDone <- struct{}{}
		}()
	}

	for i := 0; i < numSessions; i++ {
		<-releaseDone
	}

	// Socket should be gone
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Errorf("socket should be removed after all releases")
	}

	// Manager should be empty
	manager.mu.Lock()
	if len(manager.servers) != 0 {
		t.Errorf("manager should be empty, has %d entries", len(manager.servers))
	}
	manager.mu.Unlock()
}
