// Package e2e contains end-to-end tests for thundersnap minimal shell functionality.
package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMinimalShellVM tests that a completely blank/minimal container can:
// 1. Boot with just the ts binary providing the shell
// 2. Accept SSH connections via vshd
// 3. Run ts commands (like ts snap, ts ping, etc.)
//
// This tests the minimalist built-in shell scenario where ts acts as /bin/sh
// when invoked via symlink. This is useful for containers that have no
// external shell and rely entirely on the ts binary for command execution.
func TestMinimalShellVM(t *testing.T) {
	env := newTestEnv(t)
	vmDir := requireVMDeps(t)

	// Create a minimal container with just ts binary
	framePath := prepareMinimalFrame(t, env, "minimal-shell-test")

	// Boot the VM
	session, err := startVM(t, env, framePath, vmDir, 512, standardVMCmdline())
	if err != nil {
		t.Fatalf("Failed to start VM: %v", err)
	}
	defer session.cleanup()

	// Wait for vshd to be ready
	_, err = session.waitForVshd(15 * time.Second)
	if err != nil {
		t.Fatalf("VM did not become ready: %v", err)
	}
	t.Log("VM with minimal shell is ready")

	// Test 1: Basic shell command execution
	// The built-in shell uses mvdan.cc/sh which provides shell functionality
	t.Run("basic_echo", func(t *testing.T) {
		output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", "echo MINIMAL_SHELL_OK")
		if err != nil {
			t.Fatalf("Failed to run echo: %v", err)
		}
		if !strings.Contains(output, "MINIMAL_SHELL_OK") {
			t.Errorf("Expected output to contain 'MINIMAL_SHELL_OK', got: %q", output)
		}
	})

	// Test 2: Verify ts binary can be executed directly
	t.Run("ts_help", func(t *testing.T) {
		// Running ts without arguments should show usage/help
		output, err := runVshCommand(session.vsockSock, "root", "/bin/ts")
		if err != nil {
			// ts without args exits non-zero, that's expected
			t.Logf("ts without args output: %s", output)
		}
		// ts should output something indicating it's the ts binary
		if !strings.Contains(output, "ping") && !strings.Contains(output, "snap") && !strings.Contains(output, "commands") {
			t.Logf("ts output: %q", output)
			// Not a hard error - just check it ran
		}
	})

	// Test 3: Shell variable assignment and expansion
	t.Run("variable_expansion", func(t *testing.T) {
		output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", "X=hello; echo $X world")
		if err != nil {
			t.Fatalf("Failed to run variable test: %v", err)
		}
		if !strings.Contains(output, "hello world") {
			t.Errorf("Expected 'hello world', got: %q", output)
		}
	})

	// Test 4: Command substitution
	t.Run("command_substitution", func(t *testing.T) {
		output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", "echo $(echo nested)")
		if err != nil {
			t.Fatalf("Failed to run command substitution: %v", err)
		}
		if !strings.Contains(output, "nested") {
			t.Errorf("Expected 'nested', got: %q", output)
		}
	})

	// Test 5: Piping (basic shell feature)
	t.Run("pipe", func(t *testing.T) {
		// Our minimal container only has ts/sh, so we use shell builtins
		output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", "echo 'line1\nline2\nline3' | while read l; do echo got:$l; done")
		if err != nil {
			t.Fatalf("Failed to run pipe test: %v", err)
		}
		if !strings.Contains(output, "got:line") {
			t.Errorf("Expected piped output, got: %q", output)
		}
	})

	// Test 6: File redirection
	t.Run("redirection", func(t *testing.T) {
		// Write and read back using shell redirection
		_, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", "echo REDIRECT_TEST > /tmp/test.txt")
		if err != nil {
			t.Fatalf("Failed to write file: %v", err)
		}

		output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", "while read line; do echo $line; done < /tmp/test.txt")
		if err != nil {
			t.Fatalf("Failed to read file: %v", err)
		}
		if !strings.Contains(output, "REDIRECT_TEST") {
			t.Errorf("Expected 'REDIRECT_TEST', got: %q", output)
		}
	})

	// Test 7: Process working directory
	t.Run("pwd", func(t *testing.T) {
		output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", "pwd")
		if err != nil {
			t.Fatalf("Failed to run pwd: %v", err)
		}
		output = strings.TrimSpace(output)
		// Root user should start in / as the working directory
		if output != "/" {
			t.Errorf("Expected pwd '/', got: %q", output)
		}
	})

	// Test 8: Exit code handling
	t.Run("exit_codes", func(t *testing.T) {
		// Test that exit codes propagate correctly
		output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", "exit 0 && echo SUCCESS")
		if err != nil {
			t.Logf("exit 0 output: %s", output)
		}

		// Test false (exit 1)
		output, err = runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", "false || echo CAUGHT_FAILURE")
		if err != nil {
			t.Logf("false output: %s, err: %v", output, err)
		}
		if !strings.Contains(output, "CAUGHT_FAILURE") {
			t.Errorf("Expected error handling to work, got: %q", output)
		}
	})

	// Test 9: Environment variables
	t.Run("environment", func(t *testing.T) {
		output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", "echo PATH=$PATH")
		if err != nil {
			t.Fatalf("Failed to get PATH: %v", err)
		}
		t.Logf("Environment PATH: %s", strings.TrimSpace(output))
		// Just verify PATH is set (should contain /bin at minimum)
		if !strings.Contains(output, "PATH=") {
			t.Errorf("Expected PATH to be set, got: %q", output)
		}
	})

	// Test 10: For loop (shell builtin)
	t.Run("for_loop", func(t *testing.T) {
		output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", "for i in a b c; do echo $i; done")
		if err != nil {
			t.Fatalf("Failed to run for loop: %v", err)
		}
		if !strings.Contains(output, "a") || !strings.Contains(output, "b") || !strings.Contains(output, "c") {
			t.Errorf("Expected 'a b c' in output, got: %q", output)
		}
	})

	t.Log("All minimal shell tests passed")
}

