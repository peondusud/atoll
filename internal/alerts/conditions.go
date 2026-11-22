package alerts

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/value"
)

// The keys of the conditions, or what they begin with (specs.md §23).
const (
	KeyDiskMissing  = "disk-missing:"
	KeyDiskReadonly = "disk-readonly:"
	KeyFiles        = "files-"
	KeyGuard        = "guard-"
	KeyManual       = "manual-waiting"
	KeyPoolFull     = "pool-full"
	KeyNameRefused  = "name-refused"
	KeyTrashFull    = "trash-full:"
	KeySmartFailing = "smart-failing:"
	KeySmartSectors = "smart-sectors:"
	KeyNTFSDirty    = "ntfs-dirty:"
	KeyNTFSNames    = "ntfs-names:"
	KeyLinkUnnamed  = "link-unnamed:"
	KeySharedDevice = "shared-device:"
	KeyNoAccess     = "no-access:"
	KeyWorkdirOwner = "workdir-owner:"
	KeyUPS          = "ups-unreachable"
	KeyTooLong      = "branch-path-too-long:"
	KeyVersions     = "versions-purged:"
)

// FirstN is how many paths an alert names, at most.
const FirstN = 10

// maxBody is how long the text of an alert may be: ntfy takes a longer one only as an attachment, which a server
// may refuse. The paths an alert names stop at the first that would not fit, with room for a last line.
const maxBody = 4000

// Ntfy priorities: high for what may cost a file, default for the rest.
const (
	priorityHigh    = 4
	priorityDefault = 3
)

// Refused counts the names a driver refused in a pass, and keeps the first FirstN in byte order.
type Refused struct {
	Count int64
	First []string
}

// Add counts one refusal.
func (r *Refused) Add(path string) {
	r.Count++
	at, found := slices.BinarySearch(r.First, path)
	if found || at >= FirstN {
		return
	}
	r.First = slices.Insert(r.First, at, path)
	if len(r.First) > FirstN {
		r.First = r.First[:FirstN]
	}
}

// Facts is what the conditions after a pass come from.
type Facts struct {
	Disks   []jobs.DiskLine
	Guards  []jobs.GuardLine
	Refused Refused
	Files   *catalog.FileAlerts
	// Used and Capacity of the data disks that hold their files now: online, in standby, read-only or draining.
	Used, Capacity int64
	// SeenByNS: a file of mode manual first seen at or before it, and still unprotected, is waiting too long.
	SeenByNS int64
	// ManualAfter is defaults.manual_alert_after, as the configuration writes it.
	ManualAfter string
	// TrashFull holds the disks that hold their files now whose trash holds more than trash.max_size of their capacity,
	// in the catalog's order of the disks.
	TrashFull []TrashFull
	// MaxSize is trash.max_size, and Retention trash.retention, as the configuration writes them.
	MaxSize, Retention string
	// Versions holds the disks whose trash purged, for room, versions a modification replaced before their retention
	// ended, by id (specs.md §17).
	Versions []VersionsPurged
	// Smart holds the last reading of SMART of each disk that has one, neither failed nor retired, in the catalog's
	// order of the disks.
	Smart []SmartLine
	// Dirty holds the missing disks whose NTFS volume the daemon found marked dirty (specs.md §22).
	Dirty []string
	// Disagree holds the disks whose last reprise left operations planned as a name and its file's record disagree
	// there, in the catalog's order of the disks (specs.md §17.2).
	Disagree []NamesDisagree
	// Unnamed holds the disks whose links of the guard hold data nothing names, as its last run found them, in the
	// catalog's order of the disks (specs.md §17).
	Unnamed []UnnamedLinks
	// Shared holds the groups of disks a start found on one physical disk, by their first disk (specs.md §10, R-06),
	// in the catalog's order of the disks.
	Shared []SharedDevice
	// Foreign holds the disks whose work folder belongs to an account that is neither root's nor Atoll's, in the
	// configuration's order (specs.md §10).
	Foreign []ForeignWorkdir
	// Unopened holds, by disk, the files a read of the daemon could not open, for a reason that says nothing of
	// their disk, in byte order (specs.md §15.10): they count with the names a walk could not reach.
	Unopened map[string][]string
	// UPSUnreachable is ups.nut when the UPS the daemon reads has said nothing for 15 s or more, "" otherwise
	// (specs.md §20).
	UPSUnreachable string
}

