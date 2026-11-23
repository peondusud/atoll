package watch

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/peondusud/atoll/internal/cache"
	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/rules"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/value"
)

// Residencies says, of each file with a copy on a cache disk, whether it stays on the SSD cache or leaves it now
// (specs.md §19), by file id. It says nothing when Atoll's own view does not present the cache, as with mergerfs: the
// copies on a cache disk count as any other then. Released is what atoll replicate names, nil for none.
func Residencies(ctx context.Context, db *sql.DB, c *config.Config, diskList []catalog.Disk, now int64,
	released func(string) bool) (map[int64]solver.Residency, error) {
	s, err := readCache(ctx, db, c, diskList, now, released)
	if err != nil || s == nil {
		return nil, err
	}
	out := make(map[int64]solver.Residency, len(s.in.Files))
	for _, f := range s.in.Files {
		out[s.ids[f.Path]] = solver.CacheResident
	}
	for _, l := range s.leave {
		out[s.ids[l.Path]] = solver.CacheTransfer
	}
	return out, nil
}

// CacheWaits counts the files resident on the SSD cache that will leave it, and says when the first may: the first
// opening of replication.window once its residency is over (specs.md §15.12, §19). A daemon wakes then, with no event to
// wake it. A file waiting for atoll replicate is not counted.
func CacheWaits(ctx context.Context, db *sql.DB, c *config.Config, now int64) (int64, sql.NullInt64, error) {
	diskList, err := catalog.Disks(ctx, db)
	if err != nil {
		return 0, sql.NullInt64{}, fmt.Errorf("catalog: %w", err)
	}
	s, err := readCache(ctx, db, c, diskList, now, nil)
	if err != nil || s == nil {
		return 0, sql.NullInt64{}, err
	}
	leaving := map[string]bool{}
	for _, l := range s.leave {
		leaving[l.Path] = true
	}
	var count int64
	var next sql.NullInt64
	for i := range s.in.Files {
		f := &s.in.Files[i]
		at, due := cache.DueAt(&s.in, f)
		if leaving[f.Path] || !due {
			continue
		}
		opens := s.window.Next(time.Unix(0, max(at, now)).In(s.location)).UnixNano()
		count++
		if !next.Valid || opens < next.Int64 {
			next = sql.NullInt64{Int64: opens, Valid: true}
		}
	}
	return count, next, nil
}

// cacheState is what the SSD cache's decision reads, what it decided, and replication.window.
type cacheState struct {
	in       cache.Input
	leave    []cache.Leave
	ids      map[string]int64
	window   value.Window
	location *time.Location
}

// readCache reads and decides the SSD cache, nil when Atoll's own view does not present it, or it holds nothing.
func readCache(ctx context.Context, db *sql.DB, c *config.Config, diskList []catalog.Disk, now int64,
	released func(string) bool) (*cacheState, error) {
	settings, err := c.Cache()
	if err != nil || !settings.InView {
		return nil, err
	}
	s := &cacheState{in: cache.Input{NowNS: now, Enabled: settings.Enabled, MaxFill: settings.MaxFill}, ids: map[string]int64{}}
	for i := range diskList {
		d := &diskList[i]
		if d.Role != solver.RoleCache {
			continue
		}
		disk := cache.Disk{ID: d.ID, Position: d.Position}
		if d.HasCapacity {
			disk.Capacity, disk.Free = &d.Capacity, &d.Free
		}
		s.in.Disks = append(s.in.Disks, disk)
	}
	if len(s.in.Disks) == 0 {
		return nil, nil
	}
	copies, err := catalog.CacheCopies(ctx, db)
	if err != nil || len(copies) == 0 {
		return nil, err
	}
	if s.window, s.location, err = c.Schedule(); err != nil {
		return nil, err
	}
	s.in.WindowOpen = s.window.Contains(time.Unix(0, now).In(s.location))
	for i := 0; i < len(copies); {
		j := i
		for j < len(copies) && copies[j].FileID == copies[i].FileID {
			j++
		}
		s.in.Files = append(s.in.Files, cacheFile(c, copies[i:j], released))
		s.ids[copies[i].Path] = copies[i].FileID
		i = j
	}
	if s.leave, err = cache.Leaving(&s.in); err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	return s, nil
}

