package main

import (
	"reflect"
	"testing"
)

func TestParseShellCommand(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want []string
	}{
		{
			name: "simple command",
			cmd:  "ls -la",
			want: []string{"ls", "-la"},
		},
		{
			name: "command with path",
			cmd:  "/bin/ts snap",
			want: []string{"/bin/ts", "snap"},
		},
		{
			name: "single quoted string",
			cmd:  "echo 'hello world'",
			want: []string{"echo", "hello world"},
		},
		{
			name: "double quoted string",
			cmd:  `echo "hello world"`,
			want: []string{"echo", "hello world"},
		},
		{
			name: "mixed quotes",
			cmd:  `echo "it's working"`,
			want: []string{"echo", "it's working"},
		},
		{
			name: "multiple args",
			cmd:  "ts --sock /thunder.sock snap",
			want: []string{"ts", "--sock", "/thunder.sock", "snap"},
		},
		{
			name: "extra whitespace",
			cmd:  "  ls   -la   /tmp  ",
			want: []string{"ls", "-la", "/tmp"},
		},
		{
			name: "tabs and spaces",
			cmd:  "ls\t-la\t/tmp",
			want: []string{"ls", "-la", "/tmp"},
		},
		{
			name: "empty command",
			cmd:  "",
			want: nil,
		},
		{
			name: "only whitespace",
			cmd:  "   ",
			want: nil,
		},
		{
			name: "escaped in double quotes",
			cmd:  `echo "hello\"world"`,
			want: []string{"echo", `hello"world`},
		},
		{
			name: "single quotes preserve everything",
			cmd:  `echo 'hello"world'`,
			want: []string{"echo", `hello"world`},
		},
		{
			name: "realistic ssh command",
			cmd:  "ts snap",
			want: []string{"ts", "snap"},
		},
		{
			name: "command with equals",
			cmd:  "env FOO=bar /bin/cmd",
			want: []string{"env", "FOO=bar", "/bin/cmd"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseShellCommand(tt.cmd)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseShellCommand(%q) = %v, want %v", tt.cmd, got, tt.want)
			}
		})
	}
}
