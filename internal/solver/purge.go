package solver

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/peondusud/atoll/internal/value"
)

// PurgeWhy says why a purge of the trash reaches an entry (specs.md §17, §17.4).
type PurgeWhy string

// The reasons a purge reaches an entry for.
const (
	PurgeExpired   PurgeWhy = "expired"
	PurgeOlderThan PurgeWhy = "older-than"
	PurgeMaxSize   PurgeWhy = "max-size"
)

// roomRank gives the order in which an entry goes for room, by the reason it reached the trash for: surplus and
// rebalance first, then overwritten, repaired and quarantined. Deleted and renamed entries never go for room before
// their time, as a renamed copy may be the file's only other copy (specs.md §17), nor do the ones a catalog rebuild
// listed again, whose reason is unknown and may be either. ok is false for a reason outside the closed list.
func roomRank(reason string) (rank int, room, ok bool) {
	switch reason {
	case "surplus", "rebalance":
		return 0, true, true
	case "overwritten", "repaired", "quarantined":
		return 1, true, true
	case "deleted", "renamed", "unknown":
		return 0, false, true
	default:
		return 0, false, false
	}
}

// TrashEntry is one entry of the trash, as the choice of what a purge reaches sees it. HasSize is false for an entry
// of unknown size. Held says that it reached the trash in this run's reprise: it counts in its disk's trash, and does
// not go (specs.md §17.2).
type TrashEntry struct {
	Disk      string
	OpID      int64
	Reason    string
	Size      int64
	HasSize   bool
	DeletedNS int64
	ExpiresNS int64
	Held      bool
}

// TrashDisk is one disk, with its capacity as the catalog last recorded it; HasCapacity is false when none was.
type TrashDisk struct {
	ID          string
	Capacity    int64
	HasCapacity bool
}

// PurgeInput is the whole trash. With OlderThanNS, what was trashed at least that long ago goes, and nothing for
// room; without it, what has reached its time, then what goes for room.
type PurgeInput struct {
	NowNS       int64
	OlderThanNS *int64
	MaxSize     value.Percent
	Disks       []TrashDisk
	Entries     []TrashEntry
}

// PurgeChoice is one entry a purge reaches, and why.
type PurgeChoice struct {
	OpID int64
	Why  PurgeWhy
}

// PurgeResult is what a purge reaches, by operation, and the disks whose trash still holds more than max_size of
// their capacity once those are gone, by id in byte order.
type PurgeResult struct {
	Purge []PurgeChoice
	Full  []string
}

// ChoosePurge says what a purge of the trash reaches (specs.md §17, §17.4). It deletes nothing. An error means that a
// reason is outside its closed list, that a size, a capacity or the age is negative, that an operation id is below 1 or
// appears twice, or that an entry's disk is not listed, or listed twice.
func ChoosePurge(in *PurgeInput) (*PurgeResult, error) {
	if in.OlderThanNS != nil && *in.OlderThanNS < 0 {
		return nil, fmt.Errorf("purge: negative age %d", *in.OlderThanNS)
	}
	disks, err := trashDisks(in.Disks)
	if err != nil {
		return nil, err
	}
	if err := checkTrash(in.Entries, disks); err != nil {
		return nil, err
	}
	used := TrashUsed(in.Entries)
	chosen := map[int64]PurgeWhy{}
	for i := range in.Entries {
		e := &in.Entries[i]
		switch {
		case e.Held:
		case in.OlderThanNS != nil:
			if e.DeletedNS <= subtractClamped(in.NowNS, *in.OlderThanNS) {
				chosen[e.OpID] = PurgeOlderThan
			}
		case e.ExpiresNS <= in.NowNS:
			chosen[e.OpID] = PurgeExpired
		}
		if chosen[e.OpID] != "" && e.HasSize {
			used[e.Disk] -= e.Size
		}
	}
	if in.OlderThanNS == nil {
		for _, e := range roomCandidates(in.Entries, chosen) {
			if TrashOver(in.MaxSize, used[e.Disk], disks[e.Disk]) {
				chosen[e.OpID] = PurgeMaxSize
				used[e.Disk] -= e.Size
			}
		}
	}
	out := &PurgeResult{Purge: []PurgeChoice{}, Full: FullTrashes(in.MaxSize, in.Disks, used)}
	for op, why := range chosen {
		out.Purge = append(out.Purge, PurgeChoice{OpID: op, Why: why})
	}
	slices.SortFunc(out.Purge, func(a, b PurgeChoice) int { return cmp.Compare(a.OpID, b.OpID) })
	return out, nil
}

