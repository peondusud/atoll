package jobs

import (
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
)

// keepsX says that t2's copies of the files a reject covered still hold what they held, x, and that each file is a
// conflict.
func (b *bench) keepsX(names ...string) {
	b.t.Helper()
	for _, n := range names {
		if b.read("t2", n) != "x" {
			b.t.Errorf("t2's copy of %s, which the reject kept, was overwritten", n)
		}
		if got := b.text("SELECT state FROM files WHERE lpath = CAST(? AS BLOB)", n); got != "conflict" {
			b.t.Errorf("%s is %s", n, got)
		}
	}
}

// reject answers the mass-modification guard of t1 with a reject, at the bench's time.
func (b *bench) reject() {
	b.t.Helper()
	if _, err := catalog.AnswerModifyGuard(b.t.Context(), b.db, "t1", catalog.EventModifyGuardReject, b.now); err != nil {
		b.t.Fatal(err)
	}
}

// twoRewritten gives a pool whose mass-modification guard lets one file through in 10 minutes, and with day its day one
// in a day, a and the files named after it rewritten on t1, a decided and the next held, rejected.
func twoRewritten(t *testing.T, day bool, held ...string) *bench {
	t.Helper()
	more := "[defaults]\nsettle = \"0s\"\n[modifies.mass_guard]\nfiles = 1\npercent = \"100%\"\nwindow = \"10m\"\n"
	if day {
		more += "[modifies.day_guard]\nfiles = 1\npercent = \"100%\"\nwindow = \"24h\"\n"
	}
	b := newBenchWith(t, more, "t1", "t2")
	for _, id := range []string{"t1", "t2"} {
		for _, n := range []string{"a", "b", "c", "d"} {
			b.write(id, n, "x")
		}
	}
	b.scan(true)
	b.rewrite(append([]string{"a"}, held...)...)
	if out := b.pass(); !out.Held || !slices.Equal(decisionsOf(out), []Decision{{Path: "a", Kind: "modified"}}) {
		t.Fatalf("%+v", out)
	}
	b.reject()
	return b
}

// Flaw 27 of conformance/models/README.md (Guards.tla, NoOverwriteRefused: FlawGuardRejectLapsed and
// FlawGuardRejectSuperseded), fixed with the maintainer's remedy, reject-kept (2026-11-25): a change made at or before
// the up_to of any reject of the mass-modification guard of its disk is a conflict at every later pass, whatever answer
// came after it and however long ago the reject was given, so that what a reject kept is never overwritten (specs.md
// §15.6). It was read only as the guard's last answer within its window: a change it covered that no pass decided by
// then was a change like any other, decided as a modification, and overwrote the other copies.
func TestARejectedChangeStaysAConflict(t *testing.T) {
	t.Run("the reject lapsed", func(t *testing.T) {
		b := twoRewritten(t, false, "c")
		// The daemon down past the window: its first pass finds the reject lapsed, and still keeps c.
		b.now += int64(10*time.Minute) + 1
		out := b.pass()
		if !slices.Equal(decisionsOf(out), []Decision{{Path: "c", Kind: "conflict"}}) || out.Held {
			t.Errorf("%+v", out)
		}
		b.keepsX("c")
	})
	t.Run("a later answer came first", func(t *testing.T) {
		b := twoRewritten(t, true, "c")
		// b changes since, and within the day trips the guard again before c is decided: c cannot be read in that pass.
		b.rewrite("b")
		b.fail[b.dirs["t1"]] = func(call, name string) error {
			if call == "openfile" && name == "c" {
				return syscall.EIO
			}
			return nil
		}
		if out := b.pass(); !out.Held {
			t.Fatalf("%+v", out)
		}
		delete(b.fail, b.dirs["t1"])
		upTo, err := catalog.AnswerModifyGuard(t.Context(), b.db, "t1", catalog.EventModifyGuardConfirm, b.now)
		if err != nil || !upTo.Valid || upTo.Int64 < b.ctimeOf("t1", "c") {
			t.Fatalf("the confirm of the new hold: %v, %v", upTo, err)
		}
		// The confirm decides b, which it answers; c stays the reject's.
		if out := b.pass(); !slices.Equal(decisionsOf(out), []Decision{{Path: "b", Kind: "modified"}, {Path: "c", Kind: "conflict"}}) {
			t.Errorf("%+v", out)
		}
		b.keepsX("c")
		if b.read("t2", "b") != "z" {
			t.Error("b, which the confirm answered, did not reach t2")
		}
	})
}

