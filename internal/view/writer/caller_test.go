package writer

import (
	"bytes"
	"database/sql"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/ops"
)

// askers gives, by the id of each row of view_ops, its kind and who asked it, as the journal holds them: "uid/pid",
// "-" for a NULL.
func askers(t *testing.T, b *bench) []string {
	t.Helper()
	db, ok := b.db.(*sql.DB)
	if !ok {
		t.Fatal("no catalog")
	}
	rows, err := db.QueryContext(t.Context(), "SELECT kind, uid, pid FROM view_ops ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var kind string
		var uid, pid sql.NullInt64
		if err := rows.Scan(&kind, &uid, &pid); err != nil {
			t.Fatal(err)
		}
		shown := func(n sql.NullInt64) string {
			if !n.Valid {
				return "-"
			}
			return fmt.Sprint(n.Int64)
		}
		out = append(out, kind+" "+shown(uid)+"/"+shown(pid))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Each operation the view journals names who asked it, as the kernel named the caller of its FUSE request: its account
// and its thread, beside what it did, so that what the view did on a user's behalf says which user. A thread the kernel
// did not name is NULL; and so is all of it for a row written outside a request, as by a test's caller here, or by the
// view's reprise (specs.md §9, §21.2).
func TestTheJournalNamesWhoAsked(t *testing.T) {
	b := newBench(t, "ext4")
	ctx := t.Context()
	who := Caller{Owner: Owner{UID: 0, GID: 0}, PID: 4242, Creds: root.Creds, InRequest: true}
	f, _, err := b.w.Create(ctx, "Films/new.mkv", 0o644, who)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := b.w.Mkdir(ctx, "Series", 0o755, who); err != nil {
		t.Fatal(err)
	}
	if _, err := b.w.Symlink(ctx, "Films/link", "a.mkv", who); err != nil {
		t.Fatal(err)
	}
	if f, err = b.w.Open(ctx, "Films/a.mkv", b.lookup("Films/a.mkv"), who); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := b.w.Link(ctx, "Films/new.mkv", "Films/new2.mkv", who); err != nil {
		t.Fatal(err)
	}
	if err := b.w.SetXattr(ctx, "Films/new.mkv", b.lookup("Films/new.mkv"), "user.a", []byte("1"), 0, who); err != nil {
		t.Fatal(err)
	}
	mode := uint32(0o600)
	if _, err := b.w.Setattr(ctx, "Series", b.lookup("Series"), &ops.UserAttrs{Mode: &mode}, Asked{Caller: who}); err != nil {
		t.Fatal(err)
	}
	if err := b.w.Rename(ctx, "Films/new2.mkv", "Films/moved.mkv", false, who); err != nil {
		t.Fatal(err)
	}
	if f, _, err = b.w.Create(ctx, "Films/other.mkv", 0o644, who); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err := b.w.Rename(ctx, "Films/other.mkv", "Films/moved.mkv", false, who); err != nil {
		t.Fatal(err)
	}
	if err := b.w.Rename(ctx, "Series", "Shows", false, who); err != nil {
		t.Fatal(err)
	}
	if err := b.w.Unlink(ctx, "Films/link", who); err != nil {
		t.Fatal(err)
	}
	if err := b.w.Unlink(ctx, "Films/new.mkv", who); err != nil {
		t.Fatal(err)
	}
	if err := b.w.Rmdir(ctx, "Shows", who); err != nil {
		t.Fatal(err)
	}
	// A thread the kernel names none of, and a caller from no request.
	if _, err := b.w.Mkdir(ctx, "Kernel", 0o755, Caller{Owner: Owner{UID: 0, GID: 0}, Creds: root.Creds, InRequest: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.w.Mkdir(ctx, "Nobody", 0o755, root); err != nil {
		t.Fatal(err)
	}
	got := askers(t, b)
	want := []string{"create 0/4242", "mkdir 0/4242", "symlink 0/4242", "write 0/4242", "link 0/4242", "xattr 0/4242", "write 0/4242",
		"rename 0/4242", "create 0/4242", "replace 0/4242", "rename 0/4242", "unlink 0/4242", "unlink 0/4242", "rmdir 0/4242", "mkdir 0/-", "mkdir -/-"}
	if !slices.Equal(got, want) {
		t.Errorf("the journal:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// What a reprise reads of the journal says it too.
	open, err := catalog.OpenViewOps(ctx, b.w.db)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) == 0 || open[0].Asker == nil || open[0].Asker.UID != 0 || open[0].Asker.PID != 4242 {
		t.Errorf("the first operation read back: %+v", open)
	}
	if last := open[len(open)-1]; last.Asker != nil {
		t.Errorf("the last, from no request, read back: %+v", last.Asker)
	}
}

// The line of the view's log that tells of an operation its reprise resolved says who asked it, as its row does
// (specs.md §21.2).
func TestTheLogOfAnOperationNamesWhoAsked(t *testing.T) {
	b := newBench(t, "ext4")
	var logged bytes.Buffer
	b.w.log = slog.New(slog.NewTextHandler(&logged, nil))
	for _, v := range []*catalog.ViewOp{
		{Kind: catalog.ViewMkdir, Disk: "t1", Path: "Asked", Folder: true, AtNS: 1, Asker: &catalog.ViewAsker{UID: 1000, PID: 4242}},
		{Kind: catalog.ViewMkdir, Disk: "t1", Path: "Nobody", Folder: true, AtNS: 1},
	} {
		if err := catalog.BeginViewOp(t.Context(), b.w.db, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.w.Reprise(t.Context()); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "path=Asked") || !strings.Contains(lines[0], " uid=1000 pid=4242") ||
		!strings.Contains(lines[1], "path=Nobody") || strings.Contains(lines[1], "uid=") {
		t.Errorf("the log:\n%s", logged.String())
	}
}

// What the view does of its own for a file a caller made names that caller: a copy written in parallel, and a move to
// another disk once a write ran out of space (specs.md §9, §21.2).
func TestThePartsAndTheMovesNameWhoMadeTheFile(t *testing.T) {
	who := Caller{Owner: Owner{UID: 0, GID: 0}, PID: 4242, Creds: root.Creds, InRequest: true}
	b := parallelBench(t, "", 400)
	f, _, err := b.w.Create(t.Context(), "Films/new.mkv", 0o644, who)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("a new film"), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	b.w.Wait()
	if got := askers(t, b); !slices.Equal(got, []string{"create 0/4242", "part 0/4242"}) {
		t.Errorf("a copy written in parallel: %v", got)
	}
	b = moveBench(t, "", 500, 400)
	if f, _, err = b.w.Create(t.Context(), "Films/new.mkv", 0o640, who); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte(first), 0); err != nil {
		t.Fatal(err)
	}
	b.full("t1", "new.mkv", syscall.ENOSPC)
	if _, err := f.WriteAt([]byte(second), int64(len(first))); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if got := askers(t, b); !slices.Equal(got, []string{"create 0/4242", "move 0/4242"}) {
		t.Errorf("a move: %v", got)
	}
}
