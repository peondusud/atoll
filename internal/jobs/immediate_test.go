package jobs

import "testing"

// A file in immediate mode the daemon's watcher saw closed, and quiet since, is copied at once; one it did not see
// waits for its settle, as in deferred mode, and so does a file in another mode the watcher saw closed (specs.md
// §12.3, §14.1).
func TestImmediateCopiesOnceClosed(t *testing.T) {
	b := newBenchWith(t, "[defaults]\nmode = \"immediate\"\nsettle = \"10m\"\n[[rule]]\nlabel = \"later\"\npath = \"c\"\nmode = \"deferred\"\n", "t1", "t2")
	for _, name := range []string{"a", "b", "c"} {
		b.write("t1", name, name)
	}
	b.scan(true)
	out, err := RunClosed(t.Context(), b.db, b.config, b, true, nil, Closed{"a": true, "c": true})
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomeOf(out, KindReplicate, "a"); got != OutcomeReplicated {
		t.Errorf("a, closed: %q", got)
	}
	for _, name := range []string{"b", "c"} {
		if got := outcomeOf(out, KindReplicate, name); got != "" {
			t.Errorf("%s copied before its settle: %q", name, got)
		}
	}
	if out.Waiting != 2 {
		t.Errorf("%d waiting, want b and c", out.Waiting)
	}
	// What the watcher saw served that pass: the next one waits for settle again.
	if out := b.pass(); outcomeOf(out, KindReplicate, "b") != "" {
		t.Error("b copied without a close")
	}
}
