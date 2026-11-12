package catalog

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
)

// The states of a replicate job (specs.md §14.1). queued is JobQueued.
const JobRunning = "running"

// ReplicateJob is a copy to make, with the file it makes a copy of as it stands now (specs.md §14.1).
type ReplicateJob struct {
	ID        int64
	FileID    int64
	Path      string // the file's path now, which is where the copy lands
	Size      int64
	Src, Dst  string
	State     string
	Attempts  int64
	Priority  int64
	NotBefore sql.NullInt64
	Part      string // the name in <workdir>/tmp/ it copies into, empty before its first operation
	Hash      []byte // the whole-file hash its verification gave, nil until then
}

// jobPayload is what a replicate job holds beyond its columns (specs.md §9).
type jobPayload struct {
	Part   string `json:"part,omitempty"`
	Blake3 string `json:"blake3,omitempty"`
}

func (j *ReplicateJob) payload() (string, error) {
	data := jobPayload{Part: j.Part}
	if j.Hash != nil {
		data.Blake3 = hex.EncodeToString(j.Hash)
	}
	text, err := json.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("payload: %w", err)
	}
	return string(text), nil
}

func (j *ReplicateJob) readPayload(text sql.NullString) error {
	if !text.Valid || text.String == "" {
		return nil
	}
	var data jobPayload
	if err := json.Unmarshal([]byte(text.String), &data); err != nil {
		return fmt.Errorf("job %d: payload: %w", j.ID, err)
	}
	j.Part = data.Part
	if data.Blake3 != "" {
		hash, err := hex.DecodeString(data.Blake3)
		if err != nil || len(hash) != 32 {
			return fmt.Errorf("job %d: payload: hash %q", j.ID, data.Blake3)
		}
		j.Hash = hash
	}
	return nil
}

// Queue is one copy the plan wants made.
type Queue struct {
	FileID   int64
	Src, Dst string
	Priority int64
}

