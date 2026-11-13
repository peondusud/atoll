package catalog

import "testing"

// TestHandOver: a user copy's part goes to another copy of its file, ok or surplus, in one transaction; a copy that is
// not there, or not in step, takes nothing, and the user copy keeps its part (specs.md §13.3).
func TestHandOver(t *testing.T) {
	for _, c := range []struct {
		name, heir, heirState, want string
	}{
		{"to an atoll copy", "u2", "ok", "u1 atoll, u2 user, u3 atoll"},
		{"to a surplus copy", "u2", "surplus", "u1 atoll, u2 user, u3 atoll"},
		{"to a pending copy", "u2", "pending", "u1 user, u2 atoll, u3 atoll"},
		{"to a disk with no copy", "u4", "ok", "u1 user, u2 atoll, u3 atoll"},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := openForOps(t)
			for i, id := range []string{"u1", "u2", "u3", "u4"} {
				b.exec(`INSERT INTO disks(id, fs_uuid, fs_type, mount_path, root, workdir, role, position, tags, state) VALUES (?, ?, 'ext4', ?, 'media', 'Atoll', 'data', ?, '[]', 'online')`,
					id, id, "/"+id, i)
			}
			b.exec("INSERT INTO files(id, lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns) VALUES (1, ?, 1, 1, 1, 'deferred', 'ok', 1, 1)", []byte("x"))
			b.exec("INSERT INTO copies(file_id, disk_id, file_key, nlink, size, mtime_ns, ctime_ns, created_by, first_seen_ns, state) VALUES (1, 'u1', '7', 1, 1, 1, 1, 'user', 1, 'ok')")
			b.exec("INSERT INTO copies(file_id, disk_id, file_key, nlink, size, mtime_ns, ctime_ns, created_by, first_seen_ns, state) VALUES (1, 'u2', '8', 1, 1, 1, 1, 'atoll', 1, ?)", c.heirState)
			b.exec("INSERT INTO copies(file_id, disk_id, file_key, nlink, size, mtime_ns, ctime_ns, created_by, first_seen_ns, state) VALUES (1, 'u3', '9', 1, 1, 1, 1, 'atoll', 1, 'ok')")
			if err := HandOver(t.Context(), b.sql(), 1, "u1", c.heir); err != nil {
				t.Fatal(err)
			}
			var got string
			if err := b.sql().QueryRowContext(t.Context(), "SELECT group_concat(line, ', ') FROM (SELECT disk_id || ' ' || created_by AS line FROM copies ORDER BY disk_id)").Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("%q, want %q", got, c.want)
			}
		})
	}
}
