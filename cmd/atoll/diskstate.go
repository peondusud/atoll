package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/layout"
)

// The changes atoll disk remove and atoll disk fail make (api/schemas/disk-change.schema.json).
const (
	changeDrain  = "drain"
	changeRemove = "remove"
	changeFail   = "fail"
)

// inService is how the output names a disk whose configuration sets no state.
const inService = "in-service"

// firstDepending is how many paths of the files that depend on a disk the output names.
const firstDepending = 10

// diskChangeOutput is the output of api/schemas/disk-change.schema.json.
type diskChangeOutput struct {
	Disk              string   `json:"disk"`
	Change            string   `json:"change"`
	StateWas          string   `json:"state_was"`
	Depending         int64    `json:"depending"`
	DependingFirst    []string `json:"depending_first"`
	OnlyThere         int64    `json:"only_there"`
	PlannedOperations int64    `json:"planned_operations"`
	Done              bool     `json:"done"`
	Warnings          []string `json:"warnings"`
}

// diskIDArgs is the arguments of a disk command that names one disk: its id, and flags each given once.
type diskIDArgs struct {
	id    string
	flags map[string]bool
}

// parseDiskID reads one disk id and the flags allowed, each once; --dry-run never goes with --yes (specs.md §25).
func parseDiskID(args []string, allowed ...string) (diskIDArgs, error) {
	out := diskIDArgs{flags: map[string]bool{}}
	flags := true
	for _, a := range args {
		switch {
		case flags && a == "--":
			flags = false
		case flags && strings.HasPrefix(a, "--"):
			if !slices.Contains(allowed, a) {
				return out, fmt.Errorf(T("unknown argument %q"), a)
			}
			if out.flags[a] {
				return out, fmt.Errorf(T("%s comes twice"), a)
			}
			out.flags[a] = true
		case out.id != "":
			return out, fmt.Errorf(T("one disk, not %q too"), a)
		case !layout.IsIdentifier(a):
			return out, fmt.Errorf(T("%q is no disk id"), a)
		default:
			out.id = a
		}
	}
	if out.id == "" {
		return out, errors.New(T("which disk?"))
	}
	if out.flags["--dry-run"] && out.flags["--yes"] {
		return out, errors.New(T("--dry-run and --yes do not go together"))
	}
	return out, nil
}

// diskOfConfig gives the disk of the configuration with that id.
func diskOfConfig(c *config.Config, id string) (*config.Disk, error) {
	for i := range c.Disks {
		if c.Disks[i].ID == id {
			return &c.Disks[i], nil
		}
	}
	return nil, fmt.Errorf(T("the configuration has no disk %q"), id)
}

func stateName(d *config.Disk) string {
	if d.State == "" {
		return inService
	}
	return d.State
}

// diskRemove runs atoll disk remove: with --evacuate it marks the disk draining, so that the daemon's passes make
// elsewhere the copies it holds; without, it takes out of the configuration a disk nothing depends on, or a
// failed one (specs.md §18).
func diskRemove(configPath string, args []string, stdout, stderr io.Writer) int {
	parsed, err := parseDiskID(args, "--evacuate", "--dry-run", "--yes", "--json")
	if err != nil {
		return usage(stderr, "disk remove: "+err.Error())
	}
	if parsed.flags["--evacuate"] {
		return diskStateChange(configPath, parsed, changeDrain, stdout, stderr)
	}
	return diskStateChange(configPath, parsed, changeRemove, stdout, stderr)
}

// diskFail runs atoll disk fail: the disk is never read or written again, and its copies count for nothing, so
// that the daemon makes their replicas elsewhere at once (specs.md §18).
func diskFail(configPath string, args []string, stdout, stderr io.Writer) int {
	parsed, err := parseDiskID(args, "--dry-run", "--yes", "--json")
	if err != nil {
		return usage(stderr, "disk fail: "+err.Error())
	}
	return diskStateChange(configPath, parsed, changeFail, stdout, stderr)
}

