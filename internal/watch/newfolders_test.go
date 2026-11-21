package watch

import (
	"slices"
	"testing"
)

// A walk says in which folders it found copies new to the catalog, each once, "" for the top of
// the root: the daemon tells the media servers of them, as no rename or deletion does of a file written straight to a
// disk (specs.md §21.5). A walk that finds nothing new names none.
func TestAWalkNamesTheFoldersOfItsNewCopies(t *testing.T) {
	b := newBench(t, "t1")
	b.write("t1", "Films/b.mkv", "x")
	b.write("t1", "Films/a.mkv", "x")
	b.write("t1", "Shows/S1/e1.mkv", "x")
	b.write("t1", "top.mkv", "x")
	out := b.scan(Options{})
	if got := slices.Sorted(slices.Values(out.Disks[0].NewFolders)); !slices.Equal(got, []string{"", "Films", "Shows/S1"}) {
		t.Errorf("%q", got)
	}
	b.write("t1", "Films/c.mkv", "x")
	if got := b.scan(Options{}).Disks[0].NewFolders; !slices.Equal(got, []string{"Films"}) {
		t.Errorf("a second walk: %q", got)
	}
	if got := b.scan(Options{}).Disks[0].NewFolders; len(got) != 0 {
		t.Errorf("nothing new: %q", got)
	}
}
