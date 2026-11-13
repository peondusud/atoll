package layout

import (
	"errors"
	"testing"
)

// A marker is four keys in one order, and reads back as it was written; anything else is refused (specs.md §10).
func TestMarker(t *testing.T) {
	m := Marker{PoolID: "home-nas", DiskID: "u2", FSUUID: "7a2b4c6d-8e0f-4a1b-9c2d-3e4f5a6b7c8d"}
	text, err := MarkerText(m)
	if err != nil {
		t.Fatal(err)
	}
	if want := "format_version = 1\npool_id = \"home-nas\"\ndisk_id = \"u2\"\nfs_uuid = \"7a2b4c6d-8e0f-4a1b-9c2d-3e4f5a6b7c8d\"\n"; text != want {
		t.Errorf("%q", text)
	}
	if got, err := ParseMarker([]byte(text)); err != nil || got != m {
		t.Errorf("%+v, %v", got, err)
	}
	if _, err := MarkerText(Marker{PoolID: "p", DiskID: "Big", FSUUID: m.FSUUID}); err == nil {
		t.Error("a disk id that is no identifier")
	}
	// A pool's id is an identifier, as the configuration has it (specs.md §29.5): the marker took the grammar of a
	// time zone for it, and so ids no configuration can hold.
	for _, id := range []string{"home/nas", "Local", "Europe/Paris", ""} {
		if _, err := MarkerText(Marker{PoolID: id, DiskID: "u2", FSUUID: m.FSUUID}); err == nil {
			t.Errorf("a marker written for the pool %q", id)
		}
		text := "format_version = 1\npool_id = \"" + id + "\"\ndisk_id = \"u2\"\nfs_uuid = \"01D9F3A2B4C5E6F7\"\n"
		if _, err := ParseMarker([]byte(text)); !errors.Is(err, ErrNotAMarker) {
			t.Errorf("a marker read for the pool %q: %v", id, err)
		}
	}
	for _, bad := range []string{
		"format_version = 2\npool_id = \"p\"\ndisk_id = \"u2\"\nfs_uuid = \"01D9F3A2B4C5E6F7\"\n",
		"format_version = 1\npool_id = \"p\"\ndisk_id = \"u2\"\n",
		"format_version = 1\npool_id = \"p\"\ndisk_id = \"u2\"\nfs_uuid = \"01D9F3A2B4C5E6F7\"\nmore = 1\n",
		"format_version = 1\npool_id = \"p\"\ndisk_id = \"U2\"\nfs_uuid = \"01D9F3A2B4C5E6F7\"\n",
		"format_version = \"1\"\npool_id = \"p\"\ndisk_id = \"u2\"\nfs_uuid = \"01D9F3A2B4C5E6F7\"\n",
		"not toml",
	} {
		if _, err := ParseMarker([]byte(bad)); !errors.Is(err, ErrNotAMarker) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if m, err := ParseMarker([]byte("fs_uuid = \"01D9F3A2B4C5E6F7\"\ndisk_id = \"u1\"\npool_id = \"p\"\nformat_version = 1\n")); err != nil || m.DiskID != "u1" {
		t.Errorf("keys in another order: %+v, %v", m, err)
	}
}
