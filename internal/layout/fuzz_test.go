package layout

import (
	"errors"
	"testing"
)

// FuzzMarker: Atoll/disk.toml is a file anyone who can write the disk can forge (specs.md §10). Whatever it holds,
// reading it gives a marker of the grammar's values or ErrNotAMarker, never a panic; a marker read, written again,
// reads the same; and a marker written from any values reads back as those values.
func FuzzMarker(f *testing.F) {
	const uuid = "7a2b4c6d-8e0f-4a1b-9c2d-3e4f5a6b7c8d"
	f.Add([]byte("format_version = 1\npool_id = \"home-nas\"\ndisk_id = \"u2\"\nfs_uuid = \""+uuid+"\"\n"), "home-nas", "u2", uuid)
	f.Add([]byte("format_version = 1\npool_id = \"p\"\ndisk_id = \"u2\"\nfs_uuid = \"01D9F3A2B4C5E6F7\"\nmore = 1\n"), "p", "U2", "01D9F3A2B4C5E6F7")
	f.Add([]byte("format_version = 1\r\npool_id = 'p'\r\ndisk_id = \"\"\"u2\"\"\"\r\n\"fs_uuid\" = \"01D9F3A2B4C5E6F7\" # a comment\r\n"), "p", "u2", "")
	f.Fuzz(func(t *testing.T, data []byte, pool, disk, fsUUID string) {
		m, err := ParseMarker(data)
		switch {
		case err != nil && !errors.Is(err, ErrNotAMarker):
			t.Fatalf("ParseMarker(%q): %v, not ErrNotAMarker", data, err)
		case err == nil:
			text, err := MarkerText(m)
			if err != nil {
				t.Fatalf("ParseMarker(%q) = %+v, which MarkerText refuses: %v", data, m, err)
			}
			if again, err := ParseMarker([]byte(text)); err != nil || again != m {
				t.Fatalf("ParseMarker(%q) = %+v, written %q, reads %+v, %v", data, m, text, again, err)
			}
		}
		want := Marker{PoolID: pool, DiskID: disk, FSUUID: fsUUID}
		text, err := MarkerText(want)
		if err != nil {
			return
		}
		if got, err := ParseMarker([]byte(text)); err != nil || got != want {
			t.Fatalf("MarkerText(%+v) = %q, which reads %+v, %v", want, text, got, err)
		}
	})
}
