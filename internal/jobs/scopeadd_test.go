package jobs

import (
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/watch"
)

// The names a pass looks at on a disk add up, and a disk walked whole stays so, whatever names come after (specs.md
// §15.13).
func TestTheNamesOfAScopeAddUp(t *testing.T) {
	s := Scope{}
	s.Add("t1", []watch.Named{{Path: "a"}})
	s.Add("t1", []watch.Named{{Path: "b", Deleted: true}})
	if want := []watch.Named{{Path: "a"}, {Path: "b", Deleted: true}}; !slices.Equal(s["t1"].Names, want) {
		t.Errorf("t1: %+v", s["t1"])
	}
	s["t2"] = Whole
	s.Add("t2", []watch.Named{{Path: "c"}})
	if s["t2"].Names != nil {
		t.Errorf("t2, walked whole, then given a name: %+v", s["t2"])
	}
	s.Add("t3", nil)
	if got, ok := s["t3"]; !ok || got.Names == nil || len(got.Names) != 0 {
		t.Errorf("t3, given no name, is no walk of the whole disk: %+v, %v", got, ok)
	}
}

// The renames left planned on a disk give their old and new names, and those of no other kind or disk.
func TestTheRenamesLeftPlannedOnADisk(t *testing.T) {
	list := []catalog.Op{
		{Disk: "t1", Kind: catalog.OpRename, Path: "a", Path2: "b"},
		{Disk: "t2", Kind: catalog.OpRename, Path: "c", Path2: "d"},
		{Disk: "t1", Kind: catalog.OpTrash, Path: "e", Path2: "trash/e"},
		{Disk: "t1", Kind: catalog.OpRename, Path: "f", Path2: "g"},
	}
	if got := renamesOn(list, "t1"); !slices.Equal(got, []string{"a", "b", "f", "g"}) {
		t.Errorf("t1: %q", got)
	}
	if got := renamesOn(list, "t3"); got != nil {
		t.Errorf("t3: %q", got)
	}
}

// Only ext4, XFS and btrfs exchange two names: every driver of NTFS refuses the flag.
func TestWhichFilesystemsExchangeNames(t *testing.T) {
	for fs, want := range map[names.Filesystem]bool{names.Ext4: true, names.XFS: true, names.Btrfs: true, names.NTFS: false} {
		if got := canExchange(fs); got != want {
			t.Errorf("%s: %v", fs, got)
		}
	}
}

// A target differs from where a copy sits only as a disk with collision keys reads one name, at a folder on the way,
// when the first folder that differs differs by case alone; a file's own name, or a folder of another name, is no such
// difference, nor are two names that are not UTF-8 (specs.md §15.7, §15.12).
func TestATargetInOneFolder(t *testing.T) {
	for _, tc := range []struct {
		place, target string
		want          bool
	}{
		{"A/x", "a/x", true},
		{"a/b/x", "a/B/x", true},
		{"a/x", "a/X", false},
		{"x", "X", false},
		{"a/x", "b/x", false},
		{"a/b/x", "a/c/x", false},
		{"\xff/x", "\xfe/x", false},
	} {
		if got := inOneFolder(tc.place, tc.target); got != tc.want {
			t.Errorf("%q to %q: %v", tc.place, tc.target, got)
		}
	}
}

// A slice of reads that starts at its time reads nothing, and leaves every file to the next (specs.md §15.13).
func TestASliceOfReadsAtItsTimeReadsNothing(t *testing.T) {
	b := touched(t)
	out := b.passLater()
	if len(out.ToRead) != 1 {
		t.Fatalf("%+v", out)
	}
	reads := b.countReads()
	slice, left := b.readChanged(out.ToRead, b.now)
	if len(left) != 1 || *reads != 0 || len(slice.Decisions)+len(slice.Operations) != 0 {
		t.Errorf("%+v, %d left, %d reads", slice, len(left), *reads)
	}
}
