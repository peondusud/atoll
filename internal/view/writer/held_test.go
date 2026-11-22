package writer

import (
	"errors"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
)

// viewStates gives the state of each row of the view's journal of a kind, in the order of their ids.
func (b *bench) viewStates(kind string) []string {
	b.t.Helper()
	rows, err := b.w.db.QueryContext(b.t.Context(), "SELECT state FROM view_ops WHERE kind = ? ORDER BY id", kind)
	if err != nil {
		b.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			b.t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		b.t.Fatal(err)
	}
	return out
}

func (b *bench) exec(q string, args ...any) {
	b.t.Helper()
	if _, err := b.w.db.ExecContext(b.t.Context(), q, args...); err != nil {
		b.t.Fatal(err)
	}
}

// openToWrite opens Films/a.mkv to write through the view, as presented now: its copy on t2, the oldest.
func (b *bench) openToWrite() (*File, error) {
	b.t.Helper()
	return b.w.Open(b.t.Context(), "Films/a.mkv", b.lookup("Films/a.mkv"), root)
}

// A file the view holds open to write keeps its row open, so that no pass takes it in, until its last handle closes
// (specs.md §21.2, "Held open"; conformance/models/README.md, flaw 10): a write, opened twice, and a new file.
func TestAFileIsHeldOpenUntilItsLastHandleCloses(t *testing.T) {
	b := newBench(t, "ext4")
	first, err := b.openToWrite()
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.openToWrite()
	if err != nil {
		t.Fatal(err)
	}
	if got := b.viewStates("write"); len(got) != 1 || got[0] != catalog.ViewHeld {
		t.Fatalf("opened twice: %v", got)
	}
	_ = first.Close()
	if got := b.viewStates("write"); got[0] != catalog.ViewHeld {
		t.Errorf("one handle closed: %v", got)
	}
	_ = second.Close()
	if got := b.viewStates("write"); got[0] != catalog.OpDone {
		t.Errorf("both closed: %v", got)
	}
	// Opened again before a pass takes it in, the same row is held open again.
	again, err := b.openToWrite()
	if err != nil {
		t.Fatal(err)
	}
	if got := b.viewStates("write"); len(got) != 1 || got[0] != catalog.ViewHeld {
		t.Errorf("opened again: %v", got)
	}
	_ = again.Close()

	f, _, err := b.w.Create(t.Context(), "Films/new.mkv", 0o644, root)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.viewStates("create"); len(got) != 1 || got[0] != catalog.ViewHeld {
		t.Errorf("a new file being written: %v", got)
	}
	_ = f.Close()
	if got := b.viewStates("create"); got[0] != catalog.OpDone {
		t.Errorf("a new file closed: %v", got)
	}
}

// atoll mount stopped, as by kill -9, while it holds files open: their rows stay open, and its next start lets go of
// them before anything else, done, for a pass to take in.
func TestAStoppedViewLetsGoAtItsStartOfWhatItHeldOpen(t *testing.T) {
	b := newBench(t, "ext4")
	if _, err := b.openToWrite(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.w.Create(t.Context(), "Films/new.mkv", 0o644, root); err != nil {
		t.Fatal(err)
	}
	// Stopped there: nothing is closed. Its next start resolves what it left (Reprise).
	if err := b.w.Reprise(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := append(b.viewStates("write"), b.viewStates("create")...); len(got) != 2 || got[0] != catalog.OpDone || got[1] != catalog.OpDone {
		t.Errorf("after the start: %v", got)
	}
}

// A catalog that refuses the row's end as the last handle closes leaves it open, never done too early: the view's next
// start lets go of it.
func TestARowTheCatalogKeepsOpenIsLetGoOfAtTheNextStart(t *testing.T) {
	b := newBench(t, "ext4")
	f, err := b.openToWrite()
	if err != nil {
		t.Fatal(err)
	}
	b.exec(`CREATE TEMP TRIGGER refuse BEFORE UPDATE OF state ON view_ops WHEN OLD.state = 'open' BEGIN SELECT RAISE(ABORT, 'refused'); END`)
	_ = f.Close()
	if got := b.viewStates("write"); got[0] != catalog.ViewHeld {
		t.Errorf("closed, the catalog refusing: %v", got)
	}
	b.exec("DROP TRIGGER refuse")
	if err := b.w.Reprise(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := b.viewStates("write"); got[0] != catalog.OpDone {
		t.Errorf("after the start: %v", got)
	}
}

// planMove journals a move of Atoll's own of the copy of Films/a.mkv on t2 to its trash, as the SSD cache's transfer
// does, and gives its id.
func (b *bench) planMove() int64 {
	b.t.Helper()
	res, err := b.w.db.ExecContext(b.t.Context(), "INSERT INTO ops(disk_id, kind, path, path2, started_ns, state) VALUES ('t2', 'trash', CAST('Films/a.mkv' AS BLOB), x'62', 0, 'planned')")
	if err != nil {
		b.t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		b.t.Fatal(err)
	}
	return id
}

// An open to write waits while a move of Atoll's own of the copy it would write is planned, and opens once the move
// is over (specs.md §21.2; flaw 10). A move a stop leaves planned for the daemon's reprise holds it past its wait, and
// it fails with ErrBusy, EBUSY for the program, the copy unwritten.
func TestAnOpenToWriteWaitsWhileAMoveOfItsCopyIsPlanned(t *testing.T) {
	saved, savedTry := moveWait, moveTry
	t.Cleanup(func() { moveWait, moveTry = saved, savedTry })
	moveWait, moveTry = 2*time.Second, 5*time.Millisecond

	b := newBench(t, "ext4")
	id := b.planMove()
	done := make(chan error, 1)
	go func() {
		f, err := b.openToWrite()
		if err == nil {
			_ = f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("it opened while the move is planned: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if got := b.viewStates("write"); len(got) != 0 {
		t.Errorf("journaled while it waits: %v", got)
	}
	// The move is undone (its copy stayed): the open goes on, to the copy the view presents.
	b.exec("UPDATE ops SET state = 'undone' WHERE id = ?", id)
	if err := <-done; err != nil {
		t.Fatalf("once the move is over: %v", err)
	}
	if got := b.viewStates("write"); len(got) != 1 || got[0] != catalog.OpDone {
		t.Errorf("once opened and closed: %v", got)
	}

	moveWait = 50 * time.Millisecond
	// The row not taken in yet, held open again: it waits too.
	id = b.planMove()
	f, err := b.openToWrite()
	if !errors.Is(err, ErrBusy) {
		t.Errorf("a row held open again while a move is planned: %v", err)
	}
	if f != nil {
		_ = f.Close()
	}
	if got := b.viewStates("write"); len(got) != 1 || got[0] != catalog.OpDone {
		t.Errorf("the row held open again while the move stays planned: %v", got)
	}
	b.exec("UPDATE ops SET state = 'undone' WHERE id = ?", id)
	// The row taken in, a new write journaled: it waits, and nothing is journaled.
	b.exec("UPDATE view_ops SET taken_ns = 1")
	b.planMove()
	if f, err = b.openToWrite(); !errors.Is(err, ErrBusy) {
		t.Errorf("a move that stays planned: %v", err)
	}
	if f != nil {
		_ = f.Close()
	}
	if got := b.viewStates("write"); len(got) != 1 {
		t.Errorf("a write journaled while the move stays planned: %v", got)
	}
}
