package catalog

import (
	"context"
	"database/sql"
	"testing"
)

// busyPool is a pool with a little of everything the catalog holds: three disks, one a cache; files with copies ok,
// stale, corrupt, pending, gone, renamed and in conflict; folders, a link and an ignored file; operations done and
// planned of each kind; the trash; jobs of each kind with their chunks; the guards' events; alerts; the view's
// journal and a scrub's cursor. Its readers read something of each.
func busyPool(b *testDB) {
	b.exec(`INSERT INTO disks(id, fs_uuid, fs_type, mount_path, root, workdir, role, position, tags, state, capacity_bytes, free_bytes, time_precision_ns, device) VALUES
		('u1', 'a', 'ext4', '/a', 'media', 'Atoll', 'data', 0, '["x"]', 'online', 1000, 500, 1, 'sda'),
		('u2', 'b', 'ntfs', '/b', 'media', 'Atoll', 'data', 1, '[]', 'online', 1000, 600, 100, 'sdb'),
		('c1', 'c', 'ext4', '/c', 'media', 'Atoll', 'cache', 2, '[]', 'online', 100, 50, 1, 'sdc')`)
	b.exec(`INSERT INTO files(id, lpath, lpath_key, size, mtime_ns, blake3, replicas_target, original_disk, rule, mode, state, first_seen_ns, updated_ns, conflict_held) VALUES
		(1, CAST('A/a' AS BLOB), 'A/A', 10, 5, zeroblob(32), 1, 'u1', 'r', 'deferred', 'ok', 1, 1, 0),
		(2, CAST('A/b' AS BLOB), 'A/B', 10, 5, zeroblob(32), 1, 'u1', NULL, 'immediate', 'ok', 1, 1, 0),
		(3, CAST('B/c' AS BLOB), 'B/C', 5, 5, NULL, 1, 'u1', NULL, 'deferred', 'degraded', 1, 1, 0),
		(4, CAST('C/d' AS BLOB), 'C/D', 7, 5, NULL, 1, 'u1', NULL, 'manual', 'conflict', 1, 1, 1),
		(5, CAST('E/e' AS BLOB), 'E/E', 9, 5, randomblob(32), 1, 'u1', NULL, 'deferred', 'corrupt', 1, 1, 0)`)
	b.exec(`INSERT INTO file_chunks(file_id, chunks) VALUES (1, zeroblob(32)), (5, zeroblob(64))`)
	b.exec(`INSERT INTO copies(file_id, disk_id, file_key, nlink, size, mtime_ns, ctime_ns, btime_ns, created_by, first_seen_ns, verified_ns, state, gone_ns, gone_kind, old_lpath, old_lpath_key, mode, uid, gid) VALUES
		(1, 'u1', '11', 1, 10, 5, 6, 4, 'user', 1, 2, 'ok', NULL, NULL, NULL, NULL, 420, 0, 0),
		(1, 'u2', '21', 1, 10, 5, 6, NULL, 'atoll', 1, NULL, 'ok', NULL, NULL, NULL, NULL, 420, 0, 0),
		(1, 'c1', '31', 1, 10, 5, 6, 4, 'user', 1, NULL, 'ok', NULL, NULL, NULL, NULL, 420, 0, 0),
		(2, 'u1', '12', 2, 10, 5, 6, 4, 'user', 1, NULL, 'ok', NULL, NULL, NULL, NULL, 420, 0, 0),
		(2, 'u2', '22', 1, 10, 5, 6, NULL, 'atoll', 1, NULL, 'ok', NULL, NULL, CAST('Z/b' AS BLOB), 'Z/B', 420, 0, 0),
		(3, 'u1', '13', 1, 5, 5, 6, 4, 'user', 1, NULL, 'ok', NULL, NULL, NULL, NULL, 420, 0, 0),
		(3, 'u2', '23', 1, 4, 3, 6, NULL, 'atoll', 1, NULL, 'stale', NULL, NULL, NULL, NULL, 420, 0, 0),
		(4, 'u1', '14', 1, 7, 5, 6, 4, 'user', 1, NULL, 'ok', NULL, NULL, NULL, NULL, 420, 0, 0),
		(4, 'u2', '24', 1, 8, 6, 7, 4, 'user', 1, NULL, 'ok', NULL, NULL, NULL, NULL, 420, 0, 0),
		(5, 'u1', '15', 1, 9, 5, 6, 4, 'user', 1, 3, 'corrupt', NULL, NULL, NULL, NULL, 420, 0, 0),
		(5, 'u2', '25', 1, 9, 5, 6, NULL, 'atoll', 1, NULL, 'ok', 9, 'gone', NULL, NULL, 420, 0, 0)`)
	b.exec(`INSERT INTO copies(file_id, disk_id, created_by, first_seen_ns, state) VALUES (2, 'c1', 'atoll', 1, 'pending')`)
	b.exec(`INSERT INTO entries(lpath, lpath_key, disk_id, kind, mode, mtime_ns, target, created_by, file_key, btime_ns, size, uid, gid) VALUES
		(CAST('A' AS BLOB), 'A', 'u1', 'dir', 493, 5, NULL, 'user', '101', 4, NULL, 0, 0),
		(CAST('A' AS BLOB), 'A', 'u2', 'dir', 493, 5, NULL, 'atoll', '201', NULL, NULL, 0, 0),
		(CAST('B' AS BLOB), 'B', 'u1', 'dir', 493, 5, NULL, 'user', '102', 4, NULL, 0, 0),
		(CAST('B' AS BLOB), 'B', 'u2', 'dir', 493, 5, NULL, 'atoll', '202', NULL, NULL, 0, 0),
		(CAST('C' AS BLOB), 'C', 'u1', 'dir', 493, 5, NULL, 'user', '103', 4, NULL, 0, 0),
		(CAST('E' AS BLOB), 'E', 'u1', 'dir', 493, 5, NULL, 'user', '105', 4, NULL, 0, 0),
		(CAST('A/l' AS BLOB), 'A/L', 'u1', 'symlink', 511, 5, CAST('a' AS BLOB), 'user', '104', 4, 1, 0, 0),
		(CAST('A/i.tmp' AS BLOB), 'A/I.TMP', 'u1', 'ignored', 420, 5, NULL, 'user', '106', 4, 3, 0, 0)`)
	b.exec(`INSERT INTO ops(id, disk_id, kind, path, path2, file_key, started_ns, state, link) VALUES
		(1, 'u1', 'trash', CAST('Q/q' AS BLOB), CAST('20260101/1/Q/q' AS BLOB), '91', 1, 'done', NULL),
		(2, 'u2', 'trash', CAST('A/a' AS BLOB), CAST('20260101/2/A/a' AS BLOB), '21', 2, 'planned', NULL),
		(3, 'u2', 'create', CAST('B/c' AS BLOB), CAST('3.part' AS BLOB), '33', 3, 'planned', NULL),
		(4, 'u2', 'rename', CAST('Z/b' AS BLOB), CAST('A/b' AS BLOB), '22', 4, 'planned', NULL),
		(5, 'u2', 'mkdir', CAST('C' AS BLOB), NULL, NULL, 5, 'planned', NULL),
		(6, 'u1', 'utimens', CAST('A/a' AS BLOB), NULL, '11', 6, 'planned', NULL),
		(7, 'u1', 'repair', CAST('E/e' AS BLOB), CAST('7.chunks' AS BLOB), '15', 7, 'planned', NULL),
		(8, 'u1', 'link', CAST('A/a' AS BLOB), NULL, '11', 8, 'done', 1),
		(9, 'u1', 'rmdir', CAST('D' AS BLOB), NULL, '109', 9, 'done', NULL),
		(10, 'u1', 'trash', CAST('R/r' AS BLOB), CAST('20260101/10/R/r' AS BLOB), '92', 10, 'done', NULL),
		(11, 'u2', 'trash', CAST('B/c' AS BLOB), CAST('20260101/11/B/c' AS BLOB), '23', 11, 'planned', NULL)`)
	b.exec(`INSERT INTO trash(id, lpath, disk_id, trash_path, op_id, size, blake3, reason, deleted_ns, expires_ns) VALUES
		(1, CAST('Q/q' AS BLOB), 'u1', CAST('20260101/1/Q/q' AS BLOB), 1, 3, zeroblob(32), 'deleted', 1, 100),
		(2, CAST('R/r' AS BLOB), 'u1', CAST('20260101/10/R/r' AS BLOB), 10, 4, NULL, 'overwritten', 1, 5000),
		(3, CAST('B/c' AS BLOB), 'u2', CAST('20260101/11/B/c' AS BLOB), 11, 4, NULL, 'overwritten', 11, 5000)`)
	b.exec(`INSERT INTO jobs(id, kind, file_id, src_disk, dst_disk, payload, priority, not_before_ns, attempts, state, reason) VALUES
		(1, 'replicate', 3, 'u1', 'u2', '{"part":"3.part","refresh":true}', 2, NULL, 0, 'queued', NULL),
		(2, 'trash', 3, 'u1', 'u2', NULL, 4, NULL, 0, 'queued', 'overwritten'),
		(3, 'rename', NULL, 'u1', 'u2','{"from_b64":"Wg==","to_b64":"QQ=="}', 0, NULL, 0, 'queued', NULL),
		(4, 'replicate', 2, 'u1', 'c1', NULL, 1, 50, 1, 'queued', NULL),
		(5, 'trash', 4, 'u1', 'u1', NULL, 0, NULL, 0, 'canceled', 'renamed')`)
	b.exec(`INSERT INTO job_chunks(job_id, idx, hash) VALUES (1, 0, zeroblob(32)), (1, 1, zeroblob(32))`)
	b.exec(`INSERT INTO events(id, ts_ns, level, kind, data) VALUES
		(1, 10, 'warning', 'guard', '{"disk":"u1"}'),
		(2, 11, 'info', 'copy-gone', '{"disk":"u1","path":"A/x","file":1,"gone_ns":11}'),
		(3, 12, 'warning', 'modify-guard', '{"disk":"u1"}'),
		(4, 13, 'info', 'modified', '{"disk":"u1","path":"A/a"}'),
		(5, 14, 'info', 'guard-confirm', '{"disk":"u2","up_to":14}'),
		(6, 15, 'info', 'trash', '{"disk":"u1","path":"Q/q","op":1}')`)
	b.exec(`INSERT INTO alerts(key, first_ns, sent_ns, count) VALUES ('disk-missing:u2', 1, 2, 1), ('files-corrupt', 1, 2, 1)`)
	b.exec(`INSERT INTO view_ops(id, kind, disk_id, lpath, lpath2, folder, file_key, op_id, state, at_ns, taken_ns) VALUES
		(1, 'write', 'u1', CAST('A/a' AS BLOB), NULL, 0, NULL, NULL, 'done', 1, NULL),
		(2, 'mkdir', 'u1', CAST('A' AS BLOB), NULL, 1, NULL, NULL, 'done', 2, NULL),
		(3, 'part', 'u2', CAST('B/c' AS BLOB), NULL, 0, NULL, 3, 'done', 3, NULL)`)
	b.exec(`INSERT INTO scrub_cursor(disk_id, file_id, chunk, size, mtime_ns, ctime_ns) VALUES ('u1', 1, 0, 10, 5, 6)`)
}

