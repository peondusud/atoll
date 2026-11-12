package watch

import (
	"encoding/base64"
	"slices"
	"testing"
	"unicode/utf8"

	"github.com/peondusud/atoll/internal/vectors"
)

type vectorEvent struct {
	Disk    string  `json:"disk"`
	Path    *string `json:"path"`
	PathB64 *string `json:"path_b64"`
	Folder  *bool   `json:"folder"`
	Kind    string  `json:"kind"`
	Origin  string  `json:"origin"`
	AtNS    int64   `json:"at_ns"`
}

type vectorGroup struct {
	Disk    string   `json:"disk"`
	Path    *string  `json:"path"`
	PathB64 *string  `json:"path_b64"`
	Folder  *bool    `json:"folder"`
	Kinds   []string `json:"kinds"`
	Origins []string `json:"origins"`
	FirstNS int64    `json:"first_ns"`
	LastNS  int64    `json:"last_ns"`
}

// flag reads a flag of the vector format: true or left out (conformance/vectors/README.md).
func flag(t *testing.T, i int, f *bool) bool {
	t.Helper()
	if f != nil && !*f {
		t.Fatalf("case %d: folder is false: write true, or leave it out", i)
	}
	return f != nil
}

func TestEventGroupingVectors(t *testing.T) {
	type vector struct {
		DebounceNS int64         `json:"debounce_ns"`
		Events     []vectorEvent `json:"events"`
		NowNS      int64         `json:"now_ns"`
		Ripe       []vectorGroup `json:"ripe"`
		Waiting    int           `json:"waiting"`
		NextDueNS  *int64        `json:"next_due_ns"`
		Error      bool          `json:"error"`
		Note       string        `json:"note"`
	}
	for i, v := range vectors.Load[vector](t, "event_grouping.json") {
		g, err := NewGrouping(v.DebounceNS)
		for _, e := range v.Events {
			if err != nil {
				break
			}
			path, perr := vectors.Path(e.Path, e.PathB64)
			if perr != nil {
				t.Fatalf("case %d: %v", i, perr)
			}
			err = g.Add(Event{Disk: e.Disk, Path: path, Folder: flag(t, i, e.Folder), Kind: e.Kind, Origin: e.Origin, AtNS: e.AtNS})
		}
		if v.Error {
			if err == nil {
				t.Errorf("case %d (%s): want an error", i, v.Note)
			}
			continue
		}
		if err != nil {
			t.Errorf("case %d (%s): %v", i, v.Note, err)
			continue
		}
		var want []Group
		for _, r := range v.Ripe {
			path, perr := vectors.Path(r.Path, r.PathB64)
			if perr != nil {
				t.Fatalf("case %d: %v", i, perr)
			}
			want = append(want, Group{Disk: r.Disk, Path: path, Folder: flag(t, i, r.Folder), Kinds: r.Kinds, Origins: r.Origins, FirstNS: r.FirstNS, LastNS: r.LastNS})
		}
		ripe, waiting, next, ok := g.Ripe(v.NowNS)
		if !slices.EqualFunc(ripe, want, sameGroup) {
			t.Errorf("case %d (%s): ripe %s, want %s", i, v.Note, groupsText(ripe), groupsText(want))
		}
		if waiting != v.Waiting || ok != (v.NextDueNS != nil) || ok && next != *v.NextDueNS || g.Len() != waiting {
			t.Errorf("case %d (%s): %d waiting (%d left), next %d (%v)", i, v.Note, waiting, g.Len(), next, ok)
		}
	}
}

func sameGroup(a, b Group) bool {
	return a.Disk == b.Disk && a.Path == b.Path && a.Folder == b.Folder && slices.Equal(a.Kinds, b.Kinds) &&
		slices.Equal(a.Origins, b.Origins) && a.FirstNS == b.FirstNS && a.LastNS == b.LastNS
}

func groupsText(list []Group) string {
	out := "["
	for _, g := range list {
		path := g.Path
		if !utf8.ValidString(path) {
			path = "b64:" + base64.StdEncoding.EncodeToString([]byte(path))
		}
		out += " " + g.Disk + ":" + path
	}
	return out + " ]"
}

// A group that ripens leaves the grouping, and a later event at its path starts a new one.
func TestARipeGroupLeaves(t *testing.T) {
	g, err := NewGrouping(10)
	if err != nil {
		t.Fatal(err)
	}
	add := func(at int64) {
		if err := g.Add(Event{Disk: "t1", Path: "a", Kind: KindWritten, Origin: OriginUser, AtNS: at}); err != nil {
			t.Fatal(err)
		}
	}
	add(0)
	if ripe, _, _, _ := g.Ripe(10); len(ripe) != 1 {
		t.Fatalf("%d ripe", len(ripe))
	}
	add(15)
	ripe, waiting, next, ok := g.Ripe(20)
	if len(ripe) != 0 || waiting != 1 || !ok || next != 25 {
		t.Errorf("%d ripe, %d waiting, next %d %v", len(ripe), waiting, next, ok)
	}
	if ripe, _, _, _ := g.Ripe(25); len(ripe) != 1 || ripe[0].FirstNS != 15 {
		t.Errorf("again: %+v", ripe)
	}
}
