package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/watch"
)

// catalogBackupOutput is the output of api/schemas/catalog-backup.schema.json.
type catalogBackupOutput struct {
	Backup  string              `json:"backup"`
	Removed []string            `json:"removed"`
	Disks   []catalogBackupDisk `json:"disks"`
}

type catalogBackupDisk struct {
	ID      string `json:"id"`
	Written bool   `json:"written"`
}

// catalogRebuildOutput is the output of api/schemas/catalog-rebuild.schema.json.
type catalogRebuildOutput struct {
	Catalog   string     `json:"catalog"`
	SetAside  *string    `json:"set_aside"`
	OpsAfter  int64      `json:"ops_after"`
	Disks     []scanDisk `json:"disks"`
	Conflicts []pathItem `json:"conflicts"`
	Done      bool       `json:"done"`
}

// catalogVerifyOutput is the output of api/schemas/catalog-verify.schema.json.
type catalogVerifyOutput struct {
	Problems []string   `json:"problems"`
	Disks    []scanDisk `json:"disks"`
}

// runCatalog runs atoll catalog backup, verify and rebuild (specs.md §9, §25).
func runCatalog(configPath string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usage(stderr, "catalog: want backup, verify or rebuild")
	}
	sub := args[0]
	allowed := map[string][]string{"backup": {"--json"}, "verify": {"--json"}, "rebuild": {"--json", "--dry-run", "--yes"}}
	if allowed[sub] == nil {
		return usage(stderr, fmt.Sprintf("catalog: unknown command %q", sub))
	}
	flags := map[string]bool{}
	for _, a := range args[1:] {
		if !slices.Contains(allowed[sub], a) {
			return usage(stderr, fmt.Sprintf("catalog %s: unknown argument %q", sub, a))
		}
		if flags[a] {
			return usage(stderr, fmt.Sprintf("catalog %s: %s comes twice", sub, a))
		}
		flags[a] = true
	}
	if flags["--dry-run"] && flags["--yes"] {
		return usage(stderr, "catalog rebuild: --dry-run and --yes do not go together")
	}
	asJSON := flags["--json"]
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	ctx := context.Background()
	switch sub {
	case "verify":
		return catalogVerify(ctx, c, asJSON, stdout, stderr)
	case "rebuild":
		return catalogRebuild(ctx, c, flags, stdout, stderr)
	}
	release, err := lockCatalog(ctx, c, stderr)
	if err != nil {
		return failure(stderr, err)
	}
	defer release()
	m, err := disks.NewMachine()
	if err != nil {
		return failure(stderr, err)
	}
	out, code, err := catalogBackup(ctx, c, m, stderr)
	if err != nil {
		return failure(stderr, err)
	}
	if asJSON {
		err = writeJSON(stdout, out)
	} else {
		var b bytes.Buffer
		fmt.Fprintf(&b, "backup %s\n", out.Backup)
		for _, name := range out.Removed {
			fmt.Fprintf(&b, "removed %s\n", name)
		}
		for _, d := range out.Disks {
			if d.Written {
				fmt.Fprintf(&b, "disk %s: copied\n", d.ID)
			} else {
				fmt.Fprintf(&b, "disk %s: not copied\n", d.ID)
			}
		}
		err = writeText(stdout, b.String())
	}
	if err != nil {
		return failure(stderr, err)
	}
	return code
}

