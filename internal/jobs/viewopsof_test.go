package jobs

import (
	"database/sql"
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/watch"
)

// An operation of the view touches a path at, below or above either of its names, and never beside them; a later
// operation at, below or above either name of an earlier one makes that one not the last there, by either of its own
// names (specs.md §21.2).
func TestWhatAViewOperationTouches(t *testing.T) {
	rename := &catalog.ViewOp{ID: 5, Kind: catalog.ViewRename, Path: "d/a", Path2: "e/a"}
	for path, want := range map[string]bool{"d/a": true, "d/a/x": true, "d": true, "e/a": true, "e": true, "d/b": false, "d/ab": false, "f": false} {
		if got := touchesEither(rename, path); got != want {
			t.Errorf("%s: %v", path, got)
		}
	}
	if touchesEither(&catalog.ViewOp{Path: "d/a"}, "x") {
		t.Error("a name the operation has not touches every path")
	}
	for _, tc := range []struct {
		name  string
		other catalog.ViewOp
		want  bool
	}{
		{"later, at its old name", catalog.ViewOp{ID: 6, Path: "d/a"}, true},
		{"later, below its new name", catalog.ViewOp{ID: 6, Path: "e/a/x"}, true},
		{"later, renamed to its old name", catalog.ViewOp{ID: 6, Path: "g", Path2: "d/a"}, true},
		{"later, beside it", catalog.ViewOp{ID: 6, Path: "d/b", Path2: "g"}, false},
		{"earlier, at its old name", catalog.ViewOp{ID: 4, Path: "d/a"}, false},
	} {
		p := &pass{viewOps: []catalog.ViewOp{*rename, tc.other}}
		if got := p.touchedLater(rename); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
}

// The view's operations still under way on a disk give their names there, a move's on the disk it goes to and on the
// disk it leaves; one done, a part, or one of another disk gives none (specs.md §21.2).
func TestTheViewsOperationsUnderWayOnADisk(t *testing.T) {
	p := &pass{viewOps: []catalog.ViewOp{
		{Kind: catalog.ViewCreate, State: catalog.OpPlanned, Disk: "t1", Path: "a"},
		{Kind: catalog.ViewRename, State: catalog.OpPlanned, Disk: "t1", Path: "b", Path2: "c"},
		{Kind: catalog.ViewCreate, State: catalog.OpDone, Disk: "t1", Path: "d"},
		{Kind: catalog.ViewPart, State: catalog.OpPlanned, Disk: "t1", Path: "e"},
		{Kind: catalog.ViewCreate, State: catalog.OpPlanned, Disk: "t2", Path: "f"},
		{Kind: catalog.ViewMove, State: catalog.OpPlanned, Disk: "t2", Disk2: "t1", Path: "g"},
		{Kind: catalog.ViewRename, State: catalog.OpPlanned, Disk: "t2", Disk2: "t1", Path: "h", Path2: "i"},
	}}
	if got := p.viewPlannedOn("t1"); !slices.Equal(got, []string{"a", "b", "c", "g"}) {
		t.Errorf("t1: %q", got)
	}
	if got := p.viewPlannedOn("t2"); !slices.Equal(got, []string{"f", "g", "h", "i"}) {
		t.Errorf("t2: %q", got)
	}
}

// A file the view deleted, or replaced by a rename over it, journaled to the trash and not taken in yet, is deleted at
// its path, and only there (specs.md §21.2).
func TestWhatTheViewDeleted(t *testing.T) {
	p := &pass{viewOps: []catalog.ViewOp{
		{Kind: catalog.ViewUnlink, Path: "a", OpID: 3, FileKey: "7"},
		{Kind: catalog.ViewReplace, Path: "b.tmp", Path2: "b", OpID: 4, OpKind: catalog.OpTrash, OpFileKey: "8"},
		{Kind: catalog.ViewUnlink, Path: "c"},
	}}
	for path, want := range map[string]bool{"a": true, "b": true, "b.tmp": false, "c": false, "d": false} {
		if got := p.deletedThroughView(path); got != want {
			t.Errorf("%s: %v", path, got)
		}
	}
}

// A copy's rename waits while it would take the copy from below the new name of a folder the view renamed to below its
// old name; any other stands (specs.md §15.12; conformance/models/README.md, observation 5). A disk is the pool's when
// the configuration lists it.
func TestWhatAFolderRenamedThroughTheViewKeeps(t *testing.T) {
	p := &pass{kept: []watch.FolderMove{{From: "d", To: "e"}}, config: &config.Config{Disks: []config.Disk{{ID: "t1"}, {ID: "t2"}}}}
	for _, tc := range []struct {
		place, target string
		want          bool
	}{
		{"e/a", "d/a", true},
		{"e", "d", true},
		{"e/a", "f/a", false},
		{"f/a", "d/a", false},
		{"d/a", "e/a", false},
	} {
		if got := p.viewKeeps(tc.place, tc.target); got != tc.want {
			t.Errorf("%s to %s: %v", tc.place, tc.target, got)
		}
	}
	for disk, want := range map[string]bool{"t1": true, "t2": true, "t3": false} {
		if got := p.configured(disk); got != want {
			t.Errorf("%s: %v", disk, got)
		}
	}
}

// A folder that only moved with a folder above it, the same name below both, is no rename of its own; one that went
// elsewhere, or took another name, is (specs.md §15.12).
func TestTheTopMovesOfALook(t *testing.T) {
	list := []watch.FolderMove{{From: "d", To: "e"}, {From: "d/x", To: "e/x"}, {From: "d/y", To: "f/y"}, {From: "d/z", To: "e/w"}, {From: "g", To: "h"}}
	want := []watch.FolderMove{{From: "d", To: "e"}, {From: "d/y", To: "f/y"}, {From: "d/z", To: "e/w"}, {From: "g", To: "h"}}
	if got := topMoves(list); !slices.Equal(got, want) {
		t.Errorf("%+v", got)
	}
	if len(list) != 5 {
		t.Error("the list given changed")
	}
}

// A file's row holds the change time a look found of a changed copy when the row of its copy on that disk says it, and
// never for a copy replaced by what is no regular file.
func TestARowHoldsTheChangeTimeALookFound(t *testing.T) {
	valid := func(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: true} }
	f := &catalog.FileRow{Copies: []catalog.CopyRow{{Disk: "t1", CtimeNS: valid(5)}, {Disk: "t2"}}}
	for _, tc := range []struct {
		name string
		f    *catalog.FileRow
		disk string
		seen watch.CopySeen
		want bool
	}{
		{"edited, its time", f, "t1", watch.CopySeen{Verdict: solver.VerdictEdited, CtimeNS: 5}, true},
		{"replaced by a file, its time", f, "t1", watch.CopySeen{Verdict: solver.VerdictReplaced, Regular: true, CtimeNS: 5}, true},
		{"replaced by a folder", f, "t1", watch.CopySeen{Verdict: solver.VerdictReplaced, CtimeNS: 5}, false},
		{"another time", f, "t1", watch.CopySeen{Verdict: solver.VerdictEdited, CtimeNS: 6}, false},
		{"a row with no time", f, "t2", watch.CopySeen{Verdict: solver.VerdictEdited}, false},
		{"no copy on that disk", f, "t3", watch.CopySeen{Verdict: solver.VerdictEdited, CtimeNS: 5}, false},
		{"no file", nil, "t1", watch.CopySeen{Verdict: solver.VerdictEdited, CtimeNS: 5}, false},
	} {
		if got := holdsSeen(tc.f, tc.disk, &tc.seen); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
}
