package main

import (
	"fmt"
	"io"
	"slices"
	"strings"
)

// commandHelp is what help says of one command (specs.md §25): its synopsis, in the CLI's own words, which stay in
// English, and what it does, which is translated.
type commandHelp struct {
	name     string
	synopsis []string
	does     string
}

// commandHelps lists the commands, in the order help gives them.
func commandHelps() []commandHelp {
	return []commandHelp{
		{"init", []string{"atoll init [--pool-id <id> --view-mount <path> [--catalog <path>]] [--json]"},
			"create the configuration and the catalog, or what of them is missing"},
		{"disk", []string{
			"atoll disk add <mount point> [--id <id>] [--domain <domain>] [--tags <tag>,...] [--dry-run] [--json]",
			"atoll disk list [--json]",
			"atoll disk show <id> [--json]",
			"atoll disk remove <id> [--evacuate] [--dry-run] [--yes] [--json]",
			"atoll disk fail <id> [--dry-run] [--yes] [--json]",
			"atoll disk replace <id> --with <mount point> [--id <id>] [--dry-run] [--json]",
		}, "add a disk, list them, show one, take one out, declare one failed, or replace one"},
		{"scan", []string{"atoll scan [--disk <id>] [--full] [--json]"},
			"walk the disks and record what they hold; --full reads every copy whole"},
		{"status", []string{"atoll status [--path <path>] [--json]"},
			"the state of the pool, or of the files at or below a path, from the catalog alone"},
		{"replicate", []string{"atoll replicate <path> [--now | --force] [--dry-run] [--json]"},
			"queue the copies the files at or below a path lack, and with --now make them"},
		{"rules", []string{"atoll rules check [--json]", "atoll rules explain <path> [--size <size>] [--json]"},
			"check the configuration, or say which rule a path takes and what it gives"},
		{"scrub", []string{"atoll scrub [--disk <id>] [--path <path>] [--budget <duration>] [--dry-run] [--yes] [--json]"},
			"read the copies due and check each chunk against its hash"},
		{"repair", []string{"atoll repair <path> [--disk <id>] [--mode chunk|file] [--dry-run] [--yes] [--json]"},
			"repair the corrupt copies of the files at or below a path from healthy ones"},
		{"dupes", []string{"atoll dupes [--path <path>] [--min-size <size>] [--json]"},
			"list the files that hold the same content under different paths, from the catalog"},
		{"trash", []string{
			"atoll trash list [--json]",
			"atoll trash restore <path> [--to <disk>] [--dry-run] [--yes] [--json]",
			"atoll trash purge [--older-than <duration>] [--dry-run] [--yes] [--json]",
		}, "list the trash, put files back from it, or delete what has reached its time"},
		{"names", []string{"atoll names check [--path <path>] [--json]"},
			"report names a disk would refuse, and those that collide"},
		{"guard", []string{
			"atoll guard list [--json]",
			"atoll guard confirm --disk <id> [--kind deletions|modifications] [--json]",
			"atoll guard reject --disk <id> [--kind deletions|modifications] [--json]",
		}, "show the mass-deletion and mass-modification guards, or answer one that tripped"},
		{"capacity", []string{"atoll capacity [--json]"},
			"say what the pool can hold with 1, 2 and 3 copies of every file, in theory and without moving a copy"},
		{"prune", []string{"atoll prune [--dry-run] [--yes] [--json]"},
			"move surplus copies to the trash"},
		{"conflicts", []string{
			"atoll conflicts list [--json]",
			"atoll conflicts resolve <path> --keep <disk> [--dry-run] [--yes] [--json]",
		}, "list the files in conflict, or keep one copy of a file and send the others to the trash"},
		{"daemon", []string{"atoll daemon [--foreground] [--dry-run]", "atoll daemon --until-idle [--dry-run] [--json]"},
			"run for good, or do all the work that is due once and stop"},
		{"mergerfs-config", []string{"atoll mergerfs-config [--json]"},
			"print the mergerfs line that presents the pool"},
		{"notify", []string{"atoll notify test [--json]"},
			"send a test alert through each channel that is set"},
		{"catalog", []string{"atoll catalog backup [--json]", "atoll catalog verify [--json]", "atoll catalog rebuild [--dry-run] [--yes] [--json]"},
			"back the catalog up, check it against itself and the disks, or make it again from the disks"},
	}
}

// helpAsked says whether a command's arguments ask for its help: --help or -h before any "--" (specs.md §25).
func helpAsked(args []string) bool {
	for _, a := range args {
		switch a {
		case "--":
			return false
		case "--help", "-h":
			return true
		}
	}
	return false
}

// writeHelp writes the list of commands, or with a name that command's synopsis and what it does, and exits with 0,
// or with 2 for a command it does not know.
func writeHelp(name string, stdout, stderr io.Writer) int {
	helps := commandHelps()
	var text strings.Builder
	if name == "" {
		text.WriteString(T("Atoll adds file-level redundancy to the disks of a media library, without moving a file.") + "\n\n")
		text.WriteString(T("Usage: atoll [--config <path>] <command> [arguments]") + "\n\n")
		text.WriteString(T("Commands:") + "\n")
		for _, h := range helps {
			fmt.Fprintf(&text, "  %-16s %s\n", h.name, T(h.does))
		}
		text.WriteString("\n" + T("atoll help <command>, or atoll <command> --help, says more of one.") + "\n")
		text.WriteString(T("--config names the configuration, /etc/atoll/atoll.toml by default; --json gives the output its schema of api/schemas/.") + "\n")
	} else {
		at := slices.IndexFunc(helps, func(h commandHelp) bool { return h.name == name })
		if at < 0 {
			return usage(stderr, fmt.Sprintf(T("unknown command %q"), name))
		}
		h := &helps[at]
		for _, line := range h.synopsis {
			text.WriteString(line + "\n")
		}
		text.WriteString("\n" + T(h.does) + "\n")
	}
	if err := writeText(stdout, text.String()); err != nil {
		return failure(stderr, err)
	}
	return exitOK
}
