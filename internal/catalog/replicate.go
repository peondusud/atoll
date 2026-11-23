package catalog

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/peondusud/atoll/internal/layout"
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
	// Look is what the part was when it was verified, recorded with Hash: what the copy's row takes, so that a write
	// into it once its name is in the library reads as an edit at the next look (specs.md §14.2). Nil until then, and
	// for a job an earlier version verified.
	Look *PartLook
	// Placed is the change time the copy had right after the rename of the operation that placed it, recorded in a
	// commit of its own before its row: what a reprise compares the change time it finds with, another being a write
	// since (specs.md §14.2). Nil until then, and for a job an earlier version ran.
	Placed *Placed
	// Replaces is the disk whose user copy this copy stands in for, as atoll balance --move-originals makes it: the
	// copy becomes a user copy, and that one an Atoll copy, when it is recorded (specs.md §13.3).
	Replaces string
	// Refresh says that this copy takes the place of a stale one that went to the trash: it waits for no window,
	// whatever its file's mode (specs.md §12.3, §15.4).
	Refresh bool
}

// PartLook is what a look at a copy gives of what its row records: its size, times, mode with setuid, setgid and
// sticky, owner and links. BtimeNS is nil where the filesystem gives no creation time. A job records the look at its
// part once verified, in its payload as part_look (specs.md §9, §14.2).
type PartLook struct {
	Size    int64  `json:"size"`
	MtimeNS int64  `json:"mtime_ns"`
	CtimeNS int64  `json:"ctime_ns"`
	BtimeNS *int64 `json:"btime_ns,omitempty"`
	Mode    int64  `json:"mode"`
	UID     uint32 `json:"uid"`
	GID     uint32 `json:"gid"`
	Nlink   int64  `json:"nlink"`
}

// Placed is what a job records of its copy right after the rename of operation Op: its change time, which the rename
// moved, in its payload as placed (specs.md §9, §14.2).
type Placed struct {
	Op      int64 `json:"op"`
	CtimeNS int64 `json:"ctime_ns"`
}

// jobPayload is what a replicate job holds beyond its columns (specs.md §9).
type jobPayload struct {
	Part     string    `json:"part,omitempty"`
	Blake3   string    `json:"blake3,omitempty"`
	PartLook *PartLook `json:"part_look,omitempty"`
	Placed   *Placed   `json:"placed,omitempty"`
	Replaces string    `json:"replaces,omitempty"`
	Refresh  bool      `json:"refresh,omitempty"`
}

