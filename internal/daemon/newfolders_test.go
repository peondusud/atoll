package daemon

import (
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/watch"
)

// The folders where a pass's walks found new copies are told to the media servers with those of its operations, from
// the disks whose watch may have missed them since their last walk. On a disk watched all along, what the watcher heard
// was told already: a file written through the view is the view's, which a media server's own watch sees, and Plex
// was asked for its folder all the same (specs.md §21.5).
func TestAPassTellsTheFoldersOfNewCopies(t *testing.T) {
	s := &passSink{folders: map[string]bool{}, unheard: map[string]bool{"t1": true, "t2": true}}
	if err := s.Disks([]jobs.DiskLine{{ID: "t1", NewFolders: []string{"Films", ""}}, {ID: "t2", NewFolders: []string{"Films", "Shows/S1"}},
		{ID: "t3", NewFolders: []string{"Music"}}}); err != nil {
		t.Fatal(err)
	}
	if got := sortedKeys(s.folders); !slices.Equal(got, []string{"", "Films", "Shows/S1"}) {
		t.Errorf("%q", got)
	}
}

// A disk is unheard from the start of the daemon, from when its watch stops, and after an overflow of its watch, which
// a rearm gives too; a walk while it is watched hears it again, and one while it is not leaves it unheard, as what
// changes before its watch begins is heard by nobody.
func TestUnheardDisks(t *testing.T) {
	grouping, err := watch.NewGrouping(1)
	if err != nil {
		t.Fatal(err)
	}
	l := &loop{grouping: grouping, closed: jobs.Closed{}, changed: map[string]bool{},
		watcher: watch.NewWatcher(watch.WatcherOptions{Now: func() int64 { return 0 }}),
		disks:   map[string]*diskState{"t1": newDiskState(disks.Online), "t2": newDiskState(disks.Online)}}
	defer l.watcher.Close()
	if !l.disks["t1"].unheard || !l.disks["t2"].unheard {
		t.Fatal("a disk heard before any walk")
	}
	l.disks["t1"].watched = true
	l.walked("t1")
	l.walked("t2")
	if l.disks["t1"].unheard || !l.disks["t2"].unheard {
		t.Errorf("after a walk: t1 %v, t2 %v", l.disks["t1"].unheard, l.disks["t2"].unheard)
	}
	// An overflow of t1's watch: nothing says what it lost.
	if err := l.grouping.Add(watch.Event{Disk: "t1", Kind: watch.KindOverflow, Origin: watch.OriginUnknown, AtNS: 10}); err != nil {
		t.Fatal(err)
	}
	l.takeRipe(100)
	if !l.disks["t1"].unheard {
		t.Error("heard after an overflow")
	}
	l.walked("t1")
	l.unwatch("t1")
	if !l.disks["t1"].unheard {
		t.Error("heard once its watch stopped")
	}
}
