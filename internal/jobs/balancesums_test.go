package jobs

import (
	"slices"
	"strings"
	"testing"
)

// What balance.trigger compares sums every file with a misplaced copy, and what each of its misplaced copies would
// move, one file's copies apart; and every user copy that breaks a strict setting counts for --move-originals
// (specs.md §6.1, item 9, §13.3).
func TestMeasureBalanceSumsEveryFile(t *testing.T) {
	b := newBenchWith(t, "[[rule]]\nlabel = \"two\"\npath = \"b\"\nreplicas = 2\n", "t1", "t2", "t3")
	b.write("t1", "a", "x")
	b.write("t1", "b", "yyy")
	b.scan(true)
	for path, want := range map[string]int{"a": 1, "b": 2} {
		if items, _ := b.replicate(path, ReplicateOptions{Now: true}); len(items) != want {
			t.Fatalf("%s: %+v", path, items)
		}
	}
	// No disk carries the tag: every copy breaks the rule, and only those Atoll made count as misplaced.
	b.reconfigure("[[rule]]\nlabel = \"two\"\npath = \"b\"\nreplicas = 2\ntags = [\"usb\"]\ntags_mode = \"strict\"\n" +
		"[[rule]]\nlabel = \"x\"\npath = \"**\"\ntags = [\"usb\"]\ntags_mode = \"strict\"\n")
	m, err := MeasureBalance(t.Context(), b.db, b.config, b)
	if err != nil {
		t.Fatal(err)
	}
	if *m != (BalanceMeasure{Total: 4, Misplaced: 4, ToMove: 7}) {
		t.Errorf("%+v", *m)
	}
	if n, err := UserCopiesMisplaced(t.Context(), b.db, b.config, b.now); err != nil || n != 2 {
		t.Errorf("%d user copies misplaced, %v", n, err)
	}
}

// atoll balance sums the bytes of every file it would move, and of every one it moves (specs.md §13.3).
func TestBalanceSumsTheBytesItMoves(t *testing.T) {
	b := newBench(t, "t1", "t2", "t3", "t4")
	b.write("t1", "a", "x")
	b.write("t1", "b", "yyy")
	b.scan(true)
	for _, path := range []string{"a", "b"} {
		if items, _ := b.replicate(path, ReplicateOptions{Now: true}); len(items) != 1 {
			t.Fatalf("%s: %+v", path, items)
		}
	}
	// The tag goes to a disk that holds neither copy Atoll made: both copies move there.
	held := strings.Split(b.text("SELECT group_concat(disk_id) FROM copies WHERE created_by = 'atoll'"), ",")
	free := ""
	for _, id := range []string{"t2", "t3", "t4"} {
		if !slices.Contains(held, id) {
			free = id
			break
		}
	}
	b.base = strings.Replace(b.base, "id = \""+free+"\"\n", "id = \""+free+"\"\ntags = [\"usb\"]\n", 1)
	b.reconfigure("[[rule]]\nlabel = \"x\"\npath = \"**\"\ntags = [\"usb\"]\ntags_mode = \"strict\"\n")
	b.scan(false)
	if items, sum := b.balance(false, false); len(items) != 2 || sum.ToDo != 2 || sum.Bytes != 4 {
		t.Errorf("the preview: %+v, %+v", items, sum)
	}
	items, sum := b.balance(true, false)
	if len(items) != 2 || items[0].Outcome != OutcomeMoved || items[1].Outcome != OutcomeMoved || sum.Bytes != 4 {
		t.Errorf("the run: %+v, %+v", items, sum)
	}
}

// atoll replicate sums the bytes of every copy it queues, and of every one it makes (specs.md §14.3).
func TestReplicateSumsTheBytesItCopies(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "a", "x")
	b.write("t1", "b", "yyy")
	b.scan(true)
	if items, sum := b.replicate("", ReplicateOptions{}); len(items) != 2 || items[0].Outcome != OutcomeQueued || sum.Bytes != 4 {
		t.Errorf("queued: %+v, %+v", items, sum)
	}
	if items, sum := b.replicate("", ReplicateOptions{Now: true}); len(items) != 2 || sum.Bytes != 4 || sum.Failed != 0 {
		t.Errorf("the run: %+v, %+v", items, sum)
	}
}
