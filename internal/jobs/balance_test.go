package jobs

import (
	"database/sql"
	"strings"
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
		sum, err := Balance(t.Context(), b.db, b.config, b, true, until, false, func(item *BalanceItem) error {
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

// What balance.trigger and atoll status count leaves the user copies out: no setting moves one, and only atoll
// balance --move-originals counts one as misplaced (specs.md §12.4, §13.3, §15.11).
func TestMeasureBalanceLeavesUserCopies(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "a", "x")
	b.scan(true)
	if items, _ := b.replicate("a", ReplicateOptions{Now: true}); len(items) != 1 || items[0].Outcome != OutcomeReplicated {
		t.Fatalf("%+v", items)
	}
	// No disk carries the tag: both copies break the rule, and only the one Atoll made counts as misplaced.
	b.reconfigure("[[rule]]\nlabel = \"x\"\npath = \"**\"\ntags = [\"usb\"]\ntags_mode = \"strict\"\n")
	m, err := MeasureBalance(t.Context(), b.db, b.config, b)
	if err != nil {
		t.Fatal(err)
	}
	if *m != (BalanceMeasure{Total: 1, Misplaced: 1, ToMove: 1}) {
		t.Errorf("%+v", *m)
	}
	files, err := MisplacedFiles(t.Context(), b.db, b.config, b.Now())
	if err != nil || len(files) != 1 || !files["a"] {
		t.Errorf("%v, %v", files, err)
	}
}

// The daemon's balancing moves the copy Atoll made where the rule wants it, and leaves the user copy that breaks the
// rule too where it is: no setting moves one (specs.md §12.4, §13.3).
func TestBalanceAtollLeavesUserCopies(t *testing.T) {
	b := newBench(t, "t1", "t2", "t3")
	b.write("t1", "a", "x")
	b.scan(true)
	if items, _ := b.replicate("a", ReplicateOptions{Now: true}); len(items) != 1 || items[0].Disk != "t2" {
		t.Fatalf("%+v", items)
	}
	b.base = strings.Replace(b.base, "id = \"t3\"\n", "id = \"t3\"\ntags = [\"usb\"]\n", 1)
	b.reconfigure("[[rule]]\nlabel = \"x\"\npath = \"**\"\ntags = [\"usb\"]\ntags_mode = \"strict\"\n")
	b.scan(false)
	var items []BalanceItem
	if _, err := BalanceAtoll(t.Context(), b.db, b.config, b, 0, func(item *BalanceItem) error {
		items = append(items, *item)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].From != "t2" || items[0].To != "t3" || items[0].Outcome != OutcomeMoved {
		t.Errorf("%+v", items)
	}
	var got string
	if err := b.db.QueryRowContext(t.Context(), "SELECT group_concat(line, ', ') FROM (SELECT disk_id || ' ' || created_by || ' ' || state AS line FROM copies ORDER BY disk_id)").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "t1 user ok, t3 atoll ok" {
		t.Errorf("the copies: %s", got)
	}
}