// SharedDevice is a group of disks of the pool on one physical disk, by its first disk: each disk of it in the
// catalog's order, with the physical disks a start found it on.
type SharedDevice struct {
	ID    string
	Disks []SharedDisk
}

// SharedDisk is a disk of such a group, and the physical disks it lives on, joined by a comma.
type SharedDisk struct {
	ID     string
	Device string
}

// UnnamedLinks is a disk whose links of the guard hold data nothing names, and how many.
type UnnamedLinks struct {
	ID    string
	Links int
}

// NamesDisagree is a disk whose volume holds names and records that disagree, and the operations left for that.
type NamesDisagree struct {
	ID  string
	Ops int
}

// ForeignWorkdir is a disk's work folder, or the folder of its parts, of its links or of its trash, that another
// account owns: where it is, whose it is, and whose it should be.
type ForeignWorkdir struct {
	ID   string
	Path string
	UID  uint32
	Want uint32
}

// SmartLine is a disk's last reading of SMART.
type SmartLine struct {
	ID string
	disks.SmartRecord
}

// VersionsPurged is a disk whose trash purged, for room under trash.max_size, entries of the reason overwritten,
// versions of files a modification replaced, before their retention ended: how many, and their first paths in byte
// order (specs.md §17, §23).
type VersionsPurged struct {
	ID    string
	Paths Refused
}

// TrashFull is a disk whose trash holds more than trash.max_size of its capacity: Used bytes of Capacity.
type TrashFull struct {
	ID             string
	Used, Capacity int64
}

