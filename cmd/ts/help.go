// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/pborman/getopt/v2"
)

// cmdInfo describes a subcommand for help purposes. The fields drive both the
// main "ts --help" listing (name + category + short) and the per-command
// "ts <cmd> --help" output (everything, plus the option list generated from
// the command's getopt.Set).
type cmdInfo struct {
	name     string // canonical name, e.g. "snap" or "ref create"
	category string // "main", "other", or "ref" (hidden from main help)
	short    string // one-line summary, shown in main help and as the lead line
	long     string // paragraph description; newlines are treated as soft wraps
	outputs  string // "Outputs:" body; newlines are preserved, each line indented 2
	examples string // optional "Examples:" body, printed verbatim
}

// commands is the single source of truth for the subcommand list. Order is
// preserved within each category when printing the main help.
var commands = []cmdInfo{
	// --- Main commands: the snapshot/frame workflow ----------------------
	{
		name:     "snap",
		category: "main",
		short:    "create a snapshot of the current container/VM",
		long: `Captures a snapshot of the current frame's filesystem, or just a subtree
of it when a path is given. The snapshot is content-addressed. By default the
command waits for indexing and prints the ID. --quick captures immediately and
returns silently while indexing continues in the background. --wait is retained
for compatibility and overrides --quick. --delete removes a snapshot by ID.`,
		outputs: `default/--wait: the content-addressable snapshot ID, one line.
--quick: nothing on successful capture; indexing continues in the background.
--delete <id>: "Deleted snapshot <id>".`,
	},
	{
		name:     "snaps",
		category: "main",
		short:    "list all snapshots with sizes",
		long: `Lists every snapshot known to thundersnapd together with its on-disk
size, sorted by snapshot ID. The format is du-like: size first, then ID.`,
		outputs: `One line per snapshot: an 8-character right-justified size in
gigabytes, two spaces, then the snapshot ID, e.g.
"   1.234G  sha256-abc...".`,
	},
	{
		name:     "frame",
		category: "main",
		short:    "resolve or create frames",
		long: `With no argument, prints the current frame's UUID. Given a UUID or ref
name, resolves it to a UUID and prints that. Given a snap triplet
(root:home:work), creates a new frame from those snapshots and prints its
UUID; empty components inherit from the current frame, and "::" forks the
current frame. --delete removes a frame by UUID (a frame still referenced
by a ref must have the ref removed first).`,
		outputs: `no argument: the current frame's UUID, one line.
<uuid> or <ref>: the resolved frame UUID, one line.
snap triplet or "::": the new frame's UUID, one line.
--delete <uuid>: "Deleted frame <uuid>".`,
		examples: `  ts frame                       print current frame UUID
  ts frame myref                 resolve ref "myref" to a UUID
  ts frame abc:def:ghi           create a frame from three snaps
  ts frame :<snap>:              replace /home only, keep root and /work
  ts frame ::                    fork the current frame
  ts frame --ref r ::            fork and name the new frame "r"
  ts frame --delete <uuid>       delete a frame by UUID`,
	},
	{
		name:     "frames",
		category: "main",
		short:    "list all frames with status",
		long: `Lists every frame known to thundersnapd with its current status
("running" or "stopped"), sorted by frame name.`,
		outputs: `One line per frame: a 7-character left-justified status, two
spaces, then the frame UUID, e.g.
"stopped  01234567-89ab-cdef-0123-456789abcdef".`,
	},
	{
		name:     "go",
		category: "main",
		short:    "enter a frame (create/resolve + start a session)",
		long: `Resolves or creates the target frame and starts an interactive login
session inside it over the container/VM control socket. With no argument,
re-enters the current frame. Given a UUID or ref, enters that frame. Given
a snap triplet, creates a new frame and enters it; "::" forks the current
frame. With -c, runs a single shell command non-interactively and exits
with its status.

Any spec may carry a "<user>@" prefix to run the session as a specific
Unix user (e.g. ts go root@myref, ts go alice@::); without it the host
auto-detects the user. The username is validated server-side before the
session's su is invoked.`,
		outputs: `The session's stdout and stderr are forwarded to this process's
stdout and stderr. The process exits with the session's exit code.`,
		examples: `  ts go                          re-enter the current frame
  ts go myref                    enter the frame named "myref"
  ts go root@myref               enter "myref" as user root
  ts go ::                       fork the current frame and enter it
  ts go alice@:: -c 'make test'  fork as alice, run "make test", exit with its status`,
	},
	{
		name:     "undo",
		category: "main",
		short:    "jump backward in time by one snap",
		long: `Jumps backward in time by one snap: snapshots the current state,
creates a new frame from the previous snap in the current frame's history,
clones and prunes that history, then enters the new frame. With -c, runs a
command instead of starting an interactive session.`,
		outputs: `A one-line notice ("Undoing to snap <id>...") is printed to stderr,
then the new session's stdout/stderr are forwarded. Exits with the
session's exit code.`,
	},
	{
		name:     "log",
		category: "main",
		short:    "show frame snapshot history",
		long: `Shows the snapshot history of a frame (the snaps that make up its
lineage, newest first). With no argument, uses the current frame.`,
		outputs: `"(no snapshots)" if the frame has no history, otherwise one line
per entry: "<time>  <snap>", or "<time>  <snap>  <message>" when the
entry has a message, newest first.`,
	},

	// --- Other commands ---------------------------------------------------
	{
		name:     "ping",
		category: "other",
		short:    "send a ping to thundersnapd and print the reply",
		long: `Sends a ping to thundersnapd over the control socket and prints the
reply. Mainly useful for checking that the daemon is reachable and the
socket is wired up correctly.`,
		outputs: `The daemon's reply message on a single line (typically "pong").
Nothing else is printed on success.`,
	},
	{
		name:     "ref",
		category: "other",
		short:    "manage refs (named pointers to frames)",
		long: `Manages refs, which are named pointers to frame UUIDs. Use one of the
subcommands listed below; "ts ref" with no subcommand prints this help.`,
	},
	{
		name:     "refs",
		category: "other",
		short:    "list all refs",
		long: `Lists every ref known to thundersnapd with the frame UUID it points at
and any configured autorun command, in the order returned by the daemon.`,
		outputs: `"(no refs)" if there are none, otherwise one line per ref:
"<name> -> <uuid>", or "<name> -> <uuid> [autorun: <argv>]" when an
autorun is configured.`,
	},
	{
		name:     "reflog",
		category: "other",
		short:    "show ref history",
		long: `Shows the history of a ref (the sequence of frame UUIDs it has pointed
at, newest first). With no argument, uses the unique ref pointing at the
current frame if exactly one exists; otherwise the server returns an
error with suggestions.`,
		outputs: `"(empty reflog)" if the ref has no history, otherwise one line per
entry: "<uuid>  <time>", newest first.`,
	},
	{
		name:     "taint",
		category: "other",
		short:    "add a taint to the current frame",
		long: `With no argument, lists the current frame's taints. With a taint name,
adds it to the current frame. Taints are free-form labels (e.g.
"pii:customers") used to mark frames that carry sensitive data.`,
		outputs: `no argument: "(no taints)" if the frame has none, otherwise one
taint per line. With a name: "Added taint: <name>", then "Current taints:
[...]" listing all taints now on the frame.`,
	},
	{
		name:     "autorun",
		category: "other",
		short:    "configure a program to run automatically",
		long: `Configures a command to run automatically when a ref's frame is
entered. --ref selects the ref (required). Provide a program and
arguments to set the autorun, or --stop to clear it.`,
		outputs: `Nothing on success. Errors are written to stderr.`,
		examples: `  ts autorun --ref web nginx -g 'daemon off;'
  ts autorun --ref web --stop`,
	},
	{
		name:     "autoruns",
		category: "other",
		short:    "list configured autorun commands",
		long: `Lists configured autorun commands. Each command is encoded independently,
so arbitrary argument contents cannot make the output ambiguous.`,
		outputs: `One compact JSON argv array per configured autorun, one per line.
Nothing is printed when no autoruns are configured.`,
	},
	{
		name:     "download-docker",
		category: "other",
		short:    "download a Docker image as a snap",
		long: `Downloads a Docker image from a registry and stores it as a
content-addressed snapshot that can be used as a frame's rootfs. Download and
extraction progress is streamed to stderr, followed by the downloaded size and
Mbps. If the image is cached, a cache notice goes to stderr instead.`,
		outputs: `stdout: the snapshot ID, one line, whether downloaded or cached.
stderr: progress and final size/rate for a download, or a cache notice.`,
		examples: `  ts download-docker ubuntu:24.04
  ts download-docker docker.io/library/golang:1.22`,
	},
	{
		name:     "who-has",
		category: "other",
		short:    "query peers to find which ones have a snap",
		long: `Queries mesh peers to find which ones already have a given snapshot, so
you know where to download it from. Only a single snapshot ID may be
queried; pass a frame spec and the command will suggest querying each snap
separately.`,
		outputs: `One bupdate URL per line for each peer that has the snapshot, e.g.
"http://host:7575/bupdate/". If no peer has it, prints "No peers have
snapshot <id>" to stderr and exits 1.`,
	},
	{
		name:     "download-snap",
		category: "other",
		short:    "download a snap from mesh peers",
		long: `Downloads a snapshot from mesh peers that have it (as found by
who-has). Progress is streamed to stderr. Accepts a single snapshot ID or
a frame spec (root:home:work), in which case all non-empty snaps are
downloaded in parallel.`,
		outputs: `single snap: "Downloaded snapshot to <path>" on success; nothing if
this daemon already had it. frame spec: per-snap errors go to stderr;
nothing on stdout. Exits 1 if any download failed.`,
	},

	// --- ref subcommands (hidden from the main help) ----------------------
	{
		name:     "ref create",
		category: "ref",
		short:    "create a new ref pointing at a frame",
		long: `Creates a new ref named <name> that points at the frame identified by
<uuid-or-ref>. If the target is a ref, its current frame UUID is used. The new
ref name must start with a letter and contain only letters, digits, dashes, and
underscores.`,
		examples: `  ts ref create alias <uuid>       point "alias" at a frame UUID
  ts ref create alias existing     point "alias" at "existing"'s current UUID`,
		outputs: `Nothing on success.`,
	},
	{
		name:     "ref move",
		category: "ref",
		short:    "move a ref to point at a different UUID",
		long:     `Repoints an existing ref named <name> at the frame UUID <uuid>. Use -f to force the move even if the target frame has running processes.`,
		outputs:  `"Moved ref <name> -> <uuid>" on success.`,
	},
	{
		name:     "ref delete",
		category: "ref",
		short:    "delete a ref",
		long:     `Deletes the ref named <name>. Use -f to force the delete even if the frame it points at has running processes or a non-empty id directory.`,
		outputs:  `"Deleted ref <name>" on success.`,
	},
}

