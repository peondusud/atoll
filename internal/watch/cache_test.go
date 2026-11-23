package watch

import (
	"database/sql"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
)

// A file's residency on the SSD cache began at the earliest change time of its copies there that hold its current
// version: a mirror copy is made after the copy the file was written to, and a stale copy holds an older version, whose
// time says nothing of the current one (specs.md §19). With none in step, the earliest of them all; with no change time
// recorded, a copy's first sight.
func TestAResidencyBeginsAtTheCurrentVersion(t *testing.T) {
	c, err := config.Parse([]byte("pool_id = \"p\"\n[view]\nengine = \"atoll\"\nmount = \"/srv/pool\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	at := func(ns int64) sql.NullInt64 { return sql.NullInt64{Int64: ns, Valid: true} }
	for _, tc := range []struct {
		name   string
		copies []catalog.CacheCopy
		since  int64
	}{
		{"one copy", []catalog.CacheCopy{{Disk: "c1", State: "ok", CtimeNS: at(500), FirstSeenNS: 100}}, 500},
		{"a mirror made after it", []catalog.CacheCopy{{Disk: "c1", State: "ok", CtimeNS: at(500)}, {Disk: "c2", State: "ok", CtimeNS: at(900)}}, 500},
		{"a stale copy, older", []catalog.CacheCopy{{Disk: "c1", State: "stale", CtimeNS: at(200)}, {Disk: "c2", State: "ok", CtimeNS: at(900)}}, 900},
		{"none in step", []catalog.CacheCopy{{Disk: "c1", State: "stale", CtimeNS: at(200)}, {Disk: "c2", State: "corrupt", CtimeNS: at(100)}}, 100},
		{"none in step, the earliest first", []catalog.CacheCopy{{Disk: "c1", State: "stale", CtimeNS: at(100)}, {Disk: "c2", State: "corrupt", CtimeNS: at(200)}}, 100},
		{"two in step, the earliest last", []catalog.CacheCopy{{Disk: "c1", State: "ok", CtimeNS: at(900)}, {Disk: "c2", State: "surplus", CtimeNS: at(500)}}, 500},
		{"no change time recorded", []catalog.CacheCopy{{Disk: "c1", State: "ok", FirstSeenNS: 300}}, 300},
	} {
		for i := range tc.copies {
			tc.copies[i].FileID, tc.copies[i].Path, tc.copies[i].Size = 1, "Films/a.mkv", 7
		}
		if f := cacheFile(c, tc.copies, nil); f.SinceNS != tc.since || len(f.Disks) != len(tc.copies) {
			t.Errorf("%s: since %d on %q, want %d", tc.name, f.SinceNS, f.Disks, tc.since)
		}
	}
}