func (j *ReplicateJob) payload() (string, error) {
	data := jobPayload{Part: j.Part, Replaces: j.Replaces, Refresh: j.Refresh}
	if j.Hash != nil {
		data.Blake3 = hex.EncodeToString(j.Hash)
		data.PartLook = j.Look
		data.Placed = j.Placed
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
	j.Part, j.Replaces, j.Refresh = data.Part, data.Replaces, data.Refresh
	if data.Blake3 != "" {
		hash, err := hex.DecodeString(data.Blake3)
		if err != nil || len(hash) != 32 {
			return fmt.Errorf("job %d: payload: hash %q", j.ID, data.Blake3)
		}
		j.Hash, j.Look, j.Placed = hash, data.PartLook, data.Placed
	}
	return nil
}

// dropPart forgets a job's part, with what its verification and its rename recorded: the next operation makes one of its
// own.
func (j *ReplicateJob) dropPart() {
	j.Part, j.Hash, j.Look, j.Placed = "", nil, nil, nil
}

// PlacedBy gives the change time the job recorded right after the rename of an operation, and nil when it recorded none
// for that one: the crash came between that rename and its record, or the record is an earlier operation's.
func (j *ReplicateJob) PlacedBy(op int64) *int64 {
	if j == nil || j.Placed == nil || j.Placed.Op != op {
		return nil
	}
	ctime := j.Placed.CtimeNS
	return &ctime
}

// Queue is one copy the plan wants made.
type Queue struct {
	FileID   int64
	Src, Dst string
	Priority int64
	// Replaces is the disk whose user copy the copy stands in for (ReplicateJob.Replaces).
	Replaces string
}

// HandOver gives a user copy's part to another copy of its file, ok or surplus: that one becomes a user copy, if it is
// not one already, and this one an Atoll copy, in one transaction, so that the file never has fewer user copies
// (specs.md §6.1, §13.3). A copy no longer there, or no longer in step, takes nothing, and the user copy keeps its part.
func HandOver(ctx context.Context, db *sql.DB, fileID int64, from, to string) error {
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "UPDATE copies SET created_by = 'user' WHERE file_id = ? AND disk_id = ? AND state IN ('ok', 'surplus')", fileID, to)
		if err != nil {
			return fmt.Errorf("heir: %w", err)
		}
		if n, err := result.RowsAffected(); err != nil || n == 0 {
			return err //nolint:wrapcheck // nil, or what the driver says of a count
		}
		if _, err := tx.ExecContext(ctx, "UPDATE copies SET created_by = 'atoll' WHERE file_id = ? AND disk_id = ? AND created_by = 'user'", fileID, from); err != nil {
			return fmt.Errorf("user copy: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("file %d: %w", fileID, err)
	}
	return nil
}

// Pending is a pending copy of a disk, as the room it holds there counts it: its file's size, and the part in
// <workdir>/tmp/ its job owns, empty for none yet.
type Pending struct {
	Size int64
	Part string
}

// PendingOn gives the pending copies of a disk: what a plan made them hold the room of there (specs.md §13.4), which
// what else writes to that disk leaves them (§21.2; conformance/models/README.md, flaw 12).
func PendingOn(ctx context.Context, db *sql.DB, disk string) ([]Pending, error) {
	rows, err := db.QueryContext(ctx, `SELECT f.size, j.payload FROM copies c JOIN files f ON f.id = c.file_id
		LEFT JOIN jobs j ON j.file_id = c.file_id AND j.dst_disk = c.disk_id AND j.kind = 'replicate' AND j.state IN ('queued', 'running')
		WHERE c.disk_id = ? AND c.state = 'pending' ORDER BY c.file_id`, disk)
	if err != nil {
		return nil, fmt.Errorf("pending copies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Pending
	for rows.Next() {
		var size int64
		var payload sql.NullString
		if err := rows.Scan(&size, &payload); err != nil {
			return nil, fmt.Errorf("pending copies: %w", err)
		}
		var j ReplicateJob
		if err := j.readPayload(payload); err != nil {
			return nil, err
		}
		out = append(out, Pending{Size: size, Part: j.Part})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pending copies: %w", err)
	}
	return out, nil
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
			payload := sql.NullString{}
			if q.Replaces != "" {
				text, err := (&ReplicateJob{Replaces: q.Replaces}).payload()
				if err != nil {
					return err
				}
				payload = sql.NullString{String: text, Valid: true}
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO jobs(kind, file_id, src_disk, dst_disk, priority, state, payload) VALUES ('replicate', ?, ?, ?, ?, 'queued', ?)`,
				q.FileID, q.Src, q.Dst, q.Priority, payload); err != nil {
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

// The replicate jobs still to run: every one, and those of the files at or below a path, which are looked for by their
// path and then by file. Both in one query, an OR with the empty path, made SQLite read every job for a path too.
const (
	replicateJobsSQL = `
		SELECT ` + replicateColumns + `
		FROM jobs j JOIN files f ON f.id = j.file_id JOIN disks d ON d.id = j.dst_disk
		WHERE j.kind = 'replicate' AND j.state IN ('queued', 'running')
		ORDER BY f.lpath, d.position`
	replicateJobsBelowSQL = `
		SELECT ` + replicateColumns + `
		FROM files f CROSS JOIN jobs j ON j.file_id = f.id JOIN disks d ON d.id = j.dst_disk
		WHERE (f.lpath = ?1 OR (f.lpath >= ?2 AND f.lpath < ?3)) AND j.kind = 'replicate' AND j.state IN ('queued', 'running')
		ORDER BY f.lpath, d.position`
)

// ReplicateJobs gives the replicate jobs still to run of the files at or below a path, every one for an
// empty path, by path in byte order, then by the target's position (specs.md §14.3).
func ReplicateJobs(ctx context.Context, db *sql.DB, path string) ([]ReplicateJob, error) {
	query, args := replicateJobsSQL, []any(nil)
	if path != "" {
		lo, hi := folderRange(path)
		query, args = replicateJobsBelowSQL, []any{[]byte(path), lo, hi}
	}
	rows, err := db.QueryContext(ctx, query, args...)
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
	page         int
	started      bool
	priority, id int64
}

// NewDueReader reads the jobs due at a time.
func NewDueReader(db *sql.DB, nowNS int64) *DueReader {
	return &DueReader{db: db, nowNS: nowNS, page: duePage}
}

// What DueReader reads: its first page from the start of jobs_to_do, and each next one from past the last job it gave,
// the rest of that job's priority, then the lower ones, each a seek in jobs_to_do. Its key in an OR, as one condition,
// had SQLite walk jobs_to_do from its start for each page: the 200 pages of 200,000 copies took 2.6 s, and take half a second
// (specs.md §26).
const (
	dueFrom = `
		FROM jobs j JOIN files f ON f.id = j.file_id
		WHERE j.kind = 'replicate' AND j.state IN ('queued', 'running') AND (j.not_before_ns IS NULL OR j.not_before_ns <= ?1)`
	dueFirstSQL = `SELECT ` + replicateColumns + dueFrom + `
		ORDER BY j.priority DESC, j.id LIMIT ?2`
	dueNextSQL = `SELECT ` + replicateColumns + dueFrom + ` AND j.priority = ?2 AND j.id > ?3
		UNION ALL SELECT ` + replicateColumns + dueFrom + ` AND j.priority < ?2
		ORDER BY j.priority DESC, j.id LIMIT ?4`
)

// Next gives the next due jobs, and none at the end.
func (r *DueReader) Next(ctx context.Context) ([]ReplicateJob, error) {
	query, args := dueFirstSQL, []any{r.nowNS, r.page}
	if r.started {
		query, args = dueNextSQL, []any{r.nowNS, r.priority, r.id, r.page}
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
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

// The copies to make on a disk that own a part, or that one: their payload names it, so they have one, and jobs_parts
// holds them alone (specs.md §14.2). Without the payload named, SQLite would read every job.
const (
	jobOfPartSQL = `
		SELECT ` + replicateColumns + `
		FROM jobs j JOIN files f ON f.id = j.file_id
		WHERE j.kind = 'replicate' AND j.dst_disk = ? AND j.payload IS NOT NULL AND json_extract(j.payload, '$.part') = ?`
	partsInUseSQL = `
		SELECT json_extract(payload, '$.part') FROM jobs
		WHERE kind = 'replicate' AND dst_disk = ?1 AND payload IS NOT NULL AND json_extract(payload, '$.part') IS NOT NULL
		UNION SELECT CAST(path2 AS TEXT) FROM ops WHERE kind = 'create' AND disk_id = ?1 AND state = 'planned' AND path2 IS NOT NULL`
)

// JobOfPart gives the replicate job that owns a part on a disk, and nil when none does.
func JobOfPart(ctx context.Context, db *sql.DB, disk, part string) (*ReplicateJob, error) {
	rows, err := db.QueryContext(ctx, jobOfPartSQL, disk, part)
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
	rows, err := db.QueryContext(ctx, partsInUseSQL, disk)
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

// PartStates gives, of the names a disk's tmp/ holds, those an operation names, each with the state of those
// operations: tmp/ loses only the parts of operations done or undone (specs.md §14.2). A copy's part, <op-id>.part,
// is named by the operation create that made it, and only that one and those that resume it, later, name it: each is
// looked for from that id on, never over the whole journal, which only grows. Read at each pass, that took 0.35 s
// over a million copies. A chunk a repair saves on its way to the trash, <op-id>.<index>.part, is named by that
// repair alone, whatever its index: no column holds its name, and no operation resumes it (specs.md §16.1). A name
// written otherwise than Atoll writes it is named by none.
func PartStates(ctx context.Context, db *sql.DB, disk string, names []string) (map[string]string, error) {
	out := map[string]string{}
	stmt, err := db.PrepareContext(ctx, partStatesQuery)
	if err != nil {
		return nil, fmt.Errorf("parts: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, name := range names {
		id, _, chunk, ok := layout.PartOp(name)
		switch {
		case !ok:
		case chunk:
			var state string
			switch err := db.QueryRowContext(ctx, repairStateQuery, id, disk).Scan(&state); {
			case isNoRows(err):
			case err != nil:
				return nil, fmt.Errorf("parts: %w", err)
			default:
				out[name] = state
			}
		default:
			if err := partState(ctx, stmt, id, disk, name, out); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

const partStatesQuery = "SELECT state FROM ops WHERE id >= ? AND kind = 'create' AND disk_id = ? AND path2 = ? ORDER BY id"

// repairStateQuery reads the state of the repair of an id, on a disk.
const repairStateQuery = "SELECT state FROM ops WHERE id = ? AND kind = 'repair' AND disk_id = ?"

func partState(ctx context.Context, stmt *sql.Stmt, id int64, disk, name string, out map[string]string) error {
	rows, err := stmt.QueryContext(ctx, id, disk, []byte(name))
	if err != nil {
		return fmt.Errorf("parts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			return fmt.Errorf("parts: %w", err)
		}
		// A part named by several operations, one resuming another, is in use while any of them is.
		if out[name] != "planned" {
			out[name] = state
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("parts: %w", err)
	}
	return nil
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
		// What an earlier operation recorded of its rename is no longer of the copy this one makes.
		job.Placed = nil
		if job.Part == "" {
			job.dropPart()
			job.Part = layout.PartName(op.ID)
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

// RecordVerified records the whole-file hash a verification gave, and what the part was then, before anything is
// renamed, so that a reprise that finds the copy at its name can finish it, its row what was verified (specs.md §14.2).
func RecordVerified(ctx context.Context, db *sql.DB, job *ReplicateJob, hash []byte, look *PartLook) error {
	job.Hash, job.Look, job.Placed = hash, look, nil
	payload, err := job.payload()
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "UPDATE jobs SET payload = ? WHERE id = ?", payload, job.ID); err != nil {
		return fmt.Errorf("job %d: %w", job.ID, err)
	}
	return nil
}

// RecordPlaced records, in a commit of its own, the change time a job's copy had right after the rename of its
// operation, before the copy's row is written: a reprise that finds another knows that someone wrote into the copy since,
// however long Atoll was down (specs.md §14.2).
func RecordPlaced(ctx context.Context, db *sql.DB, job *ReplicateJob, op *Op, ctimeNS int64) error {
	job.Placed = &Placed{Op: op.ID, CtimeNS: ctimeNS}
	payload, err := job.payload()
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "UPDATE jobs SET payload = ? WHERE id = ?", payload, job.ID); err != nil {
		return fmt.Errorf("job %d: %w", job.ID, err)
	}
	return nil
}

// FinishCreate says that a copy is made: the operation done, the pending copy ok with made, what its part was when
// verified, the file's hash where it had none, the source verified, and the job gone with its chunks (specs.md §14.2).
// A job gone meanwhile leaves the copy unverified, as nothing says what was checked; so does unverified, for a copy a
// reprise found whose change time may be a write's as well as the rename's: the source and the file's hash stand, as
// they are what was read.
func FinishCreate(ctx context.Context, db *sql.DB, op *Op, job *ReplicateJob, made *NewCopy, nowNS int64, unverified bool) error {
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
		own := verified
		if unverified {
			own = sql.NullInt64{}
		}
		// A copy made in place of a user copy takes its part: it becomes the user copy, and the one it replaces an
		// Atoll copy, in this transaction, so that the file never has fewer user copies (specs.md §6.1, §13.3).
		createdBy := "atoll"
		if job != nil && job.Replaces != "" {
			createdBy = "user"
			if _, err := tx.ExecContext(ctx, "UPDATE copies SET created_by = 'atoll' WHERE file_id = ? AND disk_id = ? AND created_by = 'user'",
				fileID, job.Replaces); err != nil {
				return fmt.Errorf("replaced copy: %w", err)
			}
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE copies SET state = 'ok', created_by = ?14, file_key = ?3, nlink = ?4, size = ?5, mtime_ns = ?6, ctime_ns = ?7,
				btime_ns = ?8, first_seen_ns = ?9, verified_ns = ?10, gone_ns = NULL, gone_kind = NULL, old_lpath = NULL, old_lpath_key = NULL,
				mode = ?11, uid = ?12, gid = ?13
			WHERE file_id = ?1 AND disk_id = ?2`,
			fileID, op.Disk, made.FileKey, made.Nlink, made.Size, made.MtimeNS, made.CtimeNS, made.BtimeNS, nowNS, own, made.Mode, made.UID, made.GID, createdBy)
		if err != nil {
			return fmt.Errorf("copy: %w", err)
		}
		if n, err := result.RowsAffected(); err != nil {
			return fmt.Errorf("copy: %w", err)
		} else if n == 0 {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO copies(file_id, disk_id, file_key, nlink, size, mtime_ns, ctime_ns, btime_ns, created_by, first_seen_ns, verified_ns, state, mode, uid, gid)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'ok', ?, ?, ?)`,
				fileID, op.Disk, made.FileKey, made.Nlink, made.Size, made.MtimeNS, made.CtimeNS, made.BtimeNS, createdBy, nowNS, own, made.Mode, made.UID, made.GID); err != nil {
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
		job.dropPart()
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
		job.dropPart()
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

// runningThere is true of a pending copy whose replicate job is running: its operation is left planned, and only a
// reprise may end it (specs.md §14.1). It reads the copy's file_id and disk_id from the row c.
const runningThere = "EXISTS(SELECT 1 FROM jobs j WHERE j.kind = 'replicate' AND j.file_id = c.file_id AND j.dst_disk = c.disk_id AND j.state = 'running')"

// cancelPlanned cancels a pending copy a plan no longer keeps, with its job, but for one whose job is running, which the
// plan never cancels (specs.md §13.4, §14.1).
func cancelPlanned(ctx context.Context, tx *sql.Tx, fileID int64, disk string) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM copies AS c WHERE c.file_id = ? AND c.disk_id = ? AND c.state = 'pending' AND NOT "+runningThere,
		fileID, disk); err != nil {
		return fmt.Errorf("pending copy: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE kind = 'replicate' AND file_id = ? AND dst_disk = ? AND state <> 'running'", fileID, disk); err != nil {
		return fmt.Errorf("job: %w", err)
	}
	return nil
}
