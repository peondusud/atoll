package jobs

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/trash"
)

// The points of an operation create after which its copy is at its name in the library, before its row is written
// (specs.md §14.2).
var placedPoints = []string{"moved", "target-synced", "source-synced"}

// propagated says whether a write into the copy on t2 reached t1, the version before in t1's trash.
func (b *bench) propagated(text string) {
	b.t.Helper()
	if got := b.read("t1", "a"); got != text {
		b.t.Fatalf("the edit was not propagated: t1 holds %q", got)
	}
	if got := b.text("SELECT group_concat(disk_id || ' ' || created_by, ',') FROM (SELECT * FROM copies ORDER BY disk_id)"); got != "t1 atoll,t2 user" {
		b.t.Errorf("the copies are %s", got)
	}
	if got := b.count("SELECT count(*) FROM trash WHERE disk_id = 't1' AND reason = 'overwritten'"); got != 1 {
		b.t.Errorf("%d versions before in the trash of t1", got)
	}
}

// A user's write into a new copy once its name is in the library, before its row is written, is an edit: the row takes
// what the part was when it was verified, and the next look reads the write as a modification, propagated to the
// other copy. The row took what a look found after the syncs, recorded as verified against the version before: no
// look ever saw the edit, and a scrub would have repaired it away (conformance/models/README.md, flaw 1).
func TestAWriteIntoANewCopyBeforeItsRowIsAnEdit(t *testing.T) {
	for _, point := range placedPoints {
		t.Run(point, func(t *testing.T) {
			b := replicatePool(t)
			b.at[point] = func() { b.changeAgain("t2", "a", "CONTENT!!") }
			items, _ := b.replicate("a", ReplicateOptions{Now: true})
			only(t, items, OutcomeReplicated)
			if got := b.count("SELECT size FROM copies WHERE disk_id = 't2'"); got != len("content") {
				t.Errorf("the row records %d bytes, not the part's", got)
			}
			b.now = b.ctimeOf("t2", "a") + settleNS
			out := b.pass()
			if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "modified"}}) {
				t.Fatalf("%+v", out)
			}
			b.propagated("CONTENT!!")
		})
	}
}

// The same with a crash at each of those points: the reprise records what the part was when verified, from what its
// job recorded then, however long Atoll was down, and the next look reads the write as a modification. The reprise
// recorded what was there when it ran: the edit was taken for the copy, verified, and never propagated.
func TestAWriteIntoANewCopyWhileAtollIsDown(t *testing.T) {
	for _, point := range placedPoints {
		t.Run(point, func(t *testing.T) {
			b := replicatePool(t)
			b.replicateStopping("a", point)
			b.changeAgain("t2", "a", "CONTENT!!")
			b.now = b.ctimeOf("t2", "a") + settleNS
			out := b.pass()
			if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "modified"}}) {
				t.Fatalf("%+v", out)
			}
			b.propagated("CONTENT!!")
			if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
				t.Errorf("%d operations left planned", got)
			}
		})
	}
}

// A disk lost after the rename, written into while away, and back: its reprise finds the write an edit, as above.
func TestAWriteIntoANewCopyOnADiskLostAfterItsRename(t *testing.T) {
	b := replicatePool(t)
	at("moved", failOn("t2", "sync", syscall.EIO))(b)
	items, _ := b.replicate("a", ReplicateOptions{Now: true})
	only(t, items, trash.OutcomeIOError)
	if got := b.text("SELECT state FROM ops WHERE kind = 'create'"); got != "planned" {
		t.Fatalf("the operation is %s", got)
	}
	delete(b.fail, b.dirs["t2"])
	b.changeAgain("t2", "a", "CONTENT!!")
	b.now = b.ctimeOf("t2", "a") + settleNS
	out := b.pass()
	if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "modified"}}) {
		t.Fatalf("%+v", out)
	}
	b.propagated("CONTENT!!")
}

// A chmod of a new copy while Atoll is down moves its mode and its change time, not its size or its dates: the reprise
// records the part's mode, with the change time it had then, so that the next look reads a change of metadata, given to
// the other copy (specs.md §14.2, §15.4). The reprise recorded the mode it found, and the chmod never reached the other
// copy.
func TestAChmodOfANewCopyWhileAtollIsDown(t *testing.T) {
	b := replicatePool(t)
	b.replicateStopping("a", "source-synced")
	mode := int64(b.chmod("t2", "a", 0o600, 0o640))
	b.now = b.ctimeOf("t2", "a") + settleNS
	out := b.pass()
	if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "metadata"}}) || !slices.Equal(out.Operations, []Operation{modeTo("t1", OutcomeApplied)}) {
		t.Fatalf("%+v", out)
	}
	if b.modeOf("t1", "a") != mode {
		t.Errorf("t1 holds %o, want the mode given to t2, %o", b.modeOf("t1", "a"), mode)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE mode = ?", mode); got != 2 {
		t.Errorf("%d rows say the mode", got)
	}
}

// Atoll killed at each step of an operation create, and nothing else: the next run makes or finishes the copy, verified,
// its row what its disk holds, so that a pass finds nothing to do, and a full scan reads both copies as the file.
func TestACrashAtEachPointOfACopy(t *testing.T) {
	for _, point := range []string{"journaled", "copied", "verified", "moved", "target-synced", "source-synced"} {
		t.Run(point, func(t *testing.T) {
			b := replicatePool(t)
			b.replicateStopping("a", point)
			b.replicate("a", ReplicateOptions{Now: true})
			if got := b.read("t2", "a"); got != "content" {
				t.Fatalf("%q", got)
			}
			if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'") + b.count("SELECT count(*) FROM jobs"); got != 0 {
				t.Errorf("%d operations or jobs left", got)
			}
			if parts := b.parts("t2"); len(parts) != 0 {
				t.Errorf("tmp/ holds %q", parts)
			}
			if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND state = 'ok' AND verified_ns IS NOT NULL"); got != 1 {
				t.Error("the copy is not verified")
			}
			out := b.settle()
			if len(out.Decisions) != 0 || len(out.Operations) != 0 || out.Waiting != 0 {
				t.Errorf("a pass after: %+v", out)
			}
			b.scan(true)
			if got := b.count("SELECT count(*) FROM copies WHERE state = 'ok' AND verified_ns IS NOT NULL"); got != 2 {
				t.Errorf("%d copies verified", got)
			}
		})
	}
}

// A copy made with no crash and no write is verified at once, its row the part's with the change time the rename
// gave, which the next look finds as it is.
func TestACopyMadeIsVerifiedAndThere(t *testing.T) {
	b := replicatePool(t)
	items, _ := b.replicate("a", ReplicateOptions{Now: true})
	only(t, items, OutcomeReplicated)
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND verified_ns IS NOT NULL AND ctime_ns = ?", b.ctimeOf("t2", "a")); got != 1 {
		t.Error("the copy is not verified with the change time its disk holds")
	}
	st, err := os.Stat(filepath.Join(b.dirs["t2"], "media", "a"))
	if err != nil {
		t.Fatal(err)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND mtime_ns = ? AND size = ?", st.ModTime().UnixNano(), st.Size()); got != 1 {
		t.Error("the row is not what the disk holds")
	}
	b.now += int64(time.Hour)
	if out := b.pass(); len(out.Decisions) != 0 || len(out.Operations) != 0 {
		t.Errorf("%+v", out)
	}
}
