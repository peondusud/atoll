package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/layout"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/trash"
)

// conflictCopyOutput is one copy of a file in conflict, in api/schemas/conflicts-list.schema.json and
// api/schemas/conflicts-resolve.schema.json: its size and modification time as its disk holds them.
type conflictCopyOutput struct {
	Disk    string `json:"disk"`
	Size    int64  `json:"size"`
	MtimeNS int64  `json:"mtime_ns"`
	Outcome string `json:"outcome,omitempty"`
}

// conflictFileOutput is one file of api/schemas/conflicts-list.schema.json.
type conflictFileOutput struct {
	pathItem
	Copies []conflictCopyOutput `json:"copies"`
}

// conflictsListOutput is the output of api/schemas/conflicts-list.schema.json.
type conflictsListOutput struct {
	Files []conflictFileOutput `json:"files"`
}

// conflictsResolveOutput is the output of api/schemas/conflicts-resolve.schema.json.
type conflictsResolveOutput struct {
	pathItem
	Keep     string               `json:"keep"`
	Applied  bool                 `json:"applied"`
	Resolved bool                 `json:"resolved"`
	Bytes    int64                `json:"bytes"`
	Copies   []conflictCopyOutput `json:"copies"`
}

// runConflicts runs atoll conflicts list and atoll conflicts resolve (specs.md §15.7, §25).
func runConflicts(configPath string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usage(stderr, T("conflicts: want list or resolve"))
	}
	switch args[0] {
	case "list":
		return conflictsList(configPath, args[1:], stdout, stderr)
	case "resolve":
		return conflictsResolve(configPath, args[1:], stdout, stderr)
	default:
		return usage(stderr, fmt.Sprintf(T("conflicts: unknown command %q"), args[0]))
	}
}

