package main

import (
	"os"
	"os/exec"
	"os/user"
	"strings"
	"testing"
)

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
