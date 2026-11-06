package solver

import (
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

type pruneVector struct {
	State    *State `json:"state"`
	Replicas *int   `json:"replicas"`
	Hash     *bool  `json:"hash"`
	Copies   []struct {
		Disk      *string    `json:"disk"`
		Position  *int       `json:"position"`
		State     *CopyState `json:"state"`
		CreatedBy *string    `json:"created_by"`
		DiskState *DiskState `json:"disk_state"`
		Verified  *bool      `json:"verified"`
		Unchanged *bool      `json:"unchanged"`
	} `json:"copies"`
	Outcomes *[]struct {
		Disk    string       `json:"disk"`
		Outcome PruneOutcome `json:"outcome"`
	} `json:"outcomes"`
	Error bool   `json:"error"`
	Note  string `json:"note"`
}

func TestPruneVectors(t *testing.T) {
	for i, v := range vectors.Load[pruneVector](t, "prune.json") {
		if v.State == nil || v.Replicas == nil || v.Hash == nil || v.Copies == nil {
			t.Errorf("vector %d: want state, replicas, hash and copies", i)
			continue
		}
		if v.Error == (v.Outcomes != nil) {
			t.Errorf("vector %d: want exactly one of outcomes and error", i)
			continue
		}
		in := PruneInput{State: *v.State, Replicas: *v.Replicas, HasHash: *v.Hash}
		for j := range v.Copies {
			c := &v.Copies[j]
			if c.Disk == nil || c.Position == nil || c.State == nil || c.CreatedBy == nil || c.DiskState == nil || c.Verified == nil || c.Unchanged == nil {
				t.Fatalf("vector %d: copy %d: every field is required", i, j)
			}
			in.Copies = append(in.Copies, PruneCopy{
				Disk: *c.Disk, Position: *c.Position, State: *c.State, CreatedBy: *c.CreatedBy,
				DiskState: *c.DiskState, Verified: *c.Verified, Unchanged: *c.Unchanged,
			})
		}
		got, err := DecidePrune(in)
		if v.Error {
			if err == nil {
				t.Errorf("vector %d (%s): got %v, want an error", i, v.Note, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("vector %d (%s): %v", i, v.Note, err)
			continue
		}
		want := *v.Outcomes
		if len(got) != len(want) {
			t.Errorf("vector %d (%s): %v, want %v", i, v.Note, got, want)
			continue
		}
		for j := range want {
			if got[j].Disk != want[j].Disk || got[j].Outcome != want[j].Outcome {
				t.Errorf("vector %d (%s): %v, want %v", i, v.Note, got, want)
				break
			}
		}
	}
}

// Whatever a file looks like, a copy that goes leaves the level behind it, in copies that can be
// read, were read, and are what their rows say, with a user copy among them if it was one.
func TestPruneNeverLeavesLess(t *testing.T) {
	copyStates := []CopyState{CopyOK, CopySurplus, CopyStale, CopyCorrupt, CopyPending, CopyUnavailable}
	diskStates := []DiskState{DiskOnline, DiskStandby, DiskReadOnly, DiskMissing, DiskDraining, DiskFailed, DiskRetired}
	x := uint32(2463534242)
	next := func(n int) int {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		return int(x % uint32(n))
	}
	bit := func() bool { return next(2) == 1 }
	moved := 0
	for range 200000 {
		in := PruneInput{State: StateOK, Replicas: next(3), HasHash: next(8) != 0}
		for p := range next(5) + 1 {
			by := CreatedByUser
			if bit() {
				by = CreatedByAtoll
			}
			in.Copies = append(in.Copies, PruneCopy{
				Disk: string(rune('a' + p)), Position: p, State: copyStates[next(2)*next(len(copyStates))], CreatedBy: by,
				DiskState: diskStates[next(2)*next(len(diskStates))], Verified: next(5) != 0, Unchanged: next(5) != 0,
			})
		}
		results, err := DecidePrune(in)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range results {
			if r.Outcome != PruneToTrash {
				continue
			}
			moved++
			var it PruneCopy
			good, user := 0, 0
			for i := range in.Copies {
				c := &in.Copies[i]
				if c.Disk == r.Disk {
					it = *c
				}
				if c.State == CopyOK && c.Unchanged && c.Verified && readableNow(c.DiskState) {
					good++
					if c.CreatedBy == CreatedByUser {
						user++
					}
				}
			}
			if good < in.Replicas+1 || it.State != CopySurplus || !it.Verified || !it.Unchanged || !in.HasHash {
				t.Fatalf("%+v: %s goes", in, r.Disk)
			}
			if it.CreatedBy == CreatedByUser {
				stays := 0
				for i := range in.Copies {
					c := &in.Copies[i]
					if c.State == CopyOK && c.CreatedBy == CreatedByUser && c.Unchanged && readableNow(c.DiskState) {
						stays++
					}
				}
				if stays == 0 {
					t.Fatalf("%+v: the last user copy goes", in)
				}
			}
		}
	}
	if moved == 0 {
		t.Fatal("no copy ever went: this checked nothing")
	}
}