// QueueReplicate queues copies, each a job and its pending copy on the target, in one transaction. The
// pending copy holds the room and the name for later plans, and keeps the file from being queued twice
// (specs.md §14.1).
func QueueReplicate(ctx context.Context, db *sql.DB, nowNS int64, list []Queue) error {
	if len(list) == 0 {
		return nil
	}
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		for i := range list {
			q := &list[i]
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO copies(file_id, disk_id, created_by, first_seen_ns, state) VALUES (?, ?, 'atoll', ?, 'pending')`,
				q.FileID, q.Dst, nowNS); err != nil {
				return fmt.Errorf("file %d on %q: pending copy: %w", q.FileID, q.Dst, err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO jobs(kind, file_id, src_disk, dst_disk, priority, state) VALUES ('replicate', ?, ?, ?, ?, 'queued')`,
				q.FileID, q.Src, q.Dst, q.Priority); err != nil {
				return fmt.Errorf("file %d on %q: job: %w", q.FileID, q.Dst, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("queue: %w", err)
	}
	return nil
}

const replicateColumns = `j.id, j.file_id, f.lpath, f.size, j.src_disk, j.dst_disk, j.state, j.attempts, j.priority, j.not_before_ns, j.payload`

func scanReplicateJobs(rows *sql.Rows) ([]ReplicateJob, error) {
	var out []ReplicateJob
	for rows.Next() {
		var j ReplicateJob
		var path []byte
		var payload sql.NullString
		if err := rows.Scan(&j.ID, &j.FileID, &path, &j.Size, &j.Src, &j.Dst, &j.State, &j.Attempts, &j.Priority, &j.NotBefore, &payload); err != nil {
			return nil, fmt.Errorf("jobs: %w", err)
		}
		j.Path = string(path)
		if err := j.readPayload(payload); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	return out, nil
}

// ReplicateJobs gives the replicate jobs still to run of the files at or below a path, every one for an
// empty path, by path in byte order, then by the target's position (specs.md §14.3).
func ReplicateJobs(ctx context.Context, db *sql.DB, path string) ([]ReplicateJob, error) {
	lo, hi := folderRange(path)
	rows, err := db.QueryContext(ctx, `
		SELECT `+replicateColumns+`
		FROM jobs j JOIN files f ON f.id = j.file_id JOIN disks d ON d.id = j.dst_disk
		WHERE j.kind = 'replicate' AND j.state IN ('queued', 'running')
			AND (?4 OR f.lpath = ?1 OR (f.lpath >= ?2 AND f.lpath < ?3))
		ORDER BY f.lpath, d.position`, []byte(path), lo, hi, path == "")
	if err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanReplicateJobs(rows)
}

// duePage is how many due jobs a DueReader reads at a time: at 200,000 jobs, the whole queue at once
// would cost more than the plan (specs.md §26).
const duePage = 1000

// DueReader reads the replicate jobs that are due, a page at a time, highest priority first, then in the
// order they were queued (specs.md §14.1). It reads past each job it gives, whatever becomes of it, so
// that a job put back in the queue waits for the next pass.
type DueReader struct {
	db           *sql.DB
	nowNS        int64
	started      bool
	priority, id int64
}

// NewDueReader reads the jobs due at a time.
func NewDueReader(db *sql.DB, nowNS int64) *DueReader {
	return &DueReader{db: db, nowNS: nowNS}
}

// Next gives the next due jobs, and none at the end.
func (r *DueReader) Next(ctx context.Context) ([]ReplicateJob, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+replicateColumns+`
		FROM jobs j JOIN files f ON f.id = j.file_id
		WHERE j.kind = 'replicate' AND j.state IN ('queued', 'running')
			AND (j.not_before_ns IS NULL OR j.not_before_ns <= ?1)
			AND (NOT ?2 OR j.priority < ?3 OR (j.priority = ?3 AND j.id > ?4))
		ORDER BY j.priority DESC, j.id LIMIT ?5`, r.nowNS, r.started, r.priority, r.id, duePage)
	if err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	list, err := scanReplicateJobs(rows)
	if err != nil {
		return nil, err
	}
	if n := len(list); n > 0 {
		r.started, r.priority, r.id = true, list[n-1].Priority, list[n-1].ID
	}
	return list, nil
}

// CopiesLater counts the replicate jobs still to run that are not yet due, and says when the first is
// (specs.md §15.12): what DueReader leaves for later.
func CopiesLater(ctx context.Context, db *sql.DB, nowNS int64) (count int64, next sql.NullInt64, err error) {
	err = db.QueryRowContext(ctx, `
		SELECT count(*), min(not_before_ns) FROM jobs
		WHERE kind = 'replicate' AND state IN ('queued', 'running') AND not_before_ns > ?`, nowNS).Scan(&count, &next)
	if err != nil {
		return 0, sql.NullInt64{}, fmt.Errorf("jobs: %w", err)
	}
	return count, next, nil
}

// JobOfPart gives the replicate job that owns a part on a disk, and nil when none does.
func JobOfPart(ctx context.Context, db *sql.DB, disk, part string) (*ReplicateJob, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT `+replicateColumns+`
		FROM jobs j JOIN files f ON f.id = j.file_id
		WHERE j.kind = 'replicate' AND j.dst_disk = ? AND json_extract(j.payload, '$.part') = ?`, disk, part)
	if err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	list, err := scanReplicateJobs(rows)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

// PartsInUse gives the parts of a disk that its tmp/ keeps: those a job owns, and those an operation left
// planned names, which only its reprise can tell of (specs.md §14.2).
func PartsInUse(ctx context.Context, db *sql.DB, disk string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT json_extract(payload, '$.part') FROM jobs WHERE kind = 'replicate' AND dst_disk = ?1 AND json_extract(payload, '$.part') IS NOT NULL
		UNION SELECT CAST(path2 AS TEXT) FROM ops WHERE kind = 'create' AND disk_id = ?1 AND state = 'planned' AND path2 IS NOT NULL`, disk)
	if err != nil {
		return nil, fmt.Errorf("parts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("parts: %w", err)
		}
		out[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parts: %w", err)
	}
	return out, nil
}

// PartOps gives the state of each operation that names a part on a disk: tmp/ loses only the parts of
// operations done or undone (specs.md §14.2).
func PartOps(ctx context.Context, db *sql.DB, disk string) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT CAST(path2 AS TEXT), state FROM ops WHERE kind = 'create' AND disk_id = ? AND path2 IS NOT NULL", disk)
	if err != nil {
		return nil, fmt.Errorf("parts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var name, state string
		if err := rows.Scan(&name, &state); err != nil {
			return nil, fmt.Errorf("parts: %w", err)
		}
		// A part named by several operations, one resuming another, is in use while any of them is.
		if out[name] != "planned" {
			out[name] = state
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parts: %w", err)
	}
	return out, nil
}

// BeginCreate journals the operation of a job, with its event, before anything is written to a disk.
// The job is running from then on, and owns its part: the one it had, which this operation resumes, or
// one named by this operation (specs.md §14.2).
func BeginCreate(ctx context.Context, db *sql.DB, job *ReplicateJob, nowNS int64) (*Op, error) {
	op := &Op{Disk: job.Dst, Kind: OpCreate, Path: job.Path}
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "INSERT INTO ops(disk_id, kind, path, started_ns, state) VALUES (?, 'create', ?, ?, 'planned')",
			job.Dst, []byte(job.Path), nowNS)
		if err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		if op.ID, err = result.LastInsertId(); err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		if job.Part == "" {
			job.Part, job.Hash = strconv.FormatInt(op.ID, 10)+".part", nil
			if _, err := tx.ExecContext(ctx, "DELETE FROM job_chunks WHERE job_id = ?", job.ID); err != nil {
				return fmt.Errorf("job: %w", err)
			}
		}
		op.Path2 = job.Part
		if _, err := tx.ExecContext(ctx, "UPDATE ops SET path2 = ? WHERE id = ?", []byte(op.Path2), op.ID); err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		payload, err := job.payload()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE jobs SET state = 'running', payload = ? WHERE id = ?", payload, job.ID); err != nil {
			return fmt.Errorf("job: %w", err)
		}
		job.State = JobRunning
		return writeEventData(ctx, tx, nowNS, "info", "replicate", op, eventData{Source: job.Src})
	})
	if err != nil {
		return nil, fmt.Errorf("%q: %w", job.Path, err)
	}
	return op, nil
}

