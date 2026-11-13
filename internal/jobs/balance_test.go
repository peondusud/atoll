package jobs

import (
	"database/sql"
	"strings"
	"syscall"
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

// An old copy goes to the trash only once a copy is made that stands in for it (specs.md §13.3): a copy still to make,
// which its job left pending, stands in for nothing. With strict domains the plan keeps a pending copy for the domain
// it is to cover, and marks an old copy surplus for it. The old copy went to the trash on that mark alone: the file
// was left with two copies of three, and the third in the trash.
func TestBalanceRetiresNothingForACopyStillToMake(t *testing.T) {
	ids := []string{"a1", "a2", "a3", "b1", "c1"}
	b := newBenchWith(t, "[defaults]\nreplicas = 2\n", ids...)
	for _, id := range ids {
		b.base = strings.Replace(b.base, "id = \""+id+"\"\n", "id = \""+id+"\"\ndomain = \""+id[:1]+"\"\n", 1)
	}
	b.reconfigure("[defaults]\nreplicas = 2\n")
	for _, id := range ids[:3] {
		b.write(id, "a", "x")
	}
	b.scan(true)
	// Two of the three are Atoll's: only those move.
	b.exec("UPDATE copies SET created_by = 'atoll' WHERE disk_id IN ('a2', 'a3')")
	// Three domains to cover now, of which one takes no copy, as its disk may only be read.
	b.ro[b.dirs["b1"]] = true
	b.reconfigure("[defaults]\nreplicas = 2\n[placement]\nspread_mode = \"strict\"\n")
	b.scan(false)
	// The copy made for the third domain finds no room.
	b.fail[b.dirs["c1"]] = func(call, name string) error {
		if call == "create" && strings.HasSuffix(name, ".part") {
			return syscall.ENOSPC
		}
		return nil
	}
	run := func() []BalanceItem {
		t.Helper()
		var items []BalanceItem
		if _, err := BalanceAtoll(t.Context(), b.db, b.config, b, 0, func(item *BalanceItem) error {
			items = append(items, *item)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return items
	}
	copies := func() string {
		return b.text("SELECT group_concat(line, ', ') FROM (SELECT disk_id || ' ' || state AS line FROM copies ORDER BY disk_id)")
	}
	items := run()
	if len(items) != 2 || items[0].From != "a2" || items[0].To != "c1" || items[1] != (BalanceItem{Path: "a", From: "a3", Size: 1, Outcome: "below-level"}) {
		t.Errorf("%+v", items)
	}
	if got := b.text("SELECT COALESCE(group_concat(disk_id || ' ' || reason, ', '), '') FROM trash"); got != "" {
		t.Errorf("in the trash, with no copy made: %s", got)
	}
	for _, id := range ids[:3] {
		if !b.exists(id, "media/a") {
			t.Errorf("the copy on %s went, and none was made", id)
		}
	}
	if got := copies(); got != "a1 ok, a2 ok, a3 surplus, c1 pending" {
		t.Errorf("the copies: %s", got)
	}
	// Once the copy is made, it stands in for the old one, which the next run sends to the trash.
	delete(b.fail, b.dirs["c1"])
	if made, _ := b.replicate("a", ReplicateOptions{Now: true}); len(made) != 1 || made[0].Disk != "c1" || made[0].Outcome != OutcomeReplicated {
		t.Fatalf("%+v", made)
	}
	if items := run(); len(items) != 1 || items[0].From != "a3" || items[0].Outcome != OutcomeMoved {
		t.Errorf("once the copy is made: %+v", items)
	}
	if got := copies(); got != "a1 ok, a2 ok, c1 ok" {
		t.Errorf("the copies: %s", got)
	}
	if got := b.text("SELECT group_concat(disk_id || ' ' || reason, ', ') FROM trash"); got != "a3 rebalance" {
		t.Errorf("the trash: %s", got)
	}
}
