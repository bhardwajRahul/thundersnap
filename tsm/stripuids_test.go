package tsm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureUserInPasswd(t *testing.T) {
	t.Run("user already exists", func(t *testing.T) {
		tmpDir := t.TempDir()
		rootfs := filepath.Join(tmpDir, "rootfs")
		etcDir := filepath.Join(rootfs, "etc")
		if err := os.MkdirAll(etcDir, 0755); err != nil {
			t.Fatal(err)
		}

		// passwd already has "user" with home /home/user
		passwd := "root::0:0:root:/root:/bin/bash\nuser:x:1000:1000:User:/home/user:/bin/bash\n"
		if err := os.WriteFile(filepath.Join(etcDir, "passwd"), []byte(passwd), 0644); err != nil {
			t.Fatal(err)
		}

		home, err := EnsureUserInPasswd(rootfs)
		if err != nil {
			t.Fatalf("EnsureUserInPasswd: %v", err)
		}
		if home != "/home/user" {
			t.Errorf("expected home=/home/user, got %q", home)
		}

		// File should be unchanged
		got, _ := os.ReadFile(filepath.Join(etcDir, "passwd"))
		if string(got) != passwd {
			t.Errorf("file was modified unexpectedly:\n%s", got)
		}
	})

	t.Run("user does not exist", func(t *testing.T) {
		tmpDir := t.TempDir()
		rootfs := filepath.Join(tmpDir, "rootfs")
		etcDir := filepath.Join(rootfs, "etc")
		binDir := filepath.Join(rootfs, "bin")
		if err := os.MkdirAll(etcDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(binDir, 0755); err != nil {
			t.Fatal(err)
		}

		// passwd only has root
		passwd := "root::0:0:root:/root:/bin/bash\ndaemon:x:1:1:daemon:/usr/sbin:/bin/nologin\n"
		if err := os.WriteFile(filepath.Join(etcDir, "passwd"), []byte(passwd), 0644); err != nil {
			t.Fatal(err)
		}

		home, err := EnsureUserInPasswd(rootfs)
		if err != nil {
			t.Fatalf("EnsureUserInPasswd: %v", err)
		}
		if home != "/home" {
			t.Errorf("expected home=/home, got %q", home)
		}

		// Check that user was inserted after root with UID 7575
		got, _ := os.ReadFile(filepath.Join(etcDir, "passwd"))
		gotStr := string(got)
		expectedEntry := "user:x:7575:7575:user:/home:/bin/sh"
		if !strings.Contains(gotStr, expectedEntry) {
			t.Errorf("user entry not added correctly, expected %q in:\n%s", expectedEntry, gotStr)
		}

		// Verify order: user should appear after root and before daemon
		lines := strings.Split(strings.TrimSpace(gotStr), "\n")
		var rootIdx, userIdx, daemonIdx int = -1, -1, -1
		for i, line := range lines {
			if strings.HasPrefix(line, "root:") {
				rootIdx = i
			} else if strings.HasPrefix(line, "user:") {
				userIdx = i
			} else if strings.HasPrefix(line, "daemon:") {
				daemonIdx = i
			}
		}
		if rootIdx == -1 || userIdx == -1 || daemonIdx == -1 {
			t.Errorf("missing expected entries in output:\n%s", gotStr)
		}
		if !(rootIdx < userIdx && userIdx < daemonIdx) {
			t.Errorf("expected order root < user < daemon, got indices %d, %d, %d:\n%s",
				rootIdx, userIdx, daemonIdx, gotStr)
		}
	})

	t.Run("prefers bash if available", func(t *testing.T) {
		tmpDir := t.TempDir()
		rootfs := filepath.Join(tmpDir, "rootfs")
		etcDir := filepath.Join(rootfs, "etc")
		binDir := filepath.Join(rootfs, "bin")
		if err := os.MkdirAll(etcDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(binDir, 0755); err != nil {
			t.Fatal(err)
		}
		// Create a bash binary
		if err := os.WriteFile(filepath.Join(binDir, "bash"), []byte("#!/bin/sh\n"), 0755); err != nil {
			t.Fatal(err)
		}

		passwd := "root::0:0:root:/root:/bin/bash\n"
		if err := os.WriteFile(filepath.Join(etcDir, "passwd"), []byte(passwd), 0644); err != nil {
			t.Fatal(err)
		}

		_, err := EnsureUserInPasswd(rootfs)
		if err != nil {
			t.Fatalf("EnsureUserInPasswd: %v", err)
		}

		got, _ := os.ReadFile(filepath.Join(etcDir, "passwd"))
		if !strings.Contains(string(got), "user:x:7575:7575:user:/home:/bin/bash") {
			t.Errorf("expected bash shell:\n%s", got)
		}
	})

	t.Run("missing passwd file", func(t *testing.T) {
		tmpDir := t.TempDir()
		rootfs := filepath.Join(tmpDir, "rootfs")

		home, err := EnsureUserInPasswd(rootfs)
		if err != nil {
			t.Fatalf("EnsureUserInPasswd: %v", err)
		}
		if home != "" {
			t.Errorf("expected empty home for missing passwd, got %q", home)
		}
	})

	t.Run("idempotent", func(t *testing.T) {
		tmpDir := t.TempDir()
		rootfs := filepath.Join(tmpDir, "rootfs")
		etcDir := filepath.Join(rootfs, "etc")
		if err := os.MkdirAll(etcDir, 0755); err != nil {
			t.Fatal(err)
		}

		passwd := "root::0:0:root:/root:/bin/bash\n"
		if err := os.WriteFile(filepath.Join(etcDir, "passwd"), []byte(passwd), 0644); err != nil {
			t.Fatal(err)
		}

		// First call adds user
		home1, err := EnsureUserInPasswd(rootfs)
		if err != nil {
			t.Fatalf("first EnsureUserInPasswd: %v", err)
		}
		first, _ := os.ReadFile(filepath.Join(etcDir, "passwd"))

		// Second call should be a no-op
		home2, err := EnsureUserInPasswd(rootfs)
		if err != nil {
			t.Fatalf("second EnsureUserInPasswd: %v", err)
		}
		second, _ := os.ReadFile(filepath.Join(etcDir, "passwd"))

		if home1 != home2 {
			t.Errorf("home changed between calls: %q vs %q", home1, home2)
		}
		if string(first) != string(second) {
			t.Errorf("file changed between calls:\n1st: %s\n2nd: %s", first, second)
		}
	})

	t.Run("adds user to shadow when shadow exists", func(t *testing.T) {
		tmpDir := t.TempDir()
		rootfs := filepath.Join(tmpDir, "rootfs")
		etcDir := filepath.Join(rootfs, "etc")
		if err := os.MkdirAll(etcDir, 0755); err != nil {
			t.Fatal(err)
		}

		passwd := "root::0:0:root:/root:/bin/bash\n"
		shadow := "root:!:19000:0:99999:7:::\n"
		if err := os.WriteFile(filepath.Join(etcDir, "passwd"), []byte(passwd), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(etcDir, "shadow"), []byte(shadow), 0640); err != nil {
			t.Fatal(err)
		}

		_, err := EnsureUserInPasswd(rootfs)
		if err != nil {
			t.Fatalf("EnsureUserInPasswd: %v", err)
		}

		// Check passwd
		gotPasswd, _ := os.ReadFile(filepath.Join(etcDir, "passwd"))
		if !strings.Contains(string(gotPasswd), "user:x:7575:7575:user:/home:") {
			t.Errorf("user not added to passwd:\n%s", gotPasswd)
		}

		// Check shadow
		gotShadow, _ := os.ReadFile(filepath.Join(etcDir, "shadow"))
		if !strings.Contains(string(gotShadow), "user:!:") {
			t.Errorf("user not added to shadow:\n%s", gotShadow)
		}

		// Verify order in shadow: user should appear after root
		shadowLines := strings.Split(strings.TrimSpace(string(gotShadow)), "\n")
		var rootIdx, userIdx int = -1, -1
		for i, line := range shadowLines {
			if strings.HasPrefix(line, "root:") {
				rootIdx = i
			} else if strings.HasPrefix(line, "user:") {
				userIdx = i
			}
		}
		if rootIdx == -1 || userIdx == -1 {
			t.Errorf("missing expected entries in shadow:\n%s", gotShadow)
		}
		if rootIdx >= userIdx {
			t.Errorf("expected user after root in shadow, got indices root=%d, user=%d:\n%s",
				rootIdx, userIdx, gotShadow)
		}
	})

	t.Run("does not modify shadow when user already exists", func(t *testing.T) {
		tmpDir := t.TempDir()
		rootfs := filepath.Join(tmpDir, "rootfs")
		etcDir := filepath.Join(rootfs, "etc")
		if err := os.MkdirAll(etcDir, 0755); err != nil {
			t.Fatal(err)
		}

		passwd := "root::0:0:root:/root:/bin/bash\nuser:x:1000:1000:User:/home/user:/bin/bash\n"
		shadow := "root:!:19000:0:99999:7:::\nuser:!:19000:0:99999:7:::\n"
		if err := os.WriteFile(filepath.Join(etcDir, "passwd"), []byte(passwd), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(etcDir, "shadow"), []byte(shadow), 0640); err != nil {
			t.Fatal(err)
		}

		_, err := EnsureUserInPasswd(rootfs)
		if err != nil {
			t.Fatalf("EnsureUserInPasswd: %v", err)
		}

		// Shadow should be unchanged
		gotShadow, _ := os.ReadFile(filepath.Join(etcDir, "shadow"))
		if string(gotShadow) != shadow {
			t.Errorf("shadow was modified unexpectedly:\n%s", gotShadow)
		}
	})

	t.Run("shadow idempotent", func(t *testing.T) {
		tmpDir := t.TempDir()
		rootfs := filepath.Join(tmpDir, "rootfs")
		etcDir := filepath.Join(rootfs, "etc")
		if err := os.MkdirAll(etcDir, 0755); err != nil {
			t.Fatal(err)
		}

		passwd := "root::0:0:root:/root:/bin/bash\n"
		shadow := "root:!:19000:0:99999:7:::\n"
		if err := os.WriteFile(filepath.Join(etcDir, "passwd"), []byte(passwd), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(etcDir, "shadow"), []byte(shadow), 0640); err != nil {
			t.Fatal(err)
		}

		// First call
		_, err := EnsureUserInPasswd(rootfs)
		if err != nil {
			t.Fatalf("first EnsureUserInPasswd: %v", err)
		}
		firstShadow, _ := os.ReadFile(filepath.Join(etcDir, "shadow"))

		// Second call should not modify shadow
		_, err = EnsureUserInPasswd(rootfs)
		if err != nil {
			t.Fatalf("second EnsureUserInPasswd: %v", err)
		}
		secondShadow, _ := os.ReadFile(filepath.Join(etcDir, "shadow"))

		if string(firstShadow) != string(secondShadow) {
			t.Errorf("shadow changed between calls:\n1st: %s\n2nd: %s", firstShadow, secondShadow)
		}
	})
}

func TestEnsureSudoers(t *testing.T) {
	t.Run("creates sudoers drop-in", func(t *testing.T) {
		tmpDir := t.TempDir()
		rootfs := filepath.Join(tmpDir, "rootfs")
		sudoersDir := filepath.Join(rootfs, "etc", "sudoers.d")
		if err := os.MkdirAll(sudoersDir, 0755); err != nil {
			t.Fatal(err)
		}

		if err := EnsureSudoers(rootfs); err != nil {
			t.Fatalf("EnsureSudoers: %v", err)
		}

		dropinPath := filepath.Join(sudoersDir, "thundersnap-user")
		content, err := os.ReadFile(dropinPath)
		if err != nil {
			t.Fatalf("read drop-in: %v", err)
		}

		if !strings.Contains(string(content), "user ALL=(ALL) NOPASSWD: ALL") {
			t.Errorf("unexpected content:\n%s", content)
		}

		// Check mode
		info, err := os.Stat(dropinPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0440 {
			t.Errorf("expected mode 0440, got %o", info.Mode().Perm())
		}
	})

	t.Run("no-op when sudoers.d missing", func(t *testing.T) {
		tmpDir := t.TempDir()
		rootfs := filepath.Join(tmpDir, "rootfs")

		// Don't create sudoers.d
		if err := EnsureSudoers(rootfs); err != nil {
			t.Fatalf("EnsureSudoers: %v", err)
		}

		// Should not have created anything
		dropinPath := filepath.Join(rootfs, "etc", "sudoers.d", "thundersnap-user")
		if _, err := os.Stat(dropinPath); !os.IsNotExist(err) {
			t.Errorf("expected drop-in to not exist, got err: %v", err)
		}
	})
}

func TestThundersnapUID(t *testing.T) {
	// Verify the constant matches the expected value
	if ThundersnapUID != 7575 {
		t.Errorf("ThundersnapUID: got %d, want 7575", ThundersnapUID)
	}
	if ThundersnapGID != 7575 {
		t.Errorf("ThundersnapGID: got %d, want 7575", ThundersnapGID)
	}
}
