package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// The reads of the jobs that a pass makes, or a command, are each a seek in an index: never a walk of every job, which
// is 200,000 rows while a new pool makes its first replicas, and a pass reads them some ten times (specs.md §26). SQLite
// has no statistics here, so the plan is what every catalog gets. The first page of the due copies is the one read from
// the start of the queue, and it stops at its page.
func TestTheJobsAreReadThroughTheirIndexes(t *testing.T) {
	b := openForOps(t)
	for _, q := range []struct {
		name, sql string
		want      []string // what the plan must say, each in a step of its own
	}{
		{"FolderJobs", folderJobsSQL, []string{"SEARCH jobs USING INDEX jobs_folders (dst_disk=?)"}},
		{"the folder jobs of a disk", folderJobsOfSQL, []string{"SEARCH jobs USING INDEX jobs_folders (dst_disk=?)"}},
		{"the folder jobs of every disk", allFolderJobsSQL, []string{"SCAN jobs USING INDEX jobs_folders"}},
		{"a folder job queued once", queueFolderJobSQL, []string{"SEARCH jobs USING INDEX jobs_folders (dst_disk=?)"}},
		{"TrashJobs", trashJobsSQL, []string{"SEARCH jobs USING INDEX jobs_trash (dst_disk=?)"}},
		{"DropStaleCanceled", dropStaleCanceledSQL, []string{"SCAN jobs USING INDEX jobs_trash"}},
		{"a copy's trash job", staleNoJobSQL, []string{"SEARCH j USING INDEX jobs_trash (dst_disk=? AND file_id=?)"}},
		{"a copy's running job", "DELETE FROM copies AS c WHERE c.file_id = ? AND c.state = 'pending' AND NOT " + runningThere,
			[]string{"SEARCH j USING INDEX jobs_by_file (file_id=?)"}},
		{"JobOfPart", jobOfPartSQL, []string{"SEARCH j USING INDEX jobs_parts (dst_disk=?)"}},
		{"PartsInUse", partsInUseSQL, []string{"SEARCH jobs USING INDEX jobs_parts (dst_disk=?)"}},
		{"ReplicateJobs below a path", replicateJobsBelowSQL, []string{
			"SEARCH f USING INDEX sqlite_autoindex_files_1 (lpath=?)",
			"SEARCH f USING INDEX sqlite_autoindex_files_1 (lpath>? AND lpath<?)",
			"SEARCH j USING INDEX jobs_by_file (file_id=?)",
		}},
		{"the first page of DueReader", dueFirstSQL, []string{"SCAN j USING INDEX jobs_to_do"}},
		{"the next pages of DueReader", dueNextSQL, []string{
			"SEARCH j USING INDEX jobs_to_do (priority=? AND id>?)",
			"SEARCH j USING INDEX jobs_to_do (priority<?)",
		}},
	} {
		plan := planOf(t, b.sql(), q.name, q.sql)
		for _, want := range q.want {
			if !slices.Contains(plan, want) {
				t.Errorf("%s: no %q in %q", q.name, want, plan)
			}
		}
		for _, step := range plan {
			// Every job, or every job still to run, read from the start: by the table, or by an index of them all.
			whole := step == "SCAN jobs" || step == "SCAN j" || strings.HasPrefix(step, "SCAN j USING INDEX jobs_by_file") ||
				strings.HasPrefix(step, "SCAN jobs USING INDEX jobs_by_file") || strings.HasPrefix(step, "SCAN jobs USING INDEX jobs_to_do")
			if whole || strings.HasPrefix(step, "SCAN j USING INDEX jobs_to_do") && q.sql != dueFirstSQL {
				t.Errorf("%s: %q in %q", q.name, step, plan)
			}
			// The due copies come in the order of jobs_to_do, which a sort of what is left to read would undo.
			if strings.Contains(step, "TEMP B-TREE") && (q.sql == dueFirstSQL || q.sql == dueNextSQL) {
				t.Errorf("%s: %q in %q", q.name, step, plan)
			}
		}
	}
}

