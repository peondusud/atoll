// Command runner checks an atoll binary against conformance/scenarios
// (specs.md §29.4). It talks to the binary through the CLI and the filesystem
// only and imports nothing from internal/, so the same run can judge the Go and
// Rust versions.
package main

import (
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
	flag.Parse()

	if err := run(*bin, *scenarios); err != nil {
		fmt.Fprintf(os.Stderr, "conformance: %v\n", err)
		os.Exit(1)
	}
}

// run judges the binary on every scenario in the folder, and reports each failure.
func run(bin, scenarios string) error {
	absolute, err := filepath.Abs(bin)
	if err != nil {
		return fmt.Errorf("binary under test: %w", err)
	}
	entries, err := os.ReadDir(scenarios)
	if err != nil {
		return fmt.Errorf("read scenarios: %w", err)
	}
	var failed []string
	count := 0
	for _, e := range entries {
		name := e.Name()
		if name == "README.md" {
			continue
		}
		// Every other file counts, and fails if it is no scenario: a skip would read as a success.
		count++
		if err := runFile(absolute, filepath.Join(scenarios, name)); err != nil {
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
	fmt.Printf("conformance: %d scenarios\n", count)
	return nil
}
