package jobs

import (
	"testing"
)

// A transfer of the SSD cache is held whenever it may not run: a dry run lists it and writes nothing, on battery it
// does not start, and a cache that may only be read keeps its copy; the file's copies on the data disks are made all
// the same, and the copy on the cache stays until a pass may send it (specs.md §19, §20).
func TestATransferHeld(t *testing.T) {
	t.Run("a dry run", func(t *testing.T) {
		b := cacheBench(t, "[cache]\nenabled = true\nmin_age = \"0s\"\n"+windowOpen)
		b.write("c", "show/e1.mkv", "episode")
		b.write("t1", "show/e1.mkv", "episode")
		b.write("t2", "show/e1.mkv", "episode")
		b.scan(true)
		out, err := Run(t.Context(), b.db, b.config, b, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := outcomeOf(out, KindTransfer, "show/e1.mkv"); got != OutcomeToTransfer {
			t.Errorf("a dry run says %q: %+v", got, out.Operations)
		}
		if !b.exists("c", "media/show/e1.mkv") || b.count("SELECT count(*) FROM ops") != 0 {
			t.Error("a dry run wrote")
		}
	})
	t.Run("on battery", func(t *testing.T) {
		b := cacheBench(t, "[cache]\nenabled = true\nmin_age = \"0s\"\n"+windowOpen)
		b.write("c", "show/e1.mkv", "episode")
		b.write("t1", "show/e1.mkv", "episode")
		b.write("t2", "show/e1.mkv", "episode")
		b.scan(true)
		out := b.passPowered(always(true))
		if got := outcomeOf(out, KindTransfer, "show/e1.mkv"); got != "" || !b.exists("c", "media/show/e1.mkv") {
			t.Fatalf("a transfer on battery: %q", got)
		}
		if got := outcomeOf(b.passPowered(always(false)), KindTransfer, "show/e1.mkv"); got != OutcomeTransferred || b.exists("c", "media/show/e1.mkv") {
			t.Errorf("on line power: %q", got)
		}
	})
	t.Run("a cache that may only be read", func(t *testing.T) {
		b := cacheBench(t, "[cache]\nenabled = true\nmin_age = \"0s\"\n"+windowOpen)
		b.write("c", "show/e1.mkv", "episode")
		b.write("t1", "show/e1.mkv", "episode")
		b.write("t2", "show/e1.mkv", "episode")
		b.scan(true)
		b.ro[b.dirs["c"]] = true
		out := b.pass()
		if got := outcomeOf(out, KindTransfer, "show/e1.mkv"); got == OutcomeTransferred || !b.exists("c", "media/show/e1.mkv") {
			t.Errorf("a readonly cache was written to: %q", got)
		}
		if b.count("SELECT count(*) FROM ops WHERE disk_id = 'c'") != 0 {
			t.Error("an operation journaled on a readonly cache")
		}
	})
}
