package tsm

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestStripPasswdFile(t *testing.T) {
	tmpDir := t.TempDir()
	passwdPath := filepath.Join(tmpDir, "passwd")

	input := `root::0:0:root:/root:/bin/bash
daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin
postgres:x:111:115:PostgreSQL:/var/lib/postgresql:/bin/bash
www-data:x:33:33:www-data:/var/www:/usr/sbin/nologin
# a comment
ubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash
`
	if err := os.WriteFile(passwdPath, []byte(input), 0644); err != nil {
		t.Fatal(err)
	}

	if err := StripPasswdFile(passwdPath, StripOptions{}); err != nil {
		t.Fatalf("StripPasswdFile: %v", err)
	}

	out, err := os.ReadFile(passwdPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)

	// Root must be preserved.
	if !strings.Contains(got, "root::0:0:root:/root:/bin/bash") {
		t.Errorf("root entry should be preserved, got:\n%s", got)
	}
	// Comment preserved.
	if !strings.Contains(got, "# a comment") {
		t.Errorf("comment should be preserved")
	}
	// Non-root rewritten to UID/GID 1000.
	for _, want := range []string{
		"daemon:x:1000:1000:daemon:/usr/sbin:/usr/sbin/nologin",
		"postgres:x:1000:1000:PostgreSQL:/var/lib/postgresql:/bin/bash",
		"www-data:x:1000:1000:www-data:/var/www:/usr/sbin/nologin",
		"ubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing line %q in output:\n%s", want, got)
		}
	}

	// Idempotence: applying it again must produce the same content.
	if err := StripPasswdFile(passwdPath, StripOptions{}); err != nil {
		t.Fatalf("second StripPasswdFile: %v", err)
	}
	out2, _ := os.ReadFile(passwdPath)
	if string(out2) != got {
		t.Errorf("not idempotent")
	}
}

func TestStripPasswdFileCustomUID(t *testing.T) {
	tmpDir := t.TempDir()
	passwdPath := filepath.Join(tmpDir, "passwd")
	input := "alice:x:42:42:Alice:/home/alice:/bin/sh\n"
	if err := os.WriteFile(passwdPath, []byte(input), 0644); err != nil {
		t.Fatal(err)
	}
	if err := StripPasswdFile(passwdPath, StripOptions{SharedUID: 2000, SharedGID: 3000}); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(passwdPath)
	if !strings.Contains(string(out), "alice:x:2000:3000:Alice:/home/alice:/bin/sh") {
		t.Errorf("unexpected output: %s", out)
	}
}

