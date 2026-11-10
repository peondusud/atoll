package solver

import (
	"fmt"
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/value"
	"github.com/peondusud/atoll/internal/vectors"
)

type lookVector struct {
	Inode *string `json:"inode"`
	Size  *int64  `json:"size"`
	Mtime *int64  `json:"mtime"`
	Ctime *int64  `json:"ctime"`
	Btime *int64  `json:"btime"`
}

func (v *lookVector) look(t *testing.T, i int) FileLook {
	t.Helper()
	if v.Inode == nil || v.Size == nil || v.Mtime == nil || v.Ctime == nil {
		t.Fatalf("vector %d: a look needs inode, size, mtime and ctime", i)
	}
	return FileLook{Inode: *v.Inode, Size: *v.Size, MtimeNS: *v.Mtime, CtimeNS: *v.Ctime, BtimeNS: v.Btime}
}

type verdictVector struct {
	NotReached bool        `json:"not_reached"`
	Own        bool        `json:"own"`
	Row        *lookVector `json:"row"`
	AtName     *lookVector `json:"at_name"`
	Candidates []struct {
		Name *string     `json:"name"`
		Look *lookVector `json:"look"`
	} `json:"candidates"`
	Verdict *CopyVerdict `json:"verdict"`
	Target  *string      `json:"target"`
	Note    string       `json:"note"`
}

func TestVerdictVectors(t *testing.T) {
	for i, v := range vectors.Load[verdictVector](t, "copy_verdict.json") {
		if v.Row == nil || v.Verdict == nil {
			t.Fatalf("vector %d: want row and verdict", i)
		}
		in := VerdictInput{NotReached: v.NotReached, Own: v.Own, Row: v.Row.look(t, i)}
		if v.AtName != nil {
			at := v.AtName.look(t, i)
			in.AtName = &at
		}
		for j := range v.Candidates {
			c := &v.Candidates[j]
			if c.Name == nil || c.Look == nil {
				t.Fatalf("vector %d: a candidate needs name and look", i)
			}
			in.Candidates = append(in.Candidates, NamedLook{Name: *c.Name, Look: c.Look.look(t, i)})
		}
		got, target := VerdictOf(&in)
		want := ""
		if v.Target != nil {
			want = *v.Target
		}
		if got != *v.Verdict || target != want {
			t.Errorf("vector %d (%s): %s %q, want %s %q", i, v.Note, got, target, *v.Verdict, want)
		}
	}
}

type propVector struct {
	Direct *string  `json:"direct"`
	Taken  []string `json:"taken"`
	Files  []struct {
		Path   *string `json:"path"`
		State  *State  `json:"state"`
		Follow string  `json:"follow"`
		Copies []struct {
			Disk      *string      `json:"disk"`
			Position  *int         `json:"position"`
			Verdict   *CopyVerdict `json:"verdict"`
			Target    string       `json:"target"`
			Due       *bool        `json:"due"`
			GoneNS    int64        `json:"gone_ns"`
			CreatedBy *string      `json:"created_by"`
			FirstSeen *int64       `json:"first_seen"`
			Verified  *bool        `json:"verified"`
			OldPath   *string      `json:"old_path"`
			GuardHeld bool         `json:"guard_held"`
			Size      int64        `json:"size"`
		} `json:"copies"`
	} `json:"files"`
	Decisions *[]struct {
		Path          string `json:"path"`
		Kind          string `json:"kind"`
		Target        string `json:"target"`
		Src           string `json:"src"`
		DropHash      bool   `json:"drop_hash"`
		ClearConflict bool   `json:"clear_conflict"`
		Copies        []struct {
			Disk   string `json:"disk"`
			Action string `json:"action"`
			Target string `json:"target"`
		} `json:"copies"`
	} `json:"decisions"`
	Error bool   `json:"error"`
	Note  string `json:"note"`
}

func propInputOf(t *testing.T, i int, v *propVector) *PropInput {
	t.Helper()
	if v.Direct == nil || v.Files == nil {
		t.Fatalf("vector %d: want direct and files", i)
	}
	in := &PropInput{Direct: *v.Direct, Taken: v.Taken}
	for j := range v.Files {
		f := &v.Files[j]
		if f.Path == nil || f.State == nil || f.Copies == nil {
			t.Fatalf("vector %d: a file needs path, state and copies", i)
		}
		file := PropFile{Path: *f.Path, State: *f.State, Follow: f.Follow}
		for k := range f.Copies {
			c := &f.Copies[k]
			if c.Disk == nil || c.Position == nil || c.Verdict == nil || c.Due == nil || c.CreatedBy == nil || c.FirstSeen == nil || c.Verified == nil {
				t.Fatalf("vector %d: file %d: copy %d: a field is missing", i, j, k)
			}
			file.Copies = append(file.Copies, PropCopy{
				Disk: *c.Disk, Position: *c.Position, Verdict: *c.Verdict, Target: c.Target, Due: *c.Due, GoneNS: c.GoneNS,
				CreatedBy: *c.CreatedBy, FirstSeenNS: *c.FirstSeen, Verified: *c.Verified, OldPath: c.OldPath, GuardHeld: c.GuardHeld, Size: c.Size,
			})
		}
		in.Files = append(in.Files, file)
	}
	return in
}

