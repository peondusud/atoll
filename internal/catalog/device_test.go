package catalog

import (
	"maps"
	"path/filepath"
	"testing"
)

// The physical disks a start found a disk on are kept with it, read back by Disks, kept when the configuration is
// synced again, and cleared by a start that found none (specs.md §10, R-06).
func TestSetDevice(t *testing.T) {
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
	device := func() map[string]string {
		t.Helper()
		got, err := Disks(t.Context(), db)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, d := range got {
			out[d.ID] = d.Device
		}
		return out
	}
	if got := device(); !maps.Equal(got, map[string]string{"t1": "", "t2": ""}) {
		t.Errorf("before any start: %v", got)
	}
	if err := SetDevice(t.Context(), db, "t1", []string{"sda", "sdb"}); err != nil {
		t.Fatal(err)
	}
	if err := SetDevice(t.Context(), db, "t2", []string{"sdc"}); err != nil {
		t.Fatal(err)
	}
	if err := SyncDisks(t.Context(), db, list); err != nil {
		t.Fatal(err)
	}
	if got := device(); !maps.Equal(got, map[string]string{"t1": "sda,sdb", "t2": "sdc"}) {
		t.Errorf("recorded: %v", got)
	}
	if err := SetDevice(t.Context(), db, "t2", nil); err != nil {
		t.Fatal(err)
	}
	if got := device(); !maps.Equal(got, map[string]string{"t1": "sda,sdb", "t2": ""}) {
		t.Errorf("found on none: %v", got)
	}
}

// Disks on one physical disk form a group, led by the first of them by position, whatever order they meet in; a disk
// alone, a disk no start looked at, and a retired disk are in none (R-06).
func TestSharedDevices(t *testing.T) {
	disk := func(id string, position int, device, state string) Disk {
		return Disk{ID: id, Position: position, Device: device, State: state}
	}
	for _, tc := range []struct {
		name string
		list []Disk
		want map[string]string
	}{
		{"none shares", []Disk{disk("a", 0, "sda", "online"), disk("b", 1, "sdb", "online"), disk("c", 2, "", "online")}, map[string]string{}},
		{"two on one", []Disk{disk("a", 0, "sda", "online"), disk("b", 1, "sdb", "online"), disk("c", 2, "sda", "missing")},
			map[string]string{"a": "a", "c": "a"}},
		// x and y meet only through z, which comes last: the group is led by x all the same.
		{"through a third", []Disk{disk("x", 0, "p", "online"), disk("y", 1, "q", "online"), disk("z", 2, "q,p", "online")},
			map[string]string{"x": "x", "y": "x", "z": "x"}},
		{"through a third, met the other way", []Disk{disk("x", 0, "q", "online"), disk("y", 1, "p", "online"), disk("z", 2, "p,q", "online")},
			map[string]string{"x": "x", "y": "x", "z": "x"}},
		// The list is not in the order of positions: the first by position leads.
		{"out of order", []Disk{disk("b", 3, "sda", "online"), disk("a", 1, "sda", "online")}, map[string]string{"a": "a", "b": "a"}},
		{"a retired disk holds nothing", []Disk{disk("a", 0, "sda", "retired"), disk("b", 1, "sda", "online")}, map[string]string{}},
		{"two groups", []Disk{disk("a", 0, "sda", "online"), disk("b", 1, "sdb", "online"), disk("c", 2, "sdb", "online"),
			disk("d", 3, "sda", "standby")}, map[string]string{"a": "a", "d": "a", "b": "b", "c": "b"}},
	} {
		if got := SharedDevices(tc.list); !maps.Equal(got, tc.want) {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
}