// commandByName indexes commands by canonical name for O(1) lookup in
// printCommandHelp. Built once in init.
var commandByName = func() map[string]*cmdInfo {
	m := make(map[string]*cmdInfo, len(commands))
	for i := range commands {
		m[commands[i].name] = &commands[i]
	}
	return m
}()

// newCmdOpts creates a fresh getopt set for subcommand `name`, pre-registers a
// --help/-h flag, and sets a parameters hint for the usage line. The returned
// set is ready for additional flag registration; callers must parse with a
// leading dummy program-name argument (see parseCmd) so getopt does not drop
// the first real argument.
func newCmdOpts(name, params string) (*getopt.Set, *bool) {
	opts := getopt.New()
	opts.SetProgram("ts " + name)
	if params != "" {
		opts.SetParameters(params)
	} else {
		opts.SetParameters("")
	}
	help := opts.BoolLong("help", 'h', "show this help and exit")
	return opts, help
}

// parseCmd parses args against opts, prepending a dummy program-name element
// so getopt's "first arg is the program name" convention does not swallow the
// first real argument. On error it prints the getopt error and usage to stderr
// and exits 1.
func parseCmd(opts *getopt.Set, name string, args []string) {
	opts.Parse(append([]string{"ts " + name}, args...))
}

// wrap reflows s so that no line exceeds width characters, breaking at word
// boundaries. It treats s as a single paragraph (any newlines are collapsed to
// spaces).
func wrap(s string, width int) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) > width {
			b.WriteString(line)
			b.WriteByte('\n')
			line = w
		} else {
			line += " " + w
		}
	}
	b.WriteString(line)
	return b.String()
}

