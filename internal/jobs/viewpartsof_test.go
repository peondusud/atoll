package jobs

import (
	"maps"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
)

// The parts the view handed over are those of its operations part done with, which wait for no settle; any other
// operation not taken in holds a path at either of its names, and a part holds none (specs.md §21.2).
func TestThePartsTheViewHandedOverAndWhatItHolds(t *testing.T) {
	p := &pass{viewOps: []catalog.ViewOp{
		{Kind: catalog.ViewPart, State: catalog.OpDone, Path: "a"},
		{Kind: catalog.ViewPart, State: catalog.OpPlanned, Path: "b"},
		{Kind: catalog.ViewCreate, State: catalog.OpDone, Path: "c"},
		{Kind: catalog.ViewRename, State: catalog.OpDone, Path: "d", Path2: "e"},
	}}
	if got := p.partPaths(); !maps.Equal(got, map[string]bool{"a": true}) {
		t.Errorf("parts %v", got)
	}
	for path, want := range map[string]bool{"a": false, "b": false, "c": true, "d": true, "e": true, "f": false} {
		if got := p.viewOpenAt(path); got != want {
			t.Errorf("%s: %v", path, got)
		}
	}
}