// roomCandidates gives what may go for room, in the order it goes: by rank, then oldest first, then by operation.
func roomCandidates(entries []TrashEntry, chosen map[int64]PurgeWhy) []*TrashEntry {
	var out []*TrashEntry
	for i := range entries {
		e := &entries[i]
		if _, room, _ := roomRank(e.Reason); room && e.HasSize && !e.Held && chosen[e.OpID] == "" {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b *TrashEntry) int {
		ra, _, _ := roomRank(a.Reason)
		rb, _, _ := roomRank(b.Reason)
		return cmp.Or(cmp.Compare(ra, rb), cmp.Compare(a.DeletedNS, b.DeletedNS), cmp.Compare(a.OpID, b.OpID))
	})
	return out
}

// TrashUsed counts each disk's trash as the sizes of its entries, an entry of unknown size counting for nothing. A sum
// past the largest integer stays there: it is over any limit anyway.
func TrashUsed(entries []TrashEntry) map[string]int64 {
	used := map[string]int64{}
	for i := range entries {
		e := &entries[i]
		if e.HasSize && e.Size > 0 {
			used[e.Disk] = addClamped(used[e.Disk], e.Size)
		}
	}
	return used
}

// TrashOver says whether a trash that holds used bytes holds more than max_size of its disk's capacity. A disk with no
// capacity recorded never does.
func TrashOver(maxSize value.Percent, used int64, d *TrashDisk) bool {
	return d != nil && d.HasCapacity && maxSize.Exceeded(used, d.Capacity)
}

// FullTrashes gives the disks whose trash holds more than max_size of their capacity, by id in byte order.
func FullTrashes(maxSize value.Percent, list []TrashDisk, used map[string]int64) []string {
	out := []string{}
	for i := range list {
		if TrashOver(maxSize, used[list[i].ID], &list[i]) {
			out = append(out, list[i].ID)
		}
	}
	slices.SortFunc(out, strings.Compare)
	return out
}

func trashDisks(list []TrashDisk) (map[string]*TrashDisk, error) {
	disks := map[string]*TrashDisk{}
	for i := range list {
		d := &list[i]
		if disks[d.ID] != nil {
			return nil, fmt.Errorf("purge: disk %q comes twice", d.ID)
		}
		if d.HasCapacity && d.Capacity < 0 {
			return nil, fmt.Errorf("purge: disk %q: negative capacity %d", d.ID, d.Capacity)
		}
		disks[d.ID] = d
	}
	return disks, nil
}

func checkTrash(entries []TrashEntry, disks map[string]*TrashDisk) error {
	ops := map[int64]bool{}
	for i := range entries {
		e := &entries[i]
		if _, _, ok := roomRank(e.Reason); !ok {
			return fmt.Errorf("purge: operation %d: unknown reason %q", e.OpID, e.Reason)
		}
		if disks[e.Disk] == nil {
			return fmt.Errorf("purge: operation %d: unknown disk %q", e.OpID, e.Disk)
		}
		if e.OpID < 1 || ops[e.OpID] {
			return fmt.Errorf("purge: operation %d: below 1, or twice", e.OpID)
		}
		if e.HasSize && e.Size < 0 {
			return fmt.Errorf("purge: operation %d: negative size %d", e.OpID, e.Size)
		}
		ops[e.OpID] = true
	}
	return nil
}

func addClamped(a, b int64) int64 {
	if b > math.MaxInt64-a {
		return math.MaxInt64
	}
	return a + b
}

// subtractClamped gives a-b for a b of 0 or more, at the smallest integer if that is past it.
func subtractClamped(a, b int64) int64 {
	if b > 0 && a < math.MinInt64+b {
		return math.MinInt64
	}
	return a - b
}
