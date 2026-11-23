package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/names"
)

func runDisk(configPath string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usage(stderr, T("disk: want add, list, show, remove, fail, replace or mark"))
	}
	switch args[0] {
	case "mark":
		return diskMark(configPath, args[1:], stdout, stderr)
	case "add":
		return diskAdd(configPath, args[1:], stdout, stderr)
	case "show":
		return diskShow(configPath, args[1:], stdout, stderr)
	case "remove":
		return diskRemove(configPath, args[1:], stdout, stderr)
	case "fail":
		return diskFail(configPath, args[1:], stdout, stderr)
	case "replace":
		return diskReplace(configPath, args[1:], stdout, stderr)
	case "list":
		return diskList(configPath, args[1:], stdout, stderr)
	default:
		return usage(stderr, fmt.Sprintf(T("disk: unknown command %q"), args[0]))
	}
}

// diskEntry is one disk of api/schemas/disk-list.schema.json.
type diskEntry struct {
	ID      string  `json:"id"`
	UUID    string  `json:"uuid"`
	FS      string  `json:"fs"`
	Mount   string  `json:"mount"`
	Role    string  `json:"role"`
	State   string  `json:"state"`
	Reason  *string `json:"reason"`
	Message *string `json:"message"`
}

// diskListOutput is the output of api/schemas/disk-list.schema.json.
type diskListOutput struct {
	Disks []diskEntry `json:"disks"`
}

func diskList(configPath string, args []string, stdout, stderr io.Writer) int {
	return diskListOn(configPath, args, newMachine, stdout, stderr)
}

// diskListOn is diskList on the machine newMachine gives, once the arguments and the configuration are read.
func diskListOn(configPath string, args []string, newMachine func() (disks.Machine, error), stdout, stderr io.Writer) int {
	asJSON := false
	for _, a := range args {
		if a != "--json" {
			return usage(stderr, fmt.Sprintf(T("disk list: unknown argument %q"), a))
		}
		if err := setFlag(&asJSON, a); err != nil {
			return usage(stderr, "disk list: "+err.Error())
		}
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	observer, err := newMachine()
	if err != nil {
		return failure(stderr, err)
	}
	verdicts, err := knownChecks(context.Background(), c, observer, func(err error) {
		report(stderr, fmt.Sprintf(T("warning: which disks' own marker was seen cannot be read (%v): no disk is said missing for its marker gone"), err))
	})
	if err != nil {
		return failure(stderr, err)
	}
	out := diskListFrom(c.Disks, verdicts)
	if asJSON {
		err = writeJSON(stdout, out)
	} else {
		err = printDiskList(stdout, out)
	}
	if err != nil {
		return failure(stderr, err)
	}
	return exitOK
}

// knownChecks runs the mount check of every disk with the question of its marker, as a pass asks it, which takes the
// catalog's record of which disks' own marker was seen (specs.md §10). A catalog that cannot be read is said through
// warn, and the check then takes none as seen.
func knownChecks(ctx context.Context, c *config.Config, m disks.Machine, warn func(error)) ([]disks.Verdict, error) {
	seen, err := markersSeenOf(ctx, c)
	if err != nil {
		warn(err)
	}
	verdicts, err := disks.CheckAllKnown(ctx, m, c.Disks, c.PoolID, seen)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	return verdicts, nil
}

// markersSeenOf reads which disks' own marker the catalog records seen.
func markersSeenOf(ctx context.Context, c *config.Config) (seen disks.Seen, err error) {
	db, err := catalog.OpenPool(ctx, catalogPath(c), c.PoolID)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	list, err := catalog.MarkersSeen(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	return disks.Seen(list), nil
}

func diskListFrom(list []config.Disk, verdicts []disks.Verdict) diskListOutput {
	out := diskListOutput{Disks: make([]diskEntry, 0, len(list))}
	for i := range list {
		d, v := &list[i], verdicts[i]
		entry := diskEntry{ID: d.ID, UUID: d.UUID, FS: string(d.FS), Mount: d.Mount, Role: d.Role, State: string(v.State)}
		if v.State != disks.Online {
			reason, message := string(v.Reason), v.Message
			entry.Reason, entry.Message = &reason, &message
		}
		out.Disks = append(out.Disks, entry)
	}
	return out
}

func printDiskList(w io.Writer, out diskListOutput) error {
	var text strings.Builder
	for i := range out.Disks {
		d := &out.Disks[i]
		fmt.Fprintf(&text, "%s: %s (%s, %s)", d.ID, d.State, d.Mount, d.FS)
		if d.Reason != nil {
			// The message of a mount check may hold an error of the system, which names a path as it is (specs.md §25).
			fmt.Fprintf(&text, ": %s: %s", *d.Reason, names.Escaped(*d.Message))
		}
		text.WriteString("\n")
	}
	return writeText(w, text.String())
}
