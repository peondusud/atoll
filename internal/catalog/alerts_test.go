package catalog

import (
	"maps"
	"slices"
	"testing"
)

// The table alerts keeps one row a key: a row put again replaces the one before, and a key dropped goes; nothing
// else changes (specs.md §23).
func TestAlertRows(t *testing.T) {
	db, _ := newPool(t)
	ctx := t.Context()
	if rows, err := ReadAlerts(ctx, db); err != nil || len(rows) != 0 {
		t.Fatalf("a new catalog: %v, %v", rows, err)
	}
	put := []AlertRow{{"pool-full", 5, 5, 1}, {"files-corrupt", 1, 2, 3}, {"disk-missing:t1", 4, 4, 1}}
	if err := WriteAlerts(ctx, db, put, nil); err != nil {
		t.Fatal(err)
	}
	if err := WriteAlerts(ctx, db, []AlertRow{{"files-corrupt", 1, 9, 7}}, []string{"pool-full", "never-there"}); err != nil {
		t.Fatal(err)
	}
	rows, err := ReadAlerts(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if want := []AlertRow{{"disk-missing:t1", 4, 4, 1}, {"files-corrupt", 1, 9, 7}}; !slices.Equal(rows, want) {
		t.Errorf("%+v", rows)
	}
	if err := WriteAlerts(ctx, db, []AlertRow{{"x", 1, 1, -1}}, nil); err == nil {
		t.Error("a negative count went in")
	}
	if rows, _ := ReadAlerts(ctx, db); len(rows) != 2 {
		t.Errorf("a failed write left %+v", rows)
	}
}

// One read counts the files of each state that raises an alert, and the files of mode manual still unprotected
// first seen at or before a time; the first paths come in byte order (specs.md §23).
func TestCountFileAlerts(t *testing.T) {
	db, _ := newPool(t)
	ctx := t.Context()
	add := func(path, mode, state string, seen int64) {
		t.Helper()
		exec(t, db, `INSERT INTO files(lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns)
			VALUES (?, 0, 0, 1, ?, ?, ?, 0)`, []byte(path), mode, state, seen)
	}
	add("b/corrupt", "deferred", "corrupt", 0)
	add("B/corrupt", "deferred", "corrupt", 0)
	add("\xff/corrupt", "deferred", "corrupt", 0)
	add("é/corrupt", "deferred", "corrupt", 0)
	add("a/conflict", "deferred", "conflict", 0)
	add("a/collision", "manual", "name-collision", 0)
	add("a/unavailable", "deferred", "unavailable", 0)
	add("a/ok", "deferred", "ok", 0)
	add("a/pending", "deferred", "pending", 0)
	add("a/degraded", "deferred", "degraded", 0)
	add("m/old", "manual", "unprotected", 100)
	add("m/just", "manual", "unprotected", 200)
	add("m/new", "manual", "unprotected", 201)
	add("m/kept", "manual", "ok", 0)
	add("d/unprotected", "deferred", "unprotected", 0)
	got, err := CountFileAlerts(ctx, db, 200)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"corrupt": 4, "conflict": 1, "name-collision": 1, "unavailable": 1}
	if !maps.Equal(got.ByState, want) || got.Manual != 2 {
		t.Errorf("%+v", got)
	}
	paths, err := FirstStatePaths(ctx, db, "corrupt", 3)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"B/corrupt", "b/corrupt", "é/corrupt"}; !slices.Equal(paths, want) {
		t.Errorf("first corrupt: %q", paths)
	}
	if paths, _ := FirstStatePaths(ctx, db, "corrupt", 10); len(paths) != 4 || paths[3] != "\xff/corrupt" {
		t.Errorf("all corrupt: %q", paths)
	}
	if paths, _ := FirstManualPaths(ctx, db, 200, 10); !slices.Equal(paths, []string{"m/just", "m/old"}) {
		t.Errorf("manual: %q", paths)
	}
	if paths, _ := FirstStatePaths(ctx, db, "name-incompatible", 10); len(paths) != 0 {
		t.Errorf("none: %q", paths)
	}
}