// SetCreateKey records the inode of a part once it is made, before anything is copied into it, so that a
// reprise can tell it from anything else (specs.md §14.2).
func SetCreateKey(ctx context.Context, db *sql.DB, op *Op, fileKey string) error {
	if _, err := db.ExecContext(ctx, "UPDATE ops SET file_key = ? WHERE id = ?", fileKey, op.ID); err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	op.FileKey = fileKey
	return nil
}

// RecordChunks records the hashes of the chunks copied from index first on, concatenated.
func RecordChunks(ctx context.Context, db *sql.DB, jobID int64, first int, hashes []byte) error {
	if len(hashes) == 0 {
		return nil
	}
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, "INSERT OR REPLACE INTO job_chunks(job_id, idx, hash) VALUES (?, ?, ?)")
		if err != nil {
			return fmt.Errorf("prepare: %w", err)
		}
		defer func() { _ = stmt.Close() }()
		for i := 0; i*32 < len(hashes); i++ {
			if _, err := stmt.ExecContext(ctx, jobID, first+i, hashes[i*32:i*32+32]); err != nil {
				return fmt.Errorf("chunk %d: %w", first+i, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("job %d: %w", jobID, err)
	}
	return nil
}

// DropChunksFrom forgets the hashes from index first on: a resume found that chunk missing or wrong, and
// copies again from there.
func DropChunksFrom(ctx context.Context, db *sql.DB, jobID int64, first int) error {
	if _, err := db.ExecContext(ctx, "DELETE FROM job_chunks WHERE job_id = ? AND idx >= ?", jobID, first); err != nil {
		return fmt.Errorf("job %d: chunks: %w", jobID, err)
	}
	return nil
}

// JobChunks gives the hashes recorded for a job, concatenated in order, up to the first index missing.
func JobChunks(ctx context.Context, db *sql.DB, jobID int64) ([]byte, error) {
	rows, err := db.QueryContext(ctx, "SELECT idx, hash FROM job_chunks WHERE job_id = ? ORDER BY idx", jobID)
	if err != nil {
		return nil, fmt.Errorf("job %d: chunks: %w", jobID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []byte
	for rows.Next() {
		var idx int
		var hash []byte
		if err := rows.Scan(&idx, &hash); err != nil {
			return nil, fmt.Errorf("job %d: chunks: %w", jobID, err)
		}
		if idx != len(out)/32 {
			break
		}
		out = append(out, hash...)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("job %d: chunks: %w", jobID, err)
	}
	return out, nil
}

// RecordVerified records the whole-file hash a verification gave, before anything is renamed, so that a
// reprise that finds the copy at its name can finish it (specs.md §14.2).
func RecordVerified(ctx context.Context, db *sql.DB, job *ReplicateJob, hash []byte) error {
	job.Hash = hash
	payload, err := job.payload()
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "UPDATE jobs SET payload = ? WHERE id = ?", payload, job.ID); err != nil {
		return fmt.Errorf("job %d: %w", job.ID, err)
	}
	return nil
}

// FinishCreate says that a copy is made: the operation done, the pending copy ok with what a look at its
// name found, the file's hash where it had none, the source verified, and the job gone with its chunks
// (specs.md §14.2). A job gone meanwhile leaves the copy unverified, as nothing says what was checked.
func FinishCreate(ctx context.Context, db *sql.DB, op *Op, job *ReplicateJob, made *NewCopy, nowNS int64) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := markOp(ctx, tx, op.ID, "done"); err != nil {
			return err
		}
		var fileID int64
		var known []byte
		var hash []byte
		if job != nil {
			fileID, hash = job.FileID, job.Hash
		}
		err := tx.QueryRowContext(ctx, "SELECT id, blake3 FROM files WHERE id = ?1 OR (?1 = 0 AND lpath = ?2)", fileID, []byte(op.Path)).Scan(&fileID, &known)
		switch {
		case isNoRows(err):
			return nil
		case err != nil:
			return fmt.Errorf("file: %w", err)
		}
		verified := sql.NullInt64{}
		if hash != nil && (known == nil || string(known) == string(hash)) {
			verified = sql.NullInt64{Int64: nowNS, Valid: true}
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE copies SET state = 'ok', created_by = 'atoll', file_key = ?3, nlink = ?4, size = ?5, mtime_ns = ?6, ctime_ns = ?7,
				btime_ns = ?8, first_seen_ns = ?9, verified_ns = ?10, gone_ns = NULL, gone_kind = NULL, old_lpath = NULL, old_lpath_key = NULL,
				mode = ?11, uid = ?12, gid = ?13
			WHERE file_id = ?1 AND disk_id = ?2`,
			fileID, op.Disk, made.FileKey, made.Nlink, made.Size, made.MtimeNS, made.CtimeNS, made.BtimeNS, nowNS, verified, made.Mode, made.UID, made.GID)
		if err != nil {
			return fmt.Errorf("copy: %w", err)
		}
		if n, err := result.RowsAffected(); err != nil {
			return fmt.Errorf("copy: %w", err)
		} else if n == 0 {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO copies(file_id, disk_id, file_key, nlink, size, mtime_ns, ctime_ns, btime_ns, created_by, first_seen_ns, verified_ns, state, mode, uid, gid)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'atoll', ?, ?, 'ok', ?, ?, ?)`,
				fileID, op.Disk, made.FileKey, made.Nlink, made.Size, made.MtimeNS, made.CtimeNS, made.BtimeNS, nowNS, verified, made.Mode, made.UID, made.GID); err != nil {
				return fmt.Errorf("copy: %w", err)
			}
		}
		if verified.Valid && known == nil {
			if err := takeHash(ctx, tx, fileID, job.ID, hash); err != nil {
				return err
			}
		}
		if verified.Valid {
			if _, err := tx.ExecContext(ctx, "UPDATE copies SET verified_ns = ? WHERE file_id = ? AND disk_id = ?", nowNS, fileID, job.Src); err != nil {
				return fmt.Errorf("source: %w", err)
			}
		}
		if job != nil {
			if _, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE id = ?", job.ID); err != nil {
				return fmt.Errorf("job: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%q: %w", op.Path, err)
	}
	return nil
}

