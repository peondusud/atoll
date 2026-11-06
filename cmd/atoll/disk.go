package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
)

func runDisk(configPath string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usage(stderr, "disk: want list")
	}
	switch args[0] {
	case "list":
		return diskList(configPath, args[1:], stdout, stderr)
	default:
		return usage(stderr, fmt.Sprintf("disk: unknown command %q", args[0]))
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
	asJSON := false
	for _, a := range args {
		if a != "--json" {
			return usage(stderr, fmt.Sprintf("disk list: unknown argument %q", a))
		}
		asJSON = true
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	observer, err := disks.NewObserver()
	if err != nil {
		return failure(stderr, err)
	}
	verdicts, err := disks.CheckAll(context.Background(), observer, c.Disks)
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
			fmt.Fprintf(&text, ": %s: %s", *d.Reason, *d.Message)
		}
		text.WriteString("\n")
	}
	return writeText(w, text.String())
}
