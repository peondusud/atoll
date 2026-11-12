package alerts

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
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
	// MaxSize is trash.max_size, as the configuration writes it.
	MaxSize string
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
	for i := range list {
		d := &list[i]
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
		m.Title = fmt.Sprintf("Atoll: the mass-%s guard of disk %s tripped", strings.TrimSuffix(kind, "s"), id)
		m.Body = fmt.Sprintf("Atoll holds the %s it found on disk %s (%d) and spreads none of them to the other copies "+
			"until you answer: atoll guard list.", kind, id, guardCount(f, kind, id))
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

// why gives the reason of a disk's state, as its verdict says it.
func why(f *Facts, id string) string {
	for _, d := range f.Disks {
		if d.ID == id && d.Verdict.Message != "" {
			return ": " + d.Verdict.Message
		}
	}
	return ""
}

func guardCount(f *Facts, kind, id string) int64 {
	for _, g := range f.Guards {
		if g.Disk == id && g.Kind == kind {
			return g.Count
		}
	}
	return 0
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

// Shown gives a path as an alert shows it: as it is when it is UTF-8 with no control character, in base64 otherwise.
func Shown(p string) string {
	if utf8.ValidString(p) && !strings.ContainsFunc(p, unicode.IsControl) {
		return p
	}
	return "(base64) " + base64.StdEncoding.EncodeToString([]byte(p))
}