// cacheFile gives the decision what it needs of a file, from its copies on cache disks: its residency began at the
// earliest change time of those that hold its current version, ok or surplus, as a mirror copy is made after the copy
// the file was written to; of all of them when none does. A rule that now ignores the file leaves it the pool's
// residency, as it leaves it its placement (specs.md §15.10).
func cacheFile(c *config.Config, copies []catalog.CacheCopy, released func(string) bool) cache.File {
	first := &copies[0]
	size := first.Size
	policy := c.Policy(first.Path, &size)
	f := cache.File{Path: first.Path, Size: first.Size, Cache: policy.Cache, MinAgeNS: policy.CacheMinAge.Nanoseconds(),
		Held: policy.Mode == "manual", Released: released != nil && released(first.Path)}
	if policy.Action == rules.Ignore {
		f.Cache, f.MinAgeNS, f.Held = true, c.Pool.CacheMinAge.Nanoseconds(), false
	}
	current, earliest := int64(0), int64(0)
	seenCurrent := false
	for i := range copies {
		cp := &copies[i]
		f.Disks = append(f.Disks, cp.Disk)
		since := cp.FirstSeenNS
		if cp.CtimeNS.Valid {
			since = cp.CtimeNS.Int64
		}
		if i == 0 || since < earliest {
			earliest = since
		}
		if cp.State == string(solver.CopyOK) || cp.State == string(solver.CopySurplus) {
			if !seenCurrent || since < current {
				current = since
			}
			seenCurrent = true
		}
	}
	f.SinceNS = earliest
	if seenCurrent {
		f.SinceNS = current
	}
	return f
}

// allLeave says of every file with a copy on a cache disk that it leaves the cache, when Atoll's own view presents it:
// what the pool holds is what its data disks hold, as every file leaves the cache in time (specs.md §13.3, §19).
func allLeave(ctx context.Context, db *sql.DB, c *config.Config) (map[int64]solver.Residency, error) {
	settings, err := c.Cache()
	if err != nil || !settings.InView {
		return nil, err
	}
	copies, err := catalog.CacheCopies(ctx, db)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]solver.Residency, len(copies))
	for i := range copies {
		out[copies[i].FileID] = solver.CacheTransfer
	}
	return out, nil
}

// Transfer is a transfer job of the SSD cache: a file's copy on the cache disk Src goes, as the data disks hold it.
type Transfer struct {
	FileID int64
	Path   string
	Src    string
}

// CacheTransfers gives the transfer jobs of the files leaving the SSD cache now (specs.md §19), by path, then by
// disk position: each such file planned alone, from the catalog as it stands, as the copies a pass has just made are in
// no plan made before them. A plan of one file decides its transfers as a plan of the pool does: they lean on its copies
// alone, never on room or names.
func CacheTransfers(ctx context.Context, db *sql.DB, c *config.Config, now int64, leaves Leaves) ([]Transfer, error) {
	diskList, err := catalog.Disks(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	residency, err := Residencies(ctx, db, c, diskList, now, nil)
	if err != nil || len(residency) == 0 {
		return nil, err
	}
	settings, err := c.Cache()
	if err != nil {
		return nil, err
	}
	grace, err := missingGraceOf(c)
	if err != nil {
		return nil, err
	}
	minFree, err := c.MinFree()
	if err != nil {
		return nil, err
	}
	k, err := newPlanKit(ctx, db, c, diskList, nil)
	if err != nil {
		return nil, err
	}
	in := solver.Input{MinFree: minFree, Disks: planDisks(diskList, now, grace, leaves), CacheMirror: settings.Mirror, CacheEarly: settings.Early}
	ids := map[string]int64{}
	for id, r := range residency {
		if r != solver.CacheTransfer {
			continue
		}
		f, err := catalog.FileByID(ctx, db, id)
		if err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
		if f == nil {
			continue
		}
		leaving, err := catalog.Leaving(ctx, db, f.Path, f.Path)
		if err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
		file, _, err := k.planFile(f, leaving[f.ID], r)
		if err != nil {
			return nil, err
		}
		in.Files = append(in.Files, file)
		ids[f.Path] = f.ID
	}
	plan, err := solver.MakePlan(in)
	if err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}
	var out []Transfer
	for _, j := range plan.Jobs {
		if j.Kind == solver.JobTransfer {
			out = append(out, Transfer{FileID: ids[j.Path], Path: j.Path, Src: j.Src})
		}
	}
	slices.SortStableFunc(out, func(a, b Transfer) int { return cmp.Compare(a.Path, b.Path) })
	return out, nil
}
