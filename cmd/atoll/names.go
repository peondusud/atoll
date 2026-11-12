package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/names"
)

// namesFinding is one finding of api/schemas/names-check.schema.json.
type namesFinding struct {
	Kind    string   `json:"kind"`
	Path    string   `json:"path,omitempty"`
	PathB64 string   `json:"path_b64,omitempty"`
	FS      string   `json:"fs,omitempty"`
	Reasons []string `json:"reasons,omitempty"`
	With    []string `json:"with,omitempty"`
	Disk    string   `json:"disk,omitempty"`
}

// namesOutput is the output of api/schemas/names-check.schema.json.
type namesOutput struct {
	Checked  int            `json:"checked"`
	Findings []namesFinding `json:"findings"`
}

type namesArgs struct {
	under  string
	asJSON bool
}

// runNames runs atoll names check: the report of the names to fix, from the catalog and the configuration only
// (specs.md §15.9).
func runNames(configPath string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usage(stderr, T("names: want check"))
	}
	if args[0] != "check" {
		return usage(stderr, fmt.Sprintf(T("names: unknown command %q"), args[0]))
	}
	parsed, err := parseNamesCheck(args[1:])
	if err != nil {
		return usage(stderr, "names check: "+err.Error())
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	out, err := namesCheck(context.Background(), c, parsed.under)
	if err != nil {
		return failure(stderr, err)
	}
	if parsed.asJSON {
		err = writeJSON(stdout, out)
	} else {
		err = writeNamesText(stdout, out)
	}
	if err != nil {
		return failure(stderr, err)
	}
	return exitOK
}

// parseNamesCheck reads the arguments of names check, as parseStatus does those of status (specs.md §25).
func parseNamesCheck(args []string) (namesArgs, error) {
	var out namesArgs
	given := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--json" {
			if err := setFlag(&out.asJSON, a); err != nil {
				return out, err
			}
			continue
		}
		value, isPath := strings.CutPrefix(a, "--path=")
		switch {
		case isPath:
		case a != "--path":
			return out, fmt.Errorf(T("unknown argument %q"), a)
		case i+1 == len(args):
			return out, errors.New(T("--path needs a path"))
		default:
			i++
			value = args[i]
		}
		if given {
			return out, errors.New(T("--path comes twice"))
		}
		given = true
		if _, err := names.CheckPath(value, names.Ext4); err != nil {
			return out, fmt.Errorf("--path: %w", err)
		}
		out.under = value
	}
	return out, nil
}

// namesCheck reads every path of the catalog and gives the report. It looks at no disk.
func namesCheck(ctx context.Context, c *config.Config, under string) (out *namesOutput, err error) {
	db, err := catalog.OpenPool(ctx, catalogPath(c), c.PoolID)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	paths, err := catalog.AllPaths(ctx, db)
	if err != nil {
		return nil, err
	}
	disks := make([]names.CheckedDisk, 0, len(c.Disks))
	for i := range c.Disks {
		d := &c.Disks[i]
		disks = append(disks, names.CheckedDisk{ID: d.ID, Mount: d.Mount, Root: d.Root})
	}
	report, err := names.Check(paths, disks, under)
	if err != nil {
		return nil, fmt.Errorf("names: %w", err)
	}
	out = &namesOutput{Checked: report.Checked, Findings: make([]namesFinding, 0, len(report.Findings))}
	for _, f := range report.Findings {
		item := namesFinding{Kind: string(f.Kind), FS: string(f.FS), With: f.With, Disk: f.Disk}
		if utf8.ValidString(f.Path) {
			item.Path = f.Path
		} else {
			item.PathB64 = base64.StdEncoding.EncodeToString([]byte(f.Path))
		}
		for _, r := range f.Reasons {
			item.Reasons = append(item.Reasons, string(r))
		}
		out.Findings = append(out.Findings, item)
	}
	return out, nil
}

// writeNamesText writes a finding a line, then how many names were checked.
func writeNamesText(w io.Writer, out *namesOutput) error {
	buffered := bufio.NewWriter(w)
	for _, f := range out.Findings {
		name := f.Path
		if name == "" {
			name = "(base64) " + f.PathB64
		}
		line := name + ": " + f.Kind
		switch {
		case f.FS != "":
			line += fmt.Sprintf(T(" on %s (%s)"), f.FS, strings.Join(f.Reasons, ", "))
		case len(f.With) > 0:
			line += " with " + strings.Join(f.With, ", ")
		case f.Disk != "":
			line += " on disk " + f.Disk
		}
		_, _ = fmt.Fprintln(buffered, line) // a write error stays in buffered, and Flush gives it
	}
	_, _ = fmt.Fprintf(buffered, T("%d names checked, %d findings\n"), out.Checked, len(out.Findings))
	if err := buffered.Flush(); err != nil {
		return fmt.Errorf(T("writing the output: %w"), err)
	}
	return nil
}
