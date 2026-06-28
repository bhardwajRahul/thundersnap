// tsm indexes a filesystem and writes .tsm (ThunderSnap Manifest) and
// .tsc (ThunderSnap Chunks) files.
//
// Usage:
//
//	tsm [-o output] <directory>
//
// If -o is not given, the output basename defaults to the input directory path
// (e.g., indexing ./snaps/1 produces ./snaps/1.tsm and ./snaps/1.tsc).
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/pborman/getopt/v2"
	"github.com/tailscale/thundersnap/tsm"
	"golang.org/x/term"
)

var (
	outfile = getopt.StringLong("output", 'o', "", "output basename (without extension); default: input directory path")
	print_  = getopt.BoolLong("print", 'p', "print contents of existing .tsm/.tsc file")
	help    = getopt.BoolLong("help", 'h', "show help")
)

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: tsm [options] <directory>\n")
	fmt.Fprintf(os.Stderr, "\nCreates .tsm and .tsc index files for a filesystem tree.\n\n")
	getopt.PrintUsage(os.Stderr)
	os.Exit(1)
}

func main() {
	getopt.SetUsage(usage)
	getopt.Parse()
	args := getopt.Args()

	if *help || len(args) == 0 {
		usage()
	}

	if *print_ {
		for _, path := range args {
			if err := printFile(path); err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
		}
		return
	}

	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "error: exactly one directory argument required")
		os.Exit(1)
	}

	inputDir := filepath.Clean(args[0])

	// Verify input is a directory
	info, err := os.Lstat(inputDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if !info.IsDir() {
		fmt.Fprintf(os.Stderr, "error: %s is not a directory\n", inputDir)
		os.Exit(1)
	}

	// Determine output basename
	outBase := *outfile
	if outBase == "" {
		outBase = defaultOutBase(inputDir)
	}

	fmt.Printf("Indexing %s -> %s.tsm + %s.tsc\n", inputDir, outBase, outBase)

	// Only emit \r-style progress when stderr is a real terminal; a non-TTY
	// (pipe/file) invocation gets plain line-oriented progress instead.
	opts := tsm.IndexerOptions{
		ProgressWriter: os.Stderr,
		IsTTY:          term.IsTerminal(int(os.Stderr.Fd())),
	}

	if err := tsm.Create(inputDir, outBase, opts); err != nil {
		fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		os.Exit(1)
	}

	// Print file sizes
	for _, ext := range []string{".tsm", ".tsc"} {
		path := outBase + ext
		if info, err := os.Stat(path); err == nil {
			fmt.Printf("Wrote %s (%d bytes)\n", path, info.Size())
		}
	}
}

// defaultOutBase derives the output basename from a filepath.Clean'd input
// directory. Clean has already removed any trailing slash for every path except
// the filesystem root "/", which it leaves as "/"; we map that to "root" so the
// output is "root.tsm"/"root.tsc" rather than the empty, dotfile-like ".tsm".
func defaultOutBase(cleanedInputDir string) string {
	if cleanedInputDir == "/" {
		return "root"
	}
	return cleanedInputDir
}

func printFile(path string) error {
	ext := filepath.Ext(path)
	switch ext {
	case ".tsm":
		return printTSM(path)
	case ".tsc":
		return printTSC(path)
	default:
		return fmt.Errorf("unknown file extension %q (expected .tsm or .tsc)", ext)
	}
}

func printTSM(path string) error {
	reader, err := tsm.ReadTSM(path)
	if err != nil {
		return err
	}

	fmt.Printf("TSM File: %s\n", path)
	fmt.Printf("  SHA-256: %x\n", reader.SHA256)
	fmt.Printf("  File count: %d\n", reader.Header.FileCount)
	fmt.Printf("  Total size: %d bytes\n", reader.Header.TotalSize)
	fmt.Printf("  Chunk file ref: %x\n", reader.Header.ChunkFileRef)
	fmt.Printf("  TSC SHA-256: %x\n", reader.TSCSHA)
	fmt.Println()

	for _, entry := range reader.Entries {
		fmt.Printf("  %s  %s", entry.Type, entry.Path)

		switch entry.Type {
		case tsm.EntryTypeFile:
			fmt.Printf("  size=%d  chunks=%d (start=%d)  mode=%o  uid=%d gid=%d",
				entry.Size, entry.ChunkCount, entry.ChunkStart, entry.Mode, entry.UID, entry.GID)
		case tsm.EntryTypeDir:
			fmt.Printf("  mode=%o  uid=%d gid=%d", entry.Mode, entry.UID, entry.GID)
		case tsm.EntryTypeSymlink:
			fmt.Printf("  -> %s  uid=%d gid=%d", entry.LinkTarget, entry.UID, entry.GID)
		case tsm.EntryTypeHardlink:
			fmt.Printf("  => entry#%d  uid=%d gid=%d", entry.LinkIndex, entry.UID, entry.GID)
		case tsm.EntryTypeBlockDev, tsm.EntryTypeCharDev:
			fmt.Printf("  %d:%d  mode=%o  uid=%d gid=%d",
				entry.DevMajor, entry.DevMinor, entry.Mode, entry.UID, entry.GID)
		default:
			fmt.Printf("  mode=%o  uid=%d gid=%d", entry.Mode, entry.UID, entry.GID)
		}

		fmt.Println()
	}

	fmt.Printf("\nTotal entries: %d\n", len(reader.Entries))
	return nil
}

func printTSC(path string) error {
	reader, err := tsm.ReadTSC(path)
	if err != nil {
		return err
	}

	fmt.Printf("TSC File: %s\n", path)
	fmt.Printf("  SHA-256: %x\n", reader.SHA256)
	fmt.Printf("  Chunk count: %d\n", reader.Header.ChunkCount)
	fmt.Printf("  Total chunk size: %d bytes\n", reader.Header.TotalChunkSize)
	fmt.Println()

	for i, entry := range reader.Entries {
		flags := ""
		if entry.Flags&tsm.TSCEntryFlagZeroBlock != 0 {
			flags = " [zero]"
		}
		if entry.Flags&tsm.TSCEntryFlagLiteral != 0 {
			flags += " [literal]"
		}

		fmt.Printf("  [%d] %x  size=%d  level=%d%s\n",
			i, entry.SHA256, entry.Size, entry.Level, flags)
	}

	fmt.Printf("\nTotal chunks: %d  Total size: %d bytes\n",
		len(reader.Entries), reader.Header.TotalChunkSize)
	return nil
}
