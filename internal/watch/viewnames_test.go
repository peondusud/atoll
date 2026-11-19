package watch

import (
	"reflect"
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

func TestViewNamesVectors(t *testing.T) {
	type op struct {
		Kind    string   `json:"kind"`
		Disk    string   `json:"disk"`
		Path    string   `json:"path"`
		Path2   string   `json:"path2"`
		Folder  bool     `json:"folder"`
		Holders []string `json:"holders"`
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
		Note     string `json:"note"`
		MaxNames int    `json:"max_names"`
		Ops      []op   `json:"ops"`
		Disks    []disk `json:"disks"`
	}
	for _, v := range vectors.Load[vector](t, "view_names.json") {
		ops := make([]ViewOp, 0, len(v.Ops))
		for _, o := range v.Ops {
			ops = append(ops, ViewOp(o))
		}
		got := ViewNames(ops, v.MaxNames)
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
