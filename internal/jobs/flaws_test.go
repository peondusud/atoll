//go:build flaws

package jobs

import (
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
)

// Flaw 27 of conformance/models/README.md (Guards.tla, NoOverwriteRefused: FlawGuardRejectLapsed and
// FlawGuardRejectSuperseded). A reject of the mass-modification guard is applied only by the passes that read it as
// the guard's last answer, within its window (catalog.ModifyGuardRead.Answers): once it lapses, or a later answer comes,
// before a pass decided a change it covered, that change is a change like any other, decided as a modification, and
// overwrites the other copies, the version the user kept by rejecting sent to the trash (specs.md §15.6).
func TestFlawARejectedChangeOverwritesTheOtherCopies(t *testing.T) {
	t.Run("the reject lapsed", func(t *testing.T) {
		b := guarded(t, "files = 1\npercent = \"100%\"\nwindow = \"10m\"\n", "a", "b")
		b.rewrite("a", "b")
		if out := b.pass(); !out.Held {
			t.Fatalf("%+v", out)
		}
		given := b.now
		if _, err := catalog.AnswerModifyGuard(t.Context(), b.db, "t1", catalog.EventModifyGuardReject, given); err != nil {
			t.Fatal(err)
		}
		// The daemon down past the window: its first pass finds no answer.
		b.now = given + int64(10*time.Minute) + 1
		b.pass()
		if b.read("t2", "b") != "x" {
			t.Error("t2's copy of b, which the reject kept, was overwritten by the change it rejected")
		}
	})
	t.Run("a later answer came first", func(t *testing.T) {
		b := newBenchWith(t, "[defaults]\nsettle = \"0s\"\n[modifies.mass_guard]\nfiles = 1\npercent = \"100%\"\nwindow = \"10m\"\n"+
			"[modifies.day_guard]\nfiles = 1\npercent = \"100%\"\nwindow = \"24h\"\n", "t1", "t2")
		for _, id := range []string{"t1", "t2"} {
			for _, n := range []string{"a", "b", "c"} {
				b.write(id, n, "x")
			}
		}
		b.scan(true)
		b.rewrite("a", "c")
		if out := b.pass(); !out.Held {
			t.Fatalf("%+v", out)
		}
		if _, err := catalog.AnswerModifyGuard(t.Context(), b.db, "t1", catalog.EventModifyGuardReject, b.now); err != nil {
			t.Fatal(err)
		}
		// b changes since: within the day, its change trips the guard again before c, which the reject covers, is
		// decided; c cannot be read in that pass.
		b.rewrite("b")
		b.fail[b.dirs["t1"]] = func(call, name string) error {
			if call == "openfile" && name == "c" {
				return syscall.EIO
			}
			return nil
		}
		out := b.pass()
		delete(b.fail, b.dirs["t1"])
		if !out.Held || b.read("t2", "c") != "x" || b.text("SELECT state FROM files WHERE lpath = CAST('c' AS BLOB)") == "conflict" {
			t.Fatalf("the pass after the reject: %+v", out)
		}
		upTo, err := catalog.AnswerModifyGuard(t.Context(), b.db, "t1", catalog.EventModifyGuardConfirm, b.now)
		if err != nil || !upTo.Valid || upTo.Int64 < b.ctimeOf("t1", "c") {
			t.Fatalf("the confirm of the new hold: %v, %v", upTo, err)
		}
		b.pass()
		if b.read("t2", "c") != "x" {
			t.Error("t2's copy of c, which the reject kept, was overwritten under the confirm of a later hold")
		}
	})
}
