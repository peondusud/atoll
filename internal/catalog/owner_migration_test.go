package catalog

import (
	"path/filepath"
	"testing"

	"github.com/peondusud/atoll/schema"
)

// The migration that brings the owner makes the journal again for its new kind: every operation keeps its id and
// what it says, the trash still refers to its operations, and the counter keeps the highest id ever given, past the
// highest row left (specs.md §9, §17).
func TestTheOwnerMigrationKeepsTheJournal(t *testing.T) {
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
		if m.Name == "0006_owner.sql" {
			at = i
		}
	}
	if at < 0 {
		t.Fatal("no migration 0006_owner.sql")
	}
	if err := migrate(ctx, db, list[:at]); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"INSERT INTO ops(id, disk_id, kind, path, path2, file_key, started_ns, state) VALUES (3, 'd1', 'trash', x'61', x'2f33', '12', 5, 'done')",
		"INSERT INTO ops(id, disk_id, kind, path, started_ns, state) VALUES (4, 'd1', 'chmod', x'62', 6, 'planned')",
		"INSERT INTO trash(lpath, disk_id, trash_path, op_id, size, reason, deleted_ns, expires_ns) VALUES (x'61', 'd1', x'2f33', 3, 1, 'deleted', 5, 9)",
		// Operations up to 40 were given, and the rows of the others are gone.
		"UPDATE sqlite_sequence SET seq = 40 WHERE name = 'ops'",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := migrate(ctx, db, list); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := db.QueryRowContext(ctx, `SELECT group_concat(id || ' ' || kind || ' ' || CAST(path AS TEXT) || ' ' || COALESCE(file_key, '-') || ' ' || state, ', ')
		FROM (SELECT * FROM ops ORDER BY id)`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if want := "3 trash a 12 done, 4 chmod b - planned"; got != want {
		t.Errorf("%q", got)
	}
	var seq int64
	if err := db.QueryRowContext(ctx, "SELECT seq FROM sqlite_sequence WHERE name = 'ops'").Scan(&seq); err != nil || seq != 40 {
		t.Errorf("counter %d, %v", seq, err)
	}
	var stray int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_sequence WHERE name = 'ops_new'").Scan(&stray); err != nil || stray != 0 {
		t.Errorf("%d counters of ops_new, %v", stray, err)
	}
	var broken int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM pragma_foreign_key_check").Scan(&broken); err != nil || broken != 0 {
		t.Errorf("%d references broke, %v", broken, err)
	}
	res, err := db.ExecContext(ctx, "INSERT INTO ops(disk_id, kind, path, file_key, started_ns, state) VALUES ('d1', 'chown', x'63', '13', 7, 'planned')")
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := res.LastInsertId(); id != 41 {
		t.Errorf("the next operation is %d", id)
	}
	var planned int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type = 'index' AND name = 'ops_planned' AND tbl_name = 'ops'").Scan(&planned); err != nil || planned != 1 {
		t.Errorf("ops_planned: %d, %v", planned, err)
	}
	// A trash entry still needs its operation.
	if _, err := db.ExecContext(ctx, "INSERT INTO trash(lpath, disk_id, trash_path, op_id, size, reason, deleted_ns, expires_ns) VALUES (x'64', 'd1', x'2f39', 99, 1, 'deleted', 5, 9)"); err == nil {
		t.Error("a trash entry with no operation went in")
	}
}

// A journal that never held an operation keeps having no counter, and its first operation is 1.
func TestTheOwnerMigrationOnAnEmptyJournal(t *testing.T) {
	db := open(t)
	res, err := db.ExecContext(t.Context(), "INSERT INTO ops(disk_id, kind, path, started_ns, state) VALUES ('d1', 'chown', x'63', 7, 'planned')")
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := res.LastInsertId(); id != 1 {
		t.Errorf("the first operation is %d", id)
	}
}
