package catalog

import (
	"path/filepath"
	"testing"

	"github.com/peondusud/atoll/schema"
)

// A move of the view runs one operation of ops at a time, which its row names (specs.md §9, §21.2): the create of its
// part, journaled with its replicate event naming the full disk; then, once its part is at its name, the trash of the
// copy left on the full disk, surplus, the create done; then the unlink that purges it, the trash done; then the move
// done, the entry gone from the trash. The one planned operation is always the view's, which no command's reprise
// resolves; the move presents the full disk's copy only while its create is the operation it names; and what it sent to
// the trash is Atoll's own, never a user's deletion, which the journal's places keep.
func TestAViewMoveRunsOneOperationAtATime(t *testing.T) {
	b := openForOps(t)
	ctx := t.Context()
	v := &ViewOp{Disk: "u2", Disk2: "u1", Path: "F/new.mkv", FileKey: "17", AtNS: 10}
	created, err := BeginViewMove(ctx, b.sql(), v)
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != ViewMove || v.State != OpPlanned || v.OpID != created.ID || created.Kind != OpCreate || created.Disk != "u2" || created.Path2 == "" {
		t.Fatalf("the move as journaled: %+v, %+v", v, created)
	}
	var source string
	if err := b.db.QueryRowContext(ctx, "SELECT json_extract(data, '$.source') FROM events WHERE kind = 'replicate'").Scan(&source); err != nil || source != "u1" {
		t.Errorf("the create's event names %q as source, %v", source, err)
	}
	open, err := OpenViewOps(ctx, b.sql())
	if err != nil || len(open) != 1 {
		t.Fatalf("open: %+v, %v", open, err)
	}
	if got := open[0]; got.Disk2 != "u1" || got.OpKind != OpCreate || !got.AtSource() || got.FileKey != "17" {
		t.Errorf("the move as read back: %+v", got)
	}
	planned, err := PlannedOps(ctx, b.sql())
	if err != nil || len(planned) != 0 {
		t.Errorf("a command's reprise would resolve the move's create: %+v, %v", planned, err)
	}

	trashed, err := ViewMoveTrash(ctx, b.sql(), v, &TrashStart{Disk: "u1", Path: "F/new.mkv", FileKey: "17", Reason: ReasonSurplus, NowNS: 11, ExpiresNS: 99})
	if err != nil {
		t.Fatal(err)
	}
	if b.opState(created.ID) != OpDone || b.opState(trashed.ID) != OpPlanned || v.OpID != trashed.ID {
		t.Errorf("once at its name: the create %s, the trash %s, the move names %d", b.opState(created.ID), b.opState(trashed.ID), v.OpID)
	}
	if open, err = OpenViewOps(ctx, b.sql()); err != nil || len(open) != 1 || open[0].AtSource() || open[0].OpKind != OpTrash {
		t.Errorf("once at its name, the move presents the disk it went to: %+v, %v", open, err)
	}
	if planned, err = PlannedOps(ctx, b.sql()); err != nil || len(planned) != 0 {
		t.Errorf("a command's reprise would resolve the move's trash: %+v, %v", planned, err)
	}
	if is, err := IsViewDeletion(ctx, b.sql(), trashed.ID); err != nil || is {
		t.Errorf("what a move sends to the trash is a user's deletion: %v, %v", is, err)
	}
	if places, err := PlacesOf(ctx, b.sql(), "u1", "17"); err != nil || len(places) != 1 || places[0] != trashed.Path2 {
		t.Errorf("the journal's places of the copy the move left: %v, %v", places, err)
	}

	purge, err := ViewMovePurge(ctx, b.sql(), v, trashed, 12)
	if err != nil {
		t.Fatal(err)
	}
	if b.opState(trashed.ID) != OpDone || b.opState(purge.ID) != OpPlanned || purge.Kind != OpUnlink || purge.Path2 != trashed.Path2 || purge.FileKey != "17" {
		t.Errorf("the purge: the trash %s, %+v", b.opState(trashed.ID), purge)
	}
	if entries, err := TrashEntries(ctx, b.sql()); err != nil || len(entries) != 1 || entries[0].Reason != ReasonSurplus {
		t.Errorf("the trash while it is purged: %+v, %v", entries, err)
	}
	if err := EndViewMove(ctx, b.sql(), v, OpDone, OpDone, false, 13); err != nil {
		t.Fatal(err)
	}
	if b.opState(purge.ID) != OpDone || v.State != OpDone {
		t.Errorf("the end: the purge %s, the move %s", b.opState(purge.ID), v.State)
	}
	if entries, err := TrashEntries(ctx, b.sql()); err != nil || len(entries) != 0 {
		t.Errorf("the trash once purged: %+v, %v", entries, err)
	}
	var state string
	if err := b.db.QueryRowContext(ctx, "SELECT state FROM view_ops WHERE id = ?", v.ID).Scan(&state); err != nil || state != OpDone {
		t.Errorf("the move's row: %s, %v", state, err)
	}
	var events int
	if err := b.db.QueryRowContext(ctx, "SELECT count(*) FROM events WHERE kind IN ('replicate', 'trash', 'purge')").Scan(&events); err != nil || events != 3 {
		t.Errorf("events: %d, %v", events, err)
	}
}

