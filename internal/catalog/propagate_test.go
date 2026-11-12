package catalog

import (
	"reflect"
	"strings"
	"testing"
)

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

// A FileFinder reads what FileByID and the jobs do, as often as asked, with one statement each: a file with its
// copies, its path, whether a job of it runs, and nothing for an id no file has.
func TestFileFinder(t *testing.T) {
	b := newPropagationDB(t)
	b.exec("INSERT INTO copies(file_id, disk_id, file_key, nlink, size, mtime_ns, ctime_ns, gone_ns, created_by, first_seen_ns, state) VALUES (1, 'u1', '7', 1, 1, 1, 1, 9, 'user', 1, 'ok')")
	b.exec("INSERT INTO copies(file_id, disk_id, created_by, first_seen_ns, state) VALUES (1, 'u2', 'atoll', 2, 'pending')")
	finder, err := NewFileFinder(t.Context(), b.sql())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := finder.Close(); err != nil {
			t.Error(err)
		}
	}()
	want, err := FileByID(t.Context(), b.sql(), 1)
	if err != nil || want == nil || len(want.Copies) != 2 {
		t.Fatalf("%+v, %v", want, err)
	}
	for range 3 {
		got, err := finder.ByID(t.Context(), 1)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%+v, %v; want %+v", got, err, want)
		}
		if got, err := finder.ByID(t.Context(), 2); err != nil || got != nil {
			t.Fatalf("no file: %+v, %v", got, err)
		}
		if path, ok, err := finder.Path(t.Context(), 1); err != nil || !ok || path != "a" {
			t.Fatalf("%q, %v, %v", path, ok, err)
		}
		if path, ok, err := finder.Path(t.Context(), 2); err != nil || ok || path != "" {
			t.Fatalf("no file: %q, %v, %v", path, ok, err)
		}
	}
	for _, state := range []string{JobQueued, JobRunning} {
		b.exec("DELETE FROM jobs")
		b.exec("INSERT INTO jobs(kind, file_id, src_disk, dst_disk, state) VALUES ('replicate', 1, 'u1', 'u2', ?)", state)
		if running, err := finder.Running(t.Context(), 1); err != nil || running != (state == JobRunning) {
			t.Errorf("%s: running %v, %v", state, running, err)
		}
	}
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

// What reads below a folder searches the files by the range of their paths, or the copies of a disk by their old
// path: never every copy of a disk, which a pass asks once for each folder a disk lost (specs.md §26). SQLite has no
// statistics here, so the plan is what every catalog gets.
func TestQueriesBelowAFolderReadOnlyWhatIsBelowIt(t *testing.T) {
	b := openForOps(t)
	lo, hi := folderRange("a")
	for _, q := range []struct {
		name, sql string
		args      []any
	}{
		{"CopiesBelow", copiesBelowSQL, []any{"u1", lo, hi}},
		{"SettledBelow", settledBelowSQL, []any{"u1", lo, hi}},
		{"AtOwnPathBelow", atOwnPathBelowSQL, []any{"u1", lo, hi}},
		{"DisksBelow", disksBelowSQL, []any{lo, hi, "u1"}},
		{"folderRows", folderRowsSQL, []any{"u1", lo, hi, false}},
		{"folderRows in a reprise", folderRowsSQL, []any{"u1", lo, hi, true}},
	} {
		plan := queryPlan(t, b, q.sql, q.args...)
		for _, line := range plan {
			// The disks themselves are a handful of rows, and a constant row reads no table.
			scans := strings.HasPrefix(line, "SCAN") && !strings.HasPrefix(line, "SCAN disks") && line != "SCAN CONSTANT ROW"
			if scans || strings.Contains(line, "copies_by_disk") {
				t.Errorf("%s: %q in %q", q.name, line, plan)
			}
		}
	}
}

// queryPlan gives the lines of what SQLite says it would do for a query.
func queryPlan(t *testing.T, b *testDB, query string, args ...any) []string {
	t.Helper()
	rows, err := b.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plan
}
