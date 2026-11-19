package writer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
)

// parallelBench is a bench whose files are in immediate mode, with one replica, and t2 the room for one, t1 the most.
func parallelBench(t *testing.T, more string, free2 int64) *bench {
	t.Helper()
	return newBenchWith(t, "ext4", "[defaults]\nmode = \"immediate\"\nreplicas = 1\n"+more, 500, free2)
}

// parts gives what a disk's tmp/ holds, by name.
func (b *bench) parts(disk string) map[string]string {
	b.t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(filepath.Join(b.mount[disk], "Atoll", "tmp"))
	if err != nil {
		return out
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(b.mount[disk], "Atoll", "tmp", e.Name()))
		if err != nil {
			b.t.Fatal(err)
		}
		out[e.Name()] = string(data)
	}
	return out
}

// partRows gives each part of the view's journal: the disk, its row's state and its operation's.
func (b *bench) partRows() []string {
	b.t.Helper()
	rows, err := b.w.db.QueryContext(b.t.Context(), "SELECT v.disk_id || ':' || v.state || ':' || o.state FROM view_ops v JOIN ops o ON o.id = v.op_id WHERE v.kind = 'part' ORDER BY v.id")
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

// writeNew makes a file through the view and writes text to it, in writes of a few bytes, without closing it.
func (b *bench) writeNew(path, text string) *File {
	b.t.Helper()
	f, _, err := b.w.Create(b.t.Context(), path, 0o644, Owner{})
	if err != nil {
		b.t.Fatalf("create %s: %v", path, err)
	}
	for off := 0; off < len(text); off += 4 {
		end := min(off+4, len(text))
		if _, err := f.WriteAt([]byte(text[off:end]), int64(off)); err != nil {
			b.t.Fatal(err)
		}
	}
	return f
}

// A new file in immediate mode is written, as the program writes it, to its original and to a part in tmp/ of its
// replica's target, the solver's (specs.md §12.3): once the program closes it, the part holds what the original holds,
// synced, and is handed to the passes, its row done and its operation create still planned. Nothing is at the file's
// name on the target until a pass makes it a replica.
func TestANewFileIsWrittenInParallel(t *testing.T) {
	b := parallelBench(t, "", 400)
	f := b.writeNew("Films/new.mkv", "a new film, written once")
	if got := b.partRows(); len(got) != 1 || got[0] != "t2:planned:planned" {
		t.Fatalf("the parts while it is written: %v", got)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	b.w.Wait()
	if b.on("t1", "Films/new.mkv") != "a new film, written once" || b.on("t2", "Films/new.mkv") != "absent" {
		t.Errorf("t1 %q, t2 %q", b.on("t1", "Films/new.mkv"), b.on("t2", "Films/new.mkv"))
	}
	parts := b.parts("t2")
	if len(parts) != 1 {
		t.Fatalf("tmp/ of t2: %v", parts)
	}
	for _, data := range parts {
		if data != "a new film, written once" {
			t.Errorf("the part holds %q", data)
		}
	}
	if got := b.partRows(); len(got) != 1 || got[0] != "t2:done:planned" {
		t.Errorf("the parts once it is closed: %v", got)
	}
	if n := b.lookup("Films/new.mkv"); n == nil || n.Disk != "t1" {
		t.Errorf("the view presents %+v", n)
	}
}

// What gets no part: a file in another mode, one a rule gives no replica, and one that lands on the SSD cache, which
// stays there resident (specs.md §12.3, §19); and past max_parallel_per_disk, a second file being written at once.
func TestWhatIsNotWrittenInParallel(t *testing.T) {
	for _, tc := range []struct {
		name, more string
	}{
		{"deferred", "[[rule]]\nlabel = \"later\"\next = [\"mkv\"]\nmode = \"deferred\"\n"},
		{"no replica", "[[rule]]\nlabel = \"one\"\next = [\"mkv\"]\nreplicas = 0\n"},
		{"replication off", "[replication]\nenabled = false\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := parallelBench(t, tc.more, 400)
			_ = b.writeNew("Films/new.mkv", "x").Close()
			b.w.Wait()
			if got := b.partRows(); len(got) != 0 {
				t.Errorf("parts %v", got)
			}
		})
	}
	b := parallelBench(t, "", 400)
	first := b.writeNew("Films/one.mkv", "one")
	second := b.writeNew("Films/two.mkv", "two")
	if got := b.partRows(); len(got) != 1 {
		t.Errorf("two files at once on one disk, with max_parallel_per_disk = 1: %v", got)
	}
	_, _ = first.Close(), second.Close()
	b.w.Wait()
	third := b.writeNew("Films/three.mkv", "three")
	if got := b.partRows(); len(got) != 2 {
		t.Errorf("once the first is closed, the next one gets its part: %v", got)
	}
	_ = third.Close()
	b.w.Wait()
}

// The parts of a file go, with nothing left in tmp/ and their operations undone, and the program's writes never fail
// for them: when the file is truncated, renamed, deleted or opened again to be written before it is closed; when its
// part would fill its target past min_free; when a write to its target fails; when the part falls behind more than it
// may wait; and when a write to the original fails, as on a disk failing (specs.md §12.3).
func TestTheParallelPartsGo(t *testing.T) {
	cases := []struct {
		name  string
		free2 int64
		fail  func(call, name string) error
		do    func(b *bench, f *File)
	}{
		{name: "truncated", free2: 400, do: func(_ *bench, f *File) { must(t, f.Truncate(2)) }},
		{name: "renamed", free2: 400, do: func(b *bench, _ *File) {
			must(t, b.w.Rename(context.Background(), "Films/new.mkv", "Films/other.mkv", false))
		}},
		{name: "deleted", free2: 400, do: func(b *bench, _ *File) { must(t, b.w.Unlink(context.Background(), "Films/new.mkv")) }},
		{name: "opened again", free2: 400, do: func(b *bench, _ *File) {
			g, err := b.w.Open(context.Background(), "Films/new.mkv", b.lookup("Films/new.mkv"))
			must(t, err)
			must(t, g.Close())
		}},
		{name: "past min_free", free2: 60},
		{name: "a write fails on the target", free2: 400, fail: func(call, name string) error {
			if call == "write" && strings.HasSuffix(name, ".part") {
				return syscall.EIO
			}
			return nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := parallelBench(t, "", tc.free2)
			if tc.fail != nil {
				b.machine.setFail(b.mount["t2"], tc.fail)
			}
			f := b.writeNew("Films/new.mkv", "a new film, written once")
			if tc.do != nil {
				tc.do(b, f)
			}
			_ = f.Close()
			b.w.Wait()
			if got := b.partRows(); len(got) != 1 || got[0] != "t2:undone:undone" {
				t.Errorf("the parts: %v", got)
			}
			if got := b.parts("t2"); len(got) != 0 {
				t.Errorf("tmp/ of t2 holds %v", got)
			}
		})
	}
	t.Run("falls behind", func(t *testing.T) {
		b := parallelBench(t, "", 400)
		defer func(old int64) { partLimit = old }(partLimit)
		partLimit = 8
		held := make(chan struct{})
		b.machine.setFail(b.mount["t2"], func(call, name string) error {
			if call == "write" && strings.HasSuffix(name, ".part") {
				<-held
			}
			return nil
		})
		f := b.writeNew("Films/new.mkv", "a new film, written once")
		close(held)
		_ = f.Close()
		b.w.Wait()
		if b.on("t1", "Films/new.mkv") != "a new film, written once" {
			t.Errorf("the original holds %q", b.on("t1", "Films/new.mkv"))
		}
		if got := b.partRows(); len(got) != 1 || got[0] != "t2:undone:undone" {
			t.Errorf("the parts: %v", got)
		}
	})
	t.Run("the original fails", func(t *testing.T) {
		b := parallelBench(t, "", 400)
		f := b.writeNew("Films/new.mkv", "a film")
		b.machine.setFail(b.mount["t1"], func(call, _ string) error {
			if call == "write" {
				return syscall.EIO
			}
			return nil
		})
		if _, err := f.WriteAt([]byte("more"), 6); err == nil {
			t.Fatal("a write that fails on the original succeeds")
		}
		b.machine.setFail(b.mount["t1"], nil)
		_ = f.Close()
		b.w.Wait()
		if got := b.partRows(); len(got) != 1 || got[0] != "t2:undone:undone" {
			t.Errorf("the parts: %v", got)
		}
	})
}

// What atoll mount was writing in parallel when it stopped, as kill -9 stops it, is resolved at its next start, before
// it mounts: the part goes, and its operations are undone, as the program's writes were cut short too (specs.md §12.3,
// §21.2). A part handed over, done, is the passes'.
func TestTheRepriseDropsAPartBeingWritten(t *testing.T) {
	b := parallelBench(t, "[replication]\nmax_parallel_per_disk = 2\n", 400)
	_ = b.writeNew("Films/cut.mkv", "cut sh")
	done := b.writeNew("Films/whole.mkv", "whole")
	_ = done.Close()
	b.w.Wait()
	// The view stops here: a new one starts over the same catalog and disks.
	if err := b.w.Reprise(b.t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := b.partRows(); len(got) != 2 || got[0] != "t2:undone:undone" || got[1] != "t2:done:planned" {
		t.Errorf("the parts after the reprise: %v", got)
	}
	parts := b.parts("t2")
	if len(parts) != 1 {
		t.Errorf("tmp/ of t2 holds %v", parts)
	}
	for _, data := range parts {
		if data != "whole" {
			t.Errorf("the part kept holds %q", data)
		}
	}
	open, err := catalog.OpenViewParts(b.t.Context(), b.w.db)
	if err != nil || len(open) != 1 {
		t.Errorf("open parts: %+v, %v", open, err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil && !errors.Is(err, os.ErrClosed) {
		t.Fatal(err)
	}
}
