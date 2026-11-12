package jobs

import (
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
)

// TestPlannedOnLeavesTheGuardOut: an operation of the link guard left planned leaves the root as it is, so that the
// copy at its path is looked at as any other, while a trash of a copy left planned holds its path (specs.md §15.12).
func TestPlannedOnLeavesTheGuardOut(t *testing.T) {
	list := []catalog.Op{
		{Disk: "t1", Kind: catalog.OpTrash, Path: "a", Link: 7},
		{Disk: "t1", Kind: catalog.OpLink, Path: "c", Link: 9},
		{Disk: "t1", Kind: catalog.OpUnlink, Path: "d", Link: 9},
		{Disk: "t1", Kind: catalog.OpTrash, Path: "b"},
		{Disk: "t2", Kind: catalog.OpTrash, Path: "e"},
	}
	if got := plannedOn(list, "t1"); !slices.Equal(got, []string{"b"}) {
		t.Errorf("%q", got)
	}
}
