package jobs

import (
	"testing"
)

// With deletes.direct = "restore", a copy deleted straight on its disk is made again in its place, on that disk, from
// another copy, through the copy pipeline; nothing goes to the trash, and no deletion counts (specs.md §15.5). It was
// only dropped, the file left below its level for the plan to place wherever.
func TestADirectDeletionIsRestoredInPlace(t *testing.T) {
	b := newBenchWith(t, "[deletes]\ndirect = \"restore\"\n", "t1", "t2", "t3")
	b.write("t1", "Films/a.mkv", "content")
	b.write("t2", "Films/a.mkv", "content")
	b.scan(true)
	b.remove("t1", "Films/a.mkv")
	b.settle()
	b.pass()
	if !b.exists("t1", "media/Films/a.mkv") || b.read("t1", "Films/a.mkv") != "content" {
		t.Fatal("the copy was not made again on its disk")
	}
	if b.exists("t3", "media/Films/a.mkv") {
		t.Error("it was made on another disk")
	}
	if got := b.text("SELECT group_concat(disk_id || ' ' || state, ', ') FROM (SELECT disk_id, state FROM copies ORDER BY disk_id)"); got != "t1 ok, t2 ok" {
		t.Errorf("the copies: %s", got)
	}
	if got := b.count("SELECT count(*) FROM trash"); got != 0 {
		t.Errorf("%d entries in the trash", got)
	}
	if got := b.count("SELECT count(*) FROM events WHERE kind = 'copy-gone'"); got != 0 {
		t.Errorf("%d deletions counted", got)
	}
}