// diskStateChange works out, then with --yes, or for a drain, makes a change of a disk's place in the pool.
func diskStateChange(configPath string, parsed diskIDArgs, change string, stdout, stderr io.Writer) int {
	text, err := fsx.ReadFile(configPath)
	if err != nil {
		return failure(stderr, fmt.Errorf("configuration: %w", err))
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	d, err := diskOfConfig(c, parsed.id)
	if err != nil {
		return failure(stderr, fmt.Errorf("disk %s: %w", change, err))
	}
	m, err := disks.NewMachine()
	if err != nil {
		return failure(stderr, err)
	}
	ctx := context.Background()
	dryRun := parsed.flags["--dry-run"]
	if !dryRun {
		release, err := lockCatalog(ctx, c, stderr)
		if err != nil {
			return failure(stderr, err)
		}
		defer release()
	}
	out := diskChangeOutput{Disk: d.ID, Change: change, StateWas: stateName(d), DependingFirst: []string{}, Warnings: []string{}}
	warn := func(kind, message string) {
		out.Warnings = append(out.Warnings, kind)
		report(stderr, "warning: "+message)
	}
	if err := readHolds(ctx, c, d.ID, &out); err != nil {
		return failure(stderr, err)
	}
	newText, refusal := changedText(text, d, change, &out)
	if refusal != nil {
		return failure(stderr, fmt.Errorf("disk %s: %w", verbOf(change), refusal))
	}
	code := exitOK
	switch {
	case newText == nil:
		report(stderr, fmt.Sprintf(T("disk %s is %s already: nothing to do"), d.ID, stateName(d)))
	case dryRun:
	case change != changeDrain && !parsed.flags["--yes"]:
		code = exitConfirm
	default:
		edited, err := config.Parse(newText)
		if err != nil {
			return failure(stderr, fmt.Errorf(T("disk %s: the configuration would not load: %w"), verbOf(change), err))
		}
		if err := fsx.ReplaceFile(configPath, text, newText); err != nil {
			return failure(stderr, fmt.Errorf(T("disk %s: the configuration is unchanged: %w"), verbOf(change), err))
		}
		out.Done = true
		code = recordAndTell(ctx, edited, m, warn, stderr, "the next scan or pass takes it in")
	}
	if parsed.flags["--json"] {
		err = writeJSON(stdout, out)
	} else {
		err = writeText(stdout, diskChangeText(&out))
	}
	if err != nil {
		return failure(stderr, err)
	}
	return code
}

func verbOf(change string) string {
	if change == changeFail {
		return "fail"
	}
	return "remove"
}

// readHolds reads what the catalog knows the disk holds.
func readHolds(ctx context.Context, c *config.Config, id string, out *diskChangeOutput) (err error) {
	db, err := catalog.OpenPool(ctx, catalogPath(c), c.PoolID)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	holds, err := catalog.ReadDiskHolds(ctx, db, id, firstDepending)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	out.Depending, out.OnlyThere, out.PlannedOperations = holds.Depending, holds.OnlyThere, holds.Planned
	if holds.First != nil {
		out.DependingFirst = holds.First
	}
	return nil
}

// changedText gives the configuration after the change, nil when there is nothing to change, or why the change is
// refused.
func changedText(text []byte, d *config.Disk, change string, out *diskChangeOutput) ([]byte, error) {
	switch change {
	case changeDrain:
		switch d.State {
		case config.DiskFailed:
			return nil, errors.New(T("a failed disk is never read again, so nothing can be evacuated from it: atoll disk remove takes it out"))
		case config.DiskDraining:
			return nil, nil
		}
		return config.SetDiskState(text, d.ID, config.DiskDraining) //nolint:wrapcheck // config says what it refused
	case changeFail:
		if d.State == config.DiskFailed {
			return nil, nil
		}
		return config.SetDiskState(text, d.ID, config.DiskFailed) //nolint:wrapcheck // likewise
	default:
		switch {
		case out.PlannedOperations > 0:
			return nil, fmt.Errorf(T("%d operations wait on it: mount it, and let the daemon or atoll daemon --until-idle end them"), out.PlannedOperations)
		case d.State == config.DiskDraining && out.Depending > 0:
			return nil, fmt.Errorf(T("it is draining, and %d files still depend on it, among them %s: remove it once none does (atoll disk show %s)"),
				out.Depending, strings.Join(out.DependingFirst, ", "), d.ID)
		case d.State != config.DiskFailed && out.Depending > 0:
			return nil, fmt.Errorf(T("%d files depend on it, among them %s: atoll disk remove %s --evacuate first, then remove it once none does"),
				out.Depending, strings.Join(out.DependingFirst, ", "), d.ID)
		}
		return config.RemoveDisk(text, d.ID) //nolint:wrapcheck // likewise
	}
}

func diskChangeText(out *diskChangeOutput) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, T("disk %s, %s: %d files depend on it, %d have no copy elsewhere, %d operations wait on it\n"),
		out.Disk, out.StateWas, out.Depending, out.OnlyThere, out.PlannedOperations)
	for _, p := range out.DependingFirst {
		fmt.Fprintf(&b, "  %s\n", p)
	}
	switch {
	case out.Done && out.Change == changeDrain:
		b.WriteString(T("marked draining: the daemon makes elsewhere what it holds\n"))
	case out.Done && out.Change == changeFail:
		b.WriteString(T("marked failed: the daemon makes the replicas of its files elsewhere\n"))
	case out.Done:
		b.WriteString(T("taken out of the configuration: what it holds stays on it\n"))
	default:
		b.WriteString(T("nothing written\n"))
	}
	return b.String()
}