// printCommandHelp writes the "ts <cmd> --help" message for the command `name`
// to w. The option list is generated from opts (which must already have been
// fully populated with flags and parsed-or-not); the description and outputs
// text come from the command registry.
func printCommandHelp(w io.Writer, name string, opts *getopt.Set) {
	info := commandByName[name]
	if info == nil {
		// No registry entry: fall back to getopt's plain usage.
		opts.PrintUsage(w)
		return
	}

	// Usage line: "Usage: <program> [<flags>] <parameters>".
	fmt.Fprintf(w, "Usage: %s", opts.Program())
	if line := strings.TrimSpace(opts.UsageLine()); line != "" {
		fmt.Fprintf(w, " %s", line)
	}
	if p := opts.Parameters(); p != "" {
		fmt.Fprintf(w, " %s", p)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w)

	// One-line summary.
	fmt.Fprintln(w, info.short)
	fmt.Fprintln(w)

	// Paragraph description (reflowed to a comfortable width).
	if info.long != "" {
		fmt.Fprintln(w, wrap(strings.ReplaceAll(info.long, "\n", " "), 78))
		fmt.Fprintln(w)
	}

	// Outputs section.
	if info.outputs != "" {
		fmt.Fprintln(w, "Outputs:")
		for _, line := range strings.Split(info.outputs, "\n") {
			fmt.Fprintf(w, "  %s\n", line)
		}
		fmt.Fprintln(w)
	}

	// Options (generated from the getopt set, always includes --help).
	var buf bytes.Buffer
	opts.PrintOptions(&buf)
	if buf.Len() > 0 {
		fmt.Fprintln(w, "Options:")
		w.Write(buf.Bytes())
	}

	// Examples (verbatim).
	if info.examples != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Examples:")
		fmt.Fprint(w, info.examples)
		if !strings.HasSuffix(info.examples, "\n") {
			fmt.Fprintln(w)
		}
	}
}