// The daemon stopped, as by kill -9, between the reject and the pass that decides what it covered: as that pass
// commits another change before it, or right after it commits the conflict itself. The pass after the stop, past the
// reject's window, keeps the change a conflict.
func TestAStopAfterAReject(t *testing.T) {
	t.Run("before the rejected change is decided", func(t *testing.T) {
		b := twoRewritten(t, false, "c")
		// b changes since, under the threshold: decided first, its commit the stop.
		b.rewrite("b")
		b.crashAt("modified")
		if b.read("t2", "c") != "x" {
			t.Fatal("c went before the stop")
		}
		b.now += int64(10*time.Minute) + 1
		b.pass()
		b.keepsX("c")
	})
	t.Run("right after the conflict is committed", func(t *testing.T) {
		b := twoRewritten(t, false, "c")
		b.crashAt("modified")
		b.now += int64(10*time.Minute) + 1
		out := b.pass()
		if len(out.Decisions) != 0 {
			t.Errorf("%+v", out)
		}
		b.keepsX("c")
	})
	t.Run("the rejected file unreadable, then the reject lapsed", func(t *testing.T) {
		b := twoRewritten(t, false, "c")
		b.fail[b.dirs["t1"]] = func(call, name string) error {
			if call == "openfile" && name == "c" {
				return syscall.EIO
			}
			return nil
		}
		b.pass()
		delete(b.fail, b.dirs["t1"])
		b.now += int64(10*time.Minute) + 1
		b.pass()
		b.keepsX("c")
	})
}

// Several rejects: each keeps what it covered, the latest up_to covering every change made by then; a confirm between
// two decides only what it answers past the reject before it.
func TestSeveralRejects(t *testing.T) {
	b := twoRewritten(t, false, "c")
	// The pass decides c as the reject's conflict; b then changes, d after it, and d trips the guard again: rejected.
	if out := b.pass(); !slices.Equal(decisionsOf(out), []Decision{{Path: "c", Kind: "conflict"}}) {
		t.Fatalf("%+v", out)
	}
	b.rewrite("b", "d")
	if out := b.pass(); !out.Held || !slices.Equal(decisionsOf(out), []Decision{{Path: "b", Kind: "modified"}}) {
		t.Fatalf("%+v", out)
	}
	b.reject()
	b.now += int64(10*time.Minute) + 1
	if out := b.pass(); !slices.Equal(decisionsOf(out), []Decision{{Path: "d", Kind: "conflict"}}) || out.Held {
		t.Errorf("%+v", out)
	}
	b.keepsX("c", "d")
	if got := b.count("SELECT count(*) FROM events WHERE kind = 'modify-guard-reject'"); got != 2 {
		t.Errorf("%d rejects", got)
	}
	// A change made after the latest reject is another, which counts: one goes through, under the threshold.
	b.changeAgain("t1", "a", "another content")
	b.now = max(b.now, b.ctimeOf("t1", "a")+2_000_000)
	if out := b.pass(); !slices.Equal(decisionsOf(out), []Decision{{Path: "a", Kind: "modified"}}) {
		t.Errorf("a change past the rejects: %+v", out)
	}
}

// What a reject covered, a crash and a restart later, as the catalog keeps the reject's event: nothing of it lives in
// memory.
func TestARejectSurvivesARestart(t *testing.T) {
	top := t.TempDir()
	b := benchIn(t, top, "[defaults]\nsettle = \"0s\"\n[modifies.mass_guard]\nfiles = 1\npercent = \"100%\"\nwindow = \"10m\"\n", true, "t1", "t2")
	for _, id := range []string{"t1", "t2"} {
		for _, n := range []string{"a", "c"} {
			b.write(id, n, "x")
		}
	}
	b.scan(true)
	b.rewrite("a", "c")
	b.pass()
	b.reject()
	again := benchIn(t, top, "[defaults]\nsettle = \"0s\"\n[modifies.mass_guard]\nfiles = 1\npercent = \"100%\"\nwindow = \"10m\"\n", false, "t1", "t2")
	again.now = b.now + int64(10*time.Minute) + 1
	again.pass()
	again.keepsX("c")
}
