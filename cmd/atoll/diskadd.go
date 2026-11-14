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
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/watch"
)

// The warnings of atoll disk add, a closed list (api/schemas/disk-add.schema.json).
const (
	warnReadonly           = "readonly"
	warnTopologyUnchecked  = "topology-unchecked"
	warnNotImmutable       = "not-immutable"
	warnImmutableUnchecked = "immutable-unchecked"
	warnCatalogNotUpdated  = "catalog-not-updated"
	warnMarkerNotWritten   = "marker-not-written"
	warnDaemonNotTold      = "daemon-not-told"
	warnConfigNotSynced    = "config-not-synced"
)

// nameRuleReasons are the read-only reasons disk add refuses, as they change the rules of names (specs.md §10,
// §15.8): what Atoll writes on such a disk could reach another file.
var nameRuleReasons = []disks.Reason{"ads", "nocase", "casefold", "ascii-ci", "ignore-case"}

type diskAddArgs struct {
	mount, id, domain string
	tags              []string
	dryRun, asJSON    bool
}

// diskAddDisk is the disk of api/schemas/disk-add.schema.json.
type diskAddDisk struct {
	ID     string   `json:"id"`
	UUID   string   `json:"uuid"`
	FS     string   `json:"fs"`
	Mount  string   `json:"mount"`
	Domain *string  `json:"domain"`
	Tags   []string `json:"tags"`
	State  string   `json:"state"`
	Reason *string  `json:"reason"`
}

// diskAddOutput is the output of api/schemas/disk-add.schema.json.
type diskAddOutput struct {
	Disk          diskAddDisk `json:"disk"`
	PhysicalDisks []string    `json:"physical_disks"`
	DryRun        bool        `json:"dry_run"`
	Warnings      []string    `json:"warnings"`
}

// diskAdding is a disk add that passed its checks.
type diskAdding struct {
	out     diskAddOutput
	disk    *config.Disk
	text    []byte
	config  *config.Config
	marker  string // the marker to write, "" for none
	machine disks.Machine
	stderr  io.Writer
}

func (a *diskAdding) warn(kind, message string) {
	a.out.Warnings = append(a.out.Warnings, kind)
	report(a.stderr, "warning: "+message)
}

