package view

import (
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

type resolveOpVector struct {
	Kind    string `json:"kind"`
	Disk    string `json:"disk"`
	Path    string `json:"path"`
	Path2   string `json:"path2"`
	Folder  bool   `json:"folder"`
	Disk2   string `json:"disk2"`
	Planned bool   `json:"planned"`
}

type resolveVector struct {
	Ops    []resolveOpVector `json:"ops"`
	Path   *string           `json:"path"`
	Source *struct {
		Kind  SourceKind `json:"kind"`
		Disk  string     `json:"disk"`
		Paths []string   `json:"paths"`
	} `json:"source"`
	Dir         *string  `json:"dir"`
	CatalogDirs []string `json:"catalog_dirs"`
	Names       []string `json:"names"`
	Note        string   `json:"note"`
}

func TestResolveVectors(t *testing.T) {
	for i, v := range vectors.Load[resolveVector](t, "view_resolve.json") {
		ops := make([]Op, 0, len(v.Ops))
		for _, o := range v.Ops {
			ops = append(ops, Op(o))
		}
		switch {
		case v.Path != nil:
			got := Resolve(ops, *v.Path)
			if got.Kind != v.Source.Kind || got.Disk != v.Source.Disk || !slices.Equal(got.Paths, v.Source.Paths) {
				t.Errorf("vector %d (%s): %+v, want %+v", i, v.Note, got, *v.Source)
			}
		case v.Dir != nil:
			got := ListOf(ops, *v.Dir)
			if !slices.Equal(got.CatalogDirs, v.CatalogDirs) || !slices.Equal(got.Names, v.Names) {
				t.Errorf("vector %d (%s): %+v, want %v and %v", i, v.Note, got, v.CatalogDirs, v.Names)
			}
		default:
			t.Errorf("vector %d (%s): neither a path nor a folder", i, v.Note)
		}
	}
}
