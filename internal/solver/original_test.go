package solver

import (
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

type originalCopyVector struct {
	Disk        *string    `json:"disk"`
	Position    *int       `json:"position"`
	State       *CopyState `json:"state"`
	DiskState   *DiskState `json:"disk_state"`
	CreationNS  *int64     `json:"creation_ns"`
	FirstSeenNS *int64     `json:"first_seen_ns"`
}

type originalVector struct {
	Copies   *[]originalCopyVector `json:"copies"`
	Previous *string               `json:"previous"`
	Original *string               `json:"original"`
	Error    bool                  `json:"error"`
	Note     string                `json:"note"`
}

func TestSelectOriginalVectors(t *testing.T) {
	for i, v := range vectors.Load[originalVector](t, "original.json") {
		if v.Copies == nil {
			t.Errorf("vector %d (%s): want copies", i, v.Note)
			continue
		}
		if v.Error == (v.Original != nil) {
			t.Errorf("vector %d (%s): want exactly one of original and error", i, v.Note)
			continue
		}
		copies := make([]Candidate, 0, len(*v.Copies))
		complete := true
		for _, c := range *v.Copies {
			if c.Disk == nil || c.Position == nil || c.State == nil || c.DiskState == nil || c.FirstSeenNS == nil {
				complete = false
				break
			}
			copies = append(copies, Candidate{*c.Disk, *c.Position, *c.State, *c.DiskState, c.CreationNS, *c.FirstSeenNS})
		}
		if !complete {
			t.Errorf("vector %d (%s): a copy needs disk, position, state, disk_state and first_seen_ns", i, v.Note)
			continue
		}
		previous := ""
		if v.Previous != nil {
			if *v.Previous == "" {
				t.Errorf("vector %d (%s): leave previous out rather than empty", i, v.Note)
				continue
			}
			previous = *v.Previous
		}
		got, err := SelectOriginal(copies, previous)
		if v.Error {
			if err == nil {
				t.Errorf("vector %d (%s): got %q, want an error", i, v.Note, got)
			}
			continue
		}
		if err != nil || got != *v.Original {
			t.Errorf("vector %d (%s): got %q, %v, want %q", i, v.Note, got, err, *v.Original)
		}
	}
}

// The answer does not depend on the order of the copies, in any permutation of
// random copies, and it does not change the input.
func TestSelectOriginalIgnoresInputOrder(t *testing.T) {
	state := uint64(0x9E3779B97F4A7C15)
	next := func(n int) int {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		return int(state % uint64(n))
	}
	copyStates := []CopyState{CopyOK, CopySurplus, CopyStale, CopyCorrupt, CopyUnavailable, CopyPending}
	diskStates := []DiskState{DiskOnline, DiskStandby, DiskReadOnly, DiskDraining, DiskMissing, DiskFailed, DiskRetired}
	names := []string{"a", "b", "c", "d"}
	for range 5000 {
		copies := make([]Candidate, 0, len(names))
		for p, d := range names {
			c := Candidate{Disk: d, Position: p, State: copyStates[next(len(copyStates))], DiskState: diskStates[next(len(diskStates))], FirstSeenNS: int64(next(3))}
			if next(2) == 0 {
				created := int64(next(3))
				c.CreationNS = &created
			}
			copies = append(copies, c)
		}
		previous := ""
		if next(3) == 0 {
			previous = names[next(len(names))]
		}
		want, err := SelectOriginal(copies, previous)
		if err != nil {
			t.Fatal(err)
		}
		before := slices.Clone(copies)
		for range 6 {
			i, j := next(len(copies)), next(len(copies))
			copies[i], copies[j] = copies[j], copies[i]
			got, err := SelectOriginal(copies, previous)
			if err != nil || got != want {
				t.Fatalf("copies %+v, previous %q: got %q, %v, want %q", copies, previous, got, err, want)
			}
		}
		slices.SortFunc(copies, func(a, b Candidate) int { return a.Position - b.Position })
		if !slices.Equal(copies, before) {
			t.Fatalf("the input changed: %+v, was %+v", copies, before)
		}
	}
}
