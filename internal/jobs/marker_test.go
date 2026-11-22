package jobs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/disks"
)

// A disk whose own marker was seen, and that a pass finds without one, is missing, no-marker, in the pass's line, which
// the alert disk-missing reads: a filesystem with its identifier and none of its marker, as a clone made before disk
// add, is not that disk. The pass does not walk it, and takes nothing it holds for the user's: an older version of a
// file there is no modification, a file it lacks no deletion, so the other copies stay as they are, and nothing goes
// to the trash; nothing is written to it. A dry run's check says the same (specs.md §10, §15.12, invariant 4).
func TestAPassTakesNothingFromADiskWithoutItsMarker(t *testing.T) {
	b := newBenchWith(t, "", "t1", "t2")
	b.write("t1", "a", "new")
	b.write("t1", "b", "kept")
	b.write("t2", "a", "new")
	b.write("t2", "b", "kept")
	b.scan(false)
	if err := catalog.SetMarkerSeen(t.Context(), b.db, "t2", b.config.Disks[1].UUID, 1); err != nil {
		t.Fatal(err)
	}
	// What a clone made earlier would hold: an older a, and no b.
	b.write("t2", "a", "old")
	b.remove("t2", "b")
	dry, err := Run(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if line := lineOf(dry, "t2"); line == nil || line.Verdict.State != disks.Missing || line.Verdict.Reason != disks.NoMarker {
		t.Errorf("a dry run: %+v", line)
	}
	out := b.settle()
	line := lineOf(out, "t2")
	if line == nil || line.Verdict.State != disks.Missing || line.Verdict.Reason != disks.NoMarker || line.Verdict.Message == "" || line.Walked {
		t.Fatalf("the line of t2: %+v", line)
	}
	if one := lineOf(out, "t1"); one == nil || one.Verdict.State != disks.Online {
		t.Errorf("the line of t1: %+v", one)
	}
	for path, want := range map[string]string{"a": "new", "b": "kept"} {
		if got, err := os.ReadFile(filepath.Join(b.dirs["t1"], "media", path)); err != nil || string(got) != want {
			t.Errorf("t1's %s: %q, %v", path, got, err)
		}
	}
	if got := b.count("SELECT count(*) FROM trash"); got != 0 {
		t.Errorf("%d entries in the trash", got)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE gone_ns IS NOT NULL OR state <> 'ok'"); got != 0 {
		t.Errorf("%d copies taken for gone or changed", got)
	}
	if b.exists("t1", "Atoll/trash") || b.exists("t2", "Atoll") {
		t.Error("a trash on t1, or something written to t2")
	}
	if got := b.text("SELECT state FROM disks WHERE id = 't2'"); got != "missing" {
		t.Errorf("t2 is %s in the catalog", got)
	}
}

// A copy the plan wants on such a disk waits: nothing is written there, the file stays degraded, and the line says why
// (specs.md §10, §14.1).
func TestACopyGoesToNoDiskWithoutItsMarker(t *testing.T) {
	b := newBenchWith(t, "", "t1", "t2")
	b.write("t1", "a", "x")
	b.scan(false)
	if err := catalog.SetMarkerSeen(t.Context(), b.db, "t2", b.config.Disks[1].UUID, 1); err != nil {
		t.Fatal(err)
	}
	out := b.settle()
	if b.exists("t2", "media/a") || b.exists("t2", "Atoll") {
		t.Error("something was written to t2")
	}
	if got := b.text("SELECT state FROM files"); got != "degraded" {
		t.Errorf("the file is %s", got)
	}
	if line := lineOf(out, "t2"); line == nil || line.Verdict.State != disks.Missing || line.Verdict.Reason != disks.NoMarker {
		t.Errorf("the line of t2: %+v", line)
	}
}
