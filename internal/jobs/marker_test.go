package jobs

import (
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/disks"
)

// A pass that confirms a disk whose own marker was seen, and finds it without one, says the disk readonly, no-marker,
// in its line, which the alert disk-readonly reads, and writes nothing to it: a filesystem with its identifier and
// none of its marker, as a clone made before disk add, is not that disk. A dry run confirms nothing, and says what
// the mount check says (specs.md §10, §23).
func TestAPassSaysADiskWithoutItsMarkerIsReadonly(t *testing.T) {
	b := newBenchWith(t, "", "t1", "t2")
	b.write("t1", "a", "x")
	b.write("t2", "a", "x")
	b.scan(false)
	if err := catalog.SetMarkerSeen(t.Context(), b.db, "t2", b.config.Disks[1].UUID, 1); err != nil {
		t.Fatal(err)
	}
	// What the look then finds gone is concluded from once the disk confirms, which reads its marker.
	b.remove("t2", "a")
	dry, err := Run(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if line := lineOf(dry, "t2"); line == nil || line.Verdict.State != disks.Online {
		t.Errorf("a dry run: %+v", line)
	}
	out := b.pass()
	line := lineOf(out, "t2")
	if line == nil || line.Verdict.State != disks.Readonly || line.Verdict.Reason != disks.NoMarker || line.Verdict.Message == "" {
		t.Fatalf("the line of t2: %+v", line)
	}
	if one := lineOf(out, "t1"); one == nil || one.Verdict.State != disks.Online {
		t.Errorf("the line of t1: %+v", one)
	}
	if b.exists("t2", "Atoll") {
		t.Error("something was written to t2's work folder")
	}
	if got := b.text("SELECT state FROM disks WHERE id = 't2'"); got != "readonly" {
		t.Errorf("t2 is %s in the catalog", got)
	}
}

// The first confirmation of such a disk in a pass may be the one its copy job asks for: the job then writes nothing to
// it, the file waits degraded, and the line says why (specs.md §10, §14.1).
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
	if line := lineOf(out, "t2"); line == nil || line.Verdict.Reason != disks.NoMarker {
		t.Errorf("the line of t2: %+v", line)
	}
}