// catalogBackup backs the catalog up beside it, and with maintenance.catalog_on_disks copies the backup to each disk
// not failed; a disk it could not copy to is exit 4.
func catalogBackup(ctx context.Context, c *config.Config, m disks.Machine, stderr io.Writer) (out *catalogBackupOutput, code int, err error) {
	db, err := catalog.OpenPool(ctx, catalogPath(c), c.PoolID)
	if err != nil {
		return nil, 0, fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	done, err := jobs.BackupCatalog(ctx, db, c, m)
	if err != nil {
		return nil, 0, fmt.Errorf("%w", err)
	}
	out = &catalogBackupOutput{Backup: done.Path, Removed: append([]string{}, done.Removed...), Disks: []catalogBackupDisk{}}
	code = exitOK
	for _, d := range done.Disks {
		if d.Err != nil {
			report(stderr, fmt.Sprintf("warning: disk %s: the backup was not copied: %v", d.ID, d.Err))
			code = exitPartial
		}
		out.Disks = append(out.Disks, catalogBackupDisk{ID: d.ID, Written: d.Err == nil})
	}
	return out, code, nil
}

// catalogVerify runs SQLite's integrity and foreign key checks on the catalog, then compares each disk with it, as
// a scan would, writing nothing (specs.md §9, §25): exit 4 with a problem, a difference, or a disk not walked.
func catalogVerify(ctx context.Context, c *config.Config, asJSON bool, stdout, stderr io.Writer) int {
	m, err := disks.NewMachine()
	if err != nil {
		return failure(stderr, err)
	}
	out, err := verifyCatalog(ctx, c, m)
	if err != nil {
		return failure(stderr, err)
	}
	if asJSON {
		err = writeJSON(stdout, out)
	} else {
		var b bytes.Buffer
		for _, p := range out.Problems {
			fmt.Fprintf(&b, "problem: %s\n", p)
		}
		if len(out.Problems) == 0 {
			fmt.Fprintf(&b, "the catalog %s is in order\n", path.Base(catalogPath(c)))
		}
		err = printScan(&b, scanOutput{Disks: out.Disks, Conflicts: []pathItem{}})
		if err == nil {
			err = writeText(stdout, b.String())
		}
	}
	if err != nil {
		return failure(stderr, err)
	}
	if len(out.Problems) > 0 || differs(out.Disks) {
		return exitPartial
	}
	return exitOK
}

// differs says whether a disk was not walked, or does not hold what the catalog says: a file it does not know, a
// copy changed, moved, absent or whose path another file took, or a name that could not be read.
func differs(list []scanDisk) bool {
	for i := range list {
		d := &list[i]
		if !d.Walked || d.New > 0 || len(d.Changed)+len(d.Moved)+len(d.Absent)+len(d.PathTaken)+len(d.Unreadable) > 0 {
			return true
		}
	}
	return false
}

func verifyCatalog(ctx context.Context, c *config.Config, m disks.Machine) (out *catalogVerifyOutput, err error) {
	db, err := catalog.OpenPool(ctx, catalogPath(c), c.PoolID)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	problems, err := catalog.Integrity(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	out = &catalogVerifyOutput{Problems: append([]string{}, problems...), Disks: []scanDisk{}}
	// A catalog SQLite finds damaged is not read further: what it says of the disks could be wrong.
	if len(problems) > 0 {
		return out, nil
	}
	compared, err := watch.Compare(ctx, db, c, m)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	out.Disks = scanOutputFrom(compared).Disks
	return out, nil
}

// catalogRebuild runs atoll catalog rebuild (specs.md §9): a new catalog from what the disks hold, every copy read
// whole, made beside the old one, which is then set aside as <catalog>.old-<UTC time>, never removed. Without --yes it
// says what it would do. It works when the old catalog cannot be opened: of a later version, damaged, or gone.
func catalogRebuild(ctx context.Context, c *config.Config, flags map[string]bool, stdout, stderr io.Writer) int {
	file := catalogPath(c)
	out := catalogRebuildOutput{Catalog: file, Disks: []scanDisk{}, Conflicts: []pathItem{}}
	code := exitOK
	if !flags["--yes"] {
		if !flags["--dry-run"] {
			code = exitConfirm
		}
		return writeRebuild(&out, flags["--json"], code, stdout, stderr)
	}
	m, err := disks.NewMachine()
	if err != nil {
		return failure(stderr, err)
	}
	there, err := fsx.Exists(file)
	if err != nil {
		return failure(stderr, fmt.Errorf("catalog rebuild: %w", err))
	}
	if there {
		release, err := lockCatalog(ctx, c, stderr)
		if err != nil {
			return failure(stderr, err)
		}
		defer release()
		if err := catalog.Checkpoint(ctx, file); err != nil {
			report(stderr, fmt.Sprintf("warning: %v: its log is set aside with it", err))
		}
	}
	fresh := file + ".rebuild"
	for _, name := range []string{fresh, fresh + "-wal", fresh + "-shm"} {
		if err := fsx.RemoveFile(name); err != nil && !fsx.IsAbsent(err) {
			return failure(stderr, fmt.Errorf("catalog rebuild: %w", err))
		}
	}
	if _, err := catalog.Create(ctx, fresh, c.PoolID); err != nil {
		return failure(stderr, fmt.Errorf("catalog rebuild: %w", err))
	}
	// Locked before it takes the catalog's name, so that a command waiting for the lock waits for this one then.
	releaseFresh, err := fsx.LockFile(fresh)
	if err != nil {
		return failure(stderr, fmt.Errorf("catalog rebuild: %w", err))
	}
	defer releaseFresh()
	outcome, err := rebuildInto(ctx, c, m, fresh, &out, stderr)
	if err != nil {
		return failure(stderr, fmt.Errorf("catalog rebuild: %w", err))
	}
	old, err := catalog.SetAside(file, fresh, m.Now())
	if err != nil {
		return failure(stderr, fmt.Errorf("catalog rebuild: the new catalog is %s, not yet in place: %w", fresh, err))
	}
	if old != "" {
		out.SetAside = &old
	}
	out.Done = true
	scanned := scanOutputFrom(outcome)
	out.Disks, out.Conflicts = scanned.Disks, scanned.Conflicts
	if outcome.Partial() {
		code = exitPartial
	}
	return writeRebuild(&out, flags["--json"], code, stdout, stderr)
}

// rebuildInto fills the new catalog: its operations past the highest id the disks' trash and tmp/ name, then a full
// scan, which reads every copy whole.
func rebuildInto(ctx context.Context, c *config.Config, m disks.Machine, fresh string, out *catalogRebuildOutput, stderr io.Writer) (outcome *watch.Outcome, err error) {
	db, err := catalog.OpenPool(ctx, fresh, c.PoolID)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	for i := range c.Disks {
		d := &c.Disks[i]
		if d.State == config.DiskFailed {
			continue
		}
		highest, err := highestOpID(m, d)
		if err != nil {
			report(stderr, fmt.Sprintf("warning: disk %s: the operations its trash and tmp/ name are not known (%v): a new one may meet an old one there, which costs it one io-error", d.ID, err))
			continue
		}
		out.OpsAfter = max(out.OpsAfter, highest)
	}
	if err := catalog.StartOpsAfter(ctx, db, out.OpsAfter); err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	outcome, err = watch.Scan(ctx, db, c, m, watch.Options{Full: true})
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	return outcome, nil
}

// highestOpID reads the highest operation id a disk's work folder names, through a handle on its top.
func highestOpID(m disks.Machine, d *config.Disk) (int64, error) {
	if v := disks.Check(m, d); v.State == disks.Missing {
		return 0, fmt.Errorf("it is missing (%s)", v.Reason)
	}
	handle, err := ops.Open(m, d, 0)
	if err != nil {
		return 0, fmt.Errorf("%w", err)
	}
	defer handle.Close()
	n, err := handle.HighestOpID()
	if err != nil {
		return 0, fmt.Errorf("%w", err)
	}
	return n, nil
}

func writeRebuild(out *catalogRebuildOutput, asJSON bool, code int, stdout, stderr io.Writer) int {
	var err error
	if asJSON {
		err = writeJSON(stdout, out)
	} else {
		var b bytes.Buffer
		switch {
		case !out.Done:
			fmt.Fprintf(&b, "would set %s aside, and make a new catalog from what the disks hold, reading every copy whole: --yes to do it\n", out.Catalog)
		case out.SetAside != nil:
			fmt.Fprintf(&b, "a new catalog at %s; the old one is at %s\n", out.Catalog, *out.SetAside)
		default:
			fmt.Fprintf(&b, "a new catalog at %s\n", out.Catalog)
		}
		if out.Done {
			err = printScan(&b, scanOutput{Disks: out.Disks, Conflicts: out.Conflicts})
		}
		if err == nil {
			err = writeText(stdout, b.String())
		}
	}
	if err != nil {
		return failure(stderr, err)
	}
	return code
}
