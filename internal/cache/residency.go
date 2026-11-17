package cache

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/peondusud/atoll/internal/value"
)

// Disk is a cache disk, as the catalog last recorded it. Capacity and Free are nil when they are not known: nothing
// leaves it for room then.
type Disk struct {
	ID       string
	Position int
	Capacity *int64
	Free     *int64
}

// File is a file with a copy on a cache disk.
type File struct {
	Path  string
	Size  int64
	Disks []string // the cache disks that hold a copy of it
	// SinceNS is when its residency began: the change time of its copy on the cache, as the catalog last recorded it,
	// or that copy's first_seen when it has none (specs.md §19).
	SinceNS int64
	// Cache and MinAgeNS are its policy's cache and cache_min_age, read with its size.
	Cache    bool
	MinAgeNS int64
	// Held says that it is of mode manual and waits for atoll replicate: it never leaves but when Released.
	Held bool
	// Released says that atoll replicate names it: it leaves at once.
	Released bool
}

// Input is what the decision depends on.
type Input struct {
	NowNS int64
	// Enabled is cache.enabled: with false, every file on the cache is due.
	Enabled bool
	// WindowOpen says that replication.window is open now: a due file leaves only then.
	WindowOpen bool
	// MaxFill is cache.max_fill: above it, the oldest files of a cache disk leave at once.
	MaxFill value.Percent
	Disks   []Disk
	Files   []File
}

// Why is why a file leaves the cache.
type Why string

// The reasons a file leaves the cache now (specs.md §19).
const (
	// WhyDue: its residency is over, its policy says cache = false, or the cache is not enabled, and the window is
	// open.
	WhyDue Why = "due"
	// WhyFull: its cache disk is above cache.max_fill, and it is among the oldest there.
	WhyFull Why = "full"
	// WhyReleased: atoll replicate names it.
	WhyReleased Why = "released"
)

// Leave is a file that leaves the cache now.
type Leave struct {
	Path string
	Why  Why
}

// Leaving says which files leave the cache now, and why, by path in byte order; every other file on the cache is
// resident (specs.md §19, conformance/vectors/cache_residency.json). An error means that the input is not valid.
func Leaving(in *Input) ([]Leave, error) {
	disks, err := checkDisks(in.Disks)
	if err != nil {
		return nil, err
	}
	if err := checkFiles(in.Files, disks); err != nil {
		return nil, err
	}
	why := make(map[string]Why, len(in.Files))
	for i := range in.Files {
		f := &in.Files[i]
		switch {
		case f.Released:
			why[f.Path] = WhyReleased
		case f.Held:
		case in.WindowOpen && due(in, f):
			why[f.Path] = WhyDue
		}
	}
	order := slices.Clone(in.Disks)
	slices.SortFunc(order, func(a, b Disk) int { return cmp.Compare(a.Position, b.Position) })
	for i := range order {
		full(in, &order[i], why)
	}
	out := make([]Leave, 0, len(why))
	for path, w := range why {
		out = append(out, Leave{Path: path, Why: w})
	}
	slices.SortFunc(out, func(a, b Leave) int { return cmp.Compare(a.Path, b.Path) })
	return out, nil
}

// due says whether a file's time on the cache is over: its residency ended at or before now, its policy says
// cache = false, or the cache is not enabled. A residency that began after now, as with a clock set back, is not over.
func due(in *Input, f *File) bool {
	if !in.Enabled || !f.Cache {
		return true
	}
	return f.SinceNS <= in.NowNS && in.NowNS-f.SinceNS >= f.MinAgeNS
}

// full sends the oldest files of a cache disk above max_fill, until what is left on it is at most max_fill: what leaves
// already counts as gone, and a file waiting for atoll replicate never goes.
func full(in *Input, d *Disk, why map[string]Why) {
	if d.Capacity == nil || d.Free == nil {
		return
	}
	used := *d.Capacity - *d.Free
	var stay []*File
	for i := range in.Files {
		f := &in.Files[i]
		if !slices.Contains(f.Disks, d.ID) {
			continue
		}
		if _, leaves := why[f.Path]; leaves {
			used -= f.Size
		} else if !f.Held {
			stay = append(stay, f)
		}
	}
	slices.SortFunc(stay, func(a, b *File) int { return cmp.Or(cmp.Compare(a.SinceNS, b.SinceNS), cmp.Compare(a.Path, b.Path)) })
	for _, f := range stay {
		if !in.MaxFill.Exceeded(used, *d.Capacity) {
			return
		}
		why[f.Path] = WhyFull
		used -= f.Size
	}
}

func checkDisks(list []Disk) (map[string]bool, error) {
	seen := make(map[string]bool, len(list))
	for _, d := range list {
		if d.ID == "" {
			return nil, errors.New("cache: a disk with no id")
		}
		if seen[d.ID] {
			return nil, fmt.Errorf("cache: disk %q appears twice", d.ID)
		}
		seen[d.ID] = true
		if d.Capacity != nil && *d.Capacity < 0 || d.Free != nil && *d.Free < 0 {
			return nil, fmt.Errorf("cache: disk %q: a negative capacity or free space", d.ID)
		}
		if d.Capacity != nil && d.Free != nil && *d.Free > *d.Capacity {
			return nil, fmt.Errorf("cache: disk %q: more free space than capacity", d.ID)
		}
	}
	return seen, nil
}

func checkFiles(list []File, disks map[string]bool) error {
	seen := make(map[string]bool, len(list))
	for i := range list {
		f := &list[i]
		if seen[f.Path] {
			return fmt.Errorf("cache: %q appears twice", f.Path)
		}
		seen[f.Path] = true
		if f.Size < 0 || f.MinAgeNS < 0 {
			return fmt.Errorf("cache: %q: a negative size or cache_min_age", f.Path)
		}
		if len(f.Disks) == 0 {
			return fmt.Errorf("cache: %q: on no cache disk", f.Path)
		}
		for _, id := range f.Disks {
			if !disks[id] {
				return fmt.Errorf("cache: %q: on unknown disk %q", f.Path, id)
			}
		}
	}
	return nil
}

// DueAt says when a file on the cache is first due to leave it (specs.md §19): its residency's end, or now when its
// policy says cache = false or the cache is not enabled. False for a file waiting for atoll replicate, which is never
// due but when that command names it.
func DueAt(in *Input, f *File) (int64, bool) {
	switch {
	case f.Released:
		return in.NowNS, true
	case f.Held:
		return 0, false
	case !in.Enabled || !f.Cache:
		return in.NowNS, true
	case f.SinceNS > math.MaxInt64-f.MinAgeNS:
		return math.MaxInt64, true
	}
	return f.SinceNS + f.MinAgeNS, true
}
