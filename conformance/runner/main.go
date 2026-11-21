// Command runner checks an atoll binary against conformance/scenarios
// (specs.md §29.4). It talks to the binary through the CLI and the filesystem
// only and imports nothing from internal/, so the same run can judge the Go and
// Rust versions.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func main() {
	bin := flag.String("bin", "bin/atoll", "atoll binary under test")
	scenarios := flag.String("scenarios", "conformance/scenarios", "scenario folder")
	coverDir := flag.String("cover-dir", "", "folder for the coverage counters of a Go binary built with -cover: the GOCOVERDIR of each of its runs")
	flag.Parse()

	env, err := coverEnv(*coverDir)
	if err == nil {
		err = run(*bin, *scenarios, env)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "conformance: %v\n", err)
		os.Exit(1)
	}
}

// coverEnv gives what a Go binary built with -cover needs to write its counters, a file of each run in dir (make
// cover, specs.md §27): GOCOVERDIR, absolute, as the binary runs in its scenario's folder and must write nothing
// there, and the folder, made if it is missing. With no dir, nothing: the binary sees the runner's environment as it
// is, as it always did.
func coverEnv(dir string) ([]string, error) {
	if dir == "" {
		return nil, nil
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("cover dir: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o755); err != nil {
		return nil, fmt.Errorf("cover dir: %w", err)
	}
	return []string{"GOCOVERDIR=" + absolute}, nil
}

// run judges the binary on every scenario in the folder, and reports each failure. env goes to each run of the
// binary, besides the runner's own environment.
func run(bin, scenarios string, env []string) error {
	absolute, err := filepath.Abs(bin)
	if err != nil {
		return fmt.Errorf("binary under test: %w", err)
	}
	entries, err := os.ReadDir(scenarios)
	if err != nil {
		return fmt.Errorf("read scenarios: %w", err)
	}
	var failed []string
	count, skipped := 0, 0
	for _, e := range entries {
		name := e.Name()
		if name == "README.md" {
			continue
		}
		// Every other file counts, and fails if it is no scenario: a skip would read as a success.
		count++
		err := runFile(absolute, filepath.Join(scenarios, name), env)
		switch {
		case errors.Is(err, errNeedsRoot):
			// Said, and never counted as a success.
			skipped++
			fmt.Fprintf(os.Stderr, "SKIP %s: %v\n", name, err)
		case err != nil:
			failed = append(failed, name)
			fmt.Fprintf(os.Stderr, "FAIL %s: %v\n", name, err)
		}
	}
	if len(failed) > 0 {
		slices.Sort(failed)
		return fmt.Errorf("%d of %d scenarios failed: %s", len(failed), count, strings.Join(failed, ", "))
	}
	if count == 0 {
		// A folder at the wrong path must not read as a success.
		return fmt.Errorf("no scenario in %s", scenarios)
	}
	if skipped > 0 {
		fmt.Printf("conformance: %d scenarios, %d of them skipped as they need root\n", count, skipped)
		return nil
	}
	fmt.Printf("conformance: %d scenarios\n", count)
	return nil
}
