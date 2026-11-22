package catalog

import (
	"maps"
	"path/filepath"
	"testing"
)

// A disk's own marker, once a confirmation, disk add or a rebuild found it, is recorded when it was first seen, and
// kept by every sync of the disks; one found on another filesystem records nothing, and a disk whose id comes back
// with another identifier is another disk, which starts with none (specs.md §9, §10).
func TestTheMarkerSeenIsRecordedOnce(t *testing.T) {
	file := filepath.Join(t.TempDir(), "catalog.db")
	if _, err := Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	db, err := OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	list := []Disk{
		{ID: "t1", UUID: "u1", FS: "ext4", Mount: "/mnt/t1", Root: "media", Workdir: "Atoll", Role: "data", State: "online"},
		{ID: "t2", UUID: "u2", FS: "ext4", Mount: "/mnt/t2", Root: "media", Workdir: "Atoll", Role: "data", State: "online", Position: 1},
	}
	if err := SyncDisks(t.Context(), db, list); err != nil {
		t.Fatal(err)
	}
	seen := func() map[string]int64 {
		t.Helper()
		got, err := Disks(t.Context(), db)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int64{}
		for _, d := range got {
			if d.MarkerSeen {
				out[d.ID] = d.MarkerSeenNS
			}
		}
		return out
	}
	if got := seen(); len(got) != 0 {
		t.Errorf("before any marker: %v", got)
	}
	for _, at := range []int64{5, 9} {
		if err := SetMarkerSeen(t.Context(), db, "t1", "u1", at); err != nil {
			t.Fatal(err)
		}
	}
	if err := SetMarkerSeen(t.Context(), db, "t2", "u9", 7); err != nil {
		t.Fatal(err)
	}
	if got := seen(); !maps.Equal(got, map[string]int64{"t1": 5}) {
		t.Errorf("seen twice, and on another filesystem: %v", got)
	}
	if err := SyncDisks(t.Context(), db, list); err != nil {
		t.Fatal(err)
	}
	if got := seen(); !maps.Equal(got, map[string]int64{"t1": 5}) {
		t.Errorf("after a sync: %v", got)
	}
	// The id t1 given to another filesystem, the old one holding nothing: its row is made again, with no marker seen.
	list[0].UUID = "u3"
	if err := SyncDisks(t.Context(), db, list); err != nil {
		t.Fatal(err)
	}
	if got := seen(); len(got) != 0 {
		t.Errorf("another filesystem under the id: %v", got)
	}
}
