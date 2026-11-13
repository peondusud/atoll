package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/trash"
	"github.com/peondusud/atoll/internal/value"
)

// writeFlags are the flags of every command that moves or deletes (specs.md §25).
type writeFlags struct {
	asJSON, dryRun, yes bool
}

// take reads one of those flags, and says whether it was one.
func (f *writeFlags) take(a string) (bool, error) {
	var flag *bool
	switch a {
	case "--json":
		flag = &f.asJSON
	case "--dry-run":
		flag = &f.dryRun
	case "--yes":
		flag = &f.yes
	default:
		return false, nil
	}
	return true, setFlag(flag, a)
}

func (f *writeFlags) check() error {
	if f.dryRun && f.yes {
		return errors.New(T("--dry-run and --yes do not go together"))
	}
	return nil
}

// exitOf says what a command that moves or deletes exits with (specs.md §25).
func exitOf(f *writeFlags, s *trash.Summary) int {
	switch {
	case f.dryRun:
		return exitOK
	case s.ToDo > 0:
		return exitConfirm
	case s.Stayed > 0 || s.DiskLost:
		return exitPartial
	default:
		return exitOK
	}
}

// session opens what a command that writes needs, runs it and closes everything, the output
// included: nothing is left unsaid on an exit code that says the rest.
func session(c *config.Config, stdout, stderr io.Writer, apply bool, run func(*trash.Session, *bufio.Writer) error) (err error) {
	ctx := context.Background()
	machine, err := disks.NewMachine()
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	db, err := catalog.OpenPool(ctx, catalogPath(c), c.PoolID)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	if release, err = lockCatalog(ctx, c, stderr); err != nil {
		return fmt.Errorf("%w", err)
	}
	s, err := trash.Open(ctx, db, c, machine, apply)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	defer s.Close()
	buffered := bufio.NewWriter(stdout)
	if err := run(s, buffered); err != nil {
		return err
	}
	if err := s.Finish(ctx); err != nil {
		return fmt.Errorf("%w", err)
	}
	if err := buffered.Flush(); err != nil {
		return fmt.Errorf(T("writing the output: %w"), err)
	}
	return nil
}

func encodePath(p string) (path, pathB64 string) {
	if isUTF8(p) {
		return p, ""
	}
	return "", base64.StdEncoding.EncodeToString([]byte(p))
}

func shownPath(p string) string {
	if isUTF8(p) {
		return p
	}
	return "(base64) " + base64.StdEncoding.EncodeToString([]byte(p))
}

// pruneCopy is one copy of api/schemas/prune.schema.json.
type pruneCopy struct {
	Path    string `json:"path,omitempty"`
	PathB64 string `json:"path_b64,omitempty"`
	Disk    string `json:"disk"`
	Size    int64  `json:"size"`
	Outcome string `json:"outcome"`
}

// writeSummary is the rest of the output of a command that moves or deletes.
type writeSummary struct {
	Applied bool      `json:"applied"`
	Bytes   *int64    `json:"bytes,omitempty"`
	Full    *[]string `json:"full,omitempty"`
}

func runPrune(configPath string, args []string, stdout, stderr io.Writer) int {
	var flags writeFlags
	for _, a := range args {
		known, err := flags.take(a)
		if err != nil {
			return usage(stderr, "prune: "+err.Error())
		}
		if !known {
			return usage(stderr, fmt.Sprintf(T("prune: unknown argument %q"), a))
		}
	}
	if err := flags.check(); err != nil {
		return usage(stderr, "prune: "+err.Error())
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	code := exitOK
	err = session(c, stdout, stderr, flags.yes, func(s *trash.Session, out *bufio.Writer) error {
		list := newJSONList(out, "copies")
		summary, err := s.Prune(context.Background(), func(item *trash.PruneItem) error {
			if !flags.asJSON {
				return writeText(out, fmt.Sprintf("%s (%s): %s\n", shownPath(item.Path), item.Disk, item.Outcome))
			}
			path, b64 := encodePath(item.Path)
			return list.item(pruneCopy{Path: path, PathB64: b64, Disk: item.Disk, Size: item.Size, Outcome: item.Outcome})
		})
		if err != nil {
			return fmt.Errorf("%w", err)
		}
		code = exitOf(&flags, summary)
		return endWrite(out, list, flags.asJSON, summary, true)
	})
	if err != nil {
		return failure(stderr, err)
	}
	return code
}

func endWrite(out io.Writer, list *jsonList, asJSON bool, summary *trash.Summary, withBytes bool) error {
	if asJSON {
		rest := writeSummary{Applied: summary.Applied}
		if withBytes {
			rest.Bytes = &summary.Bytes
		}
		if summary.Full != nil {
			rest.Full = &summary.Full
		}
		return list.end(rest)
	}
	for _, id := range summary.Full {
		line := fmt.Sprintf(T("the trash of %s still holds more than trash.max_size: what is left there waits for its time\n"), id)
		if err := writeText(out, line); err != nil {
			return err
		}
	}
	switch {
	case !summary.Applied && summary.ToDo > 0 && withBytes:
		return writeText(out, fmt.Sprintf(T("%d to do, %d bytes, and %d staying: nothing was written\n"), summary.ToDo, summary.Bytes, summary.Stayed))
	case !summary.Applied && summary.ToDo > 0:
		return writeText(out, fmt.Sprintf(T("%d to do, and %d staying: nothing was written\n"), summary.ToDo, summary.Stayed))
	case withBytes:
		return writeText(out, fmt.Sprintf(T("%d bytes, and %d staying\n"), summary.Bytes, summary.Stayed))
	default:
		return writeText(out, fmt.Sprintf(T("%d staying\n"), summary.Stayed))
	}
}

func runTrash(configPath string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usage(stderr, T("trash: want list, restore or purge"))
	}
	switch args[0] {
	case "list":
		return trashList(configPath, args[1:], stdout, stderr)
	case "restore":
		return trashRestore(configPath, args[1:], stdout, stderr)
	case "purge":
		return trashPurge(configPath, args[1:], stdout, stderr)
	default:
		return usage(stderr, fmt.Sprintf(T("trash: unknown command %q"), args[0]))
	}
}

