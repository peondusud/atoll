package solver

import (
	"reflect"
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

type linksVector struct {
	Note  string `json:"note"`
	On    *bool  `json:"on"`
	Links []struct {
		Name  string `json:"name"`
		File  bool   `json:"file"`
		Inode string `json:"inode"`
	} `json:"links"`
	Copies []struct {
		File  int64  `json:"file"`
		Inode string `json:"inode"`
		Place string `json:"place"`
		There bool   `json:"there"`
	} `json:"copies"`
	Trash []string    `json:"trash"`
	Files []linksPath `json:"files"`
	Gone  []linksPath `json:"copy_gone"`
	Steps *[]linksOut `json:"steps"`
}

type linksPath struct {
	File int64  `json:"file"`
	Path string `json:"path"`
}

type linksOut struct {
	Do     LinkAct `json:"do"`
	Name   string  `json:"name,omitempty"`
	File   int64   `json:"file,omitempty"`
	Inode  string  `json:"inode,omitempty"`
	Path   string  `json:"path,omitempty"`
	Reason string  `json:"reason,omitempty"`
}

// outOf gives what a vector says of a step: the name of a link, with the path and reason of a trash, and the file,
// inode and path of a make.
func outOf(s *LinkStep) linksOut {
	switch s.Do {
	case LinkMake:
		return linksOut{Do: s.Do, File: s.FileID, Inode: s.Inode, Path: s.Path}
	case LinkTrash:
		return linksOut{Do: s.Do, Name: s.Name, Path: s.Path, Reason: s.Reason}
	case LinkKeep, LinkUnlink, LinkUnnamed, LinkStray:
	}
	return linksOut{Do: s.Do, Name: s.Name}
}

func TestLinkGuardVectors(t *testing.T) {
	for i, v := range vectors.Load[linksVector](t, "link_guard.json") {
		if v.On == nil || v.Steps == nil {
			t.Errorf("vector %d: want on and steps", i)
			continue
		}
		in := &LinksInput{On: *v.On, Trash: map[string]bool{}, Files: map[int64]string{}, Gone: map[int64]string{}}
		for _, l := range v.Links {
			in.Links = append(in.Links, LinkEntry{Name: l.Name, File: l.File, Inode: l.Inode})
		}
		for _, c := range v.Copies {
			in.Copies = append(in.Copies, LinkCopy{FileID: c.File, Inode: c.Inode, Place: c.Place, There: c.There})
		}
		for _, k := range v.Trash {
			in.Trash[k] = true
		}
		for _, f := range v.Files {
			in.Files[f.File] = f.Path
		}
		for _, f := range v.Gone {
			in.Gone[f.File] = f.Path
		}
		got := []linksOut{}
		for _, s := range PlanLinks(in) {
			got = append(got, outOf(&s))
		}
		if !reflect.DeepEqual(got, *v.Steps) {
			t.Errorf("vector %d (%s):\n got %+v\nwant %+v", i, v.Note, got, *v.Steps)
		}
	}
}

// TestPlanLinksLeavesItsInputAlone: the plan sorts copies of what it is given.
func TestPlanLinksLeavesItsInputAlone(t *testing.T) {
	in := &LinksInput{On: true, Links: []LinkEntry{{Name: "9", File: true, Inode: "1"}, {Name: "10", File: true, Inode: "2"}},
		Copies: []LinkCopy{{FileID: 9, Inode: "1", There: true}, {FileID: 3, Inode: "4", There: true}}}
	PlanLinks(in)
	if in.Links[0].Name != "9" || in.Copies[0].FileID != 9 {
		t.Errorf("the input was sorted: %+v %+v", in.Links, in.Copies)
	}
}
