// Command runner checks an atoll binary against conformance/scenarios
// (specs.md §29.4). It talks to the binary through the CLI and the filesystem
// only and imports nothing from internal/, so the same run can judge the Go and
// Rust versions.
package main

import (
	"flag"
	"fmt"
	"os"
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

func run(bin, scenarios string) error {
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("binary under test: %w", err)
	}
	entries, err := os.ReadDir(scenarios)
	if err != nil {
		return fmt.Errorf("read scenarios: %w", err)
	}
	for _, e := range entries {
		// Skipping a scenario it cannot run would report a false success.
		if e.Name() != ".gitkeep" {
			return fmt.Errorf("scenario %s: running scenarios is not implemented yet", e.Name())
		}
	}
	fmt.Println("conformance: 0 scenarios")
	return nil
}