// trashEntry is one entry of api/schemas/trash-list.schema.json.
type trashEntry struct {
	Path    string  `json:"path,omitempty"`
	PathB64 string  `json:"path_b64,omitempty"`
	Disk    string  `json:"disk"`
	Reason  string  `json:"reason"`
	Size    *int64  `json:"size"`
	Blake3  *string `json:"blake3"`
	Deleted string  `json:"deleted"`
	Expires string  `json:"expires"`
	Op      int64   `json:"op"`
}

func trashList(configPath string, args []string, stdout, stderr io.Writer) int {
	asJSON := false
	for _, a := range args {
		if a != "--json" {
			return usage(stderr, fmt.Sprintf(T("trash list: unknown argument %q"), a))
		}
		if err := setFlag(&asJSON, a); err != nil {
			return usage(stderr, "trash list: "+err.Error())
		}
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	if err := writeTrash(context.Background(), c, asJSON, stdout); err != nil {
		return failure(stderr, err)
	}
	return exitOK
}

// writeTrash reads the catalog and touches no disk (specs.md §17.4), and writes as it reads.
func writeTrash(ctx context.Context, c *config.Config, asJSON bool, stdout io.Writer) (err error) {
	db, err := catalog.OpenPool(ctx, catalogPath(c), c.PoolID)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	out := bufio.NewWriter(stdout)
	list := newJSONList(out, "entries")
	err = catalog.EachTrashEntry(ctx, db, func(e *catalog.TrashEntry) error {
		entry := trashEntry{
			Disk: e.Disk, Reason: e.Reason, Size: nullInt(e.Size), Deleted: timeText(e.DeletedNS), Expires: timeText(e.ExpiresNS), Op: e.OpID,
		}
		entry.Path, entry.PathB64 = encodePath(e.Path)
		if e.Hash != nil {
			hash := hex.EncodeToString(e.Hash)
			entry.Blake3 = &hash
		}
		if asJSON {
			return list.item(entry)
		}
		return writeText(out, fmt.Sprintf(T("%s (%s): %s, trashed %s, until %s\n"), shownPath(e.Path), e.Disk, e.Reason, entry.Deleted, entry.Expires))
	})
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	if asJSON {
		if err := list.close(); err != nil {
			return err
		}
	}
	if err := out.Flush(); err != nil {
		return fmt.Errorf(T("writing the output: %w"), err)
	}
	return nil
}

// restoreEntry is one entry of api/schemas/trash-restore.schema.json.
type restoreEntry struct {
	Path    string `json:"path,omitempty"`
	PathB64 string `json:"path_b64,omitempty"`
	Disk    string `json:"disk"`
	To      string `json:"to,omitempty"`
	Op      int64  `json:"op"`
	Outcome string `json:"outcome"`
}

type restoreArgs struct {
	writeFlags
	under string
	// to is the disk --to names, empty without it (specs.md §17.4).
	to string
}

func parseRestore(args []string) (restoreArgs, error) {
	var out restoreArgs
	var paths []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			paths = append(paths, args[i+1:]...)
			break
		}
		known, err := out.take(a)
		to, isTo := strings.CutPrefix(a, "--to=")
		switch {
		case err != nil:
			return out, err
		case known:
		case a == "--to" || isTo:
			if out.to != "" {
				return out, errors.New(T("--to comes twice"))
			}
			if a == "--to" {
				if i+1 == len(args) {
					return out, errors.New(T("--to needs a disk id"))
				}
				i++
				to = args[i]
			}
			if to == "" {
				return out, errors.New(T("--to needs a disk id"))
			}
			out.to = to
		case strings.HasPrefix(a, "-"):
			return out, fmt.Errorf(T("unknown argument %q"), a)
		default:
			paths = append(paths, a)
		}
	}
	if len(paths) != 1 {
		return out, fmt.Errorf(T("want one path, got %d"), len(paths))
	}
	out.under = paths[0]
	if _, err := names.CheckPath(out.under, names.Ext4); err != nil {
		return out, fmt.Errorf("%w", err)
	}
	return out, out.check()
}