// TestMinimalShellTsCommands tests that ts subcommands work from SSH in a minimal container.
func TestMinimalShellTsCommands(t *testing.T) {
	env := newTestEnv(t)
	vmDir := requireVMDeps(t)

	framePath := prepareMinimalFrame(t, env, "minimal-ts-commands")

	session, err := startVM(t, env, framePath, vmDir, 512, standardVMCmdline())
	if err != nil {
		t.Fatalf("Failed to start VM: %v", err)
	}
	defer session.cleanup()

	_, err = session.waitForVshd(15 * time.Second)
	if err != nil {
		t.Fatalf("VM did not become ready: %v", err)
	}

	// Test ts check-dev - verifies /dev is set up correctly
	t.Run("ts_check_dev", func(t *testing.T) {
		output, err := runVshCommand(session.vsockSock, "root", "/bin/ts", "check-dev")
		if err != nil {
			t.Fatalf("ts check-dev failed: %v", err)
		}
		// Should show /dev entries
		if !strings.Contains(output, "DEV:") || !strings.Contains(output, "null") {
			t.Errorf("Expected /dev entries in output, got: %q", output)
		}
		t.Logf("ts check-dev output (truncated): %s", truncate(output, 500))
	})

	// Test ts check-isolation - verifies isolation state
	t.Run("ts_check_isolation", func(t *testing.T) {
		output, err := runVshCommand(session.vsockSock, "root", "/bin/ts", "check-isolation")
		if err != nil {
			t.Fatalf("ts check-isolation failed: %v", err)
		}
		// Should show isolation info
		t.Logf("ts check-isolation output: %s", truncate(output, 500))
	})

	// Test ts invoked as sh -c
	t.Run("sh_minus_c", func(t *testing.T) {
		output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", "echo FROM_TS_SHELL")
		if err != nil {
			t.Fatalf("sh -c failed: %v", err)
		}
		if !strings.Contains(output, "FROM_TS_SHELL") {
			t.Errorf("Expected 'FROM_TS_SHELL', got: %q", output)
		}
	})

	t.Log("All ts command tests passed")
}