// A move that stops before its part is at its name is undone with its create; a trash refused is undone with its trash
// row, the move done, and its op-undone event when asked (specs.md §17.2, §21.2).
func TestAViewMoveUndone(t *testing.T) {
	b := openForOps(t)
	ctx := t.Context()
	v := &ViewOp{Disk: "u2", Disk2: "u1", Path: "F/new.mkv", FileKey: "17", AtNS: 10}
	created, err := BeginViewMove(ctx, b.sql(), v)
	if err != nil {
		t.Fatal(err)
	}
	if err := EndViewMove(ctx, b.sql(), v, OpUndone, OpUndone, false, 11); err != nil {
		t.Fatal(err)
	}
	if b.opState(created.ID) != OpUndone || v.State != OpUndone {
		t.Errorf("the create %s, the move %s", b.opState(created.ID), v.State)
	}
	if open, err := OpenViewOps(ctx, b.sql()); err != nil || len(open) != 0 {
		t.Errorf("an undone move is presented: %+v, %v", open, err)
	}

	w := &ViewOp{Disk: "u2", Disk2: "u1", Path: "F/other.mkv", FileKey: "18", AtNS: 12}
	if _, err := BeginViewMove(ctx, b.sql(), w); err != nil {
		t.Fatal(err)
	}
	trashed, err := ViewMoveTrash(ctx, b.sql(), w, &TrashStart{Disk: "u1", Path: "F/other.mkv", FileKey: "18", Reason: ReasonSurplus, NowNS: 13, ExpiresNS: 99})
	if err != nil {
		t.Fatal(err)
	}
	if err := EndViewMove(ctx, b.sql(), w, OpUndone, OpDone, true, 14); err != nil {
		t.Fatal(err)
	}
	if b.opState(trashed.ID) != OpUndone || w.State != OpDone {
		t.Errorf("the trash %s, the move %s", b.opState(trashed.ID), w.State)
	}
	var rows, events int
	if err := b.db.QueryRowContext(ctx, "SELECT count(*) FROM trash").Scan(&rows); err != nil || rows != 0 {
		t.Errorf("an undone trash keeps its row: %d, %v", rows, err)
	}
	if err := b.db.QueryRowContext(ctx, "SELECT count(*) FROM events WHERE kind = 'op-undone'").Scan(&events); err != nil || events != 1 {
		t.Errorf("op-undone events: %d, %v", events, err)
	}
	// The table refuses a move with no disk it leaves, or one on a single disk, and a disk it leaves to any other kind.
	for _, q := range []string{
		"INSERT INTO view_ops(kind, disk_id, lpath, folder, file_key, op_id, state, at_ns) VALUES ('move', 'u2', x'61', 0, '1', 1, 'planned', 1)",
		"INSERT INTO view_ops(kind, disk_id, disk2_id, lpath, folder, file_key, op_id, state, at_ns) VALUES ('move', 'u2', 'u2', x'61', 0, '1', 1, 'planned', 1)",
		"INSERT INTO view_ops(kind, disk_id, disk2_id, lpath, folder, op_id, state, at_ns) VALUES ('move', 'u2', 'u1', x'61', 0, 1, 'planned', 1)",
		"INSERT INTO view_ops(kind, disk_id, disk2_id, lpath, folder, state, at_ns) VALUES ('create', 'u2', 'u1', x'61', 0, 'planned', 1)",
	} {
		if _, err := b.db.ExecContext(ctx, q); err == nil {
			t.Errorf("accepted: %s", q)
		}
	}
}

