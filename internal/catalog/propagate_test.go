package catalog

import "testing"

func newPropagationDB(t *testing.T) *testDB {
	t.Helper()
	b := openForOps(t)
	for i, id := range []string{"u1", "u2"} {
		b.exec(`INSERT INTO disks(id, fs_uuid, fs_type, mount_path, root, workdir, role, position, tags, state) VALUES (?, ?, 'ext4', ?, 'media', 'Atoll', 'data', ?, '[]', 'online')`,
			id, id, "/"+id, i)
	}
	b.exec("INSERT INTO files(id, lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns) VALUES (1, ?, 1, 1, 1, 'deferred', 'ok', 1, 1)", []byte("a"))
	return b
}

func (b *testDB) count(query string, args ...any) int {
	b.t.Helper()
	var n int
	if err := b.db.QueryRowContext(b.t.Context(), query, args...).Scan(&n); err != nil {
		b.t.Fatal(err)
	}
	return n
}

// A deletion decided again leaves the job queued for it as it is. A job for another reason, or one no longer
// queued, is not that job: the deletion queues its own (specs.md §15.12).
func TestATrashJobStandsOnlyForItsOwnDeletion(t *testing.T) {
	for _, tc := range []struct {
		name, state, reason string
		want                int
	}{
		{"queued for that deletion", JobQueued, "deleted", 1},
		{"queued for a rename", JobQueued, "renamed", 2},
		{"canceled", JobCanceled, "deleted", 2},
		{"done", "done", "deleted", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newPropagationDB(t)
			b.exec("INSERT INTO jobs(kind, file_id, src_disk, dst_disk, state, reason) VALUES ('trash', 1, 'u1', 'u2', ?, ?)", tc.state, tc.reason)
			change := FileChange{ID: 1, Path: "a", Src: "u1", Reason: "deleted", Rows: []RowChange{{Disk: "u2", Trash: true}}}
			if err := ApplyDecisions(t.Context(), b.sql(), 5, []FileChange{change}); err != nil {
				t.Fatal(err)
			}
			if got := b.count("SELECT count(*) FROM jobs"); got != tc.want {
				t.Errorf("%d jobs", got)
			}
			if got := b.count("SELECT count(*) FROM jobs WHERE state = ? AND reason = 'deleted'", JobQueued); got != 1 {
				t.Errorf("%d jobs queued for the deletion", got)
			}
		})
	}
}

// QueuedTrash speaks of one job only: queued, for that copy, for that reason.
func TestQueuedTrashSaysOnlyOfThatJob(t *testing.T) {
	for _, tc := range []struct {
		name, disk, state, reason string
		file                      int
		want                      bool
	}{
		{"that job", "u2", JobQueued, "deleted", 1, true},
		{"one for the copy on another disk", "u1", JobQueued, "deleted", 1, false},
		{"one no longer queued", "u2", JobCanceled, "deleted", 1, false},
		{"one for another reason", "u2", JobQueued, "renamed", 1, false},
		{"one of another file", "u2", JobQueued, "deleted", 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newPropagationDB(t)
			b.exec("INSERT INTO files(id, lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns) VALUES (2, ?, 1, 1, 1, 'deferred', 'ok', 1, 1)", []byte("b"))
			b.exec("INSERT INTO jobs(kind, file_id, src_disk, dst_disk, state, reason) VALUES ('trash', ?, 'u1', ?, ?, ?)", tc.file, tc.disk, tc.state, tc.reason)
			got, err := QueuedTrash(t.Context(), b.sql(), 1, "u2", "deleted")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("%v", got)
			}
		})
	}
}

// A folder renamed again gives each other disk its job, even where the same job once ran: one no longer queued is
// not that job (specs.md §15.12).
func TestAFolderJobNoLongerQueuedIsNotThatJob(t *testing.T) {
	for _, state := range []string{JobCanceled, "done", "failed"} {
		t.Run(state, func(t *testing.T) {
			b := newPropagationDB(t)
			payload, err := encodeFolderPayload("D", "E")
			if err != nil {
				t.Fatal(err)
			}
			b.exec("INSERT INTO jobs(kind, src_disk, dst_disk, payload, state) VALUES ('rename', 'u1', 'u2', ?, ?)", payload, state)
			if err := FolderRow(t.Context(), b.sql(), "u1", "D", "E", []string{"u2"}); err != nil {
				t.Fatal(err)
			}
			jobs, err := FolderJobs(t.Context(), b.sql(), "u2")
			if err != nil {
				t.Fatal(err)
			}
			if len(jobs) != 1 || jobs[0].From != "D" || jobs[0].To != "E" {
				t.Errorf("%+v", jobs)
			}
		})
	}
}