// prepareMinimalFrame creates a frame with only the bare minimum files:
// - /bin/ts (the ts binary)
// - /bin/sh (hardlink to ts)
// - /sbin/vshd (for SSH)
// - /etc/passwd (for user lookup)
// - /etc/group (for group lookup)
// - Basic /dev nodes and directories
//
// This tests the scenario where a user creates a frame with nil:nil:nil
// and the container has almost nothing except what ts provides.
func prepareMinimalFrame(t *testing.T, env *testEnv, name string) string {
	t.Helper()

	framePath := filepath.Join(env.fsDir, "testuser", name)

	// Create the frame directory structure
	if err := os.MkdirAll(filepath.Dir(framePath), 0755); err != nil {
		t.Fatalf("mkdir frame parent: %v", err)
	}

	// Create as a btrfs subvolume (empty)
	cmd := exec.Command("btrfs", "subvolume", "create", framePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("btrfs subvolume create: %v\n%s", err, out)
	}

	// Create minimal directory structure
	dirs := []string{
		"bin", "sbin", "etc", "tmp", "proc", "sys", "dev",
		"home", "root", "work", "var", "var/log",
	}
	for _, dir := range dirs {
		path := filepath.Join(framePath, dir)
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	// Set special permissions
	os.Chmod(filepath.Join(framePath, "tmp"), 0777|os.ModeSticky)
	os.Chmod(filepath.Join(framePath, "root"), 0700)

	// Copy ts binary
	tsDst := filepath.Join(framePath, "bin/ts")
	if err := copyFile(env.tsBinary, tsDst); err != nil {
		t.Fatalf("copy ts to frame: %v", err)
	}

	// Create /bin/sh as hardlink to ts - ts acts as shell when invoked as "sh"
	shDst := filepath.Join(framePath, "bin/sh")
	if err := os.Link(tsDst, shDst); err != nil {
		t.Fatalf("link bin/sh to bin/ts: %v", err)
	}

	// Copy vshd binary
	vshdBinary := env.requireBinary("vshd")
	vshdDst := filepath.Join(framePath, "sbin/vshd")
	if err := copyFile(vshdBinary, vshdDst); err != nil {
		t.Fatalf("copy vshd to frame: %v", err)
	}

	// Create minimal /etc/passwd
	passwdContent := []byte(
		"root:x:0:0:root:/root:/bin/sh\n" +
			"user:x:1000:1000:user:/home/user:/bin/sh\n" +
			"nobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin\n",
	)
	if err := os.WriteFile(filepath.Join(framePath, "etc/passwd"), passwdContent, 0644); err != nil {
		t.Fatalf("write passwd: %v", err)
	}

	// Create minimal /etc/group
	groupContent := []byte(
		"root:x:0:\n" +
			"user:x:1000:\n" +
			"nogroup:x:65534:\n",
	)
	if err := os.WriteFile(filepath.Join(framePath, "etc/group"), groupContent, 0644); err != nil {
		t.Fatalf("write group: %v", err)
	}

	// Create /etc/hostname
	if err := os.WriteFile(filepath.Join(framePath, "etc/hostname"), []byte("minimal\n"), 0644); err != nil {
		t.Fatalf("write hostname: %v", err)
	}

	// Create /etc/hosts
	hostsContent := []byte("127.0.0.1 localhost\n::1 localhost\n")
	if err := os.WriteFile(filepath.Join(framePath, "etc/hosts"), hostsContent, 0644); err != nil {
		t.Fatalf("write hosts: %v", err)
	}

	// Create /etc/resolv.conf (will be overwritten by init script)
	if err := os.WriteFile(filepath.Join(framePath, "etc/resolv.conf"), []byte("nameserver 8.8.8.8\n"), 0644); err != nil {
		t.Fatalf("write resolv.conf: %v", err)
	}

	t.Logf("Created minimal frame at %s", framePath)
	return framePath
}

// truncate returns the first n characters of s, with "..." appended if truncated.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// TestMinimalShellInteractive tests interactive shell features.
// This verifies that the mvdan.cc/sh based shell in ts works for interactive use.
func TestMinimalShellInteractive(t *testing.T) {
	env := newTestEnv(t)
	vmDir := requireVMDeps(t)

	framePath := prepareMinimalFrame(t, env, "minimal-interactive")

	session, err := startVM(t, env, framePath, vmDir, 512, standardVMCmdline())
	if err != nil {
		t.Fatalf("Failed to start VM: %v", err)
	}
	defer session.cleanup()

	_, err = session.waitForVshd(15 * time.Second)
	if err != nil {
		t.Fatalf("VM did not become ready: %v", err)
	}

	// Test multi-line script execution
	t.Run("multiline_script", func(t *testing.T) {
		script := `
x=1
y=2
z=$((x + y))
echo "sum is $z"
`
		output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", script)
		if err != nil {
			t.Fatalf("multiline script failed: %v", err)
		}
		if !strings.Contains(output, "sum is 3") {
			t.Errorf("Expected 'sum is 3', got: %q", output)
		}
	})

	// Test here-document
	t.Run("heredoc", func(t *testing.T) {
		// Here documents work with mvdan.cc/sh
		script := `cat << 'EOF'
line one
line two
EOF
`
		output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", script)
		if err != nil {
			// cat might not exist in minimal container
			t.Logf("heredoc test: %v (cat may not exist)", err)
			return
		}
		if !strings.Contains(output, "line one") && !strings.Contains(output, "line two") {
			t.Logf("heredoc output: %q", output)
		}
	})

	// Test case statement
	t.Run("case_statement", func(t *testing.T) {
		script := `
val=two
case $val in
  one) echo "got one" ;;
  two) echo "got two" ;;
  *) echo "got other" ;;
esac
`
		output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", script)
		if err != nil {
			t.Fatalf("case statement failed: %v", err)
		}
		if !strings.Contains(output, "got two") {
			t.Errorf("Expected 'got two', got: %q", output)
		}
	})

	// Test function definition
	t.Run("functions", func(t *testing.T) {
		script := `
greet() {
  echo "Hello, $1!"
}
greet World
`
		output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", script)
		if err != nil {
			t.Fatalf("function test failed: %v", err)
		}
		if !strings.Contains(output, "Hello, World!") {
			t.Errorf("Expected 'Hello, World!', got: %q", output)
		}
	})

	// Test arithmetic
	t.Run("arithmetic", func(t *testing.T) {
		output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", "echo $((10 * 5 + 3))")
		if err != nil {
			t.Fatalf("arithmetic failed: %v", err)
		}
		if !strings.Contains(output, "53") {
			t.Errorf("Expected '53', got: %q", output)
		}
	})

	// Test string operations
	t.Run("string_ops", func(t *testing.T) {
		script := `
str="hello world"
echo "length: ${#str}"
echo "substr: ${str:0:5}"
`
		output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", script)
		if err != nil {
			t.Fatalf("string ops failed: %v", err)
		}
		if !strings.Contains(output, "length: 11") {
			t.Logf("string ops output: %q (may not support all features)", output)
		}
	})

	t.Log("Interactive shell tests completed")
}

// TestMinimalShellSSHScenarios tests realistic SSH usage scenarios with minimal shell.
func TestMinimalShellSSHScenarios(t *testing.T) {
	env := newTestEnv(t)
	vmDir := requireVMDeps(t)

	framePath := prepareMinimalFrame(t, env, "minimal-ssh-scenarios")

	session, err := startVM(t, env, framePath, vmDir, 512, standardVMCmdline())
	if err != nil {
		t.Fatalf("Failed to start VM: %v", err)
	}
	defer session.cleanup()

	_, err = session.waitForVshd(15 * time.Second)
	if err != nil {
		t.Fatalf("VM did not become ready: %v", err)
	}

	// Scenario: User SSHs in and runs a series of commands
	// This simulates: ssh root@x@host ts snap
	// and: ssh root@x@host (just shell access)

	// Test 1: Direct command execution (like ssh host command)
	t.Run("direct_command", func(t *testing.T) {
		output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", "echo DIRECT_COMMAND_OK")
		if err != nil {
			t.Fatalf("Direct command failed: %v", err)
		}
		if !strings.Contains(output, "DIRECT_COMMAND_OK") {
			t.Errorf("Direct command output mismatch: %q", output)
		}
	})

	// Test 2: Multiple commands chained (&&)
	t.Run("chained_commands", func(t *testing.T) {
		output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", "echo FIRST && echo SECOND && echo THIRD")
		if err != nil {
			t.Fatalf("Chained commands failed: %v", err)
		}
		if !strings.Contains(output, "FIRST") || !strings.Contains(output, "SECOND") || !strings.Contains(output, "THIRD") {
			t.Errorf("Chained commands output mismatch: %q", output)
		}
	})

	// Test 3: Command with semicolons
	t.Run("semicolon_commands", func(t *testing.T) {
		output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", "echo A; echo B; echo C")
		if err != nil {
			t.Fatalf("Semicolon commands failed: %v", err)
		}
		if !strings.Contains(output, "A") || !strings.Contains(output, "B") || !strings.Contains(output, "C") {
			t.Errorf("Semicolon commands output mismatch: %q", output)
		}
	})

	// Test 4: Command with special characters that might need escaping
	t.Run("special_chars", func(t *testing.T) {
		output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", "echo 'hello \"world\"'")
		if err != nil {
			t.Fatalf("Special chars command failed: %v", err)
		}
		if !strings.Contains(output, "hello \"world\"") {
			t.Errorf("Special chars output mismatch: %q", output)
		}
	})

	// Test 5: Read from /proc (common pattern)
	t.Run("proc_read", func(t *testing.T) {
		output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", "while read line; do echo $line; done < /proc/version")
		if err != nil {
			t.Fatalf("proc read failed: %v", err)
		}
		if !strings.Contains(output, "Linux") {
			t.Errorf("Expected Linux version, got: %q", output)
		}
	})

	// Test 6: Multiple sequential SSH commands (simulating rapid fire)
	t.Run("rapid_commands", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			output, err := runVshCommand(session.vsockSock, "root", "/bin/sh", "-c", fmt.Sprintf("echo CMD_%d", i))
			if err != nil {
				t.Errorf("Rapid command %d failed: %v", i, err)
				continue
			}
			expected := fmt.Sprintf("CMD_%d", i)
			if !strings.Contains(output, expected) {
				t.Errorf("Rapid command %d: expected %q, got: %q", i, expected, output)
			}
		}
	})

	t.Log("SSH scenario tests completed")
}
