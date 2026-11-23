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
	"github.com/peondusud/atoll/internal/layout"
	"github.com/peondusud/atoll/internal/ops"
)

// diskMarkOutput is the output of api/schemas/disk-mark.schema.json.
type diskMarkOutput struct {
	Disk string `json:"disk"`
	// State and Reason are what its confirmation found: online, or missing, no-marker.
	State  string         `json:"state"`
	Reason *string        `json:"reason"`
	Marker diskShowMarker `json:"marker"`
	// HadMarker says that the disk held its own marker already: nothing to write.
	HadMarker bool     `json:"had_marker"`
	Written   bool     `json:"written"`
	DryRun    bool     `json:"dry_run"`
	Warnings  []string `json:"warnings"`
}

// diskMarking is a disk mark that passed its checks: the disk's handle, confirmed, and what it is to be given.
type diskMarking struct {
	out    diskMarkOutput
	disk   *config.Disk
	handle *ops.Disk
	text   string
	seen   bool
}

// diskMark runs atoll disk mark: it writes a disk's marker again, as disk add writes it, on a disk whose mount check
// passes, but for a marker gone, missing no-marker, and records it seen (specs.md §10, §25). It refuses a disk
// missing for any other reason, readonly, failed, or that holds a marker of another pool, disk or filesystem, which
// it never writes over.
func diskMark(configPath string, args []string, stdout, stderr io.Writer) int {
	return diskMarkOn(configPath, args, newMachine, stdout, stderr)
}