// diskShowOutput is the output of api/schemas/disk-show.schema.json.
type diskShowOutput struct {
	ID              string           `json:"id"`
	UUID            string           `json:"uuid"`
	FS              string           `json:"fs"`
	Mount           string           `json:"mount"`
	Root            string           `json:"root"`
	Workdir         string           `json:"workdir"`
	Role            string           `json:"role"`
	Domain          *string          `json:"domain"`
	Tags            []string         `json:"tags"`
	Configured      string           `json:"configured"`
	State           string           `json:"state"`
	Reason          *string          `json:"reason"`
	CapacityBytes   *int64           `json:"capacity_bytes"`
	FreeBytes       *int64           `json:"free_bytes"`
	TimePrecisionNS *int64           `json:"time_precision_ns"`
	Copies          map[string]int64 `json:"copies"`
	Depending       int64            `json:"depending"`
	DependingFirst  []string         `json:"depending_first"`
	OnlyThere       int64            `json:"only_there"`
	Planned         int64            `json:"planned_operations"`
	Marker          *diskShowMarker  `json:"marker"`
}

type diskShowMarker struct {
	PoolID string `json:"pool_id"`
	DiskID string `json:"disk_id"`
	FSUUID string `json:"fs_uuid"`
}

// diskShow runs atoll disk show: a disk's configuration, its mount check, what the catalog knows of it, and its
// marker. It writes nothing, and takes no lock.
func diskShow(configPath string, args []string, stdout, stderr io.Writer) int {
	parsed, err := parseDiskID(args, "--json")
	if err != nil {
		return usage(stderr, "disk show: "+err.Error())
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	d, err := diskOfConfig(c, parsed.id)
	if err != nil {
		return failure(stderr, fmt.Errorf("disk show: %w", err))
	}
	m, err := disks.NewMachine()
	if err != nil {
		return failure(stderr, err)
	}
	out, err := showDisk(context.Background(), c, m, d)
	if err != nil {
		return failure(stderr, err)
	}
	if parsed.flags["--json"] {
		err = writeJSON(stdout, out)
	} else {
		err = writeText(stdout, diskShowText(out))
	}
	if err != nil {
		return failure(stderr, err)
	}
	return exitOK
}

func showDisk(ctx context.Context, c *config.Config, m disks.Machine, d *config.Disk) (out *diskShowOutput, err error) {
	out = &diskShowOutput{ID: d.ID, UUID: d.UUID, FS: string(d.FS), Mount: d.Mount, Root: d.Root, Workdir: d.Workdir, Role: d.Role,
		Tags: append([]string{}, d.Tags...), Configured: stateName(d), DependingFirst: []string{}}
	if d.Domain != "" {
		out.Domain = &d.Domain
	}
	verdict := disks.Check(m, d)
	out.State = string(verdict.State)
	switch {
	case d.State == config.DiskFailed:
		out.State = config.DiskFailed
	case d.State == config.DiskDraining && verdict.State == disks.Online:
		out.State = config.DiskDraining
	}
	if verdict.Reason != "" {
		reason := string(verdict.Reason)
		out.Reason = &reason
	}
	db, err := catalog.OpenPool(ctx, catalogPath(c), c.PoolID)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	rows, err := catalog.Disks(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	for i := range rows {
		if r := &rows[i]; r.ID == d.ID {
			if r.HasCapacity {
				out.CapacityBytes, out.FreeBytes = &r.Capacity, &r.Free
			}
			if r.TimePrecisionNS > 0 {
				out.TimePrecisionNS = &r.TimePrecisionNS
			}
		}
	}
	holds, err := catalog.ReadDiskHolds(ctx, db, d.ID, firstDepending)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	out.Copies, out.Depending, out.OnlyThere, out.Planned = holds.Copies, holds.Depending, holds.OnlyThere, holds.Planned
	if holds.First != nil {
		out.DependingFirst = holds.First
	}
	// The marker is read only from a disk its check finds there: a missing disk's folder may be its mount point.
	if verdict.State != disks.Missing && d.State != config.DiskFailed {
		data, err := fsx.ReadFile(filepath.Join(d.Mount, d.Workdir, layout.MarkerName))
		if err == nil {
			if mark, err := layout.ParseMarker(data); err == nil {
				out.Marker = &diskShowMarker{PoolID: mark.PoolID, DiskID: mark.DiskID, FSUUID: mark.FSUUID}
			}
		}
	}
	return out, nil
}

func diskShowText(out *diskShowOutput) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, T("disk %s: %s %s, mounted at %s, %s"), out.ID, out.FS, out.UUID, out.Mount, out.State)
	if out.Reason != nil {
		fmt.Fprintf(&b, " (%s)", *out.Reason)
	}
	fmt.Fprintf(&b, T("\n  configured: %s, role %s"), out.Configured, out.Role)
	if out.Domain != nil {
		fmt.Fprintf(&b, T(", domain %s"), *out.Domain)
	}
	if len(out.Tags) > 0 {
		fmt.Fprintf(&b, T(", tags %s"), strings.Join(out.Tags, ", "))
	}
	if out.CapacityBytes != nil {
		fmt.Fprintf(&b, T("\n  %d bytes, %d free"), *out.CapacityBytes, *out.FreeBytes)
	}
	states := make([]string, 0, len(out.Copies))
	for state, n := range out.Copies {
		states = append(states, fmt.Sprintf("%s %d", state, n))
	}
	slices.Sort(states)
	fmt.Fprintf(&b, T("\n  copies: %s\n  %d files depend on it, %d have no copy elsewhere, %d operations wait on it\n"),
		strings.Join(states, ", "), out.Depending, out.OnlyThere, out.Planned)
	for _, p := range out.DependingFirst {
		fmt.Fprintf(&b, "    %s\n", p)
	}
	return b.String()
}

