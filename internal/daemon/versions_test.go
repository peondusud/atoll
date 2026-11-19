package daemon

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/alerts"
	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/trash"
)

// smallDisks is flushDisks whose disks hold 1,000 bytes each, 800 of them free: an entry of 100 bytes fills a trash of
// trash.max_size = "5%".
type smallDisks struct {
	*flushDisks
}

func (*smallDisks) Space(string) (capacity, free int64, err error) { return 1000, 800, nil }

// The versions a modification replaced that the trash purged for room before their retention ended are counted on
// their disk, with their first paths in byte order: entries purged at their time, for another reason, or not purged,
// are not. The alert holds for a day after the last of them, so that a channel that could not take it tries again,
// then is forgotten (specs.md §17, §23).
func TestVersionsPurgedEarlyAreCounted(t *testing.T) {
	l := &loop{}
	const hour = int64(time.Hour)
	entry := func(disk, path, reason, outcome, why string) *trash.EntryItem {
		return &trash.EntryItem{TrashEntry: catalog.TrashEntry{Disk: disk, Path: path, Reason: reason}, Outcome: outcome, Why: why}
	}
	for _, e := range []*trash.EntryItem{
		entry("t2", "b", catalog.ReasonOverwritten, trash.OutcomePurged, "max-size"),
		entry("t2", "a", catalog.ReasonOverwritten, trash.OutcomePurged, "max-size"),
		entry("t2", "c", catalog.ReasonOverwritten, trash.OutcomePurged, "expired"),
		entry("t2", "d", "quarantined", trash.OutcomePurged, "max-size"),
		entry("t2", "e", "surplus", trash.OutcomePurged, "max-size"),
		entry("t1", "f", catalog.ReasonOverwritten, trash.OutcomeChanged, "max-size"),
	} {
		l.notePurged(e, 0)
	}
	l.notePurged(entry("t1", "g", catalog.ReasonOverwritten, trash.OutcomePurged, "max-size"), 10*hour)
	got := l.versionsPurged(23 * hour)
	want := []alerts.VersionsPurged{{ID: "t1", Paths: alerts.Refused{Count: 1, First: []string{"g"}}}, {ID: "t2", Paths: alerts.Refused{Count: 2, First: []string{"a", "b"}}}}
	if len(got) != len(want) || got[0].ID != "t1" || got[1].ID != "t2" || got[0].Paths.Count != 1 || got[1].Paths.Count != 2 ||
		!slices.Equal(got[1].Paths.First, want[1].Paths.First) {
		t.Errorf("%+v, want %+v", got, want)
	}
	// A day after the last on t2, its alert ends; t1's holds until a day after its own.
	if got := l.versionsPurged(24 * hour); len(got) != 1 || got[0].ID != "t1" {
		t.Errorf("a day on: %+v", got)
	}
	if got := l.versionsPurged(34 * hour); len(got) != 0 || len(l.versions) != 0 {
		t.Errorf("a day after the last: %+v, %v", got, l.versions)
	}
	// Once ended, it starts again from nothing.
	l.notePurged(entry("t2", "z", catalog.ReasonOverwritten, trash.OutcomePurged, "max-size"), 40*hour)
	if got := l.versionsPurged(40 * hour); len(got) != 1 || got[0].Paths.Count != 1 {
		t.Errorf("again: %+v", got)
	}
}

// A modification makes the other copy stale: it goes to its disk's trash as overwritten, and the purge after the pass
// takes it for room, as that trash holds more than trash.max_size. The alert says so, which the metrics give
// (specs.md §17, §23, §23.1).
func TestTheDaemonTellsOfVersionsPurgedEarly(t *testing.T) {
	window := aroundNow()
	c, m, _ := upsBench(t, "[maintenance]\nwindow = \""+window+"\"\n[modifies.mass_guard]\npercent = \"100%\"\n"+
		"[[rule]]\nlabel = \"all\"\npath = \"**\"\nmode = \"immediate\"\n")
	machine := &smallDisks{flushDisks: m}
	a := filepath.Join(c.Disks[0].Mount, "media", "a")
	if err := os.WriteFile(a, []byte(strings.Repeat("x", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	file, _ := c.Effective()["catalog"].(string)
	db, err := catalog.OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := jobs.Run(t.Context(), db, c, machine, true); err != nil {
			t.Fatal(err)
		}
	}
	var copies int
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM copies WHERE state = 'ok'").Scan(&copies); err != nil || copies != 2 {
		t.Fatalf("%d copies of a, %v", copies, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a, []byte(strings.Repeat("y", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(a, later, later); err != nil {
		t.Fatal(err)
	}
	rec := runMetrics(t, c, machine)
	waitMetrics(t, rec, "the old version purged for room", `atoll_alert{alert="versions-purged",disk="t2"} 1`)
}
