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
	} `json:"copies"`
	Plan *struct {
		Mtime *int64   `json:"mtime"`
		From  string   `json:"from"`
		Apply []string `json:"apply"`
		Stale []string `json:"stale"`
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
			if c.Disk == nil || c.Position == nil || c.State == nil || c.RowMtime == nil || c.Changed != (c.Mtime != nil) || c.Changed != (c.Ctime != nil) {
				t.Fatalf("vector %d: a copy needs a disk, a position, a state and its row's time, and a changed one its look's times", i)
			}
			mc := MetaCopy{Disk: *c.Disk, Position: *c.Position, Changed: c.Changed, State: *c.State, RowMtime: *c.RowMtime, Reachable: c.Reachable}
			if c.Changed {
				mc.Mtime, mc.Ctime = *c.Mtime, *c.Ctime
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
		if got.HasMtime != (want.Mtime != nil) || (want.Mtime != nil && got.Mtime != *want.Mtime) || got.From != want.From || !slices.Equal(got.Apply, want.Apply) || !slices.Equal(got.Stale, want.Stale) {
			t.Errorf("vector %d (%s):\n got %+v\nwant %+v", i, v.Note, got, *want)
		}
	}
}

// Whatever the copies, the time applied is one a changed copy moved to, nothing goes to the copy that gives it,
// and a copy that holds that time already, or is stale, is left alone.
func TestMetadataPlanLeavesWhatHoldsTheTime(t *testing.T) {
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
			if c.Changed {
				c.Mtime, c.Ctime = int64(next(3)), int64(next(3))
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
		for _, c := range copies {
			now := c.RowMtime
			if c.Changed {
				now = c.Mtime
			}
			touched := slices.Contains(plan.Apply, c.Disk) || slices.Contains(plan.Stale, c.Disk)
			switch {
			case touched && (!plan.HasMtime || now == plan.Mtime || c.State == CopyStale):
				t.Fatalf("%+v: %+v", copies, plan)
			case touched && slices.Contains(plan.Apply, c.Disk) != c.Reachable:
				t.Fatalf("%+v: %+v", copies, plan)
			case !touched && plan.HasMtime && now != plan.Mtime && c.State != CopyStale:
				t.Fatalf("%+v: %+v, %s left behind", copies, plan, c.Disk)
			}
		}
		if plan.HasMtime && !slices.ContainsFunc(copies, func(c MetaCopy) bool {
			return c.Disk == plan.From && c.Changed && c.Mtime != c.RowMtime && c.Mtime == plan.Mtime
		}) {
			t.Fatalf("%+v: %+v", copies, plan)
		}
	}
}
