//go:build flaws

package jobs

import (
	"syscall"
	"testing"
)

// Flaw 23 of conformance/models/README.md (ViewNames.tla, FlawViewFolderBackLost.cfg, ViewNamesSettled): Shows/D
// with e1 on both disks and e2 on t2 alone, renamed through the view to Shows/E on both; a pass finds that rename on
// both disks; t1 is lost, and the pass with it away decides the rename from t2's walk, its job for t1 queued. The view
// renames Shows/E back to Shows/D while t1 is away: made on t2, failed on t1 (EIO here, standing for the lost disk),
// done. One pass later t1 is back, its folder at Shows/E, which its entry records at Shows/D: its walk reads the first
// rename as a user's, while t2's walk finds the rename back, and the two decisions of one pass, made disk by disk, leave
// e1 as two files, Shows/D/e1.mkv and Shows/E/e1.mkv, t1 holding Shows/E, an operation of the view never taken in. The
// rename back, which no user undid, is not kept (specs.md §15.12, §21.2).
func TestFlawARenameBackMissedByALostDiskIsNotKept(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "Shows/D/e1.mkv", "the episode")
	b.write("t2", "Shows/D/e1.mkv", "the episode")
	b.write("t2", "Shows/D/e2.mkv", "the next episode")
	b.scan(true)
	b.now += 60_000_000_000
	w := b.viewOf()
	if err := w.Rename(t.Context(), "Shows/D", "Shows/E", false, viewer); err != nil {
		t.Fatal(err)
	}
	b.passes(1)
	b.gone[b.dirs["t1"]] = true
	b.passes(1)
	b.fail[b.dirs["t1"]] = func(call, name string) error {
		if call == "rename" && name == "D" {
			return syscall.EIO
		}
		return nil
	}
	if err := w.Rename(t.Context(), "Shows/E", "Shows/D", false, viewer); err != nil {
		t.Fatal(err)
	}
	b.fail[b.dirs["t1"]] = nil
	b.passes(1)
	b.gone[b.dirs["t1"]] = false
	b.passes(8)
	for _, id := range []string{"t1", "t2"} {
		if !b.exists(id, "media/Shows/D/e1.mkv") || b.exists(id, "media/Shows/E/e1.mkv") {
			t.Errorf("%s does not hold e1 at Shows/D alone, the name the view gave it back", id)
		}
	}
	if !b.exists("t2", "media/Shows/D/e2.mkv") || b.exists("t2", "media/Shows/E/e2.mkv") {
		t.Error("t2 does not hold e2 at Shows/D alone")
	}
	if got := b.text("SELECT group_concat(CAST(lpath AS TEXT), ' ') FROM (SELECT lpath FROM files ORDER BY lpath)"); got != "Shows/D/e1.mkv Shows/D/e2.mkv" {
		t.Errorf("the catalog holds %s", got)
	}
	if open := b.openViewOps(); len(open) != 0 {
		t.Errorf("not taken in: %+v", open)
	}
}
