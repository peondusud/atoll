package jobs

import (
	"slices"
	"strings"
	"testing"
)

// usbPool gives a pool of three disks, t2 and t3 tagged usb, where a's user copy is on t1, with Atoll's copies on t2
// and t3 (replicas = 2), then a rule that wants a's copies on usb disks alone, strictly.
func usbPool(t *testing.T) *bench {
	t.Helper()
	b := newBenchWith(t, "[defaults]\nreplicas = 2\n", "t1", "t2", "t3")
	b.write("t1", "a", "x")
	b.scan(true)
	if items, _ := b.replicate("a", ReplicateOptions{Now: true}); len(items) != 2 {
		t.Fatalf("%+v", items)
	}
	for _, id := range []string{"t2", "t3"} {
		b.base = strings.Replace(b.base, "id = \""+id+"\"\n", "id = \""+id+"\"\ntags = [\"usb\"]\n", 1)
	}
	b.reconfigure("[[rule]]\nlabel = \"usb\"\npath = \"**\"\ntags = [\"usb\"]\ntags_mode = \"strict\"\n")
	b.scan(false)
	return b
}

func (b *bench) balance(apply, originals bool) ([]BalanceItem, *BalanceSummary) {
	b.t.Helper()
	var items []BalanceItem
	sum, err := Balance(b.t.Context(), b.db, b.config, b, apply, 0, originals, func(item *BalanceItem) error {
		items = append(items, *item)
		return nil
	})
	if err != nil {
		b.t.Fatal(err)
	}
	return items, sum
}

func (b *bench) copiesLine() string {
	b.t.Helper()
	return b.text("SELECT group_concat(line, ', ') FROM (SELECT disk_id || ' ' || created_by || ' ' || state AS line FROM copies ORDER BY disk_id)")
}

// A user copy that breaks a strict setting moves only with --move-originals (specs.md §6.1, item 9, §13.3). The copies
// a file has on usb disks already reach its level: no copy is made, and the user copy hands its part to the first of
// them, which becomes the user copy, then goes to the trash as rebalance. Without --yes, nothing is written.
func TestMoveOriginalsHandsAUserCopyOver(t *testing.T) {
	b := usbPool(t)
	if n, err := UserCopiesMisplaced(t.Context(), b.db, b.config, b.now); err != nil || n != 1 {
		t.Fatalf("%d user copies misplaced, %v", n, err)
	}
	// Without --move-originals, the user copy stays: nothing breaks a setting that atoll balance moves.
	if items, sum := b.balance(false, false); len(items) != 0 || sum.ToDo != 0 {
		t.Fatalf("without --move-originals: %+v, %+v", items, sum)
	}
	items, sum := b.balance(false, true)
	if !slices.Equal(items, []BalanceItem{{Path: "a", From: "t1", To: "t2", Size: 1, Outcome: OutcomeToMove}}) || sum.ToDo != 1 || sum.Bytes != 1 || sum.Applied {
		t.Fatalf("the preview: %+v, %+v", items, sum)
	}
	// The plan keeps the user copy and one usb copy: the other is surplus until the user copy goes.
	if got := b.copiesLine(); got != "t1 user ok, t2 atoll ok, t3 atoll surplus" {
		t.Fatalf("a preview wrote: %s", got)
	}
	items, sum = b.balance(true, true)
	if !slices.Equal(items, []BalanceItem{{Path: "a", From: "t1", To: "t2", Size: 1, Outcome: OutcomeMoved}}) || sum.Bytes != 1 || sum.DiskLost {
		t.Fatalf("the run: %+v, %+v", items, sum)
	}
	if got := b.copiesLine(); got != "t2 user ok, t3 atoll ok" {
		t.Errorf("the copies: %s", got)
	}
	if b.exists("t1", "media/a") || b.read("t2", "a") != "x" || b.read("t3", "a") != "x" {
		t.Error("the copies on the disks")
	}
	if got := b.text("SELECT group_concat(disk_id || ' ' || reason, ', ') FROM trash"); got != "t1 rebalance" {
		t.Errorf("the trash: %s", got)
	}
	// Done: the next run has nothing to move.
	if items, sum := b.balance(true, true); len(items) != 0 || sum.Stayed != 0 {
		t.Errorf("then: %+v, %+v", items, sum)
	}
}

// A user copy whose file has nowhere else to reach its level stays where it is, with --move-originals too: no copy
// is made in its place, and none other stands in for it (specs.md §13.3).
func TestMoveOriginalsKeepsAUserCopyNothingStandsInFor(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "a", "x")
	b.scan(true)
	b.reconfigure("[[rule]]\nlabel = \"usb\"\npath = \"**\"\nreplicas = 0\ntags = [\"usb\"]\ntags_mode = \"strict\"\n")
	b.scan(false)
	items, sum := b.balance(false, true)
	if !slices.Equal(items, []BalanceItem{{Path: "a", From: "t1", Size: 1, Outcome: OutcomeNoTarget}}) || sum.Stayed != 1 || sum.ToDo != 0 {
		t.Fatalf("the preview: %+v, %+v", items, sum)
	}
	items, sum = b.balance(true, true)
	if !slices.Equal(items, []BalanceItem{{Path: "a", From: "t1", Size: 1, Outcome: OutcomeNoTarget}}) || sum.Stayed != 1 {
		t.Fatalf("the run: %+v, %+v", items, sum)
	}
	if got := b.copiesLine(); got != "t1 user ok" || b.read("t1", "a") != "x" {
		t.Errorf("the copies: %s", got)
	}
}

// With replication.enabled = false, atoll balance copies nothing, and says so (specs.md §13.3, §21.4).
func TestBalanceWithReplicationDisabled(t *testing.T) {
	b := usbPool(t)
	b.reconfigure("[replication]\nenabled = false\n[[rule]]\nlabel = \"usb\"\npath = \"**\"\ntags = [\"usb\"]\ntags_mode = \"strict\"\n")
	if items, sum := b.balance(true, true); len(items) != 0 || !sum.Disabled {
		t.Errorf("%+v, %+v", items, sum)
	}
	if got := b.copiesLine(); got != "t1 user ok, t2 atoll ok, t3 atoll surplus" {
		t.Errorf("the copies: %s", got)
	}
}
