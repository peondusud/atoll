package catalog

import (
	"path/filepath"
	"slices"
	"testing"
)

// The names directly below a folder, for Atoll's own view (specs.md §21.2): each once, whether a file or an entry
// holds it, on one disk or several, and whatever lies below it, with names that sort around '/' on each side of it.
func TestViewNames(t *testing.T) {
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
	for i, id := range []string{"t1", "t2"} {
		list = append(list, Disk{ID: id, UUID: "u-" + id, FS: "ext4", Mount: "/mnt/" + id, Root: "media", Workdir: "Atoll", Role: "data", Position: i, State: "online"})
	}
	if err := SyncDisks(t.Context(), db, list); err != nil {
		t.Fatal(err)
	}
	files := []string{"A/x.mkv", "A/y/z.mkv", "A.mkv", "A-b/c.mkv", "B/x.mkv", "top.mkv", "A/y.mkv", "A/y/deep/er.mkv"}
	for i, p := range files {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO files(id, lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns)
			VALUES (?, ?, 1, 1, 1, 'deferred', 'ok', 0, 0)`, i+1, []byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []struct{ path, disk, kind string }{
		{"A", "t1", "dir"}, {"A", "t2", "dir"}, {"A/y", "t1", "dir"}, {"A/empty", "t2", "dir"}, {"C", "t2", "dir"}, {"A/link", "t1", "symlink"}, {"A/y/deep", "t1", "dir"},
	} {
		if _, err := db.ExecContext(t.Context(), "INSERT INTO entries(lpath, disk_id, kind, created_by) VALUES (?, ?, ?, 'user')", []byte(e.path), e.disk, e.kind); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		dir  string
		want []string
	}{
		{"", []string{"A", "A-b", "A.mkv", "B", "C", "top.mkv"}},
		{"A", []string{"empty", "link", "x.mkv", "y", "y.mkv"}},
		{"A/y", []string{"deep", "z.mkv"}},
		{"A/y/deep", []string{"er.mkv"}},
		{"A/empty", []string{}},
		{"nowhere", []string{}},
	} {
		got, err := ViewNames(t.Context(), db, tc.dir)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("below %q: %q, want %q", tc.dir, got, tc.want)
		}
	}
	f, entries, err := ViewRows(t.Context(), db, "A/x.mkv")
	if err != nil || f == nil || len(f.Copies) != 0 || len(entries) != 0 {
		t.Errorf("a file with no copy: %+v, %+v, %v", f, entries, err)
	}
	f, entries, err = ViewRows(t.Context(), db, "A")
	if err != nil || f != nil || len(entries) != 2 || entries[0].Disk != "t1" || entries[1].Kind != "dir" {
		t.Errorf("a folder on two disks: %+v, %+v, %v", f, entries, err)
	}
}

// namesBelow reads the tables of the view alone, each by a query written whole, never built: a table it does not know
// is an error, whatever its name holds (scrutineer, semgrep's string-formatted-query, 2026-11-22).
func TestNamesBelowReadsOnlyItsTables(t *testing.T) {
	file := filepath.Join(t.TempDir(), "catalog.db")
	if _, err := Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	db, err := OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, table := range []string{"ops", "files WHERE 1 = 1 --", ""} {
		if err := namesBelow(t.Context(), db, table, "", map[string]bool{}); err == nil {
			t.Errorf("%q was read", table)
		}
	}
	for _, table := range []string{"files", "entries"} {
		for _, prefix := range []string{"", "A/"} {
			if err := namesBelow(t.Context(), db, table, prefix, map[string]bool{}); err != nil {
				t.Errorf("%s below %q: %v", table, prefix, err)
			}
		}
	}
}
