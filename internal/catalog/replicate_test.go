package catalog

import (
	"bytes"
	"database/sql"
	"maps"
	"testing"
)

// newReplicateDB is a file a with a user copy on u1, and a copy to make on u2.
func newReplicateDB(t *testing.T) (*testDB, *ReplicateJob) {
	t.Helper()
	b := newPropagationDB(t)
	b.exec("INSERT INTO copies(file_id, disk_id, file_key, size, mtime_ns, ctime_ns, created_by, first_seen_ns, state) VALUES (1, 'u1', '7', 1, 1, 1, 'user', 1, 'ok')")
	if err := QueueReplicate(t.Context(), b.sql(), 10, []Queue{{FileID: 1, Src: "u1", Dst: "u2", Priority: 3}}); err != nil {
		t.Fatal(err)
	}
	jobs, err := ReplicateJobs(t.Context(), b.sql(), "a")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("%+v, %v", jobs, err)
	}
	return b, &jobs[0]
}

// A queued copy is a job and a pending copy, which holds the place of the copy to come: a second one on
// that disk is refused (specs.md §14.1).
func TestQueueReplicate(t *testing.T) {
	b, job := newReplicateDB(t)
	if job.Path != "a" || job.Size != 1 || job.Src != "u1" || job.Dst != "u2" || job.State != JobQueued || job.Priority != 3 || job.Part != "" || job.Hash != nil {
		t.Errorf("%+v", job)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 'u2' AND state = 'pending' AND created_by = 'atoll' AND first_seen_ns = 10 AND file_key IS NULL AND size IS NULL"); got != 1 {
		t.Errorf("%d pending copies", got)
	}
	if err := QueueReplicate(t.Context(), b.sql(), 11, []Queue{{FileID: 1, Src: "u1", Dst: "u2"}}); err == nil {
		t.Error("queued twice on one disk")
	}
	if got := b.count("SELECT count(*) FROM jobs"); got != 1 {
		t.Errorf("%d jobs, after a refused queue", got)
	}
	// What is below a path, and nothing beside it.
	for path, want := range map[string]int{"": 1, "a": 1, "b": 0, "a/b": 0} {
		jobs, err := ReplicateJobs(t.Context(), b.sql(), path)
		if err != nil || len(jobs) != want {
			t.Errorf("%q: %+v, %v", path, jobs, err)
		}
	}
	b.exec("UPDATE files SET lpath = ? WHERE id = 1", []byte("d/a"))
	for path, want := range map[string]int{"d": 1, "d/a": 1, "d/": 0, "da": 0} {
		if jobs, err := ReplicateJobs(t.Context(), b.sql(), path); err != nil || len(jobs) != want {
			t.Errorf("%q: %+v, %v", path, jobs, err)
		}
	}
}

