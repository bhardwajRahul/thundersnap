package main

import (
	"testing"

	"github.com/pborman/getopt/v2"
)

// TestParseGoArgs tests the argument parsing for "ts go".
func TestParseGoArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantSpec string
		wantCmd  string
		wantIso  string
		wantErr  bool
	}{
		{
			name:     "no args",
			args:     []string{},
			wantSpec: "",
			wantCmd:  "",
		},
		{
			name:     "spec only",
			args:     []string{"myframe"},
			wantSpec: "myframe",
			wantCmd:  "",
		},
		{
			name:     "double colon spec",
			args:     []string{"::"},
			wantSpec: "::",
			wantCmd:  "",
		},
		{
			name:     "flags before spec",
			args:     []string{"-c", "true", "silly"},
			wantSpec: "silly",
			wantCmd:  "true",
		},
		{
			name:     "flags after spec (GNU-style)",
			args:     []string{"silly", "-c", "true"},
			wantSpec: "silly",
			wantCmd:  "true",
		},
		{
			name:     "double colon with -c after",
			args:     []string{"::", "-c", "echo hello"},
			wantSpec: "::",
			wantCmd:  "echo hello",
		},
		{
			name:     "double dash stops flag parsing",
			args:     []string{"-c", "true", "--", "-c"},
			wantSpec: "-c",
			wantCmd:  "true",
		},
		{
			name:     "isolation flag",
			args:     []string{"--isolation", "vm", "myframe"},
			wantSpec: "myframe",
			wantIso:  "vm",
		},
		{
			name:     "isolation and command",
			args:     []string{"--isolation=container", "-c", "ls", "::"},
			wantSpec: "::",
			wantCmd:  "ls",
			wantIso:  "container",
		},
		{
			name:    "too many positional args",
			args:    []string{"foo", "bar"},
			wantErr: true,
		},
		{
			name:    "unknown flag",
			args:    []string{"--unknown"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseGoArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseGoArgs(%v) = %+v, want error", tt.args, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGoArgs(%v) error = %v", tt.args, err)
			}
			if got.spec != tt.wantSpec {
				t.Errorf("spec = %q, want %q", got.spec, tt.wantSpec)
			}
			if got.command != tt.wantCmd {
				t.Errorf("command = %q, want %q", got.command, tt.wantCmd)
			}
			if got.isolation != tt.wantIso {
				t.Errorf("isolation = %q, want %q", got.isolation, tt.wantIso)
			}
		})
	}
}

// TestGlobalArgsRejectSubcommandFlags tests that global argument parsing
// rejects flags that belong to subcommands (like -c).
func TestGlobalArgsRejectSubcommandFlags(t *testing.T) {
	// Save and restore the global CommandLine state
	oldArgs := getopt.CommandLine
	defer func() { getopt.CommandLine = oldArgs }()

	// Create a fresh set that mimics the global flags
	opts := getopt.New()
	opts.SetProgram("ts")
	opts.StringLong("sock", 0, "/thunder.sock", "path to control socket")
	opts.BoolLong("help", 'h', "show help")

	// "ts -c go" should fail because -c is not a global flag
	err := opts.Getopt([]string{"ts", "-c", "go"}, nil)
	if err == nil {
		t.Errorf("expected error for 'ts -c go' (unknown flag -c)")
	}
}