// What the reads of the jobs gave before their indexes and their seeks (migration 0026), kept to compare with: the same
// rows, in the same order.
const (
	dueBeforeSQL = `
		SELECT ` + replicateColumns + `
		FROM jobs j JOIN files f ON f.id = j.file_id
		WHERE j.kind = 'replicate' AND j.state IN ('queued', 'running')
			AND (j.not_before_ns IS NULL OR j.not_before_ns <= ?1)
			AND (NOT ?2 OR j.priority < ?3 OR (j.priority = ?3 AND j.id > ?4))
		ORDER BY j.priority DESC, j.id LIMIT ?5`
	replicateJobsBeforeSQL = `
		SELECT ` + replicateColumns + `
		FROM jobs j JOIN files f ON f.id = j.file_id JOIN disks d ON d.id = j.dst_disk
		WHERE j.kind = 'replicate' AND j.state IN ('queued', 'running')
			AND (?4 OR f.lpath = ?1 OR (f.lpath >= ?2 AND f.lpath < ?3))
		ORDER BY f.lpath, d.position`
	jobOfPartBeforeSQL = `
		SELECT ` + replicateColumns + `
		FROM jobs j JOIN files f ON f.id = j.file_id
		WHERE j.kind = 'replicate' AND j.dst_disk = ? AND json_extract(j.payload, '$.part') = ?`
	partsInUseBeforeSQL = `
		SELECT json_extract(payload, '$.part') FROM jobs WHERE kind = 'replicate' AND dst_disk = ?1 AND json_extract(payload, '$.part') IS NOT NULL
		UNION SELECT CAST(path2 AS TEXT) FROM ops WHERE kind = 'create' AND disk_id = ?1 AND state = 'planned' AND path2 IS NOT NULL`
)

// dueBefore is DueReader as it read before: one query whose key, in an OR, SQLite could not seek.
type dueBefore struct {
	db           *sql.DB
	nowNS        int64
	page         int
	started      bool
	priority, id int64
}

func (r *dueBefore) next(ctx context.Context) ([]ReplicateJob, error) {
	rows, err := r.db.QueryContext(ctx, dueBeforeSQL, r.nowNS, r.started, r.priority, r.id, r.page)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	list, err := scanReplicateJobs(rows)
	if n := len(list); n > 0 {
		r.started, r.priority, r.id = true, list[n-1].Priority, list[n-1].ID
	}
	return list, err
}

