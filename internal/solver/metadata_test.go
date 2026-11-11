package solver

import (
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

type metaVector struct {
	Note   string `json:"note"`
	Copies []struct {
		Disk      *string    `json:"disk"`
		Position  *int       `json:"position"`
		Changed   bool       `json:"changed"`
		State     *CopyState `json:"state"`
		RowMtime  *int64     `json:"row_mtime"`
		Mtime     *int64     `json:"mtime"`
		Ctime     *int64     `json:"ctime"`
		Reachable bool       `json:"reachable"`
		Precision int64      `json:"precision_ns"`
		RowMode   *int64     `json:"row_mode"`
		LookMode  *int64     `json:"look_mode"`
	} `json:"copies"`
	Plan *struct {
		Mtime    *int64   `json:"mtime"`
		From     string   `json:"from"`
		Apply    []string `json:"apply"`
		Mode     *int64   `json:"mode"`
		ModeFrom string   `json:"mode_from"`
		Chmod    []string `json:"chmod"`
		Stale    []string `json:"stale"`
	} `json:"plan"`
	Error bool `json:"error"`
}

func TestMetadataVectors(t *testing.T) {
	for i, v := range vectors.Load[metaVector](t, "metadata.json") {
		if v.Copies == nil || (v.Plan == nil) == !v.Error {
			t.Fatalf("vector %d: copies, and a plan or an error", i)
		}
		var copies []MetaCopy
		for _, c := range v.Copies {
			if c.Disk == nil || c.Position == nil || c.State == nil || c.RowMtime == nil || c.Changed != (c.Mtime != nil) || c.Changed != (c.Ctime != nil) || (!c.Changed && c.LookMode != nil) {
				t.Fatalf("vector %d: a copy needs a disk, a position, a state and its row's time, and a changed one, and only that, its look's", i)
			}
			mc := MetaCopy{Disk: *c.Disk, Position: *c.Position, Changed: c.Changed, State: *c.State, RowMtime: *c.RowMtime, Reachable: c.Reachable, PrecisionNS: c.Precision}
			if c.Changed {
				mc.Mtime, mc.Ctime = *c.Mtime, *c.Ctime
			}
			if c.RowMode != nil {
				mc.RowMode, mc.HasRowMode = *c.RowMode, true
			}
			if c.LookMode != nil {
				mc.LookMode, mc.HasLookMode = *c.LookMode, true
			}
			copies = append(copies, mc)
		}
		got, err := MetadataPlan(copies)
		if v.Error {
			if err == nil {
				t.Errorf("vector %d (%s): %+v, want an error", i, v.Note, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("vector %d (%s): %v", i, v.Note, err)
			continue
		}
		want := v.Plan
		if got.HasMtime != (want.Mtime != nil) || (want.Mtime != nil && got.Mtime != *want.Mtime) || got.From != want.From || !slices.Equal(got.Apply, want.Apply) ||
			got.HasMode != (want.Mode != nil) || (want.Mode != nil && got.Mode != *want.Mode) || got.ModeFrom != want.ModeFrom || !slices.Equal(got.Chmod, want.Chmod) ||
			!slices.Equal(got.Stale, want.Stale) {
			t.Errorf("vector %d (%s):\n got %+v\nwant %+v", i, v.Note, got, *want)
		}
	}
}

// Whatever the copies, the time and the mode applied are ones a changed copy moved to; each copy that lacks one gets
// it, through an operation where the pass can write and by going stale elsewhere; and a copy that lacks neither,
// is stale or gives what it lacks gets nothing.
func TestMetadataPlanLeavesWhatHoldsItsMetadata(t *testing.T) {
	states := []CopyState{CopyOK, CopySurplus, CopyStale, CopyUnavailable}
	x := uint32(2463534242)
	next := func(n int) int {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		return int(x % uint32(n))
	}
	for range 100000 {
		var copies []MetaCopy
		for p := range next(4) + 1 {
			c := MetaCopy{Disk: string(rune('a' + p)), Position: p, Changed: next(2) == 0, State: states[next(len(states))], RowMtime: int64(next(3)), Reachable: next(3) != 0}
			c.RowMode, c.HasRowMode = int64(next(3)), next(4) != 0
			if c.Changed {
				c.Mtime, c.Ctime = int64(next(3)), int64(next(3))
				c.LookMode, c.HasLookMode = int64(next(3)), true
				if c.State == CopyStale {
					c.State = CopyOK
				}
			}
			copies = append(copies, c)
		}
		plan, err := MetadataPlan(copies)
		if err != nil {
			t.Fatal(err)
		}
		for i := range copies {
			c := &copies[i]
			mode, known := c.modeNow()
			needsTime := plan.HasMtime && c.Disk != plan.From && c.mtimeNow() != plan.Mtime && c.State != CopyStale
			needsMode := plan.HasMode && c.Disk != plan.ModeFrom && (!known || mode != plan.Mode) && c.State != CopyStale
			apply, chmod, stale := slices.Contains(plan.Apply, c.Disk), slices.Contains(plan.Chmod, c.Disk), slices.Contains(plan.Stale, c.Disk)
			want := needsTime || needsMode
			if apply != (want && c.Reachable && needsTime) || chmod != (want && c.Reachable && needsMode) || stale != (want && !c.Reachable) {
				t.Fatalf("%+v: %+v, %s", copies, plan, c.Disk)
			}
		}
		if plan.HasMtime && !slices.ContainsFunc(copies, func(c MetaCopy) bool {
			return c.Disk == plan.From && c.Changed && c.Mtime != c.RowMtime && c.Mtime == plan.Mtime
		}) {
			t.Fatalf("%+v: %+v", copies, plan)
		}
		if plan.HasMode && !slices.ContainsFunc(copies, func(c MetaCopy) bool {
			return c.Disk == plan.ModeFrom && c.Changed && c.HasRowMode && c.LookMode != c.RowMode && c.LookMode == plan.Mode
		}) {
			t.Fatalf("%+v: %+v", copies, plan)
		}
	}
}
