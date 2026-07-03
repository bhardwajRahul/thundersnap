// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"github.com/tailscale/thundersnap/vshdsession"
)

// cmdSessionServe is the in-container endpoint of a vshd session. vshd runs it
// (via nsenter + drop-caps-and-run --chroot) as:
//
//	ts session-serve <ptyFlag> <argc> <arg0> <arg1> ...
//
// where ptyFlag is "1" for a PTY session and "0" otherwise, and the args are the
// final command to run (e.g. "su - user" or a shell). It speaks the vshdproto
// TLV protocol on its own stdin/stdout, which vshd splices verbatim to/from the
// network connection. Crucially, because session-serve runs AFTER the chroot
// into the container rootfs, opening the pty here allocates the slave from the
// container's own devpts instance, so it is visible as /dev/pts/N inside the
// container (and `ps` shows the real pts as the controlling terminal).
func cmdSessionServe(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "error: session-serve requires <ptyFlag> <argc> [args...]")
		os.Exit(1)
	}
	wantPTY := args[0] == "1"
	argc, err := strconv.Atoi(args[1])
	if err != nil || argc < 0 {
		fmt.Fprintf(os.Stderr, "error: session-serve: invalid argc %q\n", args[1])
		os.Exit(1)
	}
	rest := args[2:]
	if len(rest) < argc {
		fmt.Fprintf(os.Stderr, "error: session-serve: expected %d args, got %d\n", argc, len(rest))
		os.Exit(1)
	}
	argv := rest[:argc]
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "error: session-serve: empty command")
		os.Exit(1)
	}

	// Resolve the command via PATH so a bare "su" works in minimal containers.
	exe, err := findExecutable(argv[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: session-serve: %v\n", err)
		os.Exit(1)
	}

	cmd := exec.Command(exe, argv[1:]...)
	cmd.Env = os.Environ()

	// vshd splices our stdin/stdout to the client connection; serve the session
	// over them. logf is nil (diagnostics, if any, go to our stderr which vshd
	// surfaces in its own log).
	vshdsession.Serve(os.Stdout, os.Stdin, cmd, wantPTY, nil, nil)
}
