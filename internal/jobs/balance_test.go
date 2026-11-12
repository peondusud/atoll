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

// With until reached, a run starts no file and says copies are left; the next run, with time, takes them on
// (specs.md §13.3).
func TestBalanceStopsAtItsTime(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "a", "x")
	b.scan(true)
	if items, _ := b.replicate("a", ReplicateOptions{Now: true}); len(items) != 1 || items[0].Outcome != OutcomeReplicated {
		t.Fatalf("%+v", items)
	}
	// No disk carries the tag: the copy on t2 breaks the rule, and nowhere takes it.
	b.reconfigure("[[rule]]\nlabel = \"x\"\npath = \"**\"\ntags = [\"usb\"]\ntags_mode = \"strict\"\n")
	run := func(until int64) ([]BalanceItem, *BalanceSummary) {
		t.Helper()
		var items []BalanceItem
		sum, err := Balance(t.Context(), b.db, b.config, b, true, until, func(item *BalanceItem) error {
			items = append(items, *item)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return items, sum
	}
	if items, sum := run(b.now); len(items) != 0 || !sum.Left {
		t.Errorf("at its time: %+v, %+v", items, sum)
	}
	items, sum := run(0)
	if len(items) != 1 || items[0] != (BalanceItem{Path: "a", From: "t2", Size: 1, Outcome: OutcomeNoTarget}) || sum.Left || sum.Stayed != 1 {
		t.Errorf("with time: %+v, %+v", items, sum)
	}
}
