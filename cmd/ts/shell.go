// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// runAsShell implements a POSIX-compatible shell using mvdan.cc/sh.
//
// When ts is symlinked to /bin/sh, it acts as a real shell supporting:
//   - sh -c 'command' - run a command string
//   - sh script.sh - run a script file
//   - sh (no args) - interactive shell
//
// This uses the mvdan.cc/sh/v3 interpreter which provides proper POSIX shell
// semantics including pipes, redirects, variable expansion, and control flow.
func runAsShell() {
	err := runShell(os.Stdin, os.Stdout, os.Stderr, os.Args[1:]...)
	if status, ok := interp.IsExitStatus(err); ok {
		os.Exit(int(status))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ts: %v\n", err)
		os.Exit(1)
	}
}

// runShell is the core shell implementation.
func runShell(stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	runner, err := interp.New(interp.StdIO(stdin, stdout, stderr))
	if err != nil {
		return err
	}

	parser := syntax.NewParser()

	// Parse arguments to find -c command or script file
	var commandStr string
	var scriptFile string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-c":
			if i+1 >= len(args) {
				return fmt.Errorf("-c requires an argument")
			}
			commandStr = args[i+1]
			i++
		case "-i", "-l", "--login", "-e", "-x", "-v":
			// Flags we recognize but ignore for now
		default:
			if strings.HasPrefix(args[i], "-") {
				// Unknown flag - ignore
				continue
			}
			// First non-flag argument is a script file
			scriptFile = args[i]
		}
	}

	// Execute based on what we found
	if commandStr != "" {
		// sh -c 'command'
		return runShellCommand(runner, parser, commandStr)
	}

	if scriptFile != "" {
		// sh script.sh
		return runShellScript(runner, parser, scriptFile)
	}

	// Interactive shell (or reading from stdin if not a TTY)
	if r, ok := stdin.(*os.File); ok && term.IsTerminal(int(r.Fd())) {
		return runShellInteractive(runner, parser, stdin, stdout)
	}

	// Reading commands from stdin (non-interactive)
	return runShellCommand(runner, parser, "")
}

// runShellCommand executes a command string.
func runShellCommand(runner *interp.Runner, parser *syntax.Parser, command string) error {
	var reader io.Reader
	if command != "" {
		reader = strings.NewReader(command)
	} else {
		reader = os.Stdin
	}

	prog, err := parser.Parse(reader, "")
	if err != nil {
		return err
	}

	runner.Reset()
	return runner.Run(context.Background(), prog)
}

// runShellScript executes a script file.
func runShellScript(runner *interp.Runner, parser *syntax.Parser, filename string) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	prog, err := parser.Parse(f, filename)
	if err != nil {
		return err
	}

	runner.Reset()
	return runner.Run(context.Background(), prog)
}

// runShellInteractive runs a simple interactive shell.
func runShellInteractive(runner *interp.Runner, parser *syntax.Parser, stdin io.Reader, stdout io.Writer) error {
	fmt.Fprintf(stdout, "$ ")

	scanner := bufio.NewScanner(stdin)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			fmt.Fprintf(stdout, "$ ")
			continue
		}

		prog, err := parser.Parse(strings.NewReader(line), "")
		if err != nil {
			fmt.Fprintf(stdout, "error: %v\n$ ", err)
			continue
		}

		if err := runner.Run(context.Background(), prog); err != nil {
			if _, ok := interp.IsExitStatus(err); !ok {
				fmt.Fprintf(stdout, "error: %v\n", err)
			}
		}

		if runner.Exited() {
			return nil
		}

		fmt.Fprintf(stdout, "$ ")
	}

	return scanner.Err()
}
