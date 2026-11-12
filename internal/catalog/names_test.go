package catalog

import (
	"slices"
	"testing"
)

// AllPaths gives the paths of the files and of the entries, each once, in byte order, and never the root's.
func TestAllPaths(t *testing.T) {
	db, _ := newPool(t)
	if err := SyncDisks(t.Context(), db, []Disk{oneDisk("t1", "u1", 0), oneDisk("t2", "u2", 1)}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"b/x", "B", "\xff"} {
		exec(t, db, `INSERT INTO files(lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns)
			VALUES (?, 0, 0, 1, 'deferred', 'ok', 0, 0)`, []byte(p))
	}
	for _, e := range []struct{ path, disk string }{{"b", "t1"}, {"b", "t2"}, {"", "t1"}, {"a", "t2"}} {
		exec(t, db, "INSERT INTO entries(lpath, disk_id, kind, created_by) VALUES (?, ?, 'dir', 'user')", []byte(e.path), e.disk)
	}
	got, err := AllPaths(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"B", "a", "b", "b/x", "\xff"}; !slices.Equal(got, want) {
		t.Errorf("%q", got)
	}
}
