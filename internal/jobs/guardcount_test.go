package jobs

import (
	"slices"
	"testing"
)

// The mass-deletion guard of a disk counts the deletions its window's events already took, and the copies gone
// since, which it holds: events alone trip it and hold nothing, a copy gone besides holds; the line says them all
// (specs.md §15.6).
func TestAGuardCountsEventsAndCopiesGone(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for _, name := range []string{"a", "b", "c", "d"} {
		b.write("t1", name, "x")
		b.write("t2", name, "x")
	}
	b.scan(true)
	for _, name := range []string{"a", "b", "c"} {
		b.remove("t1", name)
	}
	b.settle()
	b.config = reconfigured(t, b, "files = 1\npercent = \"100%\"\n")
	out := b.pass()
	if want := []GuardLine{{Disk: "t1", Kind: GuardDeletions, Count: 3}}; !slices.Equal(out.Guards, want) || out.Held {
		t.Errorf("events alone: guards %+v, held %v", out.Guards, out.Held)
	}
	b.remove("t1", "d")
	// A dry run counts the copy gone as the pass does, from what its look finds, writing nothing.
	dry, err := Run(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []GuardLine{{Disk: "t1", Kind: GuardDeletions, Count: 4}}; !slices.Equal(dry.Guards, want) || !dry.Held {
		t.Errorf("a dry run: guards %+v, held %v", dry.Guards, dry.Held)
	}
	out = b.pass()
	if want := []GuardLine{{Disk: "t1", Kind: GuardDeletions, Count: 4}}; !slices.Equal(out.Guards, want) || !out.Held {
		t.Errorf("a copy gone besides: guards %+v, held %v", out.Guards, out.Held)
	}
	if !b.exists("t2", "media/d") {
		t.Error("the guard let the deletion through")
	}
}
