package solver

import (
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

type modVector struct {
	Note string `json:"note"`
	File *struct {
		State   *State  `json:"state"`
		Size    *int64  `json:"size"`
		Hash    *string `json:"hash"`
		Running bool    `json:"running"`
	} `json:"file"`
	Copies []struct {
		Disk     *string      `json:"disk"`
		Position *int         `json:"position"`
		Verdict  *CopyVerdict `json:"verdict"`
		Other    bool         `json:"other"`
		State    *CopyState   `json:"state"`
		Size     *int64       `json:"size"`
		Due      bool         `json:"due"`
		Readable bool         `json:"readable"`
		Hash     *string      `json:"hash"`
	} `json:"copies"`
	Decision *struct {
		Kind      ModKind  `json:"kind"`
		Read      []string `json:"read"`
		Changed   []string `json:"changed"`
		Stale     []string `json:"stale"`
		Reference string   `json:"reference"`
		Hash      *string  `json:"hash"`
	} `json:"decision"`
	Error bool `json:"error"`
}

func hashOf(s *string) []byte {
	if s == nil {
		return nil
	}
	return []byte(*s)
}

func modInputOf(t *testing.T, i int, v *modVector) *ModInput {
	t.Helper()
	if v.File == nil || v.File.State == nil || v.File.Size == nil || v.Copies == nil || (v.Decision == nil) == !v.Error {
		t.Fatalf("vector %d: a file with a state and a size, copies, and a decision or an error", i)
	}
	in := &ModInput{State: *v.File.State, Size: *v.File.Size, Hash: hashOf(v.File.Hash), Running: v.File.Running}
	for _, c := range v.Copies {
		if c.Disk == nil || c.Position == nil || c.Verdict == nil || c.State == nil || c.Size == nil {
			t.Fatalf("vector %d: a copy needs a disk, a position, a verdict, a state and a size", i)
		}
		in.Copies = append(in.Copies, ModCopy{
			Disk: *c.Disk, Position: *c.Position, Verdict: *c.Verdict, Other: c.Other, State: *c.State, Size: *c.Size,
			Due: c.Due, Readable: c.Readable, Hash: hashOf(c.Hash),
		})
	}
	return in
}

func TestModificationVectors(t *testing.T) {
	for i, v := range vectors.Load[modVector](t, "modification.json") {
		in := modInputOf(t, i, &v)
		got, err := Modify(in)
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
		want := v.Decision
		if got.Kind != want.Kind || !slices.Equal(got.Read, want.Read) || !slices.Equal(got.Changed, want.Changed) ||
			!slices.Equal(got.Stale, want.Stale) || got.Reference != want.Reference || string(got.Hash) != string(hashOf(want.Hash)) ||
			(got.Hash == nil) != (want.Hash == nil) {
			t.Errorf("vector %d (%s):\n got %+v\nwant %+v", i, v.Note, got, *want)
		}
	}
}

// A modification never makes stale a copy it calls changed, never leaves a copy it was not given, and never
// overwrites anything without a content that differs from what it overwrites, read or told by its size.
func TestModifyNeverOverwritesTheSame(t *testing.T) {
	verdicts := []CopyVerdict{VerdictUnknown, VerdictThere, VerdictEdited, VerdictReplaced, VerdictMoved, VerdictGone}
	states := []CopyState{CopyOK, CopySurplus, CopyStale, CopyUnavailable}
	hashes := [][]byte{nil, []byte("a"), []byte("b")}
	x := uint32(88172645)
	next := func(n int) int {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		return int(x % uint32(n))
	}
	modified := 0
	for range 200000 {
		in := &ModInput{State: StateOK, Size: int64(next(3)), Hash: hashes[next(3)], Running: next(10) == 0}
		for p := range next(4) + 1 {
			v := verdicts[next(len(verdicts))]
			in.Copies = append(in.Copies, ModCopy{
				Disk: string(rune('a' + p)), Position: p, Verdict: v, Other: v == VerdictReplaced && next(5) == 0,
				State: states[next(len(states))], Size: int64(next(3)), Due: next(5) != 0, Readable: next(5) != 0, Hash: hashes[next(3)],
			})
		}
		d, err := Modify(in)
		if err != nil {
			t.Fatal(err)
		}
		if d.Kind != ModModified {
			if len(d.Stale) > 0 {
				t.Fatalf("%+v: %+v", in, d)
			}
			continue
		}
		modified++
		changed := map[string]*ModCopy{}
		for i := range in.Copies {
			if slices.Contains(d.Changed, in.Copies[i].Disk) {
				changed[in.Copies[i].Disk] = &in.Copies[i]
			}
		}
		if len(changed) == 0 || len(d.Changed)+len(d.Stale) != len(in.Copies) {
			t.Fatalf("%+v: %+v", in, d)
		}
		for _, s := range d.Stale {
			if changed[s] != nil {
				t.Fatalf("%+v: %+v", in, d)
			}
		}
		for _, c := range changed {
			if !c.Due || c.State == CopyStale || (c.Verdict != VerdictEdited && c.Verdict != VerdictReplaced) || c.Other {
				t.Fatalf("%+v: %+v", in, d)
			}
			// Another size, or another content than the reference: what goes stale is never what stays.
			if c.Size == in.Size && (c.Hash == nil || (in.Hash != nil && string(c.Hash) == string(in.Hash))) {
				t.Fatalf("%+v: %+v", in, d)
			}
		}
	}
	if modified == 0 {
		t.Fatal("no case was modified")
	}
}
