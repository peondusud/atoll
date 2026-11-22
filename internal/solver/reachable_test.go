package solver

import (
	"fmt"
	"reflect"
	"slices"
	"testing"

	"pgregory.net/rapid"

	"github.com/peondusud/atoll/internal/vectors"
)

// reachableVector is a case of conformance/vectors/capacity_reachable.json: a pool, as plan.json gives one, and what
// atoll capacity says of it.
type reachableVector struct {
	Disks       []diskVector  `json:"disks"`
	Files       []fileVector  `json:"files"`
	Entries     []entryVector `json:"entries"`
	Taken       []takenVector `json:"taken"`
	MinFree     []string      `json:"min_free"`
	NewFiles    string        `json:"new_files"`
	FillTo      string        `json:"fill_to"`
	Equalize    string        `json:"equalize"`
	CacheMirror bool          `json:"cache_mirror"`
	CacheEarly  bool          `json:"cache_early"`
	SpreadMode  string        `json:"spread_mode"`
	Levels      []levelVector `json:"levels"`
	Error       bool          `json:"error"`
	Note        string        `json:"note"`
}

// levelVector is a level of atoll capacity --json (api/schemas/capacity.schema.json), each field given.
type levelVector struct {
	Copies      *int   `json:"copies"`
	Theoretical *int64 `json:"theoretical"`
	Reachable   *int64 `json:"reachable"`
	Stored      *int64 `json:"stored"`
	Short       *int64 `json:"short"`
}

// input turns a case into the input of Levels, with the defaults of plan.json.
func (v *reachableVector) input() (Input, bool, error) {
	pool := planVector{
		Disks: v.Disks, Files: v.Files, Entries: v.Entries, Taken: v.Taken, MinFree: v.MinFree, NewFiles: v.NewFiles,
		FillTo: v.FillTo, Equalize: v.Equalize, CacheMirror: v.CacheMirror, CacheEarly: v.CacheEarly,
	}
	in, err := pool.input()
	if err != nil {
		return in, false, err
	}
	switch v.SpreadMode {
	case "", ModePrefer:
		return in, false, nil
	case ModeStrict:
		return in, true, nil
	default:
		return in, false, fmt.Errorf("spread_mode %q", v.SpreadMode)
	}
}

func (v *reachableVector) want() ([]Level, error) {
	levels := make([]Level, 0, len(v.Levels))
	for i, l := range v.Levels {
		if l.Copies == nil || l.Theoretical == nil || l.Reachable == nil || l.Stored == nil || l.Short == nil {
			return nil, fmt.Errorf("level %d: want copies, theoretical, reachable, stored and short", i)
		}
		levels = append(levels, Level{Copies: *l.Copies, Theoretical: *l.Theoretical, Reachable: *l.Reachable, Stored: *l.Stored, Short: *l.Short})
	}
	return levels, nil
}

func TestReachableVectors(t *testing.T) {
	for i, v := range vectors.Load[reachableVector](t, "capacity_reachable.json") {
		if v.Error == (v.Levels != nil) {
			t.Errorf("vector %d (%s): want levels or error", i, v.Note)
			continue
		}
		in, strict, err := v.input()
		var levels []Level
		if err == nil {
			levels, err = Levels(in, strict)
		}
		if v.Error {
			if err == nil {
				t.Errorf("vector %d (%s): got %+v, want an error", i, v.Note, levels)
			}
			continue
		}
		want, wantErr := v.want()
		switch {
		case wantErr != nil:
			t.Errorf("vector %d (%s): %v", i, v.Note, wantErr)
			continue
		case err != nil:
			t.Errorf("vector %d (%s): %v", i, v.Note, err)
			continue
		case !slices.Equal(levels, want):
			t.Errorf("vector %d (%s):\n%+v, want\n%+v", i, v.Note, levels, want)
		}
		// The same whatever the order of the disks and the files: the list of the configuration and the catalog's
		// order of the paths are no part of what the pool holds.
		again := in
		again.Disks, again.Files = slices.Clone(in.Disks), slices.Clone(in.Files)
		slices.Reverse(again.Disks)
		slices.Reverse(again.Files)
		if got, err := Levels(again, strict); err != nil || !slices.Equal(got, levels) {
			t.Errorf("vector %d (%s): the other way round, %+v, %v", i, v.Note, got, err)
		}
	}
}