// conflictsList lists the files in conflict and their copies, from the catalog alone.
func conflictsList(configPath string, args []string, stdout, stderr io.Writer) int {
	asJSON := false
	for _, a := range args {
		if a != "--json" {
			return usage(stderr, fmt.Sprintf(T("conflicts list: unknown argument %q"), a))
		}
		if err := setFlag(&asJSON, a); err != nil {
			return usage(stderr, "conflicts list: "+err.Error())
		}
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	out, err := readConflicts(context.Background(), c)
	if err != nil {
		return failure(stderr, err)
	}
	if asJSON {
		err = writeJSON(stdout, out)
	} else {
		err = writeText(stdout, conflictsText(out))
	}
	if err != nil {
		return failure(stderr, err)
	}
	return exitOK
}

func readConflicts(ctx context.Context, c *config.Config) (out *conflictsListOutput, err error) {
	db, err := catalog.OpenPool(ctx, catalogPath(c), c.PoolID)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	position := map[string]int{}
	list, err := catalog.Disks(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	for i := range list {
		position[list[i].ID] = list[i].Position
	}
	out = &conflictsListOutput{Files: []conflictFileOutput{}}
	reader := catalog.NewFileReader(db)
	for {
		page, err := catalog.ConflictFiles(ctx, reader)
		if err != nil {
			return nil, fmt.Errorf("%w", err)
		}
		if len(page) == 0 {
			return out, nil
		}
		for i := range page {
			f := &page[i]
			file := conflictFileOutput{pathItem: pathItems([]string{f.Path})[0], Copies: make([]conflictCopyOutput, 0, len(f.Copies))}
			for _, cp := range f.Copies {
				file.Copies = append(file.Copies, conflictCopyOutput{Disk: cp.Disk, Size: cp.Size.Int64, MtimeNS: cp.MtimeNS.Int64})
			}
			// By disk position, as the configuration lists them.
			sortCopies(file.Copies, position)
			out.Files = append(out.Files, file)
		}
	}
}

func sortCopies(copies []conflictCopyOutput, position map[string]int) {
	for i := 1; i < len(copies); i++ {
		for j := i; j > 0 && position[copies[j].Disk] < position[copies[j-1].Disk]; j-- {
			copies[j], copies[j-1] = copies[j-1], copies[j]
		}
	}
}

func conflictsText(out *conflictsListOutput) string {
	var b bytes.Buffer
	if len(out.Files) == 0 {
		b.WriteString(T("no file in conflict\n"))
	}
	for _, f := range out.Files {
		b.WriteString(pathText(f.pathItem) + "\n")
		for _, c := range f.Copies {
			fmt.Fprintf(&b, T("  %s: %d bytes, modified %s\n"), c.Disk, c.Size, time.Unix(0, c.MtimeNS).UTC().Format(time.RFC3339Nano))
		}
	}
	return b.String()
}

// conflictsResolve keeps one copy of a file in conflict and sends the others to the trash, with --yes, and says what
// it would do without it.
func conflictsResolve(configPath string, args []string, stdout, stderr io.Writer) int {
	var flags writeFlags
	var path, keep string
	for i := 0; i < len(args); i++ {
		a := args[i]
		known, err := flags.take(a)
		if err != nil {
			return usage(stderr, "conflicts resolve: "+err.Error())
		}
		if known {
			continue
		}
		name, v, inline := strings.Cut(a, "=")
		switch {
		case name == "--keep":
			if !inline {
				if i+1 == len(args) {
					return usage(stderr, fmt.Sprintf("conflicts resolve: "+T("%s needs a value"), name))
				}
				i++
				v = args[i]
			}
			if keep != "" {
				return usage(stderr, fmt.Sprintf("conflicts resolve: "+T("%s comes twice"), name))
			}
			if !layout.IsIdentifier(v) {
				return usage(stderr, fmt.Sprintf("conflicts resolve: "+T("--keep: %q is no disk id"), v))
			}
			keep = v
		case strings.HasPrefix(a, "--") || path != "":
			return usage(stderr, fmt.Sprintf(T("conflicts resolve: unknown argument %q"), a))
		default:
			// Only its form counts here, as for status.
			if _, err := names.CheckPath(a, names.Ext4); err != nil {
				return usage(stderr, "conflicts resolve: "+err.Error())
			}
			path = a
		}
	}
	switch {
	case path == "":
		return usage(stderr, "conflicts resolve: "+T("a path is needed"))
	case keep == "":
		return usage(stderr, "conflicts resolve: "+T("--keep is needed"))
	}
	if err := flags.check(); err != nil {
		return usage(stderr, "conflicts resolve: "+err.Error())
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	if _, err := diskOfConfig(c, keep); err != nil {
		return failure(stderr, fmt.Errorf("conflicts resolve: %w", err))
	}
	code := exitOK
	err = session(c, stdout, stderr, flags.yes, func(s *trash.Session, out *bufio.Writer) error {
		result := conflictsResolveOutput{pathItem: pathItems([]string{path})[0], Keep: keep, Copies: []conflictCopyOutput{}}
		summary, err := s.Resolve(context.Background(), path, keep, func(item *trash.ResolveItem) error {
			result.Copies = append(result.Copies, conflictCopyOutput{Disk: item.Disk, Size: item.Size, MtimeNS: item.MtimeNS, Outcome: item.Outcome})
			return nil
		})
		switch {
		case errors.Is(err, trash.ErrNoFile):
			return fmt.Errorf("conflicts resolve: %s", T("the catalog has no file at that path"))
		case errors.Is(err, trash.ErrNotConflict):
			return fmt.Errorf("conflicts resolve: %s", T("the file is not in conflict"))
		case errors.Is(err, trash.ErrNoCopy):
			return fmt.Errorf("conflicts resolve: %s", fmt.Sprintf(T("the file has no copy on %s"), keep))
		case err != nil:
			return fmt.Errorf("%w", err)
		}
		result.Applied, result.Resolved, result.Bytes = summary.Applied, summary.Resolved, summary.Bytes
		switch {
		case flags.dryRun:
		case summary.ToDo > 0:
			code = exitConfirm
		case summary.Stayed > 0 || summary.DiskLost || !summary.Resolved:
			code = exitPartial
		}
		if flags.asJSON {
			return writeJSON(out, result)
		}
		return writeText(out, resolveText(&result, summary))
	})
	if err != nil {
		return failure(stderr, err)
	}
	return code
}

func resolveText(out *conflictsResolveOutput, summary *trash.ResolveSummary) string {
	var b bytes.Buffer
	for _, c := range out.Copies {
		fmt.Fprintf(&b, "%s (%s): %s\n", pathText(out.pathItem), c.Disk, c.Outcome)
	}
	switch {
	case out.Resolved:
		fmt.Fprintf(&b, T("resolved: the next pass makes its copies from %s\n"), out.Keep)
	case !out.Applied && summary.ToDo > 0:
		fmt.Fprintf(&b, T("%d to the trash, %d bytes: nothing was written\n"), summary.ToDo, summary.Bytes)
	default:
		b.WriteString(T("still in conflict: run it again once what stayed can be reached\n"))
	}
	return b.String()
}