// Every reader of the catalog, at each step of its read failing, gives an error and nothing else: no empty list, no
// "not there", no count of 0 that a caller could take for what the catalog holds (CLAUDE.md invariant 4, which says
// so of a disk, holds of the catalog too). And none writes.
func TestEveryReaderFailsWhole(t *testing.T) {
	fb := newFaultBench(t, busyPool)
	type reader struct {
		name string
		opt  sweepOptions
		fn   func(ctx context.Context, db *sql.DB) (any, error)
	}
	one := func(v any, err error) (any, error) { return v, err }
	keeps := sweepOptions{keeps: true}
	readers := []reader{
		{"ReadAlerts", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(ReadAlerts(ctx, db)) }},
		{"CountFileAlerts", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(CountFileAlerts(ctx, db, 100)) }},
		{"FirstStatePaths", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return one(FirstStatePaths(ctx, db, "corrupt", 3))
		}},
		{"FirstManualPaths", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(FirstManualPaths(ctx, db, 100, 3)) }},
		{"Integrity", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(Integrity(ctx, db)) }},
		{"CacheCopies", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(CacheCopies(ctx, db)) }},
		{"ConflictFiles", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return one(ConflictFiles(ctx, NewFileReader(db)))
		}},
		{"ReadDiskHolds", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(ReadDiskHolds(ctx, db, "u1", 2)) }},
		{"Dupes", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(Dupes(ctx, db, 1)) }},
		{"Unhashed", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(Unhashed(ctx, db, "B")) }},
		{"LinkCopies", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			var got []LinkCopy
			err := LinkCopies(ctx, db, "u1", func(int) {}, func(c *LinkCopy) { got = append(got, *c) })
			if err != nil {
				return nil, err
			}
			return got, nil
		}},
		{"GuardUsed", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(GuardUsed(ctx, db, "u1")) }},
		{"TrashKeys", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(TrashKeys(ctx, db, "u1")) }},
		// FilePaths, GonePaths, CopyOf and TrashEntries give what they read before the failure with it: each caller
		// (internal/trash, internal/ops) drops it with the error.
		{"FilePaths", keeps, func(ctx context.Context, db *sql.DB) (any, error) { return one(FilePaths(ctx, db, []int64{1, 2, 9})) }},
		{"GonePaths", keeps, func(ctx context.Context, db *sql.DB) (any, error) {
			return one(GonePaths(ctx, db, "u2", []int64{1, 2, 5}))
		}},
		{"CountForMetrics", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(CountForMetrics(ctx, db, 100)) }},
		{"ReadModifyGuard", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return one(ReadModifyGuard(ctx, db, "u1", 20, 100))
		}},
		{"ModifiedPaths", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return one(ModifiedPaths(ctx, db, "u1", 20, 100, 0))
		}},
		{"StaleWithoutJob", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(StaleWithoutJob(ctx, db, "u2")) }},
		{"EachKnownCopyAt", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			var got []KnownCopy
			err := EachKnownCopyAt(ctx, db, "u1", []string{"A", "B/c", "C"}, func(c *KnownCopy) { got = append(got, *c) })
			if err != nil {
				return nil, err
			}
			return got, nil
		}},
		{"KnownCopiesByKey", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return one(KnownCopiesByKey(ctx, db, "u1", []string{"11", "12"}))
		}},
		{"EachFolderRowAt", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			var got []EntryIdentity
			err := EachFolderRowAt(ctx, db, "u1", []string{"A", "B"}, func(e *EntryIdentity) { got = append(got, *e) })
			if err != nil {
				return nil, err
			}
			return got, nil
		}},
		{"FolderRowsByKey", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return one(FolderRowsByKey(ctx, db, "u1", []string{"101", "102"}))
		}},
		{"ClockedCopies", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(ClockedCopies(ctx, db, "u1")) }},
		{"AllPaths", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(AllPaths(ctx, db)) }},
		{"FoldersWithCopiesOn", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return one(FoldersWithCopiesOn(ctx, db, []string{"A", "B"}, []string{"u2"}))
		}},
		{"CopyOf", keeps, func(ctx context.Context, db *sql.DB) (any, error) { return one(CopyOf(ctx, db, "u1", "A/a", "11")) }},
		{"KeyTaken", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(KeyTaken(ctx, db, "u1", "a/A")) }},
		{"KeysTaken", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			taken, done := KeysTaken(ctx, db)
			defer done()
			var got []bool
			for _, p := range []string{"a/A", "x", "c/D"} {
				ok, err := taken("u1", p)
				if err != nil {
					return nil, err
				}
				got = append(got, ok)
			}
			return got, nil
		}},
		{"KeyTakenExcept", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return one(KeyTakenExcept(ctx, db, "u1", "a/A", "A/b"))
		}},
		{"KeyStaysTaken", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return one(KeyStaysTaken(ctx, db, "u1", "a/A", "A/b", []string{"A/a"}))
		}},
		{"TakenByRename", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(TakenByRename(ctx, db, "u2", "a/B")) }},
		{"LinkedPaths", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(LinkedPaths(ctx, db, "u1", "12")) }},
		{"CopyCtimes", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(CopyCtimes(ctx, db, 1)) }},
		{"MkdirLikeOf", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(MkdirLikeOf(ctx, db, 5)) }},
		{"PlannedOps", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(PlannedOps(ctx, db)) }},
		{"PlannedAt", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(PlannedAt(ctx, db, "u1", "A/a")) }},
		{"EachTrashEntry", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			var got []TrashEntry
			err := EachTrashEntry(ctx, db, func(e *TrashEntry) error { got = append(got, *e); return nil })
			if err != nil {
				return nil, err
			}
			return got, nil
		}},
		{"EachTrashEntryBelow", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			var got []TrashEntry
			err := EachTrashEntryBelow(ctx, db, "Q", func(e *TrashEntry) error { got = append(got, *e); return nil })
			if err != nil {
				return nil, err
			}
			return got, nil
		}},
		{"TrashExpired", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(TrashExpired(ctx, db, 6000, 0, 0)) }},
		{"TrashEntryByRow", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			e, ok, err := TrashEntryByRow(ctx, db, 1)
			return pair(e, ok), err
		}},
		{"TrashEntryOfOp", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			e, ok, err := TrashEntryOfOp(ctx, db, "Q/q", 1)
			return pair(e, ok), err
		}},
		{"TrashDue", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(TrashDue(ctx, db, 200)) }},
		{"TrashUsed", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(TrashUsed(ctx, db)) }},
		{"TrashEntries", keeps, func(ctx context.Context, db *sql.DB) (any, error) { return one(TrashEntries(ctx, db)) }},
		{"TrashWeights", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(TrashWeights(ctx, db)) }},
		{"TrashWeightsOn", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(TrashWeightsOn(ctx, db, "u1")) }},
		{"PlacesOf", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(PlacesOf(ctx, db, "u2", "22")) }},
		{"OpOnDisk", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(OpOnDisk(ctx, db, "u2", 3)) }},
		{"PlanSet", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return one(PlanSet(ctx, db, map[string][]string{"u1": {"A", "B/c"}, "u2": {"A/a"}}, []string{"c1"}))
		}},
		{"RunningCopies", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(RunningCopies(ctx, db)) }},
		{"RunningFiles", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(RunningFiles(ctx, db)) }},
		{"FileIDsAt", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return one(FileIDsAt(ctx, db, []string{"A/a", "B/c"}))
		}},
		{"FileIDsBelow", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(FileIDsBelow(ctx, db, "A")) }},
		{"DueFiles", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(DueFiles(ctx, NewFileReader(db))) }},
		{"FileAt", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(FileAt(ctx, db, "A/a")) }},
		{"FileByID", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(FileByID(ctx, db, 3)) }},
		{"FileFinder", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			f, err := NewFileFinder(ctx, db)
			if err != nil {
				return nil, err
			}
			defer func() { _ = f.Close() }()
			row, err := f.ByID(ctx, 3)
			if err != nil {
				return nil, err
			}
			path, ok, err := f.Path(ctx, 2)
			if err != nil {
				return nil, err
			}
			running, err := f.Running(ctx, 3)
			if err != nil {
				return nil, err
			}
			return []any{row, path, ok, running}, nil
		}},
		{"Waiting", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			n, next, err := Waiting(ctx, db, 10, 100)
			return pair(n, next), err
		}},
		{"FilesBelow", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return one(FilesBelow(ctx, NewFileReader(db), "A"))
		}},
		{"IgnoredPaths", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(IgnoredPaths(ctx, db, "u1")) }},
		{"EachFolderRow", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			var got []EntryIdentity
			if err := EachFolderRow(ctx, db, "u1", func(e *EntryIdentity) { got = append(got, *e) }); err != nil {
				return nil, err
			}
			return got, nil
		}},
		{"EntriesBelow", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(EntriesBelow(ctx, db, "u1", "A")) }},
		{"CopiesBelow", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(CopiesBelow(ctx, db, "u1", "A")) }},
		{"MadeBelow", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(MadeBelow(ctx, db, "u2", "A")) }},
		{"SettledBelow", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(SettledBelow(ctx, db, "u2", "A")) }},
		{"AtOwnPathBelow", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(AtOwnPathBelow(ctx, db, "u2", "A")) }},
		{"PathHeld", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(PathHeld(ctx, db, "Z/b")) }},
		{"DisksBelow", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(DisksBelow(ctx, db, "A", "u1")) }},
		{"JobDisks", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(JobDisks(ctx, db, 3)) }},
		{"FolderJobs", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(FolderJobs(ctx, db, "u2")) }},
		{"PendingRenames", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(PendingRenames(ctx, db, "u2", "", 10)) }},
		{"TrashJobs", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(TrashJobs(ctx, db, "u2")) }},
		{"ReadGuard", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(ReadGuard(ctx, db, "u1", 20, 100)) }},
		{"CopiesBefore", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(CopiesBefore(ctx, db, "u1", 5)) }},
		{"CopyCount", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(CopyCount(ctx, db, "u1")) }},
		{"TrashJobState", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(TrashJobState(ctx, db, 3, "u2")) }},
		{"Leaving", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(Leaving(ctx, db, "A", "C")) }},
		{"QueuedTrash", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return one(QueuedTrash(ctx, db, 3, "u2", "overwritten"))
		}},
		{"GuardPaths", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return one(GuardPaths(ctx, db, "u1", sql.NullInt64{Int64: 100, Valid: true}))
		}},
		{"RepairOf", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			bad, mtime, ok, err := RepairOf(ctx, db, 7)
			return three(bad, mtime, ok), err
		}},
		{"RepairCopyOf", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			c, ok, err := RepairCopyOf(ctx, db, &Op{ID: 7, Disk: "u1", Kind: OpRepair, Path: "E/e", FileKey: "15"})
			return pair(c, ok), err
		}},
		{"CorruptCopies", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(CorruptCopies(ctx, db, "", "")) }},
		{"FileChunks", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(FileChunks(ctx, db, 5)) }},
		{"ReplicateJobs", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(ReplicateJobs(ctx, db, "B/c")) }},
		{"DueReader", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			r := NewDueReader(db, 100)
			var all []ReplicateJob
			for {
				page, err := r.Next(ctx)
				if err != nil {
					return nil, err
				}
				if len(page) == 0 {
					return all, nil
				}
				all = append(all, page...)
			}
		}},
		{"CopiesLater", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			n, next, err := CopiesLater(ctx, db, 10)
			return pair(n, next), err
		}},
		{"JobOfPart", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(JobOfPart(ctx, db, "u2", "3.part")) }},
		{"PartsInUse", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(PartsInUse(ctx, db, "u2")) }},
		{"PartStates", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return one(PartStates(ctx, db, "u2", []string{"3.part", "7.0.part", "x.part"}))
		}},
		{"JobChunks", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(JobChunks(ctx, db, 1)) }},
		{"IsRestoreTo", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(IsRestoreTo(ctx, db, 1)) }},
		{"OwnFolder", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			key, own, err := OwnFolder(ctx, db, "u2", "A")
			return pair(key, own), err
		}},
		{"FolderKey", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(FolderKey(ctx, db, "u1", "A")) }},
		{"NothingBelow", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(NothingBelow(ctx, db, "u1", "C")) }},
		{"EachKnownCopy", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			var got []KnownCopy
			if err := EachKnownCopy(ctx, db, "u2", func(c *KnownCopy) { got = append(got, *c) }); err != nil {
				return nil, err
			}
			return got, nil
		}},
		{"DisksWithGoneCopies", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(DisksWithGoneCopies(ctx, db)) }},
		{"FileReader", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			r := NewFileReader(db)
			var all []FileRow
			for {
				page, err := r.Next(ctx)
				if err != nil {
					return nil, err
				}
				if len(page) == 0 {
					return all, nil
				}
				all = append(all, page...)
			}
		}},
		{"NextUnhashed", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			r := NewFileReader(db)
			var all []FileRow
			for {
				page, err := r.NextUnhashed(ctx)
				if err != nil {
					return nil, err
				}
				if len(page) == 0 {
					return all, nil
				}
				all = append(all, page...)
			}
		}},
		{"RuleSubjects", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			var got []string
			if err := RuleSubjects(ctx, db, func(path string, _ *int64) { got = append(got, path) }); err != nil {
				return nil, err
			}
			return got, nil
		}},
		{"Entries", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(Entries(ctx, db)) }},
		{"Disks", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(Disks(ctx, db)) }},
		{"LargestFile", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			n, ok, err := LargestFile(ctx, db)
			return pair(n, ok), err
		}},
		{"TotalBytes", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(TotalBytes(ctx, db)) }},
		{"FileCount", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(FileCount(ctx, db)) }},
		{"ConflictPaths", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(ConflictPaths(ctx, db)) }},
		{"ScrubDue", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return one(ScrubDue(ctx, db, "u1", 100, ScrubKey{VerifiedNS: -2}, 10, ""))
		}},
		{"ScrubCopyOf", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			c, ok, err := ScrubCopyOf(ctx, db, "u1", 1)
			return pair(c, ok), err
		}},
		{"ReadScrubCursor", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			c, ok, err := ReadScrubCursor(ctx, db, "u1")
			return pair(c, ok), err
		}},
		{"FoldersTidiedThrough", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(FoldersTidiedThrough(ctx, db)) }},
		{"HighestOp", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(HighestOp(ctx, db)) }},
		{"LeftSince", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			var got []string
			if err := LeftSince(ctx, db, 0, 20, func(disk, place string) { got = append(got, disk+":"+place) }); err != nil {
				return nil, err
			}
			return got, nil
		}},
		{"UtimensTime", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			n, ok, err := UtimensTime(ctx, db, 6)
			return pair(n, ok), err
		}},
		{"ChmodMode", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			n, ok, err := ChmodMode(ctx, db, 6)
			return pair(n, ok), err
		}},
		{"ChownOwner", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			uid, gid, ok, err := ChownOwner(ctx, db, 6)
			return three(uid, gid, ok), err
		}},
		{"ViewDisks", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(ViewDisks(ctx, db)) }},
		{"ViewRows", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			f, e, err := ViewRows(ctx, db, "A/a")
			return pair(f, e), err
		}},
		{"ViewRowsOf", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return one(ViewRowsOf(ctx, db, []string{"A", "A/a", "A/l", "B/c", "none"}))
		}},
		{"ViewNames", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(ViewNames(ctx, db, "A")) }},
		{"OpenViewOps", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(OpenViewOps(ctx, db)) }},
		{"LongestBelow", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(LongestBelow(ctx, db, "u1", "A")) }},
		{"OpByID", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			op, ok, err := OpByID(ctx, db, 3)
			return pair(op, ok), err
		}},
		{"FolderDisks", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(FolderDisks(ctx, db, "A")) }},
		{"IsViewOp", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(IsViewOp(ctx, db, 3)) }},
		{"GroupHolders", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			return one(GroupHolders(ctx, db, "A", true, func(string) bool { return true }))
		}},
		{"OpenViewParts", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) { return one(OpenViewParts(ctx, db)) }},
		{"ReplicateJobPart", sweepOptions{}, func(ctx context.Context, db *sql.DB) (any, error) {
			part, ok, err := ReplicateJobPart(ctx, db, 3, "u2")
			return pair(part, ok), err
		}},
	}
	for _, r := range readers {
		before := snapshot(t, mustReset(fb))
		steps, got := fb.sweep(r.name, r.opt, r.fn)
		if steps == 0 {
			t.Errorf("%s: read nothing", r.name)
		}
		_ = got
		if after := snapshot(t, fb.db); after != before {
			t.Errorf("%s: a reader wrote:\n%s", r.name, diffLines(before, after))
		}
	}
}

// mustReset gives the bench's catalog as the template holds it.
func mustReset(fb *faultBench) *sql.DB {
	fb.reset()
	return fb.db
}

// pair puts two results in one, for a sweep.
func pair[A, B any](a A, b B) any {
	return struct {
		A A
		B B
	}{a, b}
}

// three puts three results in one, for a sweep.
func three[A, B, C any](a A, b B, c C) any {
	return struct {
		A A
		B B
		C C
	}{a, b, c}
}
