// Command atoll is the Atoll CLI and daemon, in one binary (specs.md §25).
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// The exit codes of specs.md §29.3.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// defaultConfig is where the configuration lives (specs.md §25).
const defaultConfig = "/etc/atoll/atoll.toml"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	configPath := defaultConfig
	if len(args) > 0 {
		value, isConfig := strings.CutPrefix(args[0], "--config=")
		switch {
		case isConfig:
			configPath, args = value, args[1:]
		case args[0] == "--config" && len(args) == 1:
			return usage(stderr, "--config needs a path")
		case args[0] == "--config":
			configPath, args = args[1], args[2:]
		}
	}
	if len(args) == 0 {
		return usage(stderr, "no command")
	}
	switch args[0] {
	case "rules":
		return runRules(configPath, args[1:], stdout, stderr)
	case "disk":
		return runDisk(configPath, args[1:], stdout, stderr)
	default:
		return usage(stderr, fmt.Sprintf("unknown command %q", args[0]))
	}
}

func usage(stderr io.Writer, message string) int {
	report(stderr, message)
	return exitUsage
}

func failure(stderr io.Writer, err error) int {
	report(stderr, err.Error())
	return exitError
}

func report(stderr io.Writer, message string) {
	// Nothing is left to tell, should standard error fail too: the exit code still says it.
	_, _ = fmt.Fprintf(stderr, "atoll: %s\n", message)
}
