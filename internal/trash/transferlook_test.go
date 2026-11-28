package trash

import (
	"testing"

	"github.com/peondusud/atoll/internal/config"
)

// A transfer purges the copy the SSD cache sent only while each copy on a data disk the catalog calls ok is what its
// row says, as a look finds it right before the purge: one edited since leaves the copy in the cache's trash, for the
// next pass to decide from what the data disks hold; a copy the catalog does not call ok is not looked at, edited or
// not (specs.md §19; conformance/models/README.md, flaw 8, the maintainer's remedy).
func TestATransferPurgesOnlyWhileTheDataCopiesHold(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dress   func(b *bench)
		outcome string
		trashed int
	}{
		{"as their rows say", func(*bench) {}, OutcomeTrashed, 0},
		{"a data copy edited since its row", func(b *bench) { b.write("t1", "a", "edited since") }, OutcomeChanged, 1},
		{"a stale copy edited, beside one that holds", func(b *bench) {
			if _, err := b.db.ExecContext(b.t.Context(), "UPDATE copies SET state = 'stale' WHERE disk_id = 't2'"); err != nil {
				b.t.Fatal(err)
			}
			b.write("t2", "a", "edited since")
		}, OutcomeTrashed, 0},
		{"a stale copy edited, before one that holds", func(b *bench) {
			if _, err := b.db.ExecContext(b.t.Context(), "UPDATE copies SET state = 'stale' WHERE disk_id = 't1'"); err != nil {
				b.t.Fatal(err)
			}
			b.write("t1", "a", "edited since")
		}, OutcomeTrashed, 0},
		{"no data copy the catalog calls ok", func(b *bench) {
			if _, err := b.db.ExecContext(b.t.Context(), "UPDATE copies SET state = 'stale' WHERE disk_id IN ('t1', 't2')"); err != nil {
				b.t.Fatal(err)
			}
		}, OutcomeChanged, 1},
	} {
		b := newBench(t)
		tc.dress(b)
		s := b.opened()
		f := b.fileA()
		got, err := s.Transfer(t.Context(), f, copyOn(f, "t3"))
		if err != nil || got != tc.outcome || b.sourceThere() || b.count("SELECT count(*) FROM trash") != tc.trashed {
			t.Errorf("%s: %q, %v, at its place %v, %d in the trash", tc.name, got, err, b.sourceThere(), b.count("SELECT count(*) FROM trash"))
		}
	}
}

// A disk mounted with the type ntfs leaves its copies at a wildcard name as they are, as its mount check found it, and
// no other disk does: neither one of another type, nor one the session does not know, nor one the configuration gained
// since the session looked at its disks (specs.md §15.8).
func TestOnlyADiskOfTheTypeNTFSLeavesItsWildcardNames(t *testing.T) {
	b := newBenchOf(t, "ntfs")
	s := b.opened()
	for id, want := range map[string]bool{"t1": false, "t2": false, "t3": true, "t9": false} {
		if got := s.LeavesWildcards(id); got != want {
			t.Errorf("%s: %v", id, got)
		}
	}
	b.config.Disks = append(b.config.Disks, config.Disk{ID: "t9"})
	if s.LeavesWildcards("t9") {
		t.Error("a disk the configuration gained since, and it leaves its wildcard names")
	}
}
