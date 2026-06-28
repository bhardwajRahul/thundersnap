package main

import (
	"bufio"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"testing"
)

func TestParsePasswdHome(t *testing.T) {
	const passwd = `# a comment
root:x:0:0:root:/root:/bin/bash

user:x:1000:1000:User:/home/user:/bin/sh
ubuntu:x:1001:1001::/home/ubuntu:/bin/bash
short:x:1:1:/nope
prefix:x:2:2::/home/prefix
prefixmatch:x:3:3::/home/prefixmatch
`
	tests := []struct {
		name     string
		username string
		want     string
	}{
		{name: "found", username: "user", want: "/home/user"},
		{name: "found ubuntu empty gecos", username: "ubuntu", want: "/home/ubuntu"},
		{name: "not found", username: "missing", want: ""},
		{name: "comment not matched", username: "#", want: ""},
		{name: "too few fields ignored", username: "short", want: ""},
		{name: "exact match not substring", username: "prefix", want: "/home/prefix"},
		{name: "no substring false positive", username: "prefixmat", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parsePasswdHome(passwd, tt.username); got != tt.want {
				t.Errorf("parsePasswdHome(_, %q) = %q, want %q", tt.username, got, tt.want)
			}
		})
	}
}

func TestQuoteArgsForSh(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "empty", args: nil, want: ""},
		{name: "simple", args: []string{"ls", "-l"}, want: "'ls' '-l'"},
		{name: "space", args: []string{"echo", "a b"}, want: "'echo' 'a b'"},
		{name: "single quote", args: []string{"echo", "it's"}, want: `'echo' 'it'\''s'`},
		{name: "empty arg", args: []string{""}, want: "''"},
		{name: "dollar untouched", args: []string{"echo", "$HOME"}, want: "'echo' '$HOME'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := quoteArgsForSh(tt.args); got != tt.want {
				t.Errorf("quoteArgsForSh(%q) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestSelectUserExplicit(t *testing.T) {
	// A caller-specified user is returned verbatim regardless of filesystem.
	if got := selectUser("", "alice"); got != "alice" {
		t.Errorf("selectUser(_, \"alice\") = %q, want alice", got)
	}
	if got := selectUser("/some/root", "bob"); got != "bob" {
		t.Errorf("selectUser(_, \"bob\") = %q, want bob", got)
	}
}

func TestReadArgs(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		r := bufio.NewReader(strings.NewReader("2\x00ls\x00-l\x00"))
		got, err := readArgs(r)
		if err != nil {
			t.Fatalf("readArgs: %v", err)
		}
		if len(got) != 2 || got[0] != "ls" || got[1] != "-l" {
			t.Errorf("readArgs = %q, want [ls -l]", got)
		}
	})
	t.Run("zero args", func(t *testing.T) {
		r := bufio.NewReader(strings.NewReader("0\x00"))
		got, err := readArgs(r)
		if err != nil || len(got) != 0 {
			t.Errorf("readArgs = (%q, %v), want ([], nil)", got, err)
		}
	})
	t.Run("non-numeric count", func(t *testing.T) {
		r := bufio.NewReader(strings.NewReader("abc\x00"))
		if _, err := readArgs(r); err == nil {
			t.Error("readArgs: expected error for non-numeric count")
		}
	})
	t.Run("negative count", func(t *testing.T) {
		r := bufio.NewReader(strings.NewReader("-1\x00"))
		if _, err := readArgs(r); err == nil {
			t.Error("readArgs: expected error for negative count")
		}
	})
	t.Run("truncated args", func(t *testing.T) {
		// Count claims 2 args but only one (unterminated) follows.
		r := bufio.NewReader(strings.NewReader("2\x00onlyone"))
		if _, err := readArgs(r); err == nil {
			t.Error("readArgs: expected error for truncated args")
		}
	})
}

// TestSuLoginChangesWorkingDirectory tests that "su - user -c 'pwd'" changes
// to the user's home directory, not just sets $HOME.
//
// This test verifies the fix for the bug where running a command as a non-root
// user via vshd would set $HOME correctly but not change the working directory.
func TestSuLoginChangesWorkingDirectory(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("test requires root to use su")
	}

	// Find a non-root user to test with
	targetUser := ""
	for _, candidate := range []string{"user", "nobody"} {
		if u, err := user.Lookup(candidate); err == nil && u.Uid != "0" {
			targetUser = candidate
			break
		}
	}
	if targetUser == "" {
		t.Skip("no non-root user available for testing")
	}

	u, err := user.Lookup(targetUser)
	if err != nil {
		t.Fatalf("lookup user %s: %v", targetUser, err)
	}

	// Ensure the home directory exists
	if _, err := os.Stat(u.HomeDir); os.IsNotExist(err) {
		t.Skipf("user %s home directory %s does not exist", targetUser, u.HomeDir)
	}

	// Test 1: su WITHOUT login shell - pwd should NOT change to home
	cmdNoLogin := exec.Command("su", targetUser, "-c", "pwd")
	outNoLogin, err := cmdNoLogin.CombinedOutput()
	if err != nil {
		t.Logf("su (no login) output: %s", outNoLogin)
		// This might fail if su requires a terminal, which is fine
		t.Logf("su (no login) error (may be expected): %v", err)
	} else {
		pwdNoLogin := strings.TrimSpace(string(outNoLogin))
		t.Logf("su %s -c pwd (no login shell): %q", targetUser, pwdNoLogin)
		// Without -, pwd should typically be the current directory, not home
	}

	// Test 2: su WITH login shell (the fix) - pwd SHOULD change to home
	cmdLogin := exec.Command("su", "-", targetUser, "-c", "pwd")
	outLogin, err := cmdLogin.CombinedOutput()
	if err != nil {
		t.Fatalf("su - (login) failed: %v\noutput: %s", err, outLogin)
	}
	pwdLogin := strings.TrimSpace(string(outLogin))
	t.Logf("su - %s -c pwd (login shell): %q", targetUser, pwdLogin)

	if pwdLogin != u.HomeDir {
		t.Errorf("with login shell, pwd = %q, want %q (user's home)", pwdLogin, u.HomeDir)
	}

	// Test 3: Verify $HOME is set correctly in both cases
	cmdHomeLogin := exec.Command("su", "-", targetUser, "-c", "echo $HOME")
	outHomeLogin, err := cmdHomeLogin.CombinedOutput()
	if err != nil {
		t.Fatalf("su - echo $HOME failed: %v", err)
	}
	homeLogin := strings.TrimSpace(string(outHomeLogin))
	t.Logf("su - %s -c 'echo $HOME': %q", targetUser, homeLogin)

	if homeLogin != u.HomeDir {
		t.Errorf("$HOME = %q, want %q", homeLogin, u.HomeDir)
	}
}