// The operation of a job names a part of its own, or the one it resumes, and the job owns it while it runs
// (specs.md §14.2).
func TestBeginCreate(t *testing.T) {
	b, job := newReplicateDB(t)
	op, err := BeginCreate(t.Context(), b.sql(), job, 20)
	if err != nil {
		t.Fatal(err)
	}
	if op.Kind != OpCreate || op.Disk != "u2" || op.Path != "a" || op.Path2 != "1.part" || job.Part != "1.part" || job.State != JobRunning {
		t.Errorf("%+v, %+v", op, job)
	}
	if got := b.count("SELECT count(*) FROM events WHERE kind = 'replicate' AND json_extract(data, '$.source') = 'u1' AND json_extract(data, '$.op') = ?", op.ID); got != 1 {
		t.Errorf("%d events", got)
	}
	if err := SetCreateKey(t.Context(), b.sql(), op, "42"); err != nil {
		t.Fatal(err)
	}
	if err := RecordChunks(t.Context(), b.sql(), job.ID, 0, bytes.Repeat([]byte{1}, 64)); err != nil {
		t.Fatal(err)
	}
	if owner, err := JobOfPart(t.Context(), b.sql(), "u2", "1.part"); err != nil || owner == nil || owner.ID != job.ID {
		t.Errorf("owner %+v, %v", owner, err)
	}
	if parts, err := PartsInUse(t.Context(), b.sql(), "u2"); err != nil || !parts["1.part"] || len(parts) != 1 {
		t.Errorf("%v, %v", parts, err)
	}
	// The operation goes undone, the job keeps its part, and the next operation resumes it.
	if err := Undo(t.Context(), b.sql(), op, false, 30); err != nil {
		t.Fatal(err)
	}
	if err := RetryJob(t.Context(), b.sql(), job, sql.NullInt64{}, true); err != nil {
		t.Fatal(err)
	}
	again, err := BeginCreate(t.Context(), b.sql(), job, 40)
	if err != nil {
		t.Fatal(err)
	}
	if again.Path2 != "1.part" || again.ID == op.ID {
		t.Errorf("%+v", again)
	}
	if chunks, err := JobChunks(t.Context(), b.sql(), job.ID); err != nil || len(chunks) != 64 {
		t.Errorf("a resume lost the hashes: %d, %v", len(chunks), err)
	}
	if states, err := PartStates(t.Context(), b.sql(), "u2", []string{"1.part", "2.part", "01.part", "x.part", "1"}); err != nil || !maps.Equal(states, map[string]string{"1.part": "planned"}) {
		t.Errorf("%v, %v", states, err)
	}
	// Dropped, the part goes with its hashes, and the next operation names one of its own.
	if err := RetryJob(t.Context(), b.sql(), job, sql.NullInt64{Int64: 99, Valid: true}, false); err != nil {
		t.Fatal(err)
	}
	if job.Part != "" || job.Attempts != 2 || b.count("SELECT count(*) FROM job_chunks") != 0 || b.count("SELECT count(*) FROM jobs WHERE not_before_ns = 99 AND state = 'queued'") != 1 {
		t.Errorf("%+v", job)
	}
	third, err := BeginCreate(t.Context(), b.sql(), job, 50)
	if err != nil {
		t.Fatal(err)
	}
	if third.Path2 != "3.part" {
		t.Errorf("%+v", third)
	}
}

// JobChunks stops at the first index missing: what follows a gap was never verified in order.
func TestJobChunksStopAtAGap(t *testing.T) {
	b, job := newReplicateDB(t)
	if err := RecordChunks(t.Context(), b.sql(), job.ID, 0, bytes.Repeat([]byte{1}, 64)); err != nil {
		t.Fatal(err)
	}
	if err := RecordChunks(t.Context(), b.sql(), job.ID, 3, bytes.Repeat([]byte{2}, 32)); err != nil {
		t.Fatal(err)
	}
	if chunks, err := JobChunks(t.Context(), b.sql(), job.ID); err != nil || len(chunks) != 64 {
		t.Errorf("%d, %v", len(chunks), err)
	}
	if err := DropChunksFrom(t.Context(), b.sql(), job.ID, 1); err != nil {
		t.Fatal(err)
	}
	if got := b.count("SELECT count(*) FROM job_chunks"); got != 1 {
		t.Errorf("%d chunks", got)
	}
}

// A copy made: its pending copy becomes ok with what was found at its name, the file takes the hash it had
// not, the source is verified, and the job goes with its chunks (specs.md §14.2).
func TestFinishCreate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		known    []byte
		verified int
		gone     bool
	}{
		{"a file with no hash", nil, 1, false},
		{"a file with that hash", bytes.Repeat([]byte{9}, 32), 1, false},
		{"a file with another hash", bytes.Repeat([]byte{8}, 32), 0, false},
		{"a pending copy gone meanwhile", nil, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, job := newReplicateDB(t)
			b.exec("UPDATE files SET blake3 = ? WHERE id = 1", tc.known)
			op, err := BeginCreate(t.Context(), b.sql(), job, 20)
			if err != nil {
				t.Fatal(err)
			}
			if err := RecordChunks(t.Context(), b.sql(), job.ID, 0, bytes.Repeat([]byte{5}, 32)); err != nil {
				t.Fatal(err)
			}
			if err := RecordVerified(t.Context(), b.sql(), job, bytes.Repeat([]byte{9}, 32)); err != nil {
				t.Fatal(err)
			}
			if tc.gone {
				b.exec("DELETE FROM copies WHERE disk_id = 'u2'")
			}
			made := &NewCopy{Path: "a", Size: 1, MtimeNS: 1, CtimeNS: 77, FileKey: "88", Nlink: 1, Mode: 0o640}
			if err := FinishCreate(t.Context(), b.sql(), op, job, made, 60); err != nil {
				t.Fatal(err)
			}
			if got := b.count("SELECT count(*) FROM ops WHERE state = 'done'"); got != 1 {
				t.Errorf("%d done", got)
			}
			if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 'u2' AND state = 'ok' AND created_by = 'atoll' AND file_key = '88' AND ctime_ns = 77 AND first_seen_ns = 60 AND mode = 416"); got != 1 {
				t.Errorf("%d copies made", got)
			}
			if got := b.count("SELECT count(*) FROM copies WHERE verified_ns = 60"); got != 2*tc.verified {
				t.Errorf("%d verified", got)
			}
			if got := b.count("SELECT count(*) FROM jobs") + b.count("SELECT count(*) FROM job_chunks"); got != 0 {
				t.Errorf("%d jobs and chunks left", got)
			}
			hashed := b.count("SELECT count(*) FROM files WHERE blake3 = ?", bytes.Repeat([]byte{9}, 32))
			chunked := b.count("SELECT count(*) FROM file_chunks WHERE chunks = ?", bytes.Repeat([]byte{5}, 32))
			switch {
			case tc.known == nil && (hashed != 1 || chunked != 1):
				t.Errorf("hash %d, chunks %d", hashed, chunked)
			case tc.known != nil && chunked != 0:
				t.Errorf("chunks %d, for a file that had a hash", chunked)
			}
		})
	}
}