// diskAdd runs atoll disk add (specs.md §10, §18, §25): it checks the disk, appends it to the configuration and
// the catalog, writes its marker, and tells the daemon. It moves nothing and indexes nothing itself.
func diskAdd(configPath string, args []string, stdout, stderr io.Writer) int {
	parsed, err := parseDiskAdd(args)
	if err != nil {
		return usage(stderr, "disk add: "+err.Error())
	}
	text, c, err := readConfig(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	m, err := disks.NewMachine()
	if err != nil {
		return failure(stderr, err)
	}
	ctx := context.Background()
	if !parsed.dryRun {
		var release func()
		if text, c, release, err = lockedConfig(ctx, configPath, c, stderr); err != nil {
			return failure(stderr, err)
		}
		defer release()
	}
	a, err := checkDiskAdd(ctx, c, m, text, &parsed, stderr)
	if err != nil {
		return failure(stderr, fmt.Errorf("disk add: %w", err))
	}
	code := exitOK
	if !parsed.dryRun {
		partial, err := configInPlace(fsx.ReplaceFile(configPath, text, a.text), a.warn)
		if err != nil {
			return failure(stderr, fmt.Errorf(T("disk add: the configuration is unchanged: %w"), err))
		}
		if code = a.finish(ctx); partial {
			code = exitPartial
		}
	}
	if parsed.asJSON {
		err = writeJSON(stdout, a.out)
	} else {
		err = writeText(stdout, diskAddText(&a.out, configPath))
	}
	if err != nil {
		return failure(stderr, err)
	}
	return code
}

// parseDiskAdd reads the arguments of disk add: one mount point, and --id, --domain, --tags, --dry-run and
// --json, each once (specs.md §25).
func parseDiskAdd(args []string) (diskAddArgs, error) {
	var out diskAddArgs
	given := map[string]bool{}
	flags := true
	for i := 0; i < len(args); i++ {
		a := args[i]
		if flags && a == "--" {
			flags = false
			continue
		}
		if !flags || !strings.HasPrefix(a, "--") {
			if out.mount != "" {
				return out, fmt.Errorf(T("one mount point, not %q too"), a)
			}
			if !filepath.IsAbs(a) {
				return out, fmt.Errorf(T("the mount point %q is not an absolute path"), a)
			}
			out.mount = filepath.Clean(a)
			continue
		}
		switch a {
		case "--dry-run":
			if err := setFlag(&out.dryRun, a); err != nil {
				return out, err
			}
			continue
		case "--json":
			if err := setFlag(&out.asJSON, a); err != nil {
				return out, err
			}
			continue
		}
		name, value, inline := strings.Cut(a, "=")
		if name != "--id" && name != "--domain" && name != "--tags" {
			return out, fmt.Errorf(T("unknown argument %q"), a)
		}
		if !inline {
			if i+1 == len(args) {
				return out, fmt.Errorf(T("%s needs a value"), name)
			}
			i++
			value = args[i]
		}
		if given[name] {
			return out, fmt.Errorf(T("%s comes twice"), name)
		}
		given[name] = true
		switch name {
		case "--id":
			if !layout.IsIdentifier(value) {
				return out, fmt.Errorf(T("--id: %q is no identifier"), value)
			}
			out.id = value
		case "--domain":
			if !layout.IsIdentifier(value) {
				return out, fmt.Errorf(T("--domain: %q is no identifier"), value)
			}
			out.domain = value
		default:
			for _, tag := range strings.Split(value, ",") {
				if !layout.IsIdentifier(tag) || slices.Contains(out.tags, tag) {
					return out, fmt.Errorf(T("--tags: %q is no identifier, or comes twice"), tag)
				}
				out.tags = append(out.tags, tag)
			}
		}
	}
	if out.mount == "" {
		return out, errors.New(T("which mount point?"))
	}
	return out, nil
}

// checkDiskAdd looks at the disk and at the pool, and gives what to write, or why not.
func checkDiskAdd(ctx context.Context, c *config.Config, m disks.Machine, text []byte, p *diskAddArgs, stderr io.Writer) (*diskAdding, error) {
	mount := p.mount
	id := p.id
	if id == "" {
		if id = filepath.Base(mount); !layout.IsIdentifier(id) {
			return nil, fmt.Errorf(T("the name of the mount point, %q, is no identifier: give one with --id"), id)
		}
	}
	for i := range c.Disks {
		switch {
		case c.Disks[i].ID == id:
			return nil, fmt.Errorf(T("the pool has a disk %q already: give another id with --id"), id)
		case c.Disks[i].Mount == mount:
			return nil, fmt.Errorf(T("%s is the mount point of disk %q already"), mount, c.Disks[i].ID)
		}
	}
	seen := m.Mount(mount)
	if seen.Path != disks.PathOK || seen.Entry == nil || seen.Entry.MountPoint != mount {
		return nil, fmt.Errorf(T("nothing is mounted at %s"), mount)
	}
	fs, err := filesystemOf(seen.Entry)
	if err != nil {
		return nil, err
	}
	dev := m.Device(fs, seen.Entry)
	if dev.Device != disks.DeviceOK || dev.Identity == nil {
		return nil, fmt.Errorf(T("the device mounted at %s cannot be read as %s: %s"), mount, fs, dev.Note)
	}
	uuid := dev.Identity.ID
	for i := range c.Disks {
		if c.Disks[i].UUID == uuid {
			return nil, fmt.Errorf(T("disk %q has the same identifier, %s: two disks with one are refused until one is regenerated (tune2fs -U, xfs_admin -U, btrfstune -u, ntfslabel --new-serial)"), c.Disks[i].ID, uuid)
		}
	}
	effective := c.Effective()
	root, _ := effective["layout.root"].(string)
	workdir, _ := effective["layout.workdir"].(string)
	d := &config.Disk{ID: id, UUID: uuid, Mount: mount, FS: fs, Role: "data", Domain: p.domain, Tags: p.tags, Root: root, Workdir: workdir}
	a := &diskAdding{disk: d, machine: m, stderr: stderr}
	a.out = diskAddOutput{Disk: diskAddDisk{ID: id, UUID: uuid, FS: string(fs), Mount: mount, Tags: append([]string{}, p.tags...)}, DryRun: p.dryRun, Warnings: []string{}}
	if p.domain != "" {
		a.out.Disk.Domain = &p.domain
	}
	verdict := disks.Check(m, d)
	a.out.Disk.State = string(verdict.State)
	if verdict.Reason != "" {
		reason := string(verdict.Reason)
		a.out.Disk.Reason = &reason
	}
	switch {
	case verdict.State == disks.Missing:
		return nil, fmt.Errorf("%s: %s (%s)", mount, verdict.Message, verdict.Reason)
	case verdict.State == disks.Readonly && slices.Contains(nameRuleReasons, verdict.Reason):
		return nil, fmt.Errorf(T("%s: its names do not follow the rules Atoll relies on: %s (%s)"), mount, verdict.Message, verdict.Reason)
	case verdict.State == disks.Readonly && verdict.Reason == disks.NoJournal:
		// What every operation of Atoll's rests on, a name durable once its folder is synced, does not hold there.
		return nil, fmt.Errorf(T("%s: Atoll cannot make what it writes there durable: %s (%s)"), mount, verdict.Message, verdict.Reason)
	case verdict.State == disks.Readonly:
		a.warn(warnReadonly, fmt.Sprintf(T("%s is read-only, %s (%s): Atoll makes no copy on it until it is mounted read-write"), mount, verdict.Message, verdict.Reason))
	}
	if err := a.checkTopology(c, seen.Entry); err != nil {
		return nil, err
	}
	if err := a.checkWorkdir(c); err != nil {
		return nil, err
	}
	if err := a.checkMarker(c); err != nil {
		return nil, err
	}
	if err := checkCatalogTakes(ctx, c, d); err != nil {
		return nil, err
	}
	newText, err := config.AppendDisk(text, &config.NewDisk{ID: id, UUID: uuid, Mount: mount, FS: string(fs), Domain: p.domain, Tags: p.tags})
	if err != nil {
		return nil, err
	}
	if a.config, err = config.Parse(newText); err != nil {
		return nil, fmt.Errorf(T("the configuration would not load: %w"), err)
	}
	a.text = newText
	return a, nil
}

// filesystemOf gives the fs of a mount entry's type. A fuseblk is taken as ntfs-3g, which only an NTFS head
// confirms.
func filesystemOf(entry *disks.MountEntry) (names.Filesystem, error) {
	switch entry.FSType {
	case "ext4":
		return names.Ext4, nil
	case "xfs":
		return names.XFS, nil
	case "btrfs":
		return names.Btrfs, nil
	case "ntfs3", "ntfs", "fuseblk":
		return names.NTFS, nil
	default:
		return "", fmt.Errorf(T("a filesystem of type %s: Atoll takes ext4, XFS, btrfs and NTFS"), entry.FSType)
	}
}

// checkTopology refuses a disk on a physical disk another of the pool lives on (R-06), and warns when the folder
// under its mount point is not immutable (specs.md §10).
func (a *diskAdding) checkTopology(c *config.Config, entry *disks.MountEntry) error {
	t, ok := a.machine.(disks.Topology)
	if !ok {
		a.warn(warnTopologyUnchecked, "this machine cannot tell which physical disk a filesystem lives on")
		a.warn(warnImmutableUnchecked, "this machine cannot tell whether a mount point is immutable")
		return nil
	}
	mine, err := t.PhysicalDisks(a.disk.FS, entry, a.disk.UUID)
	if err != nil {
		a.warn(warnTopologyUnchecked, fmt.Sprintf(T("which physical disk %s lives on: %v"), a.disk.Mount, err))
	} else {
		a.out.PhysicalDisks = mine
		unchecked := false
		for i := range c.Disks {
			other := &c.Disks[i]
			seen := a.machine.Mount(other.Mount)
			if seen.Path != disks.PathOK || seen.Entry == nil || seen.Entry.MountPoint != other.Mount {
				unchecked = true
				continue
			}
			theirs, err := t.PhysicalDisks(other.FS, seen.Entry, other.UUID)
			if err != nil {
				unchecked = true
				continue
			}
			for _, name := range mine {
				if slices.Contains(theirs, name) {
					return fmt.Errorf(T("%s lives on %s, as disk %q does: two filesystems of a pool on one physical disk are refused (R-06)"), a.disk.Mount, name, other.ID)
				}
			}
		}
		if unchecked {
			a.warn(warnTopologyUnchecked, "a disk of the pool is not mounted: whether it shares a physical disk with this one is not known")
		}
	}
	immutable, err := t.MountPointImmutable(a.disk.Mount)
	switch {
	case err != nil:
		a.warn(warnImmutableUnchecked, fmt.Sprintf(T("whether the folder under %s is immutable: %v"), a.disk.Mount, err))
	case !immutable:
		a.warn(warnNotImmutable, fmt.Sprintf(T("the folder under %s is not immutable: unmount the disk and run chattr +i %s, so that nothing writes there while it is away"), a.disk.Mount, a.disk.Mount))
	}
	return nil
}

// checkWorkdir refuses a disk whose work folder is there already and belongs to an account that is none of Atoll's:
// neither root's, the one that runs this command, nor the one that owns the catalog (specs.md §10). Whoever made it
// first makes its tmp/, links/ and trash/ too, reads what Atoll copies through them whatever the folders of the root
// forbid, and removes what the trash holds. It says how to give the folder back, when it is Atoll's all the same, as
// on a disk that comes from a machine where Atoll's account has another number.
func (a *diskAdding) checkWorkdir(c *config.Config) error {
	trusted := ops.TrustedOwners(catalogPath(c))
	uid, foreign, err := ops.ForeignWorkdir(a.disk, trusted)
	switch {
	case err != nil:
		return fmt.Errorf(T("its work folder: %w"), err)
	case foreign:
		path := filepath.Join(a.disk.Mount, a.disk.Workdir)
		return fmt.Errorf(T("its work folder %s belongs to the account %d, which is neither root's, this command's nor the catalog's: that account could read what Atoll copies there and remove what its trash holds. If the folder is Atoll's, give it back: chown -R %d %s"),
			path, uid, ops.WorkdirOwner(trusted), path)
	}
	return nil
}

// checkMarker refuses a disk whose marker says it is of another pool, another disk or another filesystem, and
// says what to write when the configuration asks for a marker.
func (a *diskAdding) checkMarker(c *config.Config) error {
	data, found, err := ops.ReadMarkerAt(a.disk.Mount, a.disk.Workdir)
	switch {
	case err != nil:
		return fmt.Errorf("its marker: %w", err)
	case found:
		mark, err := layout.ParseMarker(data)
		if err != nil {
			return fmt.Errorf(T("its marker %s: %w"), layout.MarkerName, err)
		}
		switch {
		case mark.PoolID != c.PoolID:
			return fmt.Errorf(T("its marker says it is a disk of the pool %q"), mark.PoolID)
		case mark.DiskID != a.disk.ID:
			return fmt.Errorf(T("its marker says it was disk %q of this pool: add it back with --id %s"), mark.DiskID, mark.DiskID)
		case mark.FSUUID != a.disk.UUID:
			return fmt.Errorf(T("its marker names the filesystem %s, and this one is %s: a marker copied from another disk"), mark.FSUUID, a.disk.UUID)
		}
		// It says the same, and is left as it is, however it is written (specs.md §10).
		return nil
	}
	if on, _ := c.Effective()["layout.marker"].(bool); on && a.out.Disk.State == string(disks.Online) {
		text, err := layout.MarkerText(layout.Marker{PoolID: c.PoolID, DiskID: a.disk.ID, FSUUID: a.disk.UUID})
		if err != nil {
			return err
		}
		a.marker = text
	}
	return nil
}

// checkCatalogTakes asks the catalog whether it knows another disk by this id or this identifier, which holds
// something, so that what it knows is never read against this one (specs.md §9).
func checkCatalogTakes(ctx context.Context, c *config.Config, d *config.Disk) (err error) {
	db, err := catalog.OpenPool(ctx, catalogPath(c), c.PoolID)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	list := make([]catalog.Disk, 0, len(c.Disks)+1)
	for i := range c.Disks {
		x := &c.Disks[i]
		list = append(list, catalog.Disk{ID: x.ID, UUID: x.UUID, FS: string(x.FS), Mount: x.Mount, Root: x.Root, Workdir: x.Workdir, Role: x.Role,
			Position: i, Domain: x.Domain, Tags: x.Tags, State: string(disks.Missing)})
	}
	list = append(list, catalog.Disk{ID: d.ID, UUID: d.UUID, FS: string(d.FS), Mount: d.Mount, Root: d.Root, Workdir: d.Workdir, Role: d.Role,
		Position: len(c.Disks), Domain: d.Domain, Tags: d.Tags, State: string(disks.Missing)})
	if err := catalog.TrySyncDisks(ctx, db, list); err != nil {
		return fmt.Errorf("the catalog: %w", err)
	}
	return nil
}

// finish records the disk in the catalog, writes its marker and tells the daemon, once the configuration holds
// it. What it cannot do is a warning, and exit 4: the next scan or pass records the disk, and a later disk add
// writes its marker.
func (a *diskAdding) finish(ctx context.Context) int {
	code := recordAndTell(ctx, a.config, a.machine, a.warn, a.stderr, "atoll scan --disk "+a.disk.ID+" records what the disk holds")
	if a.marker != "" {
		if err := a.writeMarker(); err != nil {
			a.warn(warnMarkerNotWritten, fmt.Sprintf(T("its marker was not written: %v"), err))
			code = exitPartial
		}
	}
	return code
}

// recordAndTell records every disk's state in the catalog and tells the daemon, once the configuration holds a
// disk command's edit. What it cannot do is a warning, and exit 4: the next scan or pass records the disks, and
// systemctl reload atoll tells the daemon. alone says what to do when no daemon runs.
func recordAndTell(ctx context.Context, c *config.Config, m disks.Machine, warn func(kind, message string), stderr io.Writer, alone string) int {
	code := exitOK
	if err := syncCatalog(ctx, c, m); err != nil {
		warn(warnCatalogNotUpdated, fmt.Sprintf(T("the catalog was not updated, %v: the next scan or pass records the disks"), err))
		code = exitPartial
	}
	told, err := tellDaemon()
	switch {
	case err != nil:
		warn(warnDaemonNotTold, fmt.Sprintf(T("the daemon was not told, %v: systemctl reload atoll"), err))
		code = exitPartial
	case told == 0:
		report(stderr, "no daemon runs: "+alone)
	}
	return code
}

func syncCatalog(ctx context.Context, c *config.Config, m disks.Machine) (err error) {
	db, err := catalog.OpenPool(ctx, catalogPath(c), c.PoolID)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	_, err = watch.CheckDisks(ctx, db, c, m, m.Now())
	return err //nolint:wrapcheck // watch says what failed
}

// writeMarker writes Atoll/disk.toml through a handle on the disk, confirmed first (invariant 3).
func (a *diskAdding) writeMarker() error {
	handle, err := ops.Open(a.machine, a.disk, 0)
	if err != nil {
		return err //nolint:wrapcheck // ops names the disk
	}
	defer handle.Close()
	if v := disks.Confirm(a.machine, a.disk); v.State == disks.Missing {
		return fmt.Errorf("%s (%s)", v.Message, v.Reason)
	}
	if err := handle.Verify(); err != nil {
		return err //nolint:wrapcheck // likewise
	}
	return handle.WriteMarker(a.marker) //nolint:wrapcheck // likewise
}

func diskAddText(out *diskAddOutput, configPath string) string {
	var b bytes.Buffer
	d := &out.Disk
	fmt.Fprintf(&b, T("disk %s: %s %s, mounted at %s, %s"), d.ID, d.FS, d.UUID, d.Mount, d.State)
	if d.Reason != nil {
		fmt.Fprintf(&b, " (%s)", *d.Reason)
	}
	b.WriteString("\n")
	if out.DryRun {
		fmt.Fprintf(&b, T("would be added to %s\n"), configPath)
	} else {
		fmt.Fprintf(&b, T("added to %s\n"), configPath)
	}
	return b.String()
}
