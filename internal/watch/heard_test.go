package watch

import (
	"reflect"
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

func TestHeardNamesVectors(t *testing.T) {
	type group struct {
		Disk    string   `json:"disk"`
		Path    string   `json:"path"`
		Folder  bool     `json:"folder"`
		Kinds   []string `json:"kinds"`
		Origins []string `json:"origins"`
	}
	type name struct {
		Path    string `json:"path"`
		Folder  bool   `json:"folder"`
		Deleted bool   `json:"deleted"`
	}
	type disk struct {
		Disk  string `json:"disk"`
		Whole bool   `json:"whole"`
		Names []name `json:"names"`
	}
	type vector struct {
		Note     string  `json:"note"`
		MaxNames int     `json:"max_names"`
		Groups   []group `json:"groups"`
		Disks    []disk  `json:"disks"`
	}
	for _, v := range vectors.Load[vector](t, "heard_names.json") {
		ripe := make([]Group, 0, len(v.Groups))
		for _, g := range v.Groups {
			ripe = append(ripe, Group{Disk: g.Disk, Path: g.Path, Folder: g.Folder, Kinds: g.Kinds, Origins: g.Origins})
		}
		got := HeardNames(ripe, v.MaxNames)
		want := map[string]Heard{}
		for _, d := range v.Disks {
			h := Heard{Whole: d.Whole}
			for _, n := range d.Names {
				h.Names = append(h.Names, Named(n))
			}
			want[d.Disk] = h
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %+v, want %+v", v.Note, got, want)
		}
	}
}

// What a pass has not taken yet adds up turn after turn: a disk walked whole stays so, and its names grow, each once.
func TestHeardAddsUp(t *testing.T) {
	h := map[string]Heard{}
	AddHeard(h, map[string]Heard{"t1": {Names: []Named{{Path: "b"}}}, "t2": {Names: []Named{{Path: "c"}}}}, 3)
	AddHeard(h, map[string]Heard{"t1": {Names: []Named{{Path: "a", Deleted: true}, {Path: "b"}}}, "t2": {Whole: true}}, 3)
	if want := []Named{{Path: "a", Deleted: true}, {Path: "b"}}; !slices.Equal(h["t1"].Names, want) || h["t1"].Whole {
		t.Errorf("t1 %+v", h["t1"])
	}
	if !h["t2"].Whole || h["t2"].Names != nil {
		t.Errorf("t2 %+v", h["t2"])
	}
	AddHeard(h, map[string]Heard{"t2": {Names: []Named{{Path: "x"}}}}, 3)
	if !h["t2"].Whole || h["t2"].Names != nil {
		t.Errorf("t2, whole, then given a name: %+v", h["t2"])
	}
	AddHeard(h, map[string]Heard{"t1": {Names: []Named{{Path: "c"}, {Path: "d"}}}}, 3)
	if !h["t1"].Whole || h["t1"].Names != nil {
		t.Errorf("t1 past 3 names %+v", h["t1"])
	}
}