func TestPropagationVectors(t *testing.T) {
	for i, v := range vectors.Load[propVector](t, "propagation.json") {
		in := propInputOf(t, i, &v)
		if v.Error == (v.Decisions != nil) {
			t.Errorf("vector %d: want exactly one of decisions and error", i)
			continue
		}
		got, err := Decide(in)
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
		want := *v.Decisions
		if len(got) != len(want) {
			t.Errorf("vector %d (%s): %+v, want %+v", i, v.Note, got, want)
			continue
		}
		for j := range want {
			g, w := &got[j], &want[j]
			same := g.Path == w.Path && string(g.Kind) == w.Kind && g.Target == w.Target && g.Src == w.Src &&
				g.DropHash == w.DropHash && g.ClearConflict == w.ClearConflict && len(g.Copies) == len(w.Copies)
			if same {
				for k := range w.Copies {
					if g.Copies[k].Disk != w.Copies[k].Disk || string(g.Copies[k].Action) != w.Copies[k].Action || g.Copies[k].Target != w.Copies[k].Target {
						same = false
					}
				}
			}
			if !same {
				t.Errorf("vector %d (%s): decision %d: %+v, want %+v", i, v.Note, j, *g, *w)
			}
		}
	}
}

type guardVector struct {
	Count        *int64  `json:"count"`
	CountPercent *int64  `json:"count_percent"`
	Rows         *int64  `json:"rows"`
	Files        *int64  `json:"files"`
	Percent      *string `json:"percent"`
	Tripped      *bool   `json:"tripped"`
	Note         string  `json:"note"`
}

func TestGuardVectors(t *testing.T) {
	for i, v := range vectors.Load[guardVector](t, "guard.json") {
		if v.Count == nil || v.CountPercent == nil || v.Rows == nil || v.Files == nil || v.Percent == nil || v.Tripped == nil {
			t.Fatalf("vector %d: every field is required", i)
		}
		percent, err := value.ParsePercent(*v.Percent)
		if err != nil {
			t.Fatalf("vector %d: %v", i, err)
		}
		got := GuardTrips(&GuardInput{Count: *v.Count, CountPercent: *v.CountPercent, Rows: *v.Rows, Files: *v.Files, Percent: percent})
		if got != *v.Tripped {
			t.Errorf("vector %d (%s): %v, want %v", i, v.Note, got, *v.Tripped)
		}
	}
}

// Whatever a pool looks like, nothing goes to the trash on the strength of what is not known to be the
// same file, and a decision never leaves a copy nowhere.
func TestDecideNeverSendsAwayWhatMayBeTheFile(t *testing.T) {
	verdicts := []CopyVerdict{VerdictUnknown, VerdictThere, VerdictEdited, VerdictMoved, VerdictUncertain, VerdictReplaced, VerdictOwn, VerdictGone}
	states := []State{StateOK, StateConflict, StateCorrupt, StateDegraded}
	x := uint32(2463534242)
	next := func(n int) int {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		return int(x % uint32(n))
	}
	sent := 0
	for range 200000 {
		f := PropFile{Path: "p", State: states[next(2)*next(len(states))]}
		for p := range next(4) + 1 {
			by := CreatedByUser
			if next(3) == 0 {
				by = CreatedByAtoll
			}
			c := PropCopy{
				Disk: fmt.Sprintf("d%d", p), Position: p, Verdict: verdicts[next(len(verdicts))], Due: next(5) != 0,
				CreatedBy: by, FirstSeenNS: int64(next(3)), Verified: next(3) != 0, GoneNS: int64(next(3)), GuardHeld: next(6) == 0, Size: int64(next(2)),
			}
			if c.Verdict == VerdictMoved {
				c.Target = []string{"q", "r"}[next(2)]
			}
			if next(5) == 0 {
				old := "old"
				c.OldPath = &old
			}
			f.Copies = append(f.Copies, c)
		}
		in := &PropInput{Files: []PropFile{f}, Direct: []string{"propagate", "propagate", "restore"}[next(3)]}
		got, err := Decide(in)
		if err != nil {
			t.Fatal(err)
		}
		d := &got[0]
		if len(d.Copies) != len(f.Copies) {
			t.Fatalf("%+v: %+v: a copy has no action", f, d)
		}
		trash := slices.ContainsFunc(d.Copies, func(a PropCopyAction) bool { return a.Action == ActTrash })
		if !trash {
			continue
		}
		sent++
		checkSent(t, &f, d, in.Direct)
	}
	if sent == 0 {
		t.Fatal("nothing ever went to the trash: this checked nothing")
	}
}

func checkSent(t *testing.T, f *PropFile, d *PropDecision, direct string) {
	t.Helper()
	if d.Kind != PropDeletion || direct != "propagate" || f.State == StateConflict || f.State == StateCorrupt {
		t.Fatalf("%+v: %+v: to the trash, with %s", f, d, direct)
	}
	earliest, gone := int64(0), 0
	for i := range f.Copies {
		c := &f.Copies[i]
		if !goneLike(c.Verdict) {
			continue
		}
		if c.Verdict != VerdictGone || c.OldPath != nil || !c.Due || c.GuardHeld {
			t.Fatalf("%+v: %+v: to the trash, as %+v went", f, d, *c)
		}
		if gone == 0 || c.FirstSeenNS < earliest {
			earliest = c.FirstSeenNS
		}
		gone++
	}
	if gone == 0 {
		t.Fatalf("%+v: %+v: to the trash, with nothing gone", f, d)
	}
	for i := range f.Copies {
		c := &f.Copies[i]
		if goneLike(c.Verdict) {
			continue
		}
		if c.Verdict != VerdictThere && c.Verdict != VerdictUnknown {
			t.Fatalf("%+v: %+v: %+v goes", f, d, *c)
		}
		if c.CreatedBy == CreatedByUser && (!c.Verified || c.FirstSeenNS > earliest) {
			t.Fatalf("%+v: %+v: %+v may be where the file went", f, d, *c)
		}
	}
}