// diskMarkOn is diskMark on the machine newMachine gives, once the arguments and the configuration are read.
func diskMarkOn(configPath string, args []string, newMachine func() (disks.Machine, error), stdout, stderr io.Writer) int {
	parsed, err := parseDiskID(args, "--dry-run", "--yes", "--json")
	if err != nil {
		return usage(stderr, "disk mark: "+err.Error())
	}
	_, c, err := readConfig(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	// Before any wait: a disk the configuration does not have is said at once.
	d, err := diskOfConfig(c, parsed.id)
	if err != nil {
		return failure(stderr, fmt.Errorf("disk mark: %w", err))
	}
	m, err := newMachine()
	if err != nil {
		return failure(stderr, err)
	}
	ctx := context.Background()
	dryRun := parsed.flags["--dry-run"]
	if !dryRun {
		var release func()
		if _, c, release, err = lockedConfig(ctx, configPath, c, stderr); err != nil {
			return failure(stderr, err)
		}
		defer release()
		// The disk as the configuration has it now.
		if d, err = diskOfConfig(c, parsed.id); err != nil {
			return failure(stderr, fmt.Errorf("disk mark: %w", err))
		}
	}
	a, err := checkDiskMark(ctx, c, m, d)
	if err != nil {
		return failure(stderr, fmt.Errorf("disk mark: %w", err))
	}
	defer a.handle.Close()
	a.out.DryRun = dryRun
	code := exitOK
	switch {
	case dryRun:
	case !parsed.flags["--yes"]:
		// A disk that holds its own marker already is nothing to do.
		if !a.out.HadMarker {
			code = exitConfirm
		}
	default:
		if code, err = a.finish(ctx, configPath, c, m, stderr); err != nil {
			return failure(stderr, fmt.Errorf("disk mark: %w", err))
		}
	}
	if parsed.flags["--json"] {
		err = writeJSON(stdout, a.out)
	} else {
		err = writeText(stdout, diskMarkText(&a.out, path.Join(d.Mount, d.Workdir, layout.MarkerName)))
	}
	if err != nil {
		return failure(stderr, err)
	}
	return code
}

// checkDiskMark confirms the disk, through a handle on it held to its mount, and reads its marker: what disk mark
// would write, or why it refuses.
func checkDiskMark(ctx context.Context, c *config.Config, m disks.Machine, d *config.Disk) (*diskMarking, error) {
	if d.State == config.DiskFailed {
		return nil, errors.New(T("a failed disk is never written again: atoll disk remove takes it out"))
	}
	seen, err := markerSeen(ctx, c, d)
	if err != nil {
		return nil, err
	}
	handle, err := ops.Open(m, d, 0)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	a := &diskMarking{disk: d, handle: handle, seen: seen}
	if err := a.confirm(c, m); err != nil {
		handle.Close()
		return nil, err
	}
	return a, nil
}

// confirm confirms the disk and reads its marker: online with its own marker, nothing to write; online with none,
// or missing no-marker, the marker to write; anything else refused (specs.md §10).
func (a *diskMarking) confirm(c *config.Config, m disks.Machine) error {
	d := a.disk
	v := disks.Confirm(m, d)
	switch v.State {
	case disks.Missing:
		return fmt.Errorf(T("it is missing, %s (%s): nothing is written to a disk that is not there"), v.Message, v.Reason)
	case disks.Readonly:
		return fmt.Errorf(T("it is read-only, %s (%s): Atoll writes nothing to it"), v.Message, v.Reason)
	case disks.Online:
	}
	text, found, err := a.handle.ReadMarker()
	if err != nil {
		return fmt.Errorf(T("its marker cannot be read: %w"), err)
	}
	// Missing, no-marker, is what it takes: its mount check passes but for the marker gone.
	marked, own := disks.JudgeMarker(v, disks.MarkerSeenOf(text, found, nil), a.seen, c.PoolID, d)
	if marked.Reason == disks.OtherMarker {
		return fmt.Errorf(T("%s (%s): disk mark writes no marker over another"), marked.Message, marked.Reason)
	}
	if err := a.handle.Verify(); err != nil {
		return fmt.Errorf("%w", err)
	}
	a.out = diskMarkOutput{Disk: d.ID, State: string(marked.State), Marker: diskShowMarker{PoolID: c.PoolID, DiskID: d.ID, FSUUID: d.UUID},
		HadMarker: own, Warnings: []string{}}
	if marked.Reason != "" {
		reason := string(marked.Reason)
		a.out.Reason = &reason
	}
	if !own {
		if a.text, err = layout.MarkerText(layout.Marker{PoolID: c.PoolID, DiskID: d.ID, FSUUID: d.UUID}); err != nil {
			return fmt.Errorf("%w", err)
		}
	}
	return nil
}

// finish writes the marker as disk add writes it, through the confirmed handle, records every disk's state in the
// catalog and tells the daemon, as disk add does, then records the marker seen. A marker already there is only
// recorded seen. What it cannot do once the marker is written is a warning, and exit 4.
func (a *diskMarking) finish(ctx context.Context, configPath string, c *config.Config, m disks.Machine, stderr io.Writer) (int, error) {
	warn := func(kind, message string) {
		if !slices.Contains(a.out.Warnings, kind) {
			a.out.Warnings = append(a.out.Warnings, kind)
		}
		report(stderr, "warning: "+message)
	}
	code := exitOK
	if !a.out.HadMarker {
		if err := a.handle.WriteMarker(a.text); err != nil {
			return 0, fmt.Errorf(T("its marker was not written: %w"), err)
		}
		a.out.Written = true
		code = recordAndTell(ctx, configPath, c, m, warn, stderr, "the next pass writes to it again")
	}
	if !a.seen {
		if err := recordMarkerSeen(ctx, c, a.disk, m.Now()); err != nil {
			warn(warnCatalogNotUpdated, fmt.Sprintf(T("the catalog was not updated, %v: the next pass that confirms the disk records its marker"), err))
			code = exitPartial
		}
	}
	return code, nil
}

// markerSeen says whether the catalog records the disk's own marker seen, on the filesystem the configuration gives it.
func markerSeen(ctx context.Context, c *config.Config, d *config.Disk) (seen bool, err error) {
	db, err := catalog.OpenPool(ctx, catalogPath(c), c.PoolID)
	if err != nil {
		return false, fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	rows, err := catalog.Disks(ctx, db)
	if err != nil {
		return false, fmt.Errorf("catalog: %w", err)
	}
	for i := range rows {
		if r := &rows[i]; r.ID == d.ID && r.UUID == d.UUID {
			return r.MarkerSeen, nil
		}
	}
	return false, nil
}

func diskMarkText(out *diskMarkOutput, place string) string {
	var b bytes.Buffer
	state := out.State
	if out.Reason != nil {
		state += " (" + *out.Reason + ")"
	}
	fmt.Fprintf(&b, T("disk %s: %s\n"), out.Disk, state)
	switch {
	case out.HadMarker:
		fmt.Fprintf(&b, T("it holds its own marker at %s: nothing to write\n"), place)
	case out.Written:
		fmt.Fprintf(&b, T("its marker is written at %s\n"), place)
	default:
		fmt.Fprintf(&b, T("its marker would be written at %s: nothing written\n"), place)
	}
	return b.String()
}
