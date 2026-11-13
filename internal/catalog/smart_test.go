package catalog

import (
	"path/filepath"
	"testing"
)

// A reading of SMART is kept with its disk, read back by Disks, and kept when the configuration is synced again;
// a reading that is not a JSON object, or of a disk the catalog does not have, is refused (specs.md §23).
func TestSetSmart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "catalog.db")
	if _, err := Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	db, err := OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	list := []Disk{{ID: "t1", UUID: "u1", FS: "ext4", Mount: "/mnt/t1", Root: "media", Workdir: "Atoll", Role: "data", State: "online"}}
	if err := SyncDisks(t.Context(), db, list); err != nil {
		t.Fatal(err)
	}
	reading := `{"read_ns":5,"failing":false,"reallocated":8,"pending":0,"uncorrectable":0,"temperature":41}`
	if err := SetSmart(t.Context(), db, "t1", []byte(reading)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "[]", "{", "8"} {
		if err := SetSmart(t.Context(), db, "t1", []byte(bad)); err == nil {
			t.Errorf("%q taken", bad)
		}
	}
	if err := SetSmart(t.Context(), db, "t9", []byte(reading)); err == nil {
		t.Error("a disk the catalog does not have")
	}
	if err := SyncDisks(t.Context(), db, list); err != nil {
		t.Fatal(err)
	}
	got, err := Disks(t.Context(), db)
	if err != nil || len(got) != 1 || got[0].SmartJSON != reading {
		t.Errorf("%+v, %v", got, err)
	}
}
