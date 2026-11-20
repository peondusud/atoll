package catalog

import (
	"context"
	"database/sql"
	"testing"
)

// writer is a function that writes to the catalog, for a sweep: what it writes when nothing fails is checked by the
// case, if at all, as its own tests check it; the sweep checks what it leaves when any step fails.
type writer struct {
	name string
	opt  sweepOptions
	fn   func(ctx context.Context, db *sql.DB) (any, error)
}

// sweepWriters sweeps each writer, from busyPool and, for those it names, SQL run on it first, for a case that
// starts from another state.
func sweepWriters(t *testing.T, writers []writer, preps map[string]string) {
	t.Helper()
	benches := map[string]*faultBench{}
	for _, w := range writers {
		prep := preps[w.name]
		fb := benches[prep]
		if fb == nil {
			fb = newFaultBench(t, func(b *testDB) {
				busyPool(b)
				if prep != "" {
					b.exec(prep)
				}
			})
			benches[prep] = fb
		}
		steps, _ := fb.sweep(w.name, w.opt, w.fn)
		if steps == 0 {
			t.Errorf("%s: did nothing", w.name)
		}
	}
}

// Every writer of the catalog, at each step of its transaction failing, gives an error that wraps the failure and
// leaves the catalog as it was: no operation journaled without its event or its trash row, no copy gone without its
// job, no guard answered halfway (specs.md §9, CLAUDE.md invariant 12: a filesystem operation never runs before its
// row is committed, so a failed begin must leave none). Once no step fails, it succeeds.
func TestEveryWriterFailsWhole(t *testing.T) {
	keeps := sweepOptions{keeps: true}
	none := func(err error) (any, error) { return nil, err }
	op := func(id int64, disk, kind, path, path2, key string) *Op {
		return &Op{ID: id, Disk: disk, Kind: kind, Path: path, Path2: path2, FileKey: key}
	}
	btime := int64(4)
	key := "41"
	links := &Links{FileKey: "12", CtimeNS: 9, Nlink: 1, PriorNS: sql.NullInt64{Int64: 6, Valid: true}}
	newCopy := &NewCopy{Path: "Q/q", Size: 3, MtimeNS: 5, CtimeNS: 6, BtimeNS: &btime, FileKey: "93", Nlink: 1, Mode: 420}
	entry := &TrashEntry{RowID: 1, Path: "Q/q", Disk: "u1", TrashPath: "20260101/1/Q/q", OpID: 1, Size: sql.NullInt64{Int64: 3, Valid: true}, Reason: "deleted", DeletedNS: 1, ExpiresNS: 100, FileKey: "91"}
	job := func() *ReplicateJob {
		return &ReplicateJob{ID: 1, FileID: 3, Path: "B/c", Size: 5, Src: "u1", Dst: "u2", State: JobQueued, Priority: 2, Part: "3.part", Refresh: true}
	}
	writers := []writer{
		{"WriteAlerts", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(WriteAlerts(ctx, db, []AlertRow{{Key: "k", FirstNS: 1, SentNS: 2, Count: 3}, {Key: "files-corrupt", FirstNS: 1, SentNS: 5, Count: 2}}, []string{"disk-missing:u2"}))
		}},
		{"BeginResolve", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(BeginResolve(ctx, db, "C/d", "u1", 100))
		}},
		{"RefreshCopy", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(RefreshCopy(ctx, db, 4, "u2", &SeenCopy{Size: 7, MtimeNS: 5, CtimeNS: 9, BtimeNS: &btime, FileKey: "24", Nlink: 1}))
		}},
		{"DropCopy", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return none(DropCopy(ctx, db, 4, "u2")) }},
		{"ResolveConflict", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(ResolveConflict(ctx, db, 4, "u1", 100))
		}},
		{"BeginLinks", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return BeginLinks(ctx, db, "u1", OpLink, []LinkStart{{FileID: 1, Path: "A/a", FileKey: "11"}, {FileID: 2, Path: "A/b", FileKey: "12"}}, 100)
		}},
		{"EndLinks", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(EndLinks(ctx, db, "u1", []LinkEnd{{ID: 8, Links: links}}, []int64{6}))
		}},
		{"EndOps", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(EndOps(ctx, db, []int64{5, 6}, OpDone))
		}},
		{"WriteModifyGuardEvent", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(WriteModifyGuardEvent(ctx, db, "u2", "", 100))
		}},
		// The answers to a guard give the time of the guard they answer with their error: the daemon drops it with the
		// error (cmd/atoll/daemon.go, answerGuardOn), and the answer is not recorded.
		{"AnswerModifyGuard", keeps, func(ctx context.Context, db *sql.DB) (any, error) {
			return AnswerModifyGuard(ctx, db, "u1", EventModifyGuardConfirm, 100)
		}},
		{"ApplyModification modified", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(ApplyModification(ctx, db, 100, &Modification{
				FileID: 1, Path: "A/a", Kind: ModModified, Src: "u1", Priority: 2, Hash: make([]byte, 32), Chunks: make([]byte, 32), Reference: "u1",
				Changed: []ChangedCopy{{Disk: "u1", FileKey: "11", Nlink: 1, Size: 11, MtimeNS: 8, CtimeNS: 9, Mode: 420}},
				Stale:   []string{"u2", "c1"},
			}))
		}},
		{"ApplyModification metadata", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(ApplyModification(ctx, db, 100, &Modification{
				FileID: 1, Path: "A/a", Kind: ModMetadata, Reference: "u1",
				Changed: []ChangedCopy{{Disk: "u1", FileKey: "11", Nlink: 1, Size: 10, MtimeNS: 8, CtimeNS: 9, Mode: 384}, {Disk: "u2", FileKey: "21", Nlink: 1, Size: 10, MtimeNS: 8, CtimeNS: 9, Mode: 384}},
			}))
		}},
		{"ApplyModification conflict", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(ApplyModification(ctx, db, 100, &Modification{
				FileID: 1, Path: "A/a", Kind: ModConflict,
				Changed: []ChangedCopy{{Disk: "u1", FileKey: "11", Nlink: 1, Size: 12, MtimeNS: 8, CtimeNS: 9, Mode: 420}, {Disk: "u2", FileKey: "21", Nlink: 1, Size: 13, MtimeNS: 8, CtimeNS: 9, Mode: 420, Replaced: true}},
			}))
		}},
		{"QueueOverwrite", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(QueueOverwrite(ctx, db, 1, "u1", "u2", 3))
		}},
		{"BeginTrash", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return BeginTrash(ctx, db, &TrashStart{Disk: "u1", Path: "A/b", FileKey: "12", Size: sql.NullInt64{Int64: 10, Valid: true}, Hash: make([]byte, 32), Reason: "deleted", NowNS: 100, ExpiresNS: 200})
		}},
		{"FinishTrash", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(FinishTrash(ctx, db, op(2, "u2", OpTrash, "A/a", "20260101/2/A/a", "21"), &Restart{DeletedNS: 100, ExpiresNS: 200}, links))
		}},
		{"FinishTrash stale", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(FinishTrash(ctx, db, op(11, "u2", OpTrash, "B/c", "20260101/11/B/c", "23"), nil, nil))
		}},
		{"FinishTrash of a link", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			o := op(2, "u1", OpTrash, "A/a", "20260101/2/A/a", "11")
			o.Link = 1
			return none(FinishTrash(ctx, db, o, nil, links))
		}},
		{"FinishTrash, another file", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(FinishTrash(ctx, db, op(2, "u1", OpTrash, "C/d", "20260101/2/C/d", "14"), nil, nil))
		}},
		{"BeginMkdir", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			uid, gid := int64(1), int64(2)
			return BeginMkdir(ctx, db, "u1", "F", &MkdirLike{Mode: 493, UID: &uid, GID: &gid}, 100)
		}},
		{"FinishMkdir", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			fk := "205"
			return none(FinishMkdir(ctx, db, op(5, "u2", OpMkdir, "C", "", ""), &EntryRow{Path: "C", Kind: "dir", Mode: 493, MtimeNS: 5, FileKey: &fk, BtimeNS: &btime}))
		}},
		{"Undo", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(Undo(ctx, db, op(2, "u2", OpTrash, "A/a", "20260101/2/A/a", "21"), true, 100))
		}},
		{"BeginFromTrash restore", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return BeginFromTrash(ctx, db, OpRestore, entry, 100)
		}},
		{"BeginFromTrash purge", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return BeginFromTrash(ctx, db, OpUnlink, entry, 100)
		}},
		{"FinishPurge", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(FinishPurge(ctx, db, op(1, "u1", OpUnlink, "Q/q", "20260101/1/Q/q", "91"), links))
		}},
		{"FinishRestore", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(FinishRestore(ctx, db, op(1, "u1", OpRestore, "Q/q", "20260101/1/Q/q", "91"), 100, newCopy, links))
		}},
		{"FinishRestore over a row behind", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			c := *newCopy
			c.Path = "Z/b"
			return none(FinishRestore(ctx, db, op(1, "u2", OpRestore, "Z/b", "20260101/1/Z/b", "91"), 100, &c, nil))
		}},
		{"BeginRestoreTo", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return BeginRestoreTo(ctx, db, "u2", entry, 100) }},
		{"FinishRestoreTo", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(FinishRestoreTo(ctx, db, op(1, "u2", OpRestore, "Q/q", "20260101/1/Q/q", "91"), 100, newCopy, links))
		}},
		{"BeginRmdir", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return BeginRmdir(ctx, db, "u2", "B", "202", 100) }},
		{"FinishRmdir", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(FinishRmdir(ctx, db, op(9, "u2", OpRmdir, "B", "", "202")))
		}},
		{"BeginRename", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return BeginRename(ctx, db, "u1", "A/b", "A/bb", "12", 100)
		}},
		{"FinishRename", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(FinishRename(ctx, db, op(4, "u2", OpRename, "Z/b", "A/b", "22"), &RenameDone{Nlink: 1, Ctime: sql.NullInt64{Int64: 9, Valid: true}, Unverify: true}, links))
		}},
		{"FinishRename elsewhere", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(FinishRename(ctx, db, op(4, "u2", OpRename, "Z/b", "Y/b", "22"), &RenameDone{Nlink: 1}, nil))
		}},
		{"FinishFolderRename", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(FinishFolderRename(ctx, db, op(5, "u2", OpRename, "A", "Z", "201"), 3))
		}},
		{"FinishFolderRename without a job", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(FinishFolderRename(ctx, db, op(5, "u1", OpRename, "A", "G", "101"), 0))
		}},
		{"CatchUp", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(CatchUp(ctx, db, "u2", 2, &RenameDone{Nlink: 1, Ctime: sql.NullInt64{Int64: 9, Valid: true}}))
		}},
		{"FolderRow", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(FolderRow(ctx, db, "u1", "A", "H", []string{"u2"}))
		}},
		{"SetConflict", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return none(SetConflict(ctx, db, 1, 100)) }},
		{"DropJob", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return none(DropJob(ctx, db, 3)) }},
		{"DropFileJobs", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return none(DropFileJobs(ctx, db, 3)) }},
		{"QueueRenamedJob", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return none(QueueRenamedJob(ctx, db, 1, "u2")) }},
		{"DropStaleCanceled", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return none(DropStaleCanceled(ctx, db)) }},
		{"WriteGuardEvent", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return none(WriteGuardEvent(ctx, db, "u2", 100)) }},
		{"WriteGuardClear", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return none(WriteGuardClear(ctx, db, "u1", 100)) }},
		{"ConfirmGuard", keeps, func(ctx context.Context, db *sql.DB) (any, error) {
			return ConfirmGuard(ctx, db, "u1", 100, []RefusedCopy{{FileID: 1, Place: "A/a"}})
		}},
		{"RejectGuard", keeps, func(ctx context.Context, db *sql.DB) (any, error) {
			return RejectGuard(ctx, db, "u1", 100, []RefusedCopy{{FileID: 1, Place: "A/a"}, {FileID: 2, Place: "A/b"}})
		}},
		{"ApplyDecisions", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(ApplyDecisions(ctx, db, 100, []FileChange{
				{ID: 1, Path: "A/a", NewPath: "A/a2", DropJobs: true, DropHash: true, Kept: true, KeptDisk: "u2", Rows: []RowChange{
					{Disk: "u1", Follow: true, FollowNlink: 1, FollowCtime: sql.NullInt64{Int64: 9, Valid: true}, Unverify: true, MakeUser: true},
					{Disk: "u2", CountGone: true, GoneNS: 50, Pending: "A/a"},
					{Disk: "c1", KeepGone: "A/a"},
				}},
				{ID: 2, Path: "A/b", Rows: []RowChange{{Disk: "u2", Clear: true, ClearedFrom: "Z/b"}, {Disk: "c1", Drop: true}}},
				{ID: 3, Path: "B/c", Src: "u2", Reason: ReasonOverwritten, Rows: []RowChange{{Disk: "u2", Trash: true}}},
				{ID: 4, Path: "C/d", ClearConflict: true, Own: []OwnFile{
					{Path: "C/d2", Disks: []string{"u2"}, WithHash: true, Size: 8, MtimeNS: 6, First: 1, Follow: []RowChange{{Disk: "u2", FollowNlink: 1}}},
				}},
				{ID: 5, Path: "E/e", Own: []OwnFile{{Path: "A/b", Disks: []string{"u2"}}}, DropFile: true},
			}))
		}},
		{"BeginRepair", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return BeginRepair(ctx, db, &RepairStart{Disk: "u1", Path: "E/e", FileKey: "15", Bad: []int64{0, 1}, Size: 9, MtimeNS: 5, NowNS: 100, ExpiresNS: 200})
		}},
		{"FinishRepair", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(FinishRepair(ctx, db, op(7, "u1", OpRepair, "E/e", "7.chunks", "15"), 9, 100))
		}},
		{"StopRepair saved", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(StopRepair(ctx, db, op(7, "u1", OpRepair, "E/e", "7.chunks", "15"), 8, true, sql.NullInt64{Int64: 9, Valid: true}, "failed", 100))
		}},
		{"StopRepair not saved", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(StopRepair(ctx, db, op(7, "u1", OpRepair, "E/e", "7.chunks", "15"), 0, false, sql.NullInt64{}, "failed", 100))
		}},
		{"ClearCorrupt", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return none(ClearCorrupt(ctx, db, 5, "u1", 100)) }},
		{"SettleChanged", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(SettleChanged(ctx, db, 5, &ChangedCopy{Disk: "u1", FileKey: "15", Nlink: 1, Size: 9, MtimeNS: 8, CtimeNS: 9, Mode: 420}, true, 100))
		}},
		{"SettleChanged not holding", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(SettleChanged(ctx, db, 5, &ChangedCopy{Disk: "u1", FileKey: "15", Nlink: 1, Size: 9, MtimeNS: 8, CtimeNS: 9, Mode: 420}, false, 100))
		}},
		{"HandOver", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return none(HandOver(ctx, db, 1, "c1", "u2")) }},
		{"QueueReplicate", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(QueueReplicate(ctx, db, 100, []Queue{{FileID: 4, Src: "u1", Dst: "c1", Priority: 1}, {FileID: 5, Src: "u2", Dst: "c1", Replaces: "u1"}}))
		}},
		{"BeginCreate", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			j := job()
			j.Part = ""
			return BeginCreate(ctx, db, j, 100)
		}},
		{"BeginCreate again", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return BeginCreate(ctx, db, job(), 100) }},
		{"SetCreateKey", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(SetCreateKey(ctx, db, op(3, "u2", OpCreate, "B/c", "3.part", ""), "34"))
		}},
		{"RecordChunks", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(RecordChunks(ctx, db, 1, 1, make([]byte, 96)))
		}},
		{"DropChunksFrom", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return none(DropChunksFrom(ctx, db, 1, 1)) }},
		{"RecordVerified", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(RecordVerified(ctx, db, job(), make([]byte, 32), &PartLook{Size: 5, MtimeNS: 5, CtimeNS: 9, BtimeNS: &btime, Mode: 420, Nlink: 1}))
		}},
		{"FinishCreate", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			j := job()
			j.Hash = make([]byte, 32)
			return none(FinishCreate(ctx, db, op(3, "u2", OpCreate, "B/c", "3.part", "33"), j, &NewCopy{Path: "B/c", Size: 5, MtimeNS: 5, CtimeNS: 9, FileKey: "33", Nlink: 1, Mode: 420}, 100, false))
		}},
		{"RetryJob", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(RetryJob(ctx, db, job(), sql.NullInt64{Int64: 200, Valid: true}, false))
		}},
		{"Requeue", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return none(Requeue(ctx, db, job(), false)) }},
		{"Requeue keeping its part", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return none(Requeue(ctx, db, job(), true)) }},
		{"EndJob", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return none(EndJob(ctx, db, job())) }},
		{"ListTrashAgain", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(ListTrashAgain(ctx, db, "u2", []FoundTrash{
				{OpID: 50, Path: "S/s", TrashPath: "20260102/50/S/s", Size: 4, FileKey: "77", DeletedNS: 3, ExpiresNS: 9},
				{OpID: 51, Chunks: true, Path: "S/t", TrashPath: "51.chunks", Size: 8, DeletedNS: 3, ExpiresNS: 9},
			}))
		}},
		{"StartOpsAfter", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return none(StartOpsAfter(ctx, db, 90)) }},
		{"StartFilesAfter", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return none(StartFilesAfter(ctx, db, 90)) }},
		{"WriteScrubCursor", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(WriteScrubCursor(ctx, db, &ScrubCursor{Disk: "u2", FileID: 1, Chunk: 1, Size: 10, MtimeNS: 5, CtimeNS: 6}))
		}},
		{"ClearScrubCursor", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return none(ClearScrubCursor(ctx, db, "u1")) }},
		{"SetCopyVerified", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(SetCopyVerified(ctx, db, 1, "u2", 100))
		}},
		{"MarkCopyCorrupt", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(MarkCopyCorrupt(ctx, db, 1, "u2", "A/a", 100))
		}},
		{"SetFoldersTidiedThrough", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return none(SetFoldersTidiedThrough(ctx, db, 9)) }},
		{"BeginUtimens", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return BeginUtimens(ctx, db, "u2", "A/a", "21", 7, 100)
		}},
		{"FinishUtimens", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(FinishUtimens(ctx, db, op(6, "u1", OpUtimens, "A/a", "", "11"), 7, 9))
		}},
		{"BeginChmod", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return BeginChmod(ctx, db, "u2", "A/a", "21", 384, 100)
		}},
		{"FinishChmod", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(FinishChmod(ctx, db, op(6, "u1", OpChmod, "A/a", "", "11"), 384, 9))
		}},
		{"BeginChown", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return BeginChown(ctx, db, "u2", "A/a", "21", 1, 2, 100)
		}},
		{"FinishChown", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(FinishChown(ctx, db, op(6, "u1", OpChown, "A/a", "", "11"), 1, 2, 420, 9))
		}},
		{"BeginXattr", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return BeginXattr(ctx, db, "u2", "A/a", "21", 100) }},
		{"FinishXattr", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(FinishXattr(ctx, db, op(6, "u1", OpXattr, "A/a", "", "11"), 9))
		}},
		{"WriteXattrKept", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(WriteXattrKept(ctx, db, op(3, "u2", OpCreate, "B/c", "3.part", "33"), []string{"user.a", "user.b"}, 100))
		}},
		{"BeginViewOp", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(BeginViewOp(ctx, db, &ViewOp{Kind: ViewRename, Disk: "u1", Path: "A/a", Path2: "A/z", FileKey: "11", AtNS: 100}))
		}},
		{"BeginViewTrash", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return BeginViewTrash(ctx, db, &ViewOp{Kind: ViewUnlink, Disk: "u1", Path: "A/b", FileKey: "12", AtNS: 100},
				&TrashStart{Disk: "u1", Path: "A/b", FileKey: "12", Size: sql.NullInt64{Int64: 10, Valid: true}, Reason: "deleted", NowNS: 100, ExpiresNS: 200})
		}},
		{"FinishViewOp", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(FinishViewOp(ctx, db, &ViewOp{ID: 3, Kind: ViewPart, Disk: "u2", Path: "B/c", OpID: 3, State: OpPlanned, AtNS: 3}, OpUndone, true, 100))
		}},
		{"TakeViewOps", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(TakeViewOps(ctx, db, []int64{1, 2}, 100))
		}},
		{"BeginViewPart", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			o, v, err := BeginViewPart(ctx, db, "c1", "A/b", "u1", 100)
			return pair(o, v), err
		}},
		{"ViewPartWritten", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(ViewPartWritten(ctx, db, &ViewOp{ID: 3, Kind: ViewPart, Disk: "u2", Path: "B/c", OpID: 3, State: OpPlanned, AtNS: 3}))
		}},
		{"DismissViewPart", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(DismissViewPart(ctx, db, &ViewOp{ID: 3, Kind: ViewPart, Disk: "u2", Path: "B/c", OpID: 3, State: OpDone, AtNS: 3}, 100))
		}},
		{"AdoptViewParts", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return AdoptViewParts(ctx, db, 100) }},
		{"SetDiskState", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(SetDiskState(ctx, db, "u2", "missing"))
		}},
		{"SetSpin", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return SetSpin(ctx, db, "u2", true) }},
		{"SetNamesDisagree", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return none(SetNamesDisagree(ctx, db, "u2", 3)) }},
		{"SetSmart", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(SetSmart(ctx, db, "u2", []byte(`{"ok":true}`)))
		}},
		{"SetDevice", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(SetDevice(ctx, db, "u2", []string{"sdb", "sdd"}))
		}},
		{"SetLinksUnnamed", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return none(SetLinksUnnamed(ctx, db, "u2", 2)) }},
		{"WriteWalk", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(WriteWalk(ctx, db, "c1", 100, &Walk{
				Copies:  []NewCopy{{Path: "N/n", Size: 1, MtimeNS: 5, CtimeNS: 6, FileKey: "61", Nlink: 1, Mode: 420}, {Path: "E/e", Size: 9, MtimeNS: 5, CtimeNS: 7, FileKey: "65", Nlink: 1, Mode: 420}},
				Entries: []EntryRow{{Path: "N", Kind: "dir", Mode: 493, MtimeNS: 5, FileKey: &key}},
				Clocks:  []CopyClock{{FileID: 1, Kind: "gone"}, {FileID: 2}},
				Modes:   []CopyMode{{FileID: 1, Mode: 384, UID: 1, GID: 1}},
				Kept:    map[string]bool{"A": true},
			}))
		}},
		{"ApplyPlan", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(ApplyPlan(ctx, db, 100,
				[]*FileUpdate{{ID: 1, Size: 10, MtimeNS: 5, ReplicasTarget: 2, Rule: sql.NullString{String: "r2", Valid: true}, Mode: "immediate", State: "degraded", Original: sql.NullString{String: "u1", Valid: true}}},
				[]StateChange{{ID: 2, State: "pending"}, {ID: 3, State: "ok"}},
				[]CopyMark{{FileID: 1, Disk: "c1", State: "surplus"}}))
		}},
		{"WriteHashes", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(WriteHashes(ctx, db, 100, &HashWrite{FileID: 3, Hash: make([]byte, 32), Chunks: make([]byte, 32), Verified: []string{"u1"}, Unverified: []string{"u2"}}))
		}},
		{"WriteHashesOf", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(WriteHashesOf(ctx, db, 100, []HashWrite{
				{FileID: 3, Hash: make([]byte, 32), Chunks: make([]byte, 32), Verified: []string{"u1"}},
				{FileID: 4, Hash: make([]byte, 32), Chunks: make([]byte, 32), Conflict: true},
			}))
		}},
		{"TrySyncDisks", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return none(TrySyncDisks(ctx, db, []Disk{
				{ID: "u1", UUID: "a", FS: "ext4", Mount: "/a", Root: "media", Workdir: "Atoll", Role: "data", Position: 0, Tags: []string{"x"}, State: "online"},
				{ID: "u3", UUID: "d", FS: "xfs", Mount: "/d", Root: "media", Workdir: "Atoll", Role: "data", Position: 1, State: "online"},
			}))
		}},
	}
	writers = append(writers, writer{"checkMeta, keys of another version", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
		return none(checkMeta(ctx, db, "p"))
	}})
	sweepWriters(t, writers, map[string]string{
		"ResolveConflict": "DELETE FROM copies WHERE file_id = 4 AND disk_id = 'u2'",
		"checkMeta, keys of another version": `UPDATE meta SET value = 'rust something' WHERE key = 'lpath_key_version';
			UPDATE files SET lpath_key = 'stale'; UPDATE copies SET old_lpath_key = 'stale' WHERE old_lpath IS NOT NULL`,
	})
}