// takeHash gives a file with no hash the one a copy was verified against, with the chunk hashes its source
// read gave (specs.md §16).
func takeHash(ctx context.Context, tx *sql.Tx, fileID, jobID int64, hash []byte) error {
	if _, err := tx.ExecContext(ctx, "UPDATE files SET blake3 = ? WHERE id = ?", hash, fileID); err != nil {
		return fmt.Errorf("file: %w", err)
	}
	rows, err := tx.QueryContext(ctx, "SELECT hash FROM job_chunks WHERE job_id = ? ORDER BY idx", jobID)
	if err != nil {
		return fmt.Errorf("chunks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var chunks []byte
	for rows.Next() {
		var h []byte
		if err := rows.Scan(&h); err != nil {
			return fmt.Errorf("chunks: %w", err)
		}
		chunks = append(chunks, h...)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("chunks: %w", err)
	}
	// An empty file has no chunk, and so no row (specs.md §16).
	if len(chunks) == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO file_chunks(file_id, chunks) VALUES (?, ?)", fileID, chunks); err != nil {
		return fmt.Errorf("chunks: %w", err)
	}
	return nil
}

// RetryJob puts a job back in the queue, with one more attempt, not before a time when one is given. Its
// part goes with its hashes when the copy in it was dropped (specs.md §14.1, §14.2).
func RetryJob(ctx context.Context, db *sql.DB, job *ReplicateJob, notBefore sql.NullInt64, keepPart bool) error {
	if !keepPart {
		job.Part, job.Hash = "", nil
	}
	payload, err := job.payload()
	if err != nil {
		return err
	}
	err = inTransaction(ctx, db, func(tx *sql.Tx) error {
		if !keepPart {
			if _, err := tx.ExecContext(ctx, "DELETE FROM job_chunks WHERE job_id = ?", job.ID); err != nil {
				return fmt.Errorf("chunks: %w", err)
			}
		}
		_, err := tx.ExecContext(ctx, "UPDATE jobs SET state = 'queued', attempts = attempts + 1, not_before_ns = ?, payload = ? WHERE id = ?",
			notBefore, payload, job.ID)
		if err != nil {
			return fmt.Errorf("job: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("job %d: %w", job.ID, err)
	}
	job.State, job.Attempts, job.NotBefore = JobQueued, job.Attempts+1, notBefore
	return nil
}

// Requeue puts back in the queue a job whose operation a reprise undid: a crash is no failed attempt. It
// keeps its part when that is still there, for the next operation to resume (specs.md §14.2).
func Requeue(ctx context.Context, db *sql.DB, job *ReplicateJob, keepPart bool) error {
	if !keepPart {
		job.Part, job.Hash = "", nil
	}
	payload, err := job.payload()
	if err != nil {
		return err
	}
	err = inTransaction(ctx, db, func(tx *sql.Tx) error {
		if !keepPart {
			if _, err := tx.ExecContext(ctx, "DELETE FROM job_chunks WHERE job_id = ?", job.ID); err != nil {
				return fmt.Errorf("chunks: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE jobs SET state = 'queued', payload = ? WHERE id = ?", payload, job.ID); err != nil {
			return fmt.Errorf("job: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("job %d: %w", job.ID, err)
	}
	job.State = JobQueued
	return nil
}

// EndJob ends a job that will not run again, with its pending copy, so that the next plan decides again
// (specs.md §14.1). A file with no copy left goes with it: what a stale copy held, once in the trash, may have been
// all there was of it (specs.md §15.4).
func EndJob(ctx context.Context, db *sql.DB, job *ReplicateJob) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := cancelPending(ctx, tx, job.FileID, job.Dst); err != nil {
			return err
		}
		return dropFileIfEmpty(ctx, tx, job.FileID)
	})
	if err != nil {
		return fmt.Errorf("job %d: %w", job.ID, err)
	}
	return nil
}

// cancelPending removes a pending copy and the job that would have made it. What that job left in tmp/
// goes the next time a command that writes opens the disk (specs.md §14.1, §14.2).
func cancelPending(ctx context.Context, tx *sql.Tx, fileID int64, disk string) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM copies WHERE file_id = ? AND disk_id = ? AND state = 'pending'", fileID, disk); err != nil {
		return fmt.Errorf("pending copy: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE kind = 'replicate' AND file_id = ? AND dst_disk = ?", fileID, disk); err != nil {
		return fmt.Errorf("job: %w", err)
	}
	return nil
}
