package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/watch"
)

type scanArgs struct {
	opts   watch.Options
	asJSON bool
}

// parseScan reads the arguments of scan (specs.md §25).
func parseScan(args []string) (scanArgs, error) {
	var out scanArgs
	diskGiven := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--json":
			out.asJSON = true
			continue
		case "--full":
			out.opts.Full = true
			continue
		}
		value, isDisk := strings.CutPrefix(a, "--disk=")
		switch {
		case isDisk:
		case a != "--disk":
			return out, fmt.Errorf("unknown argument %q", a)
		case i+1 == len(args):
			return out, errors.New("--disk needs an id")
		default:
			i++
			value = args[i]
		}
		if value == "" {
			return out, errors.New("--disk needs an id")
		}
		if diskGiven {
			return out, errors.New("--disk comes twice")
		}
		diskGiven = true
		out.opts.Disk = value
	}
	return out, nil
}

// pathItem is a path of specs.md §29.3.
type pathItem struct {
	Path    string `json:"path,omitempty"`
	PathB64 string `json:"path_b64,omitempty"`
}

func pathItems(list []string) []pathItem {
	out := make([]pathItem, 0, len(list))
	for _, p := range list {
		if utf8.ValidString(p) {
			out = append(out, pathItem{Path: p})
		} else {
			out = append(out, pathItem{PathB64: base64.StdEncoding.EncodeToString([]byte(p))})
		}
	}
	return out
}

// scanDisk is one disk of api/schemas/scan.schema.json.
type scanDisk struct {
	ID          string     `json:"id"`
	State       string     `json:"state"`
	Reason      *string    `json:"reason"`
	Message     *string    `json:"message"`
	Walked      bool       `json:"walked"`
	Files       int        `json:"files"`
	Bytes       int64      `json:"bytes"`
	Folders     int        `json:"folders"`
	Symlinks    int        `json:"symlinks"`
	Ignored     int        `json:"ignored"`
	New         int        `json:"new"`
	Known       int        `json:"known"`
	Hashed      int        `json:"hashed"`
	Changed     []pathItem `json:"changed"`
	Absent      []pathItem `json:"absent"`
	Unreadable  []pathItem `json:"unreadable"`
	OtherMounts []pathItem `json:"other_mounts"`
}

// scanOutput is the output of api/schemas/scan.schema.json.
type scanOutput struct {
	Disks     []scanDisk `json:"disks"`
	Conflicts []pathItem `json:"conflicts"`
}

func runScan(configPath string, args []string, stdout, stderr io.Writer) int {
	parsed, err := parseScan(args)
	if err != nil {
		return usage(stderr, "scan: "+err.Error())
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	if parsed.opts.Disk != "" && !hasDisk(c, parsed.opts.Disk) {
		return failure(stderr, fmt.Errorf("scan: no disk %q in the configuration", parsed.opts.Disk))
	}
	outcome, err := scan(c, parsed.opts)
	if err != nil {
		return failure(stderr, err)
	}
	out := scanOutputFrom(outcome)
	if parsed.asJSON {
		err = writeJSON(stdout, out)
	} else {
		err = printScan(stdout, out)
	}
	if err != nil {
		return failure(stderr, err)
	}
	if outcome.Partial() {
		return exitPartial
	}
	return exitOK
}

func hasDisk(c *config.Config, id string) bool {
	for i := range c.Disks {
		if c.Disks[i].ID == id {
			return true
		}
	}
	return false
}

func scan(c *config.Config, opts watch.Options) (outcome *watch.Outcome, err error) {
	ctx := context.Background()
	machine, err := disks.NewMachine()
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	db, err := catalog.OpenPool(ctx, catalogPath(c), c.PoolID)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	outcome, err = watch.Scan(ctx, db, c, machine, opts)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	return outcome, nil
}

func scanOutputFrom(o *watch.Outcome) scanOutput {
	out := scanOutput{Disks: make([]scanDisk, 0, len(o.Disks)), Conflicts: pathItems(o.Conflicts)}
	for i := range o.Disks {
		r := &o.Disks[i]
		d := scanDisk{
			ID: r.ID, State: string(r.Verdict.State), Walked: r.Walked,
			Files: r.Files, Bytes: r.Bytes, Folders: r.Folders, Symlinks: r.Symlinks, Ignored: r.Ignored,
			New: r.New, Known: r.Known, Hashed: r.Hashed,
			Changed: pathItems(r.Changed), Absent: pathItems(r.Absent), Unreadable: pathItems(r.Unreadable), OtherMounts: pathItems(r.OtherMounts),
		}
		if r.Verdict.State != disks.Online {
			reason, message := string(r.Verdict.Reason), r.Verdict.Message
			d.Reason, d.Message = &reason, &message
		}
		out.Disks = append(out.Disks, d)
	}
	return out
}

func printScan(w io.Writer, out scanOutput) error {
	var text strings.Builder
	for i := range out.Disks {
		d := &out.Disks[i]
		fmt.Fprintf(&text, "%s: %s", d.ID, d.State)
		if d.Reason != nil {
			fmt.Fprintf(&text, ": %s: %s", *d.Reason, *d.Message)
		}
		text.WriteString("\n")
		if d.Walked {
			fmt.Fprintf(&text, "  %d files, %d bytes, %d folders, %d symlinks, %d ignored; %d new, %d known, %d hashed\n",
				d.Files, d.Bytes, d.Folders, d.Symlinks, d.Ignored, d.New, d.Known, d.Hashed)
		}
		printPaths(&text, "  changed", d.Changed)
		printPaths(&text, "  absent", d.Absent)
		printPaths(&text, "  unreadable", d.Unreadable)
		printPaths(&text, "  on another mount", d.OtherMounts)
	}
	printPaths(&text, "conflict", out.Conflicts)
	return writeText(w, text.String())
}

func printPaths(text *strings.Builder, label string, list []pathItem) {
	for _, p := range list {
		if p.Path != "" {
			fmt.Fprintf(text, "%s: %s\n", label, p.Path)
		} else {
			fmt.Fprintf(text, "%s: (base64) %s\n", label, p.PathB64)
		}
	}
}
