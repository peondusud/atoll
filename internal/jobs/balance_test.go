package jobs

import (
	"database/sql"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
)

// An old copy goes to the trash only when the plan marked it surplus, and only an Atoll copy: a copy the plan keeps,
// or a user copy, stays, and nothing is asked of the trash for it (specs.md §13.3).
func TestRetireKeepsWhatThePlanKeeps(t *testing.T) {
	b := &balancer{}
	for _, c := range []catalog.CopyRow{
		{Disk: "t2", CreatedBy: "atoll", State: "ok"},
		{Disk: "t2", CreatedBy: "user", State: "surplus"},
	} {
		f := &catalog.FileRow{Path: "a", Size: 1, Copies: []catalog.CopyRow{{Disk: "t1", CreatedBy: "user", State: "ok"}, c}}
		if outcome, err := b.retire(t.Context(), f, "t2"); err != nil || outcome != OutcomeCopied {
			t.Errorf("%+v: %q, %v", c, outcome, err)
		}
	}
	if outcome, err := b.retire(t.Context(), nil, "t2"); err != nil || outcome != OutcomeCopied {
		t.Errorf("no file: %q, %v", outcome, err)
	}
	if outcome, err := b.retire(t.Context(), &catalog.FileRow{Path: "a", Copies: []catalog.CopyRow{{Disk: "t1", State: "ok", Size: sql.NullInt64{}}}}, "t2"); err != nil || outcome != OutcomeCopied {
		t.Errorf("no copy there: %q, %v", outcome, err)
	}
}
