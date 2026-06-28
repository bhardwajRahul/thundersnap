// The dist command builds thundersnap release packages for distribution.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/tailscale/thundersnap/release/dist/tspkgs"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "list":
		if err := runList(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	case "build":
		if err := runBuild(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: dist <command> [flags] [target filters]

Commands:
  list   List all available release targets
  build  Build release packages

Target filters are regexps matched against target names (e.g. "linux/amd64/deb").
Use "all" to select all targets.
`)
	os.Exit(1)
}

func runList(args []string) error {
	targets := tspkgs.Targets()
	if len(args) == 0 {
		args = []string{"all"}
	}
	filtered, err := tspkgs.FilterTargets(targets, args)
	if err != nil {
		return err
	}
	for _, t := range filtered {
		fmt.Println(t)
	}
	return nil
}

func runBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	var (
		verbose = fs.Bool("verbose", false, "verbose build output")
		outPath = fs.String("out", "", "output directory (default: <cwd>/dist)")
	)
	fs.Parse(args)
	filters := fs.Args()

	targets := tspkgs.Targets()
	filtered, err := tspkgs.FilterTargets(targets, filters)
	if err != nil {
		return err
	}
	if len(filtered) == 0 {
		return errors.New("no targets matched (did you mean 'dist build all'?)")
	}

	st := time.Now()
	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}
	out := filepath.Join(wd, "dist")
	if *outPath != "" {
		out = *outPath
	}
	b, err := tspkgs.NewBuild(wd, out)
	if err != nil {
		return fmt.Errorf("creating build context: %w", err)
	}
	defer b.Close()
	b.Verbose = *verbose

	files, err := b.Build(filtered)
	if err != nil {
		return fmt.Errorf("building targets: %w", err)
	}

	fmt.Println()
	fmt.Println("Built artifacts:")
	for _, f := range files {
		fmt.Println("  " + f)
	}
	fmt.Printf("\nDone! Took %s\n", time.Since(st).Round(time.Millisecond))
	return nil
}
