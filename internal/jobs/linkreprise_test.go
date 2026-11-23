package jobs

import (
	"os"
	"path/filepath"
	"testing"
)

// The reprise of an operation link of the link guard, or of a link's removal, leaves the change time the copy's row
// recorded: a program that writes into the copy, keeping its size and its modification time, as a tagger may, while
// Atoll is down after a link it made and before its commit, moves only the change time, which the next look reads as a
// change, so that the write reaches the other copy (specs.md §17.2; conformance/models/README.md, flaw 11, the
// maintainer's remedy). It once took that change time for the link's, the write never seen, and a scrub would have
// read it as corruption and repaired it away (invariant 9, G4).
func TestAWriteKeepingTheDatesSurvivesTheRepriseOfALink(t *testing.T) {
	b := newBenchWith(t, "[trash]\nlink_guard = true\n", "t1", "t2")
	b.write("t1", "a", "content")
	b.write("t2", "a", "content")
	b.scan(true)
	b.crashAt("linked")
	if n := b.count("SELECT count(*) FROM ops WHERE kind = 'link' AND state = 'planned'"); n == 0 {
		t.Fatal("no link left planned")
	}
	// While Atoll is down, a program rewrites the copy on t1 to the same size, and sets its modification time back.
	full := filepath.Join(b.dirs["t1"], "media", "a")
	st, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	b.changeAgain("t1", "a", "CONTENT")
	if err := os.Chtimes(full, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	b.now = b.ctimeOf("t1", "a") + settleNS
	b.settle()
	b.now += settleNS
	b.settle()
	data, err := os.ReadFile(filepath.Join(b.dirs["t2"], "media", "a"))
	if err != nil {
		t.Fatal(err)
	}
	if state := b.text("SELECT state FROM copies WHERE disk_id = 't2'"); string(data) != "CONTENT" && state == "ok" {
		t.Errorf("the write on t1 never reached t2, whose copy is %s with %q: the reprise of the link took its "+
			"change time for the link's", state, data)
	}
}

// With nothing written meanwhile, a link the reprise finished leaves the copy as it is: the next look finds its change
// time moved, reads it, finds its content the file's, a change of metadata alone, and nothing is copied or sent to the
// trash; and a link's removal stopped the same way, with the guard turned off, likewise.
func TestTheRepriseOfALinkLeavesTheCopyInSync(t *testing.T) {
	for _, c := range []struct{ name, point, more string }{
		{"a link", "linked", ""},
		{"a link removed", "unlinked", "off"},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newBenchWith(t, "[trash]\nlink_guard = true\n", "t1", "t2")
			b.write("t1", "a", "content")
			b.write("t2", "a", "content")
			b.scan(true)
			if c.more == "off" {
				b.pass()
				b.reconfigure("[trash]\nlink_guard = false\n")
			}
			b.crashAt(c.point)
			b.now += settleNS
			b.settle()
			b.now += settleNS
			b.settle()
			if got := b.text("SELECT group_concat(state, ' ') FROM (SELECT state FROM copies ORDER BY disk_id)"); got != "ok ok" {
				t.Errorf("copies %s", got)
			}
			if n := b.count("SELECT count(*) FROM trash"); n != 0 {
				t.Errorf("%d entries in the trash", n)
			}
			if got := b.text("SELECT state FROM files"); got != "ok" {
				t.Errorf("the file is %s", got)
			}
			if n := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); n != 0 {
				t.Errorf("%d operations left planned", n)
			}
		})
	}
}
