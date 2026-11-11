// Command atoll is the Atoll CLI and daemon, in one binary (specs.md §25).
package main

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
)

// The exit codes of specs.md §29.3.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
	// exitConfirm says that a command was not allowed to do what it would have done (specs.md §29.3).
	exitConfirm = 3
	// exitPartial says that part of what was asked could not be done (specs.md §29.3).
	exitPartial = 4
)

// defaultConfig is where the configuration lives (specs.md §25).
const defaultConfig = "/etc/atoll/atoll.toml"

func main() {
	limitMemory()
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// softMemoryLimit keeps a scan of 200,000 files under the 200 MB of specs.md §26. It costs no time
// there, measured, as the collector only works harder near the limit. A larger pool goes past it
// and is slowed down, not stopped: GOMEMLIMIT raises it.
const softMemoryLimit = 170 << 20

func limitMemory() {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(softMemoryLimit)
	}
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
	case "init":
		return runInit(configPath, args[1:], stdout, stderr)
	case "scan":
		return runScan(configPath, args[1:], stdout, stderr)
	case "status":
		return runStatus(configPath, args[1:], stdout, stderr)
	case "prune":
		return runPrune(configPath, args[1:], stdout, stderr)
	case "replicate":
		return runReplicate(configPath, args[1:], stdout, stderr)
	case "trash":
		return runTrash(configPath, args[1:], stdout, stderr)
	case "daemon":
		return runDaemon(configPath, args[1:], stdout, stderr)
	case "guard":
		return runGuard(configPath, args[1:], stdout, stderr)
	case "mergerfs-config":
		return runMergerfsConfig(configPath, args[1:], stdout, stderr)
	default:
		return usage(stderr, fmt.Sprintf("unknown command %q", args[0]))
	}
}

// setFlag records a flag that takes no value. Given twice, it is a usage error (specs.md §25).
func setFlag(flag *bool, name string) error {
	if *flag {
		return fmt.Errorf("%s comes twice", name)
	}
	*flag = true
	return nil
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