// A job that ends goes with its pending copy, and a copy found where one was pending takes its place, with
// no job left to meet it (specs.md §14.1).
func TestAJobEnds(t *testing.T) {
	b, job := newReplicateDB(t)
	if err := EndJob(t.Context(), b.sql(), job); err != nil {
		t.Fatal(err)
	}
	if got := b.count("SELECT count(*) FROM jobs") + b.count("SELECT count(*) FROM copies WHERE state = 'pending'"); got != 0 {
		t.Errorf("%d left", got)
	}
	if got := b.count("SELECT count(*) FROM files"); got != 1 {
		t.Errorf("%d files, with their source", got)
	}
	// A file whose pending copy was all there was of it, as a stale copy once in the trash can be, goes with it
	// (specs.md §15.4).
	b, job = newReplicateDB(t)
	b.exec("DELETE FROM copies WHERE state <> 'pending'")
	if err := EndJob(t.Context(), b.sql(), job); err != nil {
		t.Fatal(err)
	}
	if got := b.count("SELECT count(*) FROM files"); got != 0 {
		t.Errorf("%d files with no copy", got)
	}
	b, _ = newReplicateDB(t)
	err := inTransaction(t.Context(), b.sql(), func(tx *sql.Tx) error {
		return writeCopies(t.Context(), tx, "u2", 70, []NewCopy{{Path: "a", Size: 1, MtimeNS: 1, CtimeNS: 1, FileKey: "5", Nlink: 1}})
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 'u2' AND state = 'ok' AND created_by = 'user'"); got != 1 {
		t.Errorf("%d copies found", got)
	}
	if got := b.count("SELECT count(*) FROM jobs"); got != 0 {
		t.Errorf("%d jobs", got)
	}
}

// A pending copy is nothing on its disk: a walk is not told of it, and it does not count among the disk's
// copies, which the guard reads (specs.md §14.1, §15.6).
func TestAPendingCopyIsNotOnItsDisk(t *testing.T) {
	b, _ := newReplicateDB(t)
	seen := 0
	if err := EachKnownCopy(t.Context(), b.sql(), "u2", func(*KnownCopy) { seen++ }); err != nil {
		t.Fatal(err)
	}
	if seen != 0 {
		t.Errorf("%d known on u2", seen)
	}
	if n, err := CopyCount(t.Context(), b.sql(), "u2"); err != nil || n != 0 {
		t.Errorf("%d, %v", n, err)
	}
	if n, err := CopiesBefore(t.Context(), b.sql(), "u2", 100); err != nil || n != 0 {
		t.Errorf("%d, %v", n, err)
	}
	// With its last real copy gone, a file goes with its pending copies and their jobs.
	err := inTransaction(t.Context(), b.sql(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), "DELETE FROM copies WHERE disk_id = 'u1'"); err != nil {
			return err
		}
		return dropFileIfEmpty(t.Context(), tx, 1)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := b.count("SELECT count(*) FROM files") + b.count("SELECT count(*) FROM jobs") + b.count("SELECT count(*) FROM copies"); got != 0 {
		t.Errorf("%d rows left", got)
	}
}