func (b *testDB) opState(id int64) string {
	b.t.Helper()
	var state string
	if err := b.db.QueryRowContext(b.t.Context(), "SELECT state FROM ops WHERE id = ?", id).Scan(&state); err != nil {
		b.t.Fatal(err)
	}
	return state
}

// The migration that brings the move makes the view's journal again: every row keeps its id and what it says, an
// operation of ops still named, and the counter keeps the highest id ever given (specs.md §9).
func TestTheMoveMigrationKeepsTheViewJournal(t *testing.T) {
	ctx := t.Context()
	db, err := connect(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	list, err := schema.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	at := -1
	for i, m := range list {
		if m.Name == "0023_view_move.sql" {
			at = i
		}
	}
	if at < 0 {
		t.Fatal("no migration 0023_view_move.sql")
	}
	if err := migrate(ctx, db, list[:at]); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"INSERT INTO ops(id, disk_id, kind, path, path2, file_key, started_ns, state) VALUES (3, 'd1', 'trash', x'61', x'2f33', '12', 5, 'done')",
		"INSERT INTO ops(id, disk_id, kind, path, path2, started_ns, state) VALUES (4, 'd2', 'create', x'62', x'342e70617274', 6, 'planned')",
		"INSERT INTO view_ops(id, kind, disk_id, lpath, folder, file_key, op_id, state, at_ns) VALUES (5, 'unlink', 'd1', x'61', 0, '12', 3, 'done', 5)",
		"INSERT INTO view_ops(id, kind, disk_id, lpath, folder, op_id, state, at_ns, taken_ns) VALUES (6, 'part', 'd2', x'62', 0, 4, 'done', 6, 7)",
		"INSERT INTO view_ops(id, kind, disk_id, lpath, lpath2, folder, file_key, state, at_ns) VALUES (8, 'link', 'd1', x'61', x'63', 0, '12', 'planned', 8)",
		"UPDATE sqlite_sequence SET seq = 30 WHERE name = 'view_ops'",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := migrate(ctx, db, list); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := db.QueryRowContext(ctx, `SELECT group_concat(id || ' ' || kind || ' ' || disk_id || ' ' || COALESCE(op_id, '-') || ' ' || state || ' ' || COALESCE(taken_ns, '-') || ' ' || COALESCE(disk2_id, '-'), ', ')
		FROM (SELECT * FROM view_ops ORDER BY id)`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if want := "5 unlink d1 3 done - -, 6 part d2 4 done 7 -, 8 link d1 - planned - -"; got != want {
		t.Errorf("%q", got)
	}
	var seq int64
	if err := db.QueryRowContext(ctx, "SELECT seq FROM sqlite_sequence WHERE name = 'view_ops'").Scan(&seq); err != nil || seq != 30 {
		t.Errorf("counter %d, %v", seq, err)
	}
	var broken int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM pragma_foreign_key_check").Scan(&broken); err != nil || broken != 0 {
		t.Errorf("%d references broke, %v", broken, err)
	}
	for _, index := range []string{"view_ops_open", "view_ops_by_op"} {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type = 'index' AND name = ? AND tbl_name = 'view_ops'", index).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s: %d, %v", index, n, err)
		}
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO ops(id, disk_id, kind, path, path2, started_ns, state) VALUES (9, 'd2', 'create', x'64', x'392e70617274', 9, 'planned')"); err != nil {
		t.Fatal(err)
	}
	res, err := db.ExecContext(ctx, "INSERT INTO view_ops(kind, disk_id, disk2_id, lpath, folder, file_key, op_id, state, at_ns) VALUES ('move', 'd2', 'd1', x'64', 0, '13', 9, 'planned', 9)")
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := res.LastInsertId(); id != 31 {
		t.Errorf("the next row is %d", id)
	}
}
