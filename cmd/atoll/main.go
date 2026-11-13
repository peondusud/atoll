// Command atoll is the Atoll CLI and daemon, in one binary (specs.md §25).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/fsx"
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
	french = wantsFrench(os.Getenv)
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
			return usage(stderr, T("--config needs a path"))
		case args[0] == "--config":
			configPath, args = args[1], args[2:]
		}
	}
	if len(args) == 0 {
		return usage(stderr, T("no command: atoll --help lists them"))
	}
	switch {
	case args[0] == "--help" || args[0] == "-h" || (args[0] == "help" && len(args) == 1):
		return writeHelp("", "", stdout, stderr)
	case args[0] == "help" && len(args) == 2:
		return writeHelp(args[1], "", stdout, stderr)
	case args[0] == "help":
		return usage(stderr, T("help takes one command at most"))
	case helpAsked(args[1:]):
		// What comes right after a command's name is its subcommand, when it has any: a flag there is not one.
		sub := ""
		if !strings.HasPrefix(args[1], "-") {
			sub = args[1]
		}
		return writeHelp(args[0], sub, stdout, stderr)
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
	case "notify":
		return runNotify(configPath, args[1:], stdout, stderr)
	case "names":
		return runNames(configPath, args[1:], stdout, stderr)
	case "scrub":
		return runScrub(configPath, args[1:], stdout, stderr)
	case "repair":
		return runRepair(configPath, args[1:], stdout, stderr)
	case "dupes":
		return runDupes(configPath, args[1:], stdout, stderr)
	case "conflicts":
		return runConflicts(configPath, args[1:], stdout, stderr)
	case "capacity":
		return runCapacity(configPath, args[1:], stdout, stderr)
	case "balance":
		return runBalance(configPath, args[1:], stdout, stderr)
	case "catalog":
		return runCatalog(configPath, args[1:], stdout, stderr)
	default:
		return usage(stderr, fmt.Sprintf(T("unknown command %q"), args[0]))
	}
}

// setFlag records a flag that takes no value. Given twice, it is a usage error (specs.md §25).
func setFlag(flag *bool, name string) error {
	if *flag {
		return fmt.Errorf(T("%s comes twice"), name)
	}
	*flag = true
	return nil
}

// lockCatalog takes the catalog's lock, waiting while another command or the daemon's pass holds it, and says
// so once on standard error (specs.md §25).
func lockCatalog(ctx context.Context, c *config.Config, stderr io.Writer) (func(), error) {
	return fsx.WaitLockFile(ctx, catalogPath(c), func() {
		report(stderr, T("waiting for the catalog: another command, or a pass of the daemon, holds it"))
	})
}

// lockedConfig takes the catalog's lock for a command that edits the configuration, then reads the configuration
// again: the command that held the lock may have written it, and this one edits what that one left (specs.md §10,
// §25). first is the configuration as the command read it to find the catalog. Read only before the wait, the text
// was that of before the other command: the second of two disk commands run at once failed for a file "changed since
// it was read", having changed nothing. A configuration that names another catalog by then is one this lock does
// not cover: the command gives up.
func lockedConfig(ctx context.Context, configPath string, first *config.Config, stderr io.Writer) (text []byte, c *config.Config, release func(), err error) {
	release, err = lockCatalog(ctx, first, stderr)
	if err != nil {
		return nil, nil, nil, err
	}
	text, c, err = readConfig(configPath)
	if err == nil && catalogPath(c) != catalogPath(first) {
		err = errors.New(T("the configuration names another catalog since this command started: nothing is changed, run it again"))
	}
	if err != nil {
		release()
		return nil, nil, nil, err
	}
	return text, c, release, nil
}

// configInPlace says what a replacement of the configuration left (fsx.ReplaceFile): the new one in place, with a
// warning when its folder could not be synced after, or an error, the file then as it was.
func configInPlace(err error, warn func(kind, message string)) (partial bool, failed error) {
	if errors.Is(err, fsx.ErrNotSynced) {
		warn(warnConfigNotSynced, fmt.Sprintf(T("the configuration is written, but a power cut now could bring the one before back: %v"), err))
		return true, nil
	}
	return false, err
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