func TestStripGroupFile(t *testing.T) {
	tmpDir := t.TempDir()
	groupPath := filepath.Join(tmpDir, "group")

	input := `root:x:0:
daemon:x:1:bin
postgres:x:115:
sudo:x:27:ubuntu
ubuntu:x:1000:
`
	if err := os.WriteFile(groupPath, []byte(input), 0644); err != nil {
		t.Fatal(err)
	}

	if err := StripGroupFile(groupPath, StripOptions{}); err != nil {
		t.Fatalf("StripGroupFile: %v", err)
	}

	out, _ := os.ReadFile(groupPath)
	got := string(out)
	if !strings.Contains(got, "root:x:0:") {
		t.Errorf("root group should be preserved, got:\n%s", got)
	}
	for _, want := range []string{
		"daemon:x:1000:bin",
		"postgres:x:1000:",
		"sudo:x:1000:ubuntu",
		"ubuntu:x:1000:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestStripPasswdMissingFile(t *testing.T) {
	tmpDir := t.TempDir()
	// No passwd file present; should not error.
	if err := StripPasswdFile(filepath.Join(tmpDir, "passwd"), StripOptions{}); err != nil {
		t.Errorf("missing file should not error, got: %v", err)
	}
}

func TestChownNonRootFiles(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root to chown files to arbitrary UIDs")
	}
	tmpDir := t.TempDir()
	rootfs := filepath.Join(tmpDir, "rootfs")
	if err := os.MkdirAll(filepath.Join(rootfs, "var", "lib", "postgresql"), 0755); err != nil {
		t.Fatal(err)
	}
	pgFile := filepath.Join(rootfs, "var", "lib", "postgresql", "data")
	if err := os.WriteFile(pgFile, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Lchown(pgFile, 111, 115); err != nil {
		t.Fatal(err)
	}
	rootFile := filepath.Join(rootfs, "etc", "shadow")
	os.MkdirAll(filepath.Dir(rootFile), 0755)
	os.WriteFile(rootFile, []byte("root"), 0600)
	// already owned by current user (root in this branch)

	if err := ChownNonRootFiles(rootfs, StripOptions{}, nil); err != nil {
		t.Fatalf("chown: %v", err)
	}
	st, err := os.Lstat(pgFile)
	if err != nil {
		t.Fatal(err)
	}
	sys := st.Sys().(*syscall.Stat_t)
	if sys.Uid != DefaultSharedUID || sys.Gid != DefaultSharedGID {
		t.Errorf("pg file uid/gid: got %d/%d, want %d/%d", sys.Uid, sys.Gid, DefaultSharedUID, DefaultSharedGID)
	}
}

func TestStripRootfs(t *testing.T) {
	tmpDir := t.TempDir()
	rootfs := filepath.Join(tmpDir, "rootfs")
	etcDir := filepath.Join(rootfs, "etc")
	if err := os.MkdirAll(etcDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(etcDir, "passwd"),
		[]byte("root::0:0:root:/root:/bin/bash\nfoo:x:5:5:Foo:/home/foo:/bin/sh\n"),
		0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(etcDir, "group"),
		[]byte("root:x:0:\nfoo:x:5:\n"),
		0644); err != nil {
		t.Fatal(err)
	}
	if err := StripRootfs(rootfs, StripOptions{}); err != nil {
		t.Fatalf("StripRootfs: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(etcDir, "passwd"))
	if !strings.Contains(string(got), "foo:x:1000:1000:Foo:/home/foo:/bin/sh") {
		t.Errorf("passwd not stripped: %s", got)
	}
	got2, _ := os.ReadFile(filepath.Join(etcDir, "group"))
	if !strings.Contains(string(got2), "foo:x:1000:") {
		t.Errorf("group not stripped: %s", got2)
	}
}

func TestLookupPasswdUser(t *testing.T) {
	tmpDir := t.TempDir()
	rootfs := filepath.Join(tmpDir, "rootfs")
	etcDir := filepath.Join(rootfs, "etc")
	if err := os.MkdirAll(etcDir, 0755); err != nil {
		t.Fatal(err)
	}

	passwd := "root::0:0:root:/root:/bin/bash\nubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash\n"
	if err := os.WriteFile(filepath.Join(etcDir, "passwd"), []byte(passwd), 0644); err != nil {
		t.Fatal(err)
	}

	// Found cases
	entry := LookupPasswdUser(rootfs, "root")
	if entry == nil {
		t.Fatal("expected to find root")
	}
	if entry.UID != 0 || entry.Home != "/root" {
		t.Errorf("root entry mismatch: %+v", entry)
	}

	entry = LookupPasswdUser(rootfs, "ubuntu")
	if entry == nil {
		t.Fatal("expected to find ubuntu")
	}
	if entry.UID != 1000 || entry.Home != "/home/ubuntu" {
		t.Errorf("ubuntu entry mismatch: %+v", entry)
	}

	// Not found case
	entry = LookupPasswdUser(rootfs, "nonexistent")
	if entry != nil {
		t.Errorf("expected nil for nonexistent user, got %+v", entry)
	}

	// Missing file case
	entry = LookupPasswdUser("/nonexistent/path", "root")
	if entry != nil {
		t.Errorf("expected nil for missing passwd file, got %+v", entry)
	}
}

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

		// Check that user was inserted after root
		got, _ := os.ReadFile(filepath.Join(etcDir, "passwd"))
		gotStr := string(got)
		if !strings.Contains(gotStr, "user:x:1000:1000:user:/home:/bin/sh") {
			t.Errorf("user entry not added correctly:\n%s", gotStr)
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
		if !strings.Contains(string(got), "user:x:1000:1000:user:/home:/bin/bash") {
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
}
