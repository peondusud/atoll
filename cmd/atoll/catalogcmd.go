package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
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

// catalogVerifyOutput is the output of api/schemas/catalog-verify.schema.json.
type catalogVerifyOutput struct {
	Problems []string   `json:"problems"`
	Disks    []scanDisk `json:"disks"`
}

// runCatalog runs atoll catalog backup and atoll catalog verify (specs.md §9, §25).
func runCatalog(configPath string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usage(stderr, "catalog: want backup or verify")
	}
	sub := args[0]
	if sub != "backup" && sub != "verify" {
		return usage(stderr, fmt.Sprintf("catalog: unknown command %q", sub))
	}
	asJSON := false
	for _, a := range args[1:] {
		if a != "--json" {
			return usage(stderr, fmt.Sprintf("catalog %s: unknown argument %q", sub, a))
		}
		if err := setFlag(&asJSON, a); err != nil {
			return usage(stderr, "catalog "+sub+": "+err.Error())
		}
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	ctx := context.Background()
	if sub == "verify" {
		return catalogVerify(ctx, c, asJSON, stdout, stderr)
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
