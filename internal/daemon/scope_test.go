package daemon

import (
	"fmt"
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/watch"
)

// What a pass for events looks at (specs.md §15.13): the names the watcher heard on each disk with the names the last
// pass left to see again; the whole disk for a group at its root, a disk whose look left a copy unexplained, one whose
// watch may have missed a change, and one with more names than a look by names is worth. A pass for any other reason
// walks every disk.
func TestTheScopeOfAPassForEvents(t *testing.T) {
	c := bench(t, "[[disk]]\nid = \"t2\"\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c40\"\nmount = \"/mnt/t2\"\nfs = \"ext4\"\n"+
		"[[disk]]\nid = \"t3\"\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c41\"\nmount = \"/mnt/t3\"\nfs = \"ext4\"\n"+
		"[[disk]]\nid = \"t4\"\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c42\"\nmount = \"/mnt/t4\"\nfs = \"ext4\"\n")
	heard := func() *diskState { return &diskState{state: disks.Online, watched: true, walked: true} }
	l := &loop{opt: &Options{Config: c, Apply: true}, disks: map[string]*diskState{"t1": heard(), "t2": heard(), "t3": heard(), "t4": heard()}}
	l.heard = map[string]watch.Heard{"t1": {Names: []watch.Named{{Path: "a", Deleted: true}}}, "t2": {Whole: true}}
	l.again = map[string][]watch.Named{"t1": {{Path: "b"}}, "t3": {{Path: "c"}}}
	scope := l.scope([]string{BecauseEvents, BecauseDue})
	if got := scope["t1"].Names; !slices.Equal(got, []watch.Named{{Path: "a", Deleted: true}, {Path: "b"}}) {
		t.Errorf("t1 %+v", got)
	}
	if looking, ok := scope["t2"]; !ok || looking.Names != nil {
		t.Errorf("t2, at its root: %+v, %v", looking, ok)
	}
	if got := scope["t3"].Names; !slices.Equal(got, []watch.Named{{Path: "c"}}) {
		t.Errorf("t3 %+v", got)
	}
	if _, ok := scope["t4"]; ok {
		t.Errorf("t4, heard of nothing, is in the scope: %+v", scope["t4"])
	}
	l.waitDisks = []string{"t3"}
	l.disks["t4"].unheard = true
	scope = l.scope([]string{BecauseEvents})
	if looking, ok := scope["t3"]; !ok || looking.Names != nil {
		t.Errorf("t3, left to a walk: %+v", looking)
	}
	if looking, ok := scope["t4"]; !ok || looking.Names != nil {
		t.Errorf("t4, unheard: %+v", looking)
	}
	l.waitDisks, l.disks["t4"].unheard = nil, false
	many := make([]watch.Named, 0, watch.MaxNames+1)
	for i := range watch.MaxNames + 1 {
		many = append(many, watch.Named{Path: fmt.Sprintf("x%05d", i)})
	}
	l.heard = map[string]watch.Heard{}
	l.again = map[string][]watch.Named{"t1": many}
	if looking := l.scope([]string{BecauseDue})["t1"]; looking.Names != nil {
		t.Errorf("t1 with %d names: %d names", len(many), len(looking.Names))
	}
	if scope := l.scope([]string{BecauseEvents, BecauseReload}); scope != nil {
		t.Errorf("a reload walks every disk: %+v", scope)
	}
}
