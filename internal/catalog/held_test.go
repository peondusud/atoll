package catalog

import (
	"path/filepath"
	"slices"
	"testing"
)

// A conflict found on a file that is corrupt is held, as the file shows corrupt first (specs.md §4, §16): the file
// stays corrupt and holds it, atoll conflicts list gives it, a corrupt copy found on a file in conflict keeps the
// conflict, and a state other than corrupt lets it go, as the conflict is then the state or was resolved. The
// conflict was lost: a file corrupt kept nothing of it.
func TestAConflictOnACorruptFileIsHeld(t *testing.T) {
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
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	held := func() string {
		t.Helper()
		var got string
		if err := db.QueryRowContext(t.Context(), `SELECT group_concat(CAST(lpath AS TEXT) || ' ' || state || ' ' || conflict_held, ',')
			FROM (SELECT * FROM files ORDER BY lpath)`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	exec(`INSERT INTO files(id, lpath, size, mtime_ns, blake3, replicas_target, mode, state, first_seen_ns, updated_ns)
		VALUES (1, x'61', 2, 2, zeroblob(32), 1, 'deferred', 'corrupt', 0, 0), (2, x'62', 2, 2, zeroblob(32), 1, 'deferred', 'conflict', 0, 0),
			(3, x'63', 2, 2, zeroblob(32), 1, 'deferred', 'ok', 0, 0)`)
	exec(`INSERT INTO copies(file_id, disk_id, size, mtime_ns, created_by, first_seen_ns, verified_ns, state)
		VALUES (1, 't1', 2, 2, 'user', 0, 9, 'ok'), (1, 't2', 2, 2, 'user', 0, NULL, 'corrupt'), (2, 't1', 2, 2, 'user', 0, NULL, 'ok'),
			(2, 't2', 2, 2, 'user', 0, NULL, 'ok'), (3, 't1', 2, 2, 'user', 0, 9, 'ok'), (3, 't2', 2, 2, 'user', 0, 9, 'ok')`)
	if got := held(); got != "a corrupt 0,b conflict 0,c ok 0" {
		t.Fatalf("at first: %s", got)
	}
	for _, id := range []int64{1, 3} {
		if err := SetConflict(t.Context(), db, id, 5); err != nil {
			t.Fatal(err)
		}
	}
	if got := held(); got != "a corrupt 1,b conflict 0,c conflict 0" {
		t.Errorf("conflicts found: %s", got)
	}
	var paths []string
	for _, f := range mustConflictFiles(t, NewFileReader(db)) {
		paths = append(paths, f.Path+" "+f.State)
		if f.ConflictHeld != (f.State == "corrupt") {
			t.Errorf("%s: held %v", f.Path, f.ConflictHeld)
		}
	}
	if !slices.Equal(paths, []string{"a corrupt", "b conflict", "c conflict"}) {
		t.Errorf("listed: %q", paths)
	}
	// A copy of a file in conflict found corrupt: the file shows corrupt, and holds the conflict.
	if err := MarkCopyCorrupt(t.Context(), db, 2, "t1", "b", 6); err != nil {
		t.Fatal(err)
	}
	if got := held(); got != "a corrupt 1,b corrupt 1,c conflict 0" {
		t.Errorf("b found corrupt: %s", got)
	}
	// A plan that keeps corrupt keeps what is held; any other state lets it go, the conflict among them.
	if err := ApplyPlan(t.Context(), db, 7, nil, []StateChange{{ID: 1, State: "corrupt"}, {ID: 2, State: "conflict"}}, nil); err != nil {
		t.Fatal(err)
	}
	if got := held(); got != "a corrupt 1,b conflict 0,c conflict 0" {
		t.Errorf("a plan: %s", got)
	}
	update := &FileUpdate{ID: 1, Size: 2, MtimeNS: 2, ReplicasTarget: 1, Mode: "deferred", State: "conflict"}
	if err := ApplyPlan(t.Context(), db, 8, []*FileUpdate{update}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := held(); got != "a conflict 0,b conflict 0,c conflict 0" {
		t.Errorf("a plan that changed a: %s", got)
	}
}

func mustConflictFiles(t *testing.T, r *FileReader) []FileRow {
	t.Helper()
	var all []FileRow
	for {
		page, err := ConflictFiles(t.Context(), r)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			return all
		}
		all = append(all, page...)
	}
}
