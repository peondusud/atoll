package catalog

import (
	"slices"
	"strings"
	"testing"
)

// The stale copies that lost their trash job are those whose overwrite would stand (specs.md §15.4, §15.12): of a
// file neither in conflict nor corrupt, with a copy to make them again from, that hold a file and have no trash job
// queued. The source is the first copy ok or surplus by disk position. And the question reads the stale copies, never
// every copy of the pool, as each pass asks it.
func TestStaleWithoutJob(t *testing.T) {
	b := openForOps(t)
	for i, id := range []string{"u1", "u2", "u3"} {
		// u3 comes first by position, whatever its name.
		b.exec("INSERT INTO disks(id, fs_uuid, fs_type, mount_path, root, workdir, role, position, tags, state) VALUES (?, ?, 'ext4', ?, 'media', 'Atoll', 'data', ?, '[]', 'online')",
			id, id, "/"+id, 2-i)
	}
	file := func(id int, path, state string) {
		b.exec("INSERT INTO files(id, lpath, lpath_key, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns) VALUES (?, CAST(? AS BLOB), ?, 5, 1, 1, 'deferred', ?, 1, 1)",
			id, path, strings.ToUpper(path), state)
	}
	copyOf := func(id int, disk, state string, key any) {
		b.exec("INSERT INTO copies(file_id, disk_id, created_by, first_seen_ns, state, file_key) VALUES (?, ?, 'user', 1, ?, ?)", id, disk, state, key)
	}
	// 1: a stale copy that lost its job, with two copies to make it again from.
	file(1, "a", "degraded")
	copyOf(1, "u1", "stale", "11")
	copyOf(1, "u2", "ok", "12")
	copyOf(1, "u3", "surplus", "13")
	// 2: its job is queued; 3: in conflict; 4: nothing to make it again from; 5: it holds no file; 6: a job that was
	// canceled covers nothing.
	file(2, "b", "degraded")
	copyOf(2, "u1", "stale", "21")
	copyOf(2, "u2", "ok", "22")
	b.exec("INSERT INTO jobs(kind, file_id, src_disk, dst_disk, state, reason) VALUES ('trash', 2, 'u2', 'u1', 'queued', 'deleted')")
	file(3, "c", "conflict")
	copyOf(3, "u1", "stale", "31")
	copyOf(3, "u2", "ok", "32")
	file(4, "d", "degraded")
	copyOf(4, "u1", "stale", "41")
	copyOf(4, "u2", "pending", nil)
	file(5, "e", "degraded")
	copyOf(5, "u1", "stale", nil)
	copyOf(5, "u2", "ok", "52")
	file(6, "f", "degraded")
	copyOf(6, "u2", "stale", "61")
	copyOf(6, "u1", "ok", "62")
	b.exec("INSERT INTO jobs(kind, file_id, src_disk, dst_disk, state, reason) VALUES ('trash', 6, 'u2', 'u2', 'canceled', 'renamed')")

	all, err := StaleWithoutJob(t.Context(), b.db, "")
	want := []StaleCopy{{FileID: 1, Path: "a", Size: 5, Disk: "u1", Src: "u3"}, {FileID: 6, Path: "f", Size: 5, Disk: "u2", Src: "u1"}}
	if err != nil || !slices.Equal(all, want) {
		t.Errorf("%+v, %v", all, err)
	}
	if one, err := StaleWithoutJob(t.Context(), b.db, "u2"); err != nil || !slices.Equal(one, want[1:]) {
		t.Errorf("on u2: %+v, %v", one, err)
	}
	// Its job queued, a copy is asked about no more, and the job is queued once.
	for range 2 {
		if err := QueueOverwrite(t.Context(), b.db, 1, "u3", "u1", 7); err != nil {
			t.Fatal(err)
		}
	}
	var job string
	if err := b.db.QueryRowContext(t.Context(), "SELECT group_concat(src_disk || ' ' || dst_disk || ' ' || priority || ' ' || state || ' ' || reason) FROM jobs WHERE file_id = 1").Scan(&job); err != nil || job != "u3 u1 7 queued overwritten" {
		t.Errorf("the job: %s, %v", job, err)
	}
	if left, err := StaleWithoutJob(t.Context(), b.db, ""); err != nil || !slices.Equal(left, want[1:]) {
		t.Errorf("then: %+v, %v", left, err)
	}
	// A copy that is not stale, or holds no file, gets none.
	for _, c := range []struct {
		file int64
		disk string
	}{{1, "u2"}, {5, "u1"}} {
		if err := QueueOverwrite(t.Context(), b.db, c.file, "u3", c.disk, 0); err != nil {
			t.Fatal(err)
		}
		if n := b.count("SELECT count(*) FROM jobs WHERE file_id = ? AND dst_disk = ?", c.file, c.disk); n != 0 {
			t.Errorf("file %d on %s: %d jobs", c.file, c.disk, n)
		}
	}
	for _, disk := range []string{"", "u1"} {
		plan := queryPlan(t, b, staleNoJobSQL, disk)
		if !slices.ContainsFunc(plan, func(line string) bool { return strings.Contains(line, "copies_stale") }) ||
			slices.ContainsFunc(plan, func(line string) bool {
				return strings.HasPrefix(line, "SCAN c") && !strings.Contains(line, "copies_stale")
			}) {
			t.Errorf("disk %q: %q", disk, plan)
		}
	}
}
