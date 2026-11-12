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
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/ops"
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
	Problems []string `json:"problems"`
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
// that can be written; a disk it could not copy to is exit 4.
func catalogBackup(ctx context.Context, c *config.Config, m disks.Machine, stderr io.Writer) (out *catalogBackupOutput, code int, err error) {
	file := catalogPath(c)
	db, err := catalog.OpenPool(ctx, file, c.PoolID)
	if err != nil {
		return nil, 0, fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	written, removed, err := catalog.Backup(ctx, db, file, c.PoolID, m.Now())
	if err != nil {
		return nil, 0, fmt.Errorf("%w", err)
	}
	out = &catalogBackupOutput{Backup: written, Removed: append([]string{}, removed...), Disks: []catalogBackupDisk{}}
	code = exitOK
	if on, _ := c.Effective()["maintenance.catalog_on_disks"].(bool); !on {
		return out, code, nil
	}
	for i := range c.Disks {
		d := &c.Disks[i]
		if d.State == config.DiskFailed {
			continue
		}
		line := catalogBackupDisk{ID: d.ID}
		if err := copyBackupTo(m, d, written); err != nil {
			report(stderr, fmt.Sprintf("warning: disk %s: the backup was not copied: %v", d.ID, err))
			code = exitPartial
		} else {
			line.Written = true
		}
		out.Disks = append(out.Disks, line)
	}
	return out, code, nil
}

// copyBackupTo copies a backup to a disk's Atoll/catalog/, through a handle confirmed first (invariant 3). A disk
// missing or read-only takes none.
func copyBackupTo(m disks.Machine, d *config.Disk, backup string) error {
	if v := disks.Check(m, d); v.State != disks.Online {
		return fmt.Errorf("it is %s (%s)", v.State, v.Reason)
	}
	handle, err := ops.Open(m, d, 0)
	if err != nil {
		return err //nolint:wrapcheck // ops names the disk
	}
	defer handle.Close()
	if v := disks.Confirm(m, d); v.State == disks.Missing {
		return fmt.Errorf("%s (%s)", v.Message, v.Reason)
	}
	if err := handle.Verify(); err != nil {
		return err //nolint:wrapcheck // likewise
	}
	src, err := fsx.OpenSystemFile(backup)
	if err != nil {
		return err //nolint:wrapcheck // fsx names the file
	}
	defer func() { _ = src.Close() }()
	return handle.PutCatalog(src) //nolint:wrapcheck // ops names the disk
}

// catalogVerify runs SQLite's integrity and foreign key checks on the catalog: exit 4 with a problem.
func catalogVerify(ctx context.Context, c *config.Config, asJSON bool, stdout, stderr io.Writer) int {
	out, err := verifyCatalog(ctx, c)
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
		err = writeText(stdout, b.String())
	}
	if err != nil {
		return failure(stderr, err)
	}
	if len(out.Problems) > 0 {
		return exitPartial
	}
	return exitOK
}

func verifyCatalog(ctx context.Context, c *config.Config) (out *catalogVerifyOutput, err error) {
	db, err := catalog.OpenPool(ctx, catalogPath(c), c.PoolID)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	problems, err := catalog.Integrity(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	return &catalogVerifyOutput{Problems: append([]string{}, problems...)}, nil
}