// diskReplaceOutput is the output of api/schemas/disk-replace.schema.json: that of disk add, and the disk replaced.
type diskReplaceOutput struct {
	diskAddOutput
	Replaces diskReplaced `json:"replaces"`
}

type diskReplaced struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// diskReplace runs atoll disk replace <id> --with <mount>: the new disk comes in as disk add brings one, with the
// domain and tags of the old one, which is marked draining, unless it is failed, so that the copies it holds are
// made again; disk remove takes it out once nothing depends on it (specs.md §18).
func diskReplace(configPath string, args []string, stdout, stderr io.Writer) int {
	old, rest, err := splitReplace(args)
	if err != nil {
		return usage(stderr, "disk replace: "+err.Error())
	}
	parsed, err := parseDiskAdd(rest)
	if err != nil {
		return usage(stderr, "disk replace: "+err.Error())
	}
	if parsed.domain != "" || parsed.tags != nil {
		return usage(stderr, T("disk replace: the new disk takes the domain and tags of the old one"))
	}
	text, err := fsx.ReadFile(configPath)
	if err != nil {
		return failure(stderr, fmt.Errorf("configuration: %w", err))
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	d, err := diskOfConfig(c, old)
	if err != nil {
		return failure(stderr, fmt.Errorf("disk replace: %w", err))
	}
	parsed.domain, parsed.tags = d.Domain, d.Tags
	m, err := disks.NewMachine()
	if err != nil {
		return failure(stderr, err)
	}
	ctx := context.Background()
	if !parsed.dryRun {
		release, err := lockCatalog(ctx, c, stderr)
		if err != nil {
			return failure(stderr, err)
		}
		defer release()
	}
	a, err := checkDiskAdd(ctx, c, m, text, &parsed, stderr)
	if err != nil {
		return failure(stderr, fmt.Errorf("disk replace: %w", err))
	}
	out := diskReplaceOutput{Replaces: diskReplaced{ID: d.ID, State: config.DiskFailed}}
	if d.State != config.DiskFailed {
		out.Replaces.State = config.DiskDraining
		if a.text, err = config.SetDiskState(a.text, d.ID, config.DiskDraining); err != nil {
			return failure(stderr, fmt.Errorf("disk replace: %w", err))
		}
		if a.config, err = config.Parse(a.text); err != nil {
			return failure(stderr, fmt.Errorf(T("disk replace: the configuration would not load: %w"), err))
		}
	}
	code := exitOK
	if !parsed.dryRun {
		if err := fsx.ReplaceFile(configPath, text, a.text); err != nil {
			return failure(stderr, fmt.Errorf(T("disk replace: the configuration is unchanged: %w"), err))
		}
		code = a.finish(ctx)
	}
	out.diskAddOutput = a.out
	if parsed.asJSON {
		err = writeJSON(stdout, out)
	} else {
		err = writeText(stdout, diskAddText(&a.out, configPath)+fmt.Sprintf(T("disk %s: %s\n"), d.ID, out.Replaces.State))
	}
	if err != nil {
		return failure(stderr, err)
	}
	return code
}

// splitReplace takes the old disk's id and --with out of the arguments of disk replace, and gives the rest as disk
// add reads them, the mount point first.
func splitReplace(args []string) (old string, rest []string, err error) {
	with := ""
	flags := true
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case flags && a == "--":
			flags = false
		case flags && (a == "--with" || strings.HasPrefix(a, "--with=")):
			if with != "" {
				return "", nil, errors.New(T("--with comes twice"))
			}
			value, inline := strings.CutPrefix(a, "--with=")
			if !inline {
				if i+1 == len(args) {
					return "", nil, errors.New(T("--with needs a mount point"))
				}
				i++
				value = args[i]
			}
			if value == "" {
				return "", nil, errors.New(T("--with needs a mount point"))
			}
			with = value
		case flags && strings.HasPrefix(a, "--"):
			rest = append(rest, a)
			if (a == "--id" || a == "--domain" || a == "--tags") && i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
		case old != "":
			return "", nil, fmt.Errorf(T("one disk to replace, not %q too"), a)
		case !layout.IsIdentifier(a):
			return "", nil, fmt.Errorf(T("%q is no disk id"), a)
		default:
			old = a
		}
	}
	if old == "" || with == "" {
		return "", nil, errors.New(T("want <id> --with <mount point>"))
	}
	return old, append(rest, "--", with), nil
}