// cloneInput copies an input deep enough that a change to what Levels was given shows.
func cloneInput(in Input) Input {
	out := in
	out.Disks, out.Entries = slices.Clone(in.Disks), slices.Clone(in.Entries)
	out.Files = make([]File, len(in.Files))
	for i, f := range in.Files {
		place := *f.Placement
		f.Placement, f.Copies = &place, slices.Clone(f.Copies)
		out.Files[i] = f
	}
	return out
}

// TestLevelsRapid: on pools rapid draws, what atoll capacity says holds together (specs.md §13.1, §13.3). The formula
// is that of the disks alone, whatever the files; the room left that the reachable capacity counts is never more than
// the disks have free, as what the plan places only takes room; the bytes stored are those of the files, all of them
// when none is short and none when all are; nothing depends on the order of the input, and the input stays as it was.
func TestLevelsRapid(t *testing.T) {
	reach := map[string]int{}
	checkRapid(t, 0x9E3779B97F4A7C15, 1000, func(t *rapid.T) {
		in := drawInput(t)
		strict := rapid.Bool().Draw(t, "strict")
		before := cloneInput(in)
		levels, err := Levels(in, strict)
		if err != nil {
			t.Fatalf("%v", err)
		}
		if !reflect.DeepEqual(in.Disks, before.Disks) || !reflect.DeepEqual(in.Files, before.Files) || !reflect.DeepEqual(in.Entries, before.Entries) {
			t.Fatalf("Levels changed its input")
		}
		empty := in
		empty.Files = nil
		room, err := Levels(empty, strict)
		if err != nil {
			t.Fatalf("%v", err)
		}
		var total int64
		for _, f := range in.Files {
			total += f.Size
		}
		if len(levels) != MaxCopies || len(room) != MaxCopies {
			t.Fatalf("%d and %d levels", len(levels), len(room))
		}
		for k := range MaxCopies {
			l, r := levels[k], room[k]
			switch {
			case l.Copies != k+1 || r.Copies != k+1:
				t.Fatalf("level %d says %d copies", k+1, l.Copies)
			case l.Theoretical != r.Theoretical:
				t.Fatalf("%d copies: the formula gives %d with the files and %d without", k+1, l.Theoretical, r.Theoretical)
			case r.Stored != 0 || r.Short != 0 || r.Reachable > r.Theoretical:
				t.Fatalf("%d copies, no file: %+v", k+1, r)
			case l.Stored < 0 || l.Stored > total || l.Short < 0 || l.Short > int64(len(in.Files)):
				t.Fatalf("%d copies: %+v, of %d files and %d bytes", k+1, l, len(in.Files), total)
			case l.Short == 0 && l.Stored != total, l.Short == int64(len(in.Files)) && l.Stored != 0:
				t.Fatalf("%d copies: %d bytes stored with %d files short, of %d files and %d bytes", k+1, l.Stored, l.Short, len(in.Files), total)
			case l.Reachable < l.Stored || l.Reachable-l.Stored > r.Reachable:
				t.Fatalf("%d copies: %d reachable with %d stored, past the %d the disks have free", k+1, l.Reachable, l.Stored, r.Reachable)
			}
			if l.Short > 0 {
				reach["short"]++
			}
			if l.Stored > 0 {
				reach["stored"]++
			}
			if l.Reachable-l.Stored < r.Reachable {
				reach["room taken"]++
			}
			if strict && l.Theoretical > 0 {
				reach["strict"]++
			}
		}
		again := in
		again.Disks = rapid.Permutation(in.Disks).Draw(t, "disk order")
		again.Entries = rapid.Permutation(in.Entries).Draw(t, "entry order")
		again.Files = rapid.Permutation(in.Files).Draw(t, "file order")
		for i := range again.Files {
			again.Files[i].Copies = rapid.Permutation(again.Files[i].Copies).Draw(t, "copy order")
		}
		same, err := Levels(again, strict)
		if err != nil {
			t.Fatalf("%v", err)
		}
		if !slices.Equal(levels, same) {
			t.Fatalf("the order of the input changed the levels\n%+v\n%+v", levels, same)
		}
	})
	checkReach(t, reach, "short", "stored", "room taken", "strict")
}