// Gather reads, under the catalog's lock, what the conditions after a pass come from: the pass's outcome, the names
// refused in it, and the catalog.
func Gather(ctx context.Context, db *sql.DB, c *config.Config, nowNS int64, o *jobs.Outcome, refused *Refused) (*Facts, error) {
	after, _ := c.Effective()["defaults.manual_alert_after"].(string)
	wait, err := value.ParseDuration(after)
	if err != nil {
		return nil, fmt.Errorf("defaults.manual_alert_after: %w", err)
	}
	f := &Facts{Disks: o.Disks, Guards: o.Guards, Refused: *refused, SeenByNS: nowNS - wait.Nanoseconds(), ManualAfter: after}
	f.MaxSize, _ = c.Effective()["trash.max_size"].(string)
	f.Retention, _ = c.Effective()["trash.retention"].(string)
	maxSize, err := value.ParsePercent(f.MaxSize)
	if err != nil {
		return nil, fmt.Errorf("trash.max_size: %w", err)
	}
	if f.Files, err = catalog.CountFileAlerts(ctx, db, f.SeenByNS); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	list, err := catalog.Disks(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	used, err := catalog.TrashUsed(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	shared := catalog.SharedDevices(list)
	for i := range list {
		d := &list[i]
		f.Smart = appendSmart(f.Smart, d)
		if shared[d.ID] == d.ID {
			s := SharedDevice{ID: d.ID}
			for j := range list {
				if other := &list[j]; shared[other.ID] == d.ID {
					s.Disks = append(s.Disks, SharedDisk{ID: other.ID, Device: other.Device})
				}
			}
			f.Shared = append(f.Shared, s)
		}
		if d.NamesDisagree > 0 && d.State != "retired" {
			f.Disagree = append(f.Disagree, NamesDisagree{ID: d.ID, Ops: d.NamesDisagree})
		}
		if d.LinksUnnamed > 0 && d.State != "retired" {
			f.Unnamed = append(f.Unnamed, UnnamedLinks{ID: d.ID, Links: d.LinksUnnamed})
		}
		if !slices.Contains([]string{"online", "standby", "readonly", "draining"}, d.State) {
			continue
		}
		if solver.TrashOver(maxSize, used[d.ID], &solver.TrashDisk{ID: d.ID, Capacity: d.Capacity, HasCapacity: d.HasCapacity}) {
			f.TrashFull = append(f.TrashFull, TrashFull{ID: d.ID, Used: used[d.ID], Capacity: d.Capacity})
		}
		if d.Role != "data" || !d.HasCapacity {
			continue
		}
		f.Capacity += d.Capacity
		f.Used += max(d.Capacity-d.Free, 0)
	}
	return f, nil
}

// appendSmart adds a disk's last reading of SMART, unless it has none, or is failed or retired: the user knows of it
// then. A reading the catalog holds that does not decode counts as none, so that it cannot hold up every alert.
func appendSmart(lines []SmartLine, d *catalog.Disk) []SmartLine {
	if d.SmartJSON == "" || d.State == "failed" || d.State == "retired" {
		return lines
	}
	var r disks.SmartRecord
	if err := json.Unmarshal([]byte(d.SmartJSON), &r); err != nil {
		return lines
	}
	return append(lines, SmartLine{ID: d.ID, SmartRecord: r})
}

// Holdings gives the conditions that hold, each once, with its count.
func Holdings(f *Facts) []Holding {
	counts := map[string]int64{}
	for _, d := range f.Disks {
		switch d.Verdict.State {
		case disks.Missing:
			counts[KeyDiskMissing+d.ID] = 1
		case disks.Readonly:
			counts[KeyDiskReadonly+d.ID] = 1
		case disks.Online:
		}
		// Only a walk that ended says what it could not reach, and what a read could not open is said of a disk that
		// is there.
		if n := d.Refused + int64(len(f.Unopened[d.ID])); d.Walked && n > 0 {
			counts[KeyNoAccess+d.ID] = n
		}
		if d.Walked && d.TooLong > 0 {
			counts[KeyTooLong+d.ID] = d.TooLong
		}
	}
	for i := range f.Foreign {
		counts[KeyWorkdirOwner+f.Foreign[i].ID] = 1
	}
	if f.UPSUnreachable != "" {
		counts[KeyUPS] = 1
	}
	for _, g := range f.Guards {
		counts[KeyGuard+g.Kind+":"+g.Disk] = 1
	}
	if f.Files != nil {
		for _, state := range catalog.AlertStates {
			if n := f.Files.ByState[state]; n > 0 {
				counts[KeyFiles+state] = n
			}
		}
		if f.Files.Manual > 0 {
			counts[KeyManual] = f.Files.Manual
		}
	}
	if poolFull(f.Used, f.Capacity) {
		counts[KeyPoolFull] = 1
	}
	if f.Refused.Count > 0 {
		counts[KeyNameRefused] = f.Refused.Count
	}
	for _, d := range f.TrashFull {
		counts[KeyTrashFull+d.ID] = 1
	}
	for _, v := range f.Versions {
		if v.Paths.Count > 0 {
			counts[KeyVersions+v.ID] = v.Paths.Count
		}
	}
	for _, id := range f.Dirty {
		counts[KeyNTFSDirty+id] = 1
	}
	for _, d := range f.Disagree {
		counts[KeyNTFSNames+d.ID] = int64(d.Ops)
	}
	for _, d := range f.Unnamed {
		counts[KeyLinkUnnamed+d.ID] = int64(d.Links)
	}
	for _, d := range f.Shared {
		counts[KeySharedDevice+d.ID] = 1
	}
	for i := range f.Smart {
		s := &f.Smart[i]
		if s.Failing {
			counts[KeySmartFailing+s.ID] = 1
		}
		if n := s.Sectors(); n > 0 {
			counts[KeySmartSectors+s.ID] = n
		}
	}
	out := make([]Holding, 0, len(counts))
	for key, n := range counts {
		out = append(out, Holding{Key: key, Count: n})
	}
	slices.SortFunc(out, func(a, b Holding) int { return strings.Compare(a.Key, b.Key) })
	return out
}

// poolFull says that more than 90 % of the capacity is used.
func poolFull(used, capacity int64) bool {
	return capacity > 0 && used > capacity/10*9+capacity%10*9/10
}

// Message is an alert to send.
type Message struct {
	Key      string
	Title    string
	Body     string
	Priority int
}

// Describe writes the alert of a condition that holds, reading the catalog for the paths it names.
func Describe(ctx context.Context, db *sql.DB, f *Facts, h Holding) (*Message, error) {
	m := &Message{Key: h.Key, Priority: priorityDefault}
	switch {
	case strings.HasPrefix(h.Key, KeyDiskMissing):
		id := strings.TrimPrefix(h.Key, KeyDiskMissing)
		m.Priority = priorityHigh
		m.Title = fmt.Sprintf("Atoll: disk %s is missing", id)
		m.Body = fmt.Sprintf("The disk %s is missing%s. Atoll writes nothing to it and reads its files from their other copies; "+
			"a file with no other copy is unavailable until the disk comes back.", id, why(f, id))
	case strings.HasPrefix(h.Key, KeyDiskReadonly):
		id := strings.TrimPrefix(h.Key, KeyDiskReadonly)
		m.Title = fmt.Sprintf("Atoll: disk %s is read-only", id)
		m.Body = fmt.Sprintf("The disk %s is read-only%s. Atoll reads it but makes no copy on it.", id, why(f, id))
	case strings.HasPrefix(h.Key, KeyGuard):
		kind, id, _ := strings.Cut(strings.TrimPrefix(h.Key, KeyGuard), ":")
		m.Priority = priorityHigh
		line := guardLine(f, kind, id)
		m.Title = fmt.Sprintf("Atoll: the mass-%s guard of disk %s tripped", strings.TrimSuffix(kind, "s"), id)
		count := fmt.Sprint(line.Count)
		if line.Window == jobs.WindowDay {
			// Its day tripped it, modifies.day_guard: a rewrite slower than its window lets through (specs.md §15.6).
			m.Title += " on what a day changed"
			count += " within a day"
		}
		m.Body = fmt.Sprintf("Atoll holds the %s it found on disk %s (%s) and spreads none of them to the other copies "+
			"until you answer: atoll guard list.", kind, id, count)
	case h.Key == KeyManual:
		paths, err := catalog.FirstManualPaths(ctx, db, f.SeenByNS, FirstN)
		if err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
		m.Title = fmt.Sprintf("Atoll: %d %s in manual mode left unprotected", h.Count, files(h.Count))
		m.Body = list(fmt.Sprintf("These files have had no copy for more than %s; atoll replicate makes their copies.", f.ManualAfter),
			paths, h.Count)
	case strings.HasPrefix(h.Key, KeyTrashFull):
		id := strings.TrimPrefix(h.Key, KeyTrashFull)
		var d TrashFull
		for _, full := range f.TrashFull {
			if full.ID == id {
				d = full
			}
		}
		m.Title = fmt.Sprintf("Atoll: the trash of disk %s is full", id)
		m.Body = fmt.Sprintf("The trash of disk %s holds %d bytes, more than trash.max_size (%s) of its %d bytes, once purged: "+
			"what is left there, deleted or renamed files, waits for its retention. atoll trash list shows it; "+
			"atoll trash purge --older-than <duration> --yes purges it sooner.", id, d.Used, f.MaxSize, d.Capacity)
	case strings.HasPrefix(h.Key, KeyVersions):
		id := strings.TrimPrefix(h.Key, KeyVersions)
		var first []string
		for _, v := range f.Versions {
			if v.ID == id {
				first = v.Paths.First
			}
		}
		m.Priority = priorityHigh
		m.Title = fmt.Sprintf("Atoll: the trash of disk %s purged %d versions of files before their time", id, h.Count)
		if h.Count == 1 {
			m.Title = fmt.Sprintf("Atoll: the trash of disk %s purged 1 version of a file before its time", id)
		}
		m.Body = list(fmt.Sprintf("To make room under trash.max_size (%s), the trash of disk %s purged %d %s that a modification "+
			"replaced, before their %s of retention ended: those versions are lost, and the trash no longer holds what these files "+
			"were. A library rewritten slowly, as by a tool that encrypts it, ends so with no clean version. Look at what changes "+
			"them; raise trash.max_size, or free room on the disk, to keep the versions to come. Backups are the answer to "+
			"ransomware.", f.MaxSize, id, h.Count, plural(h.Count, "version", "versions"), f.Retention), first, h.Count)
	case strings.HasPrefix(h.Key, KeyNTFSDirty):
		id := strings.TrimPrefix(h.Key, KeyNTFSDirty)
		m.Priority = priorityHigh
		m.Title = fmt.Sprintf("Atoll: the NTFS volume of disk %s is marked dirty", id)
		m.Body = fmt.Sprintf("The disk %s is missing: its NTFS volume is marked dirty, as an unclean unplugging leaves it, and Linux "+
			"mounts it only by force, which Atoll never does. Repair it with chkdsk /f on Windows, then plug it back.", id)
	case strings.HasPrefix(h.Key, KeySharedDevice):
		id := strings.TrimPrefix(h.Key, KeySharedDevice)
		var ids, each []string
		for i := range f.Shared {
			if f.Shared[i].ID == id {
				for _, d := range f.Shared[i].Disks {
					ids = append(ids, d.ID)
					each = append(each, fmt.Sprintf("%s (%s)", d.ID, strings.ReplaceAll(d.Device, ",", ", ")))
				}
			}
		}
		if len(ids) < 2 {
			return nil, fmt.Errorf("alert %q: no such group of disks", h.Key)
		}
		m.Priority = priorityHigh
		m.Title = fmt.Sprintf("Atoll: disks %s share a physical disk", andList(ids))
		m.Body = fmt.Sprintf("The disks %s live on one physical disk: one failure of it loses them all (R-06). Atoll counts them as one "+
			"and places no two copies of a file on them, moving an extra copy it made elsewhere, and goes on writing to them. Put "+
			"each on a physical disk of its own, or take one out of the pool: atoll disk remove %s --evacuate --dry-run.",
			andList(each), ids[len(ids)-1])
	case strings.HasPrefix(h.Key, KeyLinkUnnamed):
		id := strings.TrimPrefix(h.Key, KeyLinkUnnamed)
		m.Title = fmt.Sprintf("Atoll: %d %s of disk %s %s data nothing names", h.Count, plural(h.Count, "link", "links"), id,
			plural(h.Count, "holds", "hold"))
		m.Body = fmt.Sprintf("The link guard of disk %s holds %d %s in Atoll/links whose data has no other name Atoll knows, and no "+
			"row or event says where it was: nothing is removed, as it may be someone's file. Look at it there, and move it back "+
			"under the root, or remove the link once it is nobody's.", id, h.Count, plural(h.Count, "link", "links"))
	case strings.HasPrefix(h.Key, KeyNTFSNames):
		id := strings.TrimPrefix(h.Key, KeyNTFSNames)
		m.Priority = priorityHigh
		m.Title = fmt.Sprintf("Atoll: the NTFS volume of disk %s needs a repair", id)
		m.Body = fmt.Sprintf("%d %s on disk %s stay planned: a name and its file's record disagree on its volume, as ntfs3 leaves "+
			"them when the device fails during a move. Atoll concludes nothing of them, and every run on that disk exits with 4 "+
			"until the volume is repaired. Repair it with chkdsk /f on Windows, then plug it back: the next run finishes them.",
			h.Count, plural(h.Count, "operation", "operations"), id)
	case strings.HasPrefix(h.Key, KeyNoAccess):
		id := strings.TrimPrefix(h.Key, KeyNoAccess)
		var first []string
		for _, d := range f.Disks {
			if d.ID == id {
				first = slices.Concat(d.RefusedFirst, f.Unopened[id])
			}
		}
		slices.Sort(first)
		first = slices.Compact(first)
		first = first[:min(len(first), FirstN)]
		m.Title = fmt.Sprintf("Atoll: %d %s of disk %s %s the account Atoll runs as", h.Count, plural(h.Count, "name", "names"), id,
			plural(h.Count, "refuses", "refuse"))
		m.Body = list(fmt.Sprintf("A walk of the disk %s could not reach these names, or a read could not open them: each refuses "+
			"the account Atoll runs as, as the mount of another user does, a rule of the system's, a folder when Atoll lacks its "+
			"rights, or a file whose key is not loaded. Nothing is concluded of them or of what is below them, and nothing there is "+
			"copied or verified until they can be read.", id), first, h.Count)
	case strings.HasPrefix(h.Key, KeyTooLong):
		id := strings.TrimPrefix(h.Key, KeyTooLong)
		var first []string
		for _, d := range f.Disks {
			if d.ID == id {
				first = d.TooLongFirst
			}
		}
		m.Title = fmt.Sprintf("Atoll: %d %s of disk %s %s past the limit of its mergerfs branch", h.Count, plural(h.Count, "name", "names"), id,
			plural(h.Count, "is", "are"))
		m.Body = list(fmt.Sprintf("The path of each of these names on the disk %s is past 4095 bytes, which mergerfs cannot present: "+
			"a walk of the disk neither records them nor goes below them. Nothing is concluded of them or of what is below them, and "+
			"while one is there no copy of that disk is taken for deleted, as it may have gone there: its file is kept, and its "+
			"copies made again. Give them a shorter path, or "+
			"set walk.long_paths = \"walk\".", id), first, h.Count)
	case strings.HasPrefix(h.Key, KeyWorkdirOwner):
		id := strings.TrimPrefix(h.Key, KeyWorkdirOwner)
		var w ForeignWorkdir
		for i := range f.Foreign {
			if f.Foreign[i].ID == id {
				w = f.Foreign[i]
			}
		}
		m.Title = fmt.Sprintf("Atoll: a folder Atoll works in on disk %s belongs to another account", id)
		m.Body = fmt.Sprintf("The folder %s, which Atoll works in on disk %s, belongs to the account %d, which is neither root's "+
			"nor the one Atoll runs as. That account can read what Atoll copies, through the folders of its parts and of its links, "+
			"and remove what its trash holds. Atoll goes on writing there. Give the folder back: chown -R %d %s", Shown(w.Path), id, w.UID, w.Want, Shown(w.Path))
	case h.Key == KeyUPS:
		m.Title = "Atoll: the UPS cannot be read"
		m.Body = fmt.Sprintf("NUT has said nothing of the UPS %s for 15 s or more. Atoll goes on as on line power: a power cut "+
			"would neither stop its copies nor have it write its disks out before the battery runs low. Check that upsd runs "+
			"there and knows that UPS: upsc %s ups.status", f.UPSUnreachable, f.UPSUnreachable)
	case strings.HasPrefix(h.Key, KeySmartFailing):
		id := strings.TrimPrefix(h.Key, KeySmartFailing)
		m.Priority = priorityHigh
		m.Title = fmt.Sprintf("Atoll: disk %s is failing", id)
		m.Body = fmt.Sprintf("The disk %s failed its own SMART health check%s. Move what it holds to the other disks while it "+
			"still answers: atoll disk remove %s --evacuate --dry-run says what that takes.", id, smartSays(f, id), id)
	case strings.HasPrefix(h.Key, KeySmartSectors):
		id := strings.TrimPrefix(h.Key, KeySmartSectors)
		s := smartOf(f, id)
		if s.Pending > 0 || s.Uncorrectable > 0 {
			m.Priority = priorityHigh
		}
		m.Title = fmt.Sprintf("Atoll: disk %s has %d bad %s", id, h.Count, plural(h.Count, "sector", "sectors"))
		m.Body = fmt.Sprintf("SMART says that the disk %s moved %d sectors, waits to move %d and could not read %d%s. A count "+
			"that grows is a disk failing: atoll scrub reads its copies against their hashes, and atoll disk remove %s "+
			"--evacuate --dry-run says what moving its files would take.", id, s.Reallocated, s.Pending, s.Uncorrectable, temperature(&s), id)
	case h.Key == KeyPoolFull:
		m.Title = fmt.Sprintf("Atoll: the pool is %d %% full", f.Used*100/f.Capacity)
		m.Body = fmt.Sprintf("The data disks hold %d of their %d bytes. New copies may find no room.", f.Used, f.Capacity)
	case h.Key == KeyNameRefused:
		m.Title = fmt.Sprintf("Atoll: a driver refused %d %s", h.Count, plural(h.Count, "name", "names"))
		m.Body = list("A disk's driver refused these names; their copies go to another disk, or wait.", f.Refused.First, h.Count)
	case strings.HasPrefix(h.Key, KeyFiles):
		state := strings.TrimPrefix(h.Key, KeyFiles)
		paths, err := catalog.FirstStatePaths(ctx, db, state, FirstN)
		if err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
		if state == "corrupt" || state == "unavailable" {
			m.Priority = priorityHigh
		}
		m.Title = fmt.Sprintf("Atoll: %d %s %s", h.Count, state, files(h.Count))
		m.Body = list("atoll status --path PATH shows the copies of each.", paths, h.Count)
	default:
		return nil, fmt.Errorf("alerts: no condition %q", h.Key)
	}
	return m, nil
}

// smartOf gives a disk's last reading of SMART, none when it has none.
func smartOf(f *Facts, id string) disks.Smart {
	for i := range f.Smart {
		if f.Smart[i].ID == id {
			return f.Smart[i].Smart
		}
	}
	return disks.Smart{}
}

// smartSays tells what else a disk's last reading of SMART said: its bad sectors and its temperature.
func smartSays(f *Facts, id string) string {
	s := smartOf(f, id)
	if s.Sectors() == 0 {
		return temperature(&s)
	}
	return fmt.Sprintf(", with %d sectors moved, %d waiting to move and %d it could not read%s", s.Reallocated, s.Pending, s.Uncorrectable,
		temperature(&s))
}

// temperature says a reading's temperature, when it gave one.
func temperature(s *disks.Smart) string {
	if s.Temperature == 0 {
		return ""
	}
	return fmt.Sprintf(", at %d °C", s.Temperature)
}

// why gives the reason of a disk's state, as its verdict says it.
func why(f *Facts, id string) string {
	for _, d := range f.Disks {
		if d.ID == id && d.Verdict.Message != "" {
			return ": " + d.Verdict.Message
		}
	}
	return ""
}

// guardLine gives the line of the guard of that kind of a disk that tripped, an empty one when none did.
func guardLine(f *Facts, kind, id string) jobs.GuardLine {
	for _, g := range f.Guards {
		if g.Disk == id && g.Kind == kind {
			return g
		}
	}
	return jobs.GuardLine{}
}

func files(n int64) string { return plural(n, "file", "files") }

func plural(n int64, one, more string) string {
	if n == 1 {
		return one
	}
	return more
}

// list writes a first line, then paths, one a line, as many as fit in maxBody with room for the last lines, and
// says how many more there are.
func list(first string, paths []string, count int64) string {
	var b bytes.Buffer
	b.WriteString(first)
	named := int64(0)
	for _, p := range paths {
		shown := Shown(p)
		if b.Len()+1+len(shown) > maxBody-100 {
			break
		}
		b.WriteByte('\n')
		b.WriteString(shown)
		named++
	}
	if more := count - named; more > 0 {
		fmt.Fprintf(&b, "\nand %d more", more)
	}
	return b.String()
}

// Shown gives a path as an alert shows it: as it is when it is UTF-8 with no control character and no format
// character, in base64 otherwise, as the human output does (names.Shown, specs.md §23, §25).
func Shown(p string) string {
	return names.Shown(p)
}

// andList joins names as a sentence does: "a", "a and b", "a, b and c".
func andList(list []string) string {
	if len(list) < 2 {
		return strings.Join(list, "")
	}
	return strings.Join(list[:len(list)-1], ", ") + " and " + list[len(list)-1]
}