func trashRestore(configPath string, args []string, stdout, stderr io.Writer) int {
	parsed, err := parseRestore(args)
	if err != nil {
		return usage(stderr, "trash restore: "+err.Error())
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	if parsed.to != "" && !slices.ContainsFunc(c.Disks, func(d config.Disk) bool { return d.ID == parsed.to }) {
		return usage(stderr, fmt.Sprintf(T("trash restore: --to: the configuration has no disk %q"), parsed.to))
	}
	code := exitOK
	err = session(c, stdout, stderr, parsed.yes, func(s *trash.Session, out *bufio.Writer) error {
		list := newJSONList(out, "entries")
		each := func(item *trash.EntryItem) error {
			if !parsed.asJSON {
				if parsed.to != "" {
					return writeText(out, fmt.Sprintf(T("%s (%s, to %s): %s\n"), shownPath(item.Path), item.Disk, parsed.to, item.Outcome))
				}
				return writeText(out, fmt.Sprintf("%s (%s): %s\n", shownPath(item.Path), item.Disk, item.Outcome))
			}
			path, b64 := encodePath(item.Path)
			return list.item(restoreEntry{Path: path, PathB64: b64, Disk: item.Disk, To: parsed.to, Op: item.OpID, Outcome: item.Outcome})
		}
		var summary *trash.Summary
		var err error
		if parsed.to != "" {
			summary, err = s.RestoreTo(context.Background(), parsed.under, parsed.to, each)
		} else {
			summary, err = s.Restore(context.Background(), parsed.under, each)
		}
		if err != nil {
			return fmt.Errorf("%w", err)
		}
		code = exitOf(&parsed.writeFlags, summary)
		return endWrite(out, list, parsed.asJSON, summary, false)
	})
	if err != nil {
		return failure(stderr, err)
	}
	return code
}

// purgeEntry is one entry of api/schemas/trash-purge.schema.json.
type purgeEntry struct {
	Path    string `json:"path,omitempty"`
	PathB64 string `json:"path_b64,omitempty"`
	Disk    string `json:"disk"`
	Op      int64  `json:"op"`
	Size    *int64 `json:"size"`
	Why     string `json:"why"`
	Outcome string `json:"outcome"`
}

type purgeArgs struct {
	writeFlags
	olderThan *time.Duration
}

func parsePurge(args []string) (purgeArgs, error) {
	var out purgeArgs
	for i := 0; i < len(args); i++ {
		a := args[i]
		known, err := out.take(a)
		if err != nil {
			return out, err
		}
		if known {
			continue
		}
		text, isOlder := strings.CutPrefix(a, "--older-than=")
		switch {
		case isOlder:
		case a != "--older-than":
			return out, fmt.Errorf(T("unknown argument %q"), a)
		case i+1 == len(args):
			return out, errors.New(T("--older-than needs a duration"))
		default:
			i++
			text = args[i]
		}
		if out.olderThan != nil {
			return out, errors.New(T("--older-than comes twice"))
		}
		duration, err := value.ParseDuration(text)
		if err != nil {
			return out, fmt.Errorf("--older-than: %w", err)
		}
		out.olderThan = &duration
	}
	return out, out.check()
}

func trashPurge(configPath string, args []string, stdout, stderr io.Writer) int {
	parsed, err := parsePurge(args)
	if err != nil {
		return usage(stderr, "trash purge: "+err.Error())
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	code := exitOK
	err = session(c, stdout, stderr, parsed.yes, func(s *trash.Session, out *bufio.Writer) error {
		list := newJSONList(out, "entries")
		summary, err := s.Purge(context.Background(), parsed.olderThan, func(item *trash.EntryItem) error {
			if !parsed.asJSON {
				return writeText(out, fmt.Sprintf("%s (%s): %s, %s\n", shownPath(item.Path), item.Disk, item.Outcome, item.Why))
			}
			path, b64 := encodePath(item.Path)
			return list.item(purgeEntry{Path: path, PathB64: b64, Disk: item.Disk, Op: item.OpID, Size: nullInt(item.Size), Why: item.Why, Outcome: item.Outcome})
		})
		if err != nil {
			return fmt.Errorf("%w", err)
		}
		code = exitOf(&parsed.writeFlags, summary)
		return endWrite(out, list, parsed.asJSON, summary, true)
	})
	if err != nil {
		return failure(stderr, err)
	}
	return code
}