// printRefGroupHelp writes the "ts ref" / "ts ref --help" message: a summary
// plus the list of ref subcommands. It does not use the generic printer because
// "ref" has subcommands rather than its own flags.
func printRefGroupHelp(w io.Writer) {
	fmt.Fprintln(w, "Usage: ts ref <subcommand> [options]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Manages refs, which are named pointers to frame UUIDs. Use one of the")
	fmt.Fprintln(w, "subcommands below; \"ts ref\" with no subcommand prints this help.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Subcommands:")
	fmt.Fprintln(w, "  create <name> <uuid>  create a new ref pointing at a frame UUID")
	fmt.Fprintln(w, "  move   <name> <uuid>  move a ref to point at a different UUID (-f to force)")
	fmt.Fprintln(w, "  delete <name>         delete a ref (-f to force)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run \"ts ref <subcommand> --help\" for subcommand-specific help.")
}

// printCmdGroup prints a heading followed by a two-column command listing
// (name, short summary), with names padded to a common width.
func printCmdGroup(w io.Writer, heading string, cmds []cmdInfo) {
	if len(cmds) == 0 {
		return
	}
	fmt.Fprintln(w, heading)
	width := 0
	for _, c := range cmds {
		if len(c.name) > width {
			width = len(c.name)
		}
	}
	for _, c := range cmds {
		fmt.Fprintf(w, "  %-*s  %s\n", width, c.name, c.short)
	}
	fmt.Fprintln(w)
}

// printMainHelp writes the top-level "ts --help" message to w: a short
// description, the usage line, the categorized command list, the global
// options, and a hint to run "<cmd> --help".
func printMainHelp(w io.Writer) {
	prog := getopt.CommandLine.Program()
	if prog == "" {
		prog = "ts"
	}
	fmt.Fprintln(w, "ts is a client for thundersnapd, the snapshot/frame daemon. It talks to the")
	fmt.Fprintln(w, "daemon over a Unix control socket (in containers) or vsock (in VMs) to take")
	fmt.Fprintln(w, "snapshots, create and enter frames, and manage refs.")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Usage: %s [global-options] <command> [command-options]\n", prog)
	fmt.Fprintln(w)

	var main, other []cmdInfo
	for _, c := range commands {
		switch c.category {
		case "main":
			main = append(main, c)
		case "other":
			other = append(other, c)
		}
	}
	printCmdGroup(w, "Main commands (the snapshot/frame workflow):", main)
	printCmdGroup(w, "Other commands:", other)

	fmt.Fprintln(w, "Global options:")
	var buf bytes.Buffer
	getopt.CommandLine.PrintOptions(&buf)
	w.Write(buf.Bytes())
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Run \"%s <command> --help\" for command-specific help.\n", prog)
}
