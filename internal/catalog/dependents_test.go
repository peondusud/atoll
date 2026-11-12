package catalog

import (
	"maps"
	"slices"
	"testing"
)

// A file depends on a disk when it has a copy there, ok or unavailable, and fewer copies ok elsewhere on disks
// online, in standby or read-only than its level, or when it is in conflict. Copies on a disk draining, failed or
// missing count for nothing, and a stale or surplus copy on the disk asks nothing of it (specs.md §18).
func TestReadDiskHolds(t *testing.T) {
	db, _ := newPool(t)
	ctx := t.Context()
	list := []Disk{oneDisk("x", "u1", 0), oneDisk("on", "u2", 1), oneDisk("ro", "u3", 2), oneDisk("dr", "u4", 3), oneDisk("gone", "u5", 4)}
	list[2].State, list[3].State, list[4].State = "readonly", "draining", "missing"
	if err := SyncDisks(ctx, db, list); err != nil {
		t.Fatal(err)
	}
	id := int64(0)
	file := func(path, state string, replicas int, copies map[string]string) {
		t.Helper()
		id++
		exec(t, db, `INSERT INTO files(id, lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns)
			VALUES (?, ?, 0, 0, ?, 'deferred', ?, 0, 0)`, id, []byte(path), replicas, state)
		for disk, copyState := range copies {
			exec(t, db, "INSERT INTO copies(file_id, disk_id, created_by, first_seen_ns, state) VALUES (?, ?, 'user', 0, ?)", id, disk, copyState)
		}
	}
	file("a/enough", "ok", 1, map[string]string{"x": "ok", "on": "ok", "ro": "ok"})
	file("b/one-short", "degraded", 1, map[string]string{"x": "ok", "on": "ok"})
	file("c/elsewhere-leaving", "ok", 1, map[string]string{"x": "ok", "dr": "ok", "gone": "ok", "on": "ok"})
	file("d/conflict", "conflict", 0, map[string]string{"x": "ok", "on": "ok", "ro": "ok"})
	file("e/stale-here", "ok", 0, map[string]string{"x": "stale", "on": "ok"})
	file("f/surplus-here", "ok", 0, map[string]string{"x": "surplus", "on": "ok"})
	file("g/only-here", "unprotected", 0, map[string]string{"x": "ok"})
	file("h/unavailable-here", "unavailable", 0, map[string]string{"x": "unavailable"})
	file("i/not-here", "degraded", 2, map[string]string{"on": "ok"})
	file("j/x0-elsewhere", "ok", 0, map[string]string{"x": "ok", "ro": "ok"})
	exec(t, db, "INSERT INTO ops(disk_id, kind, path, started_ns, state) VALUES ('x', 'trash', x'61', 0, 'planned'), ('x', 'trash', x'62', 0, 'done'), ('on', 'trash', x'63', 0, 'planned')")
	h, err := ReadDiskHolds(ctx, db, "x", 3)
	if err != nil {
		t.Fatal(err)
	}
	if h.Depending != 5 || !slices.Equal(h.First, []string{"b/one-short", "c/elsewhere-leaving", "d/conflict"}) {
		t.Errorf("depending %d: %q", h.Depending, h.First)
	}
	if all, _ := ReadDiskHolds(ctx, db, "x", 10); !slices.Equal(all.First, []string{"b/one-short", "c/elsewhere-leaving", "d/conflict", "g/only-here", "h/unavailable-here"}) {
		t.Errorf("all depending: %q", all.First)
	}
	if h.OnlyThere != 2 || h.Planned != 1 {
		t.Errorf("only there %d, planned %d", h.OnlyThere, h.Planned)
	}
	if want := map[string]int64{"ok": 6, "stale": 1, "surplus": 1, "unavailable": 1}; !maps.Equal(h.Copies, want) {
		t.Errorf("copies %v", h.Copies)
	}
	if h, _ := ReadDiskHolds(ctx, db, "nothing", 3); h.Depending != 0 || h.First != nil || len(h.Copies) != 0 {
		t.Errorf("a disk with nothing: %+v", h)
	}
}
