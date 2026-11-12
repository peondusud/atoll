package catalog

import (
	"path/filepath"
	"slices"
	"testing"
)

// A folder is held while a file below it, at any depth, has a copy on a missing disk; the top of the view holds
// every file, and a folder whose name only begins like another's holds nothing of it (specs.md §21.4).
func TestFoldersWithCopiesOn(t *testing.T) {
	file := filepath.Join(t.TempDir(), "catalog.db")
	if _, err := Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	db, err := OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var list []Disk
	for i, id := range []string{"t1", "t2", "t3"} {
		list = append(list, Disk{ID: id, UUID: "u-" + id, FS: "ext4", Mount: "/mnt/" + id, Root: "media", Workdir: "Atoll", Role: "data", Position: i, State: "online"})
	}
	if err := SyncDisks(t.Context(), db, list); err != nil {
		t.Fatal(err)
	}
	// tv-old sorts between "tv" and "tv/": it is not below tv.
	for path, on := range map[string][]string{"tv/a/1.mkv": {"t1", "t2"}, "tv/b/2.mkv": {"t2"}, "tvx/3.mkv": {"t3"}, "tv-old/5.mkv": {"t3"},
		"films/4.mkv": {"t1"}} {
		res, err := db.ExecContext(t.Context(), `INSERT INTO files(lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns)
			VALUES (?, 0, 0, 1, 'deferred', 'ok', 0, 0)`, []byte(path))
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		for _, disk := range on {
			if _, err := db.ExecContext(t.Context(), `INSERT INTO copies(file_id, disk_id, created_by, first_seen_ns, state)
				VALUES (?, ?, 'user', 0, 'ok')`, id, disk); err != nil {
				t.Fatal(err)
			}
		}
	}
	folders := []string{"tv", "tv/a", "tv/b", "", "tvx", "films", "music"}
	for _, tc := range []struct {
		missing []string
		held    []string
	}{
		{[]string{"t3"}, []string{"", "tvx"}},
		{[]string{"t2"}, []string{"tv", "tv/a", "tv/b", ""}},
		{[]string{"t1", "t3"}, []string{"tv", "tv/a", "", "tvx", "films"}},
		{nil, nil},
	} {
		got, err := FoldersWithCopiesOn(t.Context(), db, folders, tc.missing)
		if err != nil || !slices.Equal(got, tc.held) {
			t.Errorf("%v missing: %q, %v, want %q", tc.missing, got, err, tc.held)
		}
	}
}
