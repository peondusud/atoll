package jobs

import (
	"os"
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/view/writer"
)

// takeInByNames runs passes of the daemon, as it runs them, until no operation of the view is left to take in: the
// first given nothing heard, as one due for another reason, and each after it the names the one before left to see
// again, or the disks it left to walk whole. A pass that walks a disk whole fails the test: the view's operations are
// taken in by their names (specs.md §15.13, §21.2; the maintainer, 2026-11-19).
func (b *bench) takeInByNames(t *testing.T) {
	t.Helper()
	scope := Scope{}
	for range 6 {
		out, err := RunLaterOn(t.Context(), b.db, b.config, b, true, nil, nil, scope, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range out.Disks {
			if d.Walked && !d.Partial {
				t.Errorf("%s walked whole", d.ID)
			}
		}
		if len(b.openViewOps()) == 0 {
			return
		}
		scope = Scope{}
		for id, names := range out.Again {
			scope.Add(id, names)
		}
		for _, id := range out.Whole {
			scope[id] = Whole
		}
		b.now += 31_000_000_000
	}
	t.Fatalf("not taken in: %+v", b.openViewOps())
}

// Each operation of Atoll's own view is taken in by a pass that looks at the names it touched alone, and what the pass
// makes of it is what a walk of the whole disk made: a file made recorded, a deletion sent to the other copy's trash, a
// rename followed by the other copy, a folder made, renamed on both disks or removed from both, a symbolic link
// recorded (specs.md §15.13, §21.2).
func TestThePassTakesInTheViewByNames(t *testing.T) {
	t.Parallel()
	who := writer.Owner{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	for _, tc := range []struct {
		name  string
		do    func(t *testing.T, b *bench, w *writer.Writer)
		check func(t *testing.T, b *bench)
	}{
		{"a file made", func(t *testing.T, b *bench, w *writer.Writer) {
			f, _, err := w.Create(t.Context(), "Films/new.mkv", 0o644, who)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteAt([]byte("new film"), 0); err != nil {
				t.Fatal(err)
			}
			_ = f.Close()
		}, func(t *testing.T, b *bench) {
			if got := b.count("SELECT count(*) FROM files WHERE lpath = CAST('Films/new.mkv' AS BLOB)"); got != 1 {
				t.Errorf("%d files recorded", got)
			}
		}},
		{"a file deleted", func(t *testing.T, b *bench, w *writer.Writer) {
			if err := w.Unlink(t.Context(), "Films/a.mkv"); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, b *bench) {
			for _, id := range []string{"t1", "t2"} {
				if b.exists(id, "media/Films/a.mkv") {
					t.Errorf("%s still holds the file", id)
				}
			}
			if got := b.count("SELECT count(*) FROM trash WHERE reason = 'deleted'"); got != 2 {
				t.Errorf("%d copies in the trash, want 2", got)
			}
		}},
		{"a file renamed", func(t *testing.T, b *bench, w *writer.Writer) {
			if err := w.Rename(t.Context(), "Films/a.mkv", "Films/b.mkv", false); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, b *bench) {
			for _, id := range []string{"t1", "t2"} {
				if !b.exists(id, "media/Films/b.mkv") || b.exists(id, "media/Films/a.mkv") {
					t.Errorf("%s did not follow the rename", id)
				}
			}
			if got := b.count("SELECT count(*) FROM copies c JOIN files f ON f.id = c.file_id WHERE f.lpath = CAST('Films/b.mkv' AS BLOB) AND c.old_lpath IS NULL AND c.gone_ns IS NULL"); got != 2 {
				t.Errorf("%d copies at the new name", got)
			}
		}},
		{"a folder made", func(t *testing.T, b *bench, w *writer.Writer) {
			if _, err := w.Mkdir(t.Context(), "Séries/X", 0o755, who); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, b *bench) {
			if got := b.count("SELECT count(*) FROM entries WHERE lpath = CAST('Séries/X' AS BLOB) AND kind = 'dir'"); got != 1 {
				t.Errorf("%d entries of the folder", got)
			}
		}},
		{"a folder renamed on both disks", func(t *testing.T, b *bench, w *writer.Writer) {
			if err := w.Rename(t.Context(), "Séries/D", "Séries/E", false); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, b *bench) {
			if got := b.count("SELECT count(*) FROM files WHERE lpath = CAST('Séries/E/e1.mkv' AS BLOB)"); got != 1 {
				t.Errorf("the file below did not follow: %d", got)
			}
			if got := b.count("SELECT count(*) FROM entries WHERE CAST(lpath AS TEXT) LIKE 'Séries/D%'"); got != 0 {
				t.Errorf("%d entries left at the old name", got)
			}
			if got := b.count("SELECT count(*) FROM entries WHERE lpath = CAST('Séries/E' AS BLOB)"); got != 2 {
				t.Errorf("%d entries at the new name, want both disks'", got)
			}
		}},
		{"a folder removed from both disks", func(t *testing.T, b *bench, w *writer.Writer) {
			if err := w.Rmdir(t.Context(), "Empty"); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, b *bench) {
			for _, id := range []string{"t1", "t2"} {
				if b.exists(id, "media/Empty") {
					t.Errorf("%s still holds the folder", id)
				}
			}
			if got := b.count("SELECT count(*) FROM entries WHERE lpath = CAST('Empty' AS BLOB)"); got != 0 {
				t.Errorf("%d entries of the folder left", got)
			}
		}},
		{"a symbolic link made", func(t *testing.T, b *bench, w *writer.Writer) {
			if _, err := w.Symlink(t.Context(), "Films/latest", "a.mkv", who); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, b *bench) {
			if got := b.count("SELECT count(*) FROM entries WHERE lpath = CAST('Films/latest' AS BLOB) AND kind = 'symlink'"); got != 1 {
				t.Errorf("%d entries of the link", got)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := newBench(t, "t1", "t2")
			for _, id := range []string{"t1", "t2"} {
				b.write(id, "Films/a.mkv", "x")
				b.write(id, "Séries/D/e1.mkv", "e1")
				b.write(id, "Empty/.gone", "")
				b.remove(id, "Empty/.gone")
			}
			b.scan(true)
			b.now += 60_000_000_000
			w := b.viewOf()
			tc.do(t, b, w)
			if len(b.openViewOps()) == 0 {
				t.Fatal("the view journaled nothing")
			}
			b.takeInByNames(t)
			tc.check(t, b)
			if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
				t.Errorf("%d operations left planned", got)
			}
		})
	}
}

// A pass looks at the names the view's operations touched on the disk they ran on, and leaves the other disks alone
// (specs.md §15.13): the whole disk only past watch.MaxNames, which view_names.json pins.
func TestTheViewsOperationsAreLookedAtByTheirNames(t *testing.T) {
	t.Parallel()
	b := newBench(t, "t1", "t2")
	b.write("t1", "a", "x")
	b.scan(true)
	b.now += 60_000_000_000
	for i := range 3 {
		b.exec("INSERT INTO view_ops(kind, disk_id, lpath, folder, state, at_ns) VALUES ('create', 't1', CAST(? AS BLOB), 0, 'done', ?)",
			[]byte(string(rune('b'+i))), b.now)
	}
	p := newPass(b.db, b.config, b, true, nil, nil)
	p.scope = Scope{}
	if err := p.readViewOps(t.Context()); err != nil {
		t.Fatal(err)
	}
	walk, err := p.toWalk(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := walk["t1"]; got.Names == nil || !slices.Equal(pathsOf(got), []string{"b", "c", "d"}) {
		t.Errorf("t1 looked at %+v", got)
	}
	if _, ok := walk["t2"]; ok {
		t.Error("t2 is looked at for the view's operations on t1")
	}
}

func pathsOf(l Looking) []string {
	out := make([]string, 0, len(l.Names))
	for _, n := range l.Names {
		out = append(out, n.Path)
	}
	return out
}