func readBefore(t *testing.T, db *sql.DB, query string, args ...any) []ReplicateJob {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	list, err := scanReplicateJobs(rows)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// readNames gives the column of text a query gives.
func readNames(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

// queuePaths are the paths a drawn queue's files take: a folder, what is below it, and the names beside it whose bytes
// sort between the folder and what is below it, or just after.
var queuePaths = []string{"a", "a/x", "a/y/z", "a.x", "a-b", "a0", "ab", "a/", "b", "b/a", "b/a/c", "set/001/1.mkv", "set/001/2.mkv", "set/002/3.mkv", "é", "é/x"}

// drawing writes a queue drawn by r, or changes it, in one transaction: a commit a row would take minutes.
type drawing struct {
	t  *testing.T
	tx *sql.Tx
	r  *rand.Rand
}

func draw(t *testing.T, db *sql.DB, r *rand.Rand, fn func(d *drawing)) {
	t.Helper()
	if err := inTransaction(t.Context(), db, func(tx *sql.Tx) error {
		fn(&drawing{t: t, tx: tx, r: r})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func (d *drawing) exec(query string, args ...any) {
	d.t.Helper()
	if _, err := d.tx.ExecContext(d.t.Context(), query, args...); err != nil {
		d.t.Fatal(err)
	}
}

// number gives what a query of one integer gives, 0 for NULL.
func (d *drawing) number(query string, args ...any) int64 {
	d.t.Helper()
	var n sql.NullInt64
	if err := d.tx.QueryRowContext(d.t.Context(), query, args...).Scan(&n); err != nil {
		d.t.Fatal(err)
	}
	return n.Int64
}

// pick gives the id of one row of a table, drawn by r, and false when it has none.
func (d *drawing) pick(table string) (int64, bool) {
	n := d.number("SELECT count(*) FROM " + table)
	if n == 0 {
		return 0, false
	}
	return d.number("SELECT id FROM "+table+" ORDER BY id LIMIT 1 OFFSET ?", d.r.Int64N(n)), true
}

// queue fills the catalog with a queue drawn by r: files with ids that skip, and jobs of every kind and state, of
// priorities that tie and reach both ends of int64, with ids that skip, due before now, at it, after it or at any time,
// some naming a part.
func (d *drawing) queue(nowNS int64) {
	d.exec("DELETE FROM jobs")
	d.exec("DELETE FROM files")
	id := int64(0)
	for _, p := range queuePaths {
		if d.r.IntN(4) == 0 {
			continue
		}
		id += 1 + d.r.Int64N(3)
		d.exec("INSERT INTO files(id, lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns) VALUES (?, ?, ?, 1, 1, 'deferred', 'ok', 1, 1)",
			id, []byte(p), d.r.Int64N(100))
	}
	for range d.r.IntN(60) {
		d.job(nowNS)
	}
}

var drawnPriorities = []int64{math.MinInt64, -3, 0, 0, 0, 1, 2, 2, math.MaxInt64}

// job adds a job drawn by r, its id past the last by 1 to 3.
func (d *drawing) job(nowNS int64) {
	id := d.number("SELECT max(id) FROM jobs") + 1 + d.r.Int64N(3)
	disks := []string{"u1", "u2", "u3"}
	kinds := []string{"replicate", "replicate", "replicate", "replicate", "trash", "rename", "purge", "transfer"}
	states := []string{"queued", "queued", "queued", "running", "done", "failed", "canceled"}
	kind := kinds[d.r.IntN(len(kinds))]
	var file, payload any
	if kind == "rename" {
		payload = `{"from_b64":"YQ==","to_b64":"Yg=="}`
	} else {
		f, ok := d.pick("files")
		if !ok {
			return
		}
		file = f
		switch d.r.IntN(4) {
		case 1:
			payload = fmt.Sprintf(`{"part":"%d.part"}`, id)
		case 2:
			payload = `{"refresh":true}`
		case 3:
			payload = fmt.Sprintf(`{"part":"%d.part","refresh":true}`, id)
		}
	}
	var notBefore any
	switch d.r.IntN(5) {
	case 1:
		notBefore = nowNS - 1
	case 2:
		notBefore = nowNS
	case 3:
		notBefore = nowNS + 1
	}
	d.exec("INSERT INTO jobs(id, kind, file_id, src_disk, dst_disk, payload, priority, not_before_ns, attempts, state) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		id, kind, file, disks[d.r.IntN(len(disks))], disks[d.r.IntN(len(disks))], payload, drawnPriorities[d.r.IntN(len(drawnPriorities))],
		notBefore, d.r.Int64N(3), states[d.r.IntN(len(states))])
}

// change does to the queue what a pass may do between two pages: a job ends or goes, runs or is put back to wait, a
// file goes with its jobs, a copy is queued.
func (d *drawing) change(nowNS int64) {
	for range d.r.IntN(3) {
		job, ok := d.pick("jobs")
		if !ok {
			return
		}
		switch d.r.IntN(6) {
		case 0:
			d.exec("DELETE FROM jobs WHERE id = ?", job)
		case 1:
			d.exec("UPDATE jobs SET state = 'running' WHERE id = ?", job)
		case 2:
			d.exec("UPDATE jobs SET state = 'queued', attempts = attempts + 1, not_before_ns = ? WHERE id = ?", nowNS+d.r.Int64N(3)-1, job)
		case 3:
			d.exec("UPDATE jobs SET state = 'done' WHERE id = ?", job)
		case 4:
			if file, ok := d.pick("files"); ok {
				d.exec("DELETE FROM files WHERE id = ?", file)
			}
		case 5:
			d.job(nowNS)
		}
	}
}

// DueReader gives what it gave when its key was in an OR (migration 0026): the same jobs, in the same pages, in the
// same order, on queues drawn with ties of priority, gaps in the ids and jobs not due, across the bounds of its pages,
// and while a pass changes the queue between two pages. So do ReplicateJobs at and below each path, JobOfPart and
// PartsInUse. The seed of a failure is in its message.
func TestTheJobsReadAsBefore(t *testing.T) {
	b := openForOps(t)
	b.exec(`INSERT INTO disks(id, fs_uuid, fs_type, mount_path, root, workdir, role, position, tags, state) VALUES
		('u1', 'a', 'ext4', '/a', 'media', 'Atoll', 'data', 0, '[]', 'online'),
		('u2', 'b', 'ext4', '/b', 'media', 'Atoll', 'data', 1, '[]', 'online'),
		('u3', 'c', 'ntfs', '/c', 'media', 'Atoll', 'data', 2, '[]', 'online')`)
	b.exec(`INSERT INTO ops(id, disk_id, kind, path, path2, started_ns, state) VALUES
		(1, 'u2', 'create', CAST('a' AS BLOB), CAST('1.part' AS BLOB), 1, 'planned'),
		(2, 'u3', 'create', CAST('b' AS BLOB), CAST('2.part' AS BLOB), 1, 'done')`)
	ctx := t.Context()
	const nowNS = 100
	for seed := range uint64(150) {
		r := rand.New(rand.NewPCG(seed, 0x6a6f6273))
		draw(t, b.sql(), r, func(d *drawing) { d.queue(nowNS) })
		page := 1 + r.IntN(7)
		for _, p := range append([]string{""}, queuePaths...) {
			lo, hi := folderRange(p)
			got, err := ReplicateJobs(ctx, b.sql(), p)
			if err != nil {
				t.Fatal(err)
			}
			if want := readBefore(t, b.sql(), replicateJobsBeforeSQL, []byte(p), lo, hi, p == ""); !reflect.DeepEqual(got, want) {
				t.Fatalf("seed %d: ReplicateJobs(%q):\n%+v\nbefore:\n%+v", seed, p, got, want)
			}
		}
		// The parts the jobs name, and one none does.
		names := append([]string{"0.part"}, readNames(t, b.sql(), "SELECT DISTINCT json_extract(payload, '$.part') FROM jobs WHERE json_extract(payload, '$.part') IS NOT NULL ORDER BY 1")...)
		for _, disk := range []string{"u1", "u2", "u3"} {
			for _, name := range names {
				got, err := JobOfPart(ctx, b.sql(), disk, name)
				if err != nil {
					t.Fatal(err)
				}
				want := readBefore(t, b.sql(), jobOfPartBeforeSQL, disk, name)
				if len(want) > 1 {
					t.Fatalf("seed %d: two jobs own %s on %s", seed, name, disk)
				}
				if (got == nil) != (len(want) == 0) || got != nil && !reflect.DeepEqual(*got, want[0]) {
					t.Fatalf("seed %d: JobOfPart(%s, %s) = %+v, before %+v", seed, disk, name, got, want)
				}
			}
			got, err := PartsInUse(ctx, b.sql(), disk)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]bool{}
			for _, name := range readNames(t, b.sql(), partsInUseBeforeSQL, disk) {
				want[name] = true
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("seed %d: PartsInUse(%s) = %v, before %v", seed, disk, got, want)
			}
		}
		reader := NewDueReader(b.sql(), nowNS)
		reader.page = page
		before := &dueBefore{db: b.sql(), nowNS: nowNS, page: page}
		for step := 0; ; step++ {
			want, err := before.next(ctx)
			if err != nil {
				t.Fatal(err)
			}
			got, err := reader.Next(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("seed %d, page %d of %d: DueReader gave\n%+v\nbefore\n%+v", seed, step, page, got, want)
			}
			if len(got) == 0 {
				break
			}
			if step > 500 {
				t.Fatalf("seed %d: no end", seed)
			}
			draw(t, b.sql(), r, func(d *drawing) { d.change(nowNS) })
		}
	}
}

// BenchmarkJobReadsAt200000 times the reads of the jobs on the queue of a new pool that makes its first replicas: 200,000
// files and a copy to make of each, to two disks, of one priority; 100 folder jobs and 1,000 trash jobs on others
// (specs.md §26). Run it with -bench JobReads -benchtime 3x -run '^$': it takes some seconds to fill.
func BenchmarkJobReadsAt200000(b *testing.B) {
	const files, nowNS = 200000, 100
	file := filepath.Join(b.TempDir(), "catalog.db")
	ctx := context.Background()
	if _, err := Create(ctx, file, "p"); err != nil {
		b.Fatal(err)
	}
	db, err := OpenPool(ctx, file, "p")
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	began := time.Now()
	err = inTransaction(ctx, db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO disks(id, fs_uuid, fs_type, mount_path, root, workdir, role, position, tags, state) VALUES
			('u1', 'a', 'ext4', '/a', 'media', 'Atoll', 'data', 0, '[]', 'online'),
			('u2', 'b', 'ext4', '/b', 'media', 'Atoll', 'data', 1, '[]', 'online'),
			('u3', 'c', 'ext4', '/c', 'media', 'Atoll', 'data', 2, '[]', 'online')`); err != nil {
			return err
		}
		for i := range files {
			if _, err := tx.ExecContext(ctx, "INSERT INTO files(id, lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns) VALUES (?, ?, 1, 1, 2, 'deferred', 'ok', 1, 1)",
				i+1, fmt.Appendf(nil, "set/%03d/%d.mkv", i%100, i)); err != nil {
				return err
			}
			var payload any
			if i == 7 {
				payload = `{"part":"7.part"}`
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO jobs(kind, file_id, src_disk, dst_disk, payload, state) VALUES ('replicate', ?, 'u1', ?, ?, 'queued')",
				i+1, []string{"u2", "u3"}[i%2], payload); err != nil {
				return err
			}
		}
		for i := range 1000 {
			if _, err := tx.ExecContext(ctx, "INSERT INTO jobs(kind, file_id, src_disk, dst_disk, state, reason) VALUES ('trash', ?, 'u2', 'u1', 'queued', 'deleted')", i+1); err != nil {
				return err
			}
		}
		for i := range 100 {
			payload, err := encodeFolderPayload(fmt.Sprintf("set/%03d", i), fmt.Sprintf("new/%03d", i))
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO jobs(kind, src_disk, dst_disk, payload, state) VALUES ('rename', 'u1', 'u2', ?, 'queued')", payload); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("filled in %v", time.Since(began).Round(time.Millisecond))
	for _, r := range []struct {
		name string
		read func() (int, error)
	}{
		{"DueReader, every page", func() (int, error) {
			reader, n := NewDueReader(db, nowNS), 0
			for {
				page, err := reader.Next(ctx)
				if err != nil || len(page) == 0 {
					return n, err
				}
				n += len(page)
			}
		}},
		{"FolderJobs", func() (int, error) { list, err := FolderJobs(ctx, db, "u2"); return len(list), err }},
		{"TrashJobs", func() (int, error) { list, err := TrashJobs(ctx, db, "u1"); return len(list), err }},
		{"ReplicateJobs below a folder", func() (int, error) { list, err := ReplicateJobs(ctx, db, "set/042"); return len(list), err }},
		{"JobOfPart", func() (int, error) {
			job, err := JobOfPart(ctx, db, "u3", "7.part")
			if job == nil {
				return 0, err
			}
			return 1, err
		}},
		{"PartsInUse", func() (int, error) { parts, err := PartsInUse(ctx, db, "u3"); return len(parts), err }},
		{"DropStaleCanceled", func() (int, error) { return 0, DropStaleCanceled(ctx, db) }},
		// What is left as it was, to compare with.
		{"CopiesLater", func() (int, error) { n, _, err := CopiesLater(ctx, db, nowNS); return int(n), err }},
		{"RunningCopies", func() (int, error) { running, err := RunningCopies(ctx, db); return len(running), err }},
		{"RunningFiles", func() (int, error) { running, err := RunningFiles(ctx, db); return len(running), err }},
	} {
		b.Run(r.name, func(b *testing.B) {
			n := 0
			for b.Loop() {
				var err error
				if n, err = r.read(); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(n), "rows")
		})
	}
}
