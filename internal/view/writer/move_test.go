package writer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
)

// moveBench is a bench with view.move_on_enospc, and more after the disks; t1 has the most room, so that a new file
// lands there.
func moveBench(t *testing.T, more string, free1, free2 int64) *bench {
	t.Helper()
	return newBenchEdited(t, "ext4", "", free1, free2, func(_ *bench, text string) string {
		return strings.Replace(text, "[view]\n", "[view]\nmove_on_enospc = true\n", 1) + more
	})
}

// full makes a disk answer each write to a file of that name with errno, as a disk with no room left does, or one
// whose owner's quota is reached.
func (b *bench) full(disk, name string, errno syscall.Errno) {
	b.machine.setFail(b.mount[disk], func(call, n string) error {
		if call == "write" && n == name {
			return errno
		}
		return nil
	})
}

// moves gives each move of the view's journal, by id: its disks, its state, and the kind and state of the operation it
// names.
func (b *bench) moves() []string {
	b.t.Helper()
	rows, err := b.w.db.QueryContext(b.t.Context(), `SELECT v.disk2_id || '>' || v.disk_id || ':' || v.state || ':' || o.kind || ':' || o.state
		FROM view_ops v JOIN ops o ON o.id = v.op_id WHERE v.kind = 'move' ORDER BY v.id`)
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

// trashed gives what a disk's trash holds, each file by its path below the trash, and what it holds.
func (b *bench) trashed(disk string) map[string]string {
	b.t.Helper()
	out := map[string]string{}
	top := filepath.Join(b.mount[disk], "Atoll", "trash")
	err := filepath.WalkDir(top, func(path string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(top, path)
		out[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		b.t.Fatal(err)
	}
	return out
}

// planned counts the operations of ops left planned.
func (b *bench) planned() int {
	b.t.Helper()
	var n int
	if err := b.w.db.QueryRowContext(b.t.Context(), "SELECT count(*) FROM ops WHERE state = 'planned'").Scan(&n); err != nil {
		b.t.Fatal(err)
	}
	return n
}

const (
	first  = "first part "
	second = "second part"
)

// startNew makes Films/new.mkv through the view, on t1, and writes its first part.
func (b *bench) startNew() *File {
	b.t.Helper()
	f, _, err := b.w.Create(b.t.Context(), "Films/new.mkv", 0o640, root)
	if err != nil {
		b.t.Fatal(err)
	}
	if f.OnDisk() != "t1" {
		b.t.Fatalf("a new file lands on %s", f.OnDisk())
	}
	if _, err := f.WriteAt([]byte(first), 0); err != nil {
		b.t.Fatal(err)
	}
	return f
}

// A write the new file's disk has no room for, ENOSPC or EDQUOT, moves the file to the other disk, where the write is
// made, with view.move_on_enospc (specs.md §21.2): the file holds every byte written, with its mode, on the disk it
// went to, which the view presents it from; the copy it left went to its disk's trash and was purged from there; the
// part it was copied into is at its name; the move's operations are done, each journaled with its event; and what is
// written next goes there too.
func TestANewFileMovesWhenItsDiskRunsOutOfSpace(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.ENOSPC, syscall.EDQUOT} {
		t.Run(errno.Error(), func(t *testing.T) {
			b := moveBench(t, "", 500, 400)
			f := b.startNew()
			b.full("t1", "new.mkv", errno)
			if n, err := f.WriteAt([]byte(second), int64(len(first))); err != nil || n != len(second) {
				t.Fatalf("the write that ran out of space: %d, %v", n, err)
			}
			if f.OnDisk() != "t2" {
				t.Errorf("the file is on %s", f.OnDisk())
			}
			if _, err := f.WriteAt([]byte("!"), int64(len(first+second))); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			want := first + second + "!"
			if b.on("t2", "Films/new.mkv") != want || b.on("t1", "Films/new.mkv") != "absent" {
				t.Errorf("t1 %q, t2 %q", b.on("t1", "Films/new.mkv"), b.on("t2", "Films/new.mkv"))
			}
			if n := b.lookup("Films/new.mkv"); n == nil || n.Disk != "t2" || n.Size != int64(len(want)) {
				t.Errorf("the view presents %+v", n)
			}
			st, err := os.Stat(filepath.Join(b.mount["t2"], "media", "Films", "new.mkv"))
			if err != nil || st.Mode().Perm() != 0o640 {
				t.Errorf("the moved file's mode: %v, %v", st, err)
			}
			if got := b.trashed("t1"); len(got) != 0 {
				t.Errorf("t1's trash holds %v", got)
			}
			if got := b.parts("t2"); len(got) != 0 {
				t.Errorf("t2's tmp/ holds %v", got)
			}
			if got := b.moves(); !slices.Equal(got, []string{"t1>t2:done:unlink:done"}) {
				t.Errorf("the moves: %v", got)
			}
			if b.planned() != 0 {
				t.Errorf("%d operations left planned", b.planned())
			}
			entries, err := catalog.TrashEntries(t.Context(), b.w.db)
			if err != nil || len(entries) != 0 {
				t.Errorf("the trash's rows: %+v, %v", entries, err)
			}
			var events string
			if err := b.w.db.QueryRowContext(t.Context(), "SELECT group_concat(kind, ' ') FROM (SELECT kind FROM events WHERE kind IN ('replicate', 'trash', 'purge') ORDER BY id)").Scan(&events); err != nil || events != "replicate trash purge" {
				t.Errorf("events %q, %v", events, err)
			}
		})
	}
}

// What never moves, and answers the write with ENOSPC as before, the file left as it was on its disk, nothing made on
// the other (specs.md §21.2): with the key off; a file opened again to be written; one at whose path the catalog holds
// a file; one no other disk has room for, past min_free, its size and the write; one with another name, which would
// keep what the move left; one written without a place; and a file of a data disk, which never goes to the cache.
func TestWhatNeverMovesForRoom(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bench func(t *testing.T) *bench
		do    func(b *bench, f *File)
	}{
		{name: "the key off", bench: func(t *testing.T) *bench { return newBenchWith(t, "ext4", "", 500, 400) }},
		{name: "opened again", do: func(b *bench, _ *File) {
			g, err := b.w.Open(context.Background(), "Films/new.mkv", b.lookup("Films/new.mkv"), root)
			must(b.t, err)
			must(b.t, g.Close())
		}},
		{name: "the catalog holds its path", do: func(b *bench, _ *File) {
			if _, err := b.w.db.ExecContext(b.t.Context(), `INSERT INTO files(id, lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns)
				VALUES (2, CAST('Films/new.mkv' AS BLOB), 11, 9, 1, 'deferred', 'ok', 0, 0)`); err != nil {
				b.t.Fatal(err)
			}
		}},
		{name: "no room on the other disk", bench: func(t *testing.T) *bench { return moveBench(t, "", 500, 70) }},
		{name: "another name", do: func(b *bench, _ *File) {
			if _, err := b.w.Link(context.Background(), "Films/new.mkv", "Films/other.mkv", root); err != nil {
				b.t.Fatal(err)
			}
		}},
		{name: "written without a place", do: func(_ *bench, f *File) {
			if _, err := f.Write(nil); err != nil {
				panic(err)
			}
		}},
		{name: "no data disk but a cache", bench: func(t *testing.T) *bench {
			return newBenchEdited(t, "ext4", "", 500, 400, func(b *bench, text string) string {
				text = strings.Replace(text, "[view]\n", "[view]\nengine = \"atoll\"\nmove_on_enospc = true\n", 1)
				disk := fmt.Sprintf("mount = %q\nfs = \"ext4\"\n", b.mount["t2"])
				return strings.Replace(text, disk, disk+"role = \"cache\"\n", 1)
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b *bench
			if tc.bench != nil {
				b = tc.bench(t)
			} else {
				b = moveBench(t, "", 500, 400)
			}
			f := b.startNew()
			if tc.do != nil {
				tc.do(b, f)
			}
			b.full("t1", "new.mkv", syscall.ENOSPC)
			if _, err := f.WriteAt([]byte(second), int64(len(first))); !errors.Is(err, syscall.ENOSPC) {
				t.Errorf("the write answers %v", err)
			}
			if f.OnDisk() != "t1" {
				t.Errorf("the file moved to %s", f.OnDisk())
			}
			b.machine.setFail(b.mount["t1"], nil)
			_ = f.Close()
			b.w.Wait()
			if b.on("t1", "Films/new.mkv") != first || b.on("t2", "Films/new.mkv") != "absent" {
				t.Errorf("t1 %q, t2 %q", b.on("t1", "Films/new.mkv"), b.on("t2", "Films/new.mkv"))
			}
			if got := b.moves(); len(got) != 0 {
				t.Errorf("the moves: %v", got)
			}
			if got := b.parts("t2"); len(got) != 0 {
				t.Errorf("t2's tmp/ holds %v", got)
			}
		})
	}
}

// A move that fails before its copy is at its name leaves the file as it was, on its disk, where the program goes on
// writing, and nothing on the other disk: the part removed, the move and its create undone, and the write answered
// ENOSPC (specs.md §21.2): the other disk filling during the copy; a copy that does not read back. On a disk that
// cannot be relied on, whose part cannot be removed, the move stays planned, the full disk's copy presented, and the
// view's reprise at its next start removes the part and undoes it.
func TestAMoveThatFailsLeavesTheFileAsItWas(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fail  func(call, name string) error
		moves []string
	}{
		{name: "the other disk fills", fail: func(call, name string) error {
			if call == "write" && strings.HasSuffix(name, ".part") {
				return syscall.ENOSPC
			}
			return nil
		}, moves: []string{"t1>t2:undone:create:undone"}},
		{name: "a copy that does not read back", fail: func(call, name string) error {
			if call == "readback" && strings.HasSuffix(name, ".part") {
				return syscall.EBADMSG
			}
			return nil
		}, moves: []string{"t1>t2:undone:create:undone"}},
		{name: "the other disk lost", fail: func(call, name string) error {
			if (call == "filesync" || call == "unlink") && strings.HasSuffix(name, ".part") {
				return syscall.EIO
			}
			return nil
		}, moves: []string{"t1>t2:planned:create:planned"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := moveBench(t, "", 500, 400)
			f := b.startNew()
			b.machine.setFail(b.mount["t2"], tc.fail)
			b.full("t1", "new.mkv", syscall.ENOSPC)
			if _, err := f.WriteAt([]byte(second), int64(len(first))); !errors.Is(err, syscall.ENOSPC) {
				t.Errorf("the write answers %v", err)
			}
			if f.OnDisk() != "t1" {
				t.Errorf("the file moved to %s", f.OnDisk())
			}
			if got := b.moves(); !slices.Equal(got, tc.moves) {
				t.Errorf("the moves: %v", got)
			}
			if n := b.lookup("Films/new.mkv"); n == nil || n.Disk != "t1" {
				t.Errorf("the view presents %+v", n)
			}
			b.machine.setFail(b.mount["t1"], nil)
			b.machine.setFail(b.mount["t2"], nil)
			if _, err := f.WriteAt([]byte("then"), int64(len(first))); err != nil {
				t.Errorf("the program goes on writing where the file is: %v", err)
			}
			_ = f.Close()
			if err := b.w.Reprise(t.Context()); err != nil {
				t.Fatal(err)
			}
			if b.on("t1", "Films/new.mkv") != first+"then" || b.on("t2", "Films/new.mkv") != "absent" {
				t.Errorf("t1 %q, t2 %q", b.on("t1", "Films/new.mkv"), b.on("t2", "Films/new.mkv"))
			}
			if got := b.parts("t2"); len(got) != 0 {
				t.Errorf("t2's tmp/ holds %v", got)
			}
			if got := b.moves(); !slices.Equal(got, []string{"t1>t2:undone:create:undone"}) {
				t.Errorf("the moves after the reprise: %v", got)
			}
			if b.planned() != 0 {
				t.Errorf("%d operations left planned", b.planned())
			}
		})
	}
}

// A new file in immediate mode is written in parallel to a part on its replica's target: a write that runs out of space
// drops that part first, as any write that fails, then moves the file (specs.md §21.2).
func TestTheParallelPartsGoBeforeAMove(t *testing.T) {
	b := moveBench(t, "[defaults]\nmode = \"immediate\"\nreplicas = 1\n", 500, 400)
	f := b.startNew()
	if got := b.partRows(); len(got) != 1 || got[0] != "t2:planned:planned" {
		t.Fatalf("the parts while it is written: %v", got)
	}
	b.full("t1", "new.mkv", syscall.ENOSPC)
	if _, err := f.WriteAt([]byte(second), int64(len(first))); err != nil {
		t.Fatal(err)
	}
	must(t, f.Close())
	b.w.Wait()
	if got := b.partRows(); len(got) != 1 || got[0] != "t2:undone:undone" {
		t.Errorf("the parts: %v", got)
	}
	if b.on("t2", "Films/new.mkv") != first+second || b.on("t1", "Films/new.mkv") != "absent" {
		t.Errorf("t1 %q, t2 %q", b.on("t1", "Films/new.mkv"), b.on("t2", "Films/new.mkv"))
	}
	if got := b.parts("t2"); len(got) != 0 {
		t.Errorf("t2's tmp/ holds %v", got)
	}
}

// A new file that landed on the SSD cache moves, once the cache has no room for a write, to a data disk, never another
// cache disk (specs.md §19, §21.2).
func TestAFileOnAFullCacheMovesToADataDisk(t *testing.T) {
	b := newBenchEdited(t, "ext4", "", 500, 400, func(b *bench, text string) string {
		text = strings.Replace(text, "[view]\n", "[view]\nengine = \"atoll\"\nmove_on_enospc = true\n", 1)
		disk := fmt.Sprintf("mount = %q\nfs = \"ext4\"\n", b.mount["t2"])
		return strings.Replace(text, disk, disk+"role = \"cache\"\n", 1) + "[cache]\nenabled = true\n"
	})
	f, _, err := b.w.Create(t.Context(), "Films/new.mkv", 0o644, root)
	if err != nil {
		t.Fatal(err)
	}
	if f.OnDisk() != "t2" {
		t.Fatalf("a new file lands on %s, not the cache", f.OnDisk())
	}
	if _, err := f.WriteAt([]byte(first), 0); err != nil {
		t.Fatal(err)
	}
	b.full("t2", "new.mkv", syscall.ENOSPC)
	if _, err := f.WriteAt([]byte(second), int64(len(first))); err != nil {
		t.Fatal(err)
	}
	must(t, f.Close())
	if f.OnDisk() != "t1" || b.on("t1", "Films/new.mkv") != first+second || b.on("t2", "Films/new.mkv") != "absent" {
		t.Errorf("on %s: t1 %q, t2 %q", f.OnDisk(), b.on("t1", "Films/new.mkv"), b.on("t2", "Films/new.mkv"))
	}
	if got := b.moves(); !slices.Equal(got, []string{"t2>t1:done:unlink:done"}) {
		t.Errorf("the moves: %v", got)
	}
}

// stopAt runs a write that stops the view at a crash point, as kill -9 would, and says whether it stopped.
func (b *bench) stopAt(point string, write func()) bool {
	b.t.Helper()
	b.machine.mu.Lock()
	b.machine.crashAt = point
	b.machine.mu.Unlock()
	defer func() {
		b.machine.mu.Lock()
		b.machine.crashAt = ""
		b.machine.mu.Unlock()
	}()
	return func() (stopped bool) {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(crashed); !ok {
					panic(r)
				}
				stopped = true
			}
		}()
		write()
		return false
	}()
}

// Stopped once its copy was at its name on the other disk, before its trash was journaled, a move whose copy left on
// the full disk was written to since, its size or modification time another, keeps both at their paths, and the view
// presents the full disk's, the move undone and its create done: a pass then finds the two (specs.md §21.2).
func TestAMoveWhoseLeftCopyChangedKeepsBoth(t *testing.T) {
	b := moveBench(t, "", 500, 400)
	f := b.startNew()
	b.full("t1", "new.mkv", syscall.ENOSPC)
	if !b.stopAt("move-placed", func() { _, _ = f.WriteAt([]byte(second), int64(len(first))) }) {
		t.Fatal("no stop")
	}
	b.machine.setFail(b.mount["t1"], nil)
	left, err := os.OpenFile(filepath.Join(b.mount["t1"], "media", "Films", "new.mkv"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := left.WriteString("more"); err != nil {
		t.Fatal(err)
	}
	_ = left.Close()
	if err := b.w.Reprise(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := b.lookup("Films/new.mkv"); n == nil || n.Disk != "t1" {
		t.Errorf("the view presents %+v", n)
	}
	if b.on("t1", "Films/new.mkv") != first+"more" || b.on("t2", "Films/new.mkv") != first {
		t.Errorf("t1 %q, t2 %q", b.on("t1", "Films/new.mkv"), b.on("t2", "Films/new.mkv"))
	}
	if got := b.moves(); !slices.Equal(got, []string{"t1>t2:undone:create:done"}) {
		t.Errorf("the moves: %v", got)
	}
	if b.planned() != 0 {
		t.Errorf("%d operations left planned", b.planned())
	}
}

// atoll mount stopped at each step of a move, as kill -9 stops it, then started again over the same catalog and disks:
// its reprise leaves exactly one copy of the file presented, which holds every byte the program wrote, and the other
// nowhere in the root, in the trash or purged, its part in tmp/ gone; nothing is left planned (specs.md §21.2).
func TestAMoveStoppedAtEachStep(t *testing.T) {
	points := func() []string {
		b := moveBench(t, "", 500, 400)
		f := b.startNew()
		b.machine.passed()
		b.full("t1", "new.mkv", syscall.ENOSPC)
		if _, err := f.WriteAt([]byte(second), int64(len(first))); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		return b.machine.passed()
	}()
	if !slices.Contains(points, "move-journaled") || !slices.Contains(points, "move-purged") {
		t.Fatalf("the crash points of a move: %v", points)
	}
	for i, point := range points {
		t.Run(fmt.Sprintf("%d %s", i+1, point), func(t *testing.T) {
			b := moveBench(t, "", 500, 400)
			f := b.startNew()
			b.machine.passed()
			b.full("t1", "new.mkv", syscall.ENOSPC)
			b.machine.mu.Lock()
			b.machine.crash = i + 1
			b.machine.mu.Unlock()
			stopped := func() (stopped bool) {
				defer func() {
					if r := recover(); r != nil {
						if _, ok := r.(crashed); !ok {
							panic(r)
						}
						stopped = true
					}
				}()
				_, _ = f.WriteAt([]byte(second), int64(len(first)))
				return false
			}()
			if !stopped {
				t.Fatalf("no stop at %s", point)
			}
			b.machine.mu.Lock()
			b.machine.crash = 0
			b.machine.mu.Unlock()
			b.machine.setFail(b.mount["t1"], nil)
			// atoll mount starts again: its reprise, before it mounts.
			if err := b.w.Reprise(t.Context()); err != nil {
				t.Fatal(err)
			}
			n := b.lookup("Films/new.mkv")
			if n == nil {
				t.Fatal("the file presents nothing")
			}
			other := map[string]string{"t1": "t2", "t2": "t1"}[n.Disk]
			if b.on(n.Disk, "Films/new.mkv") != first || b.on(other, "Films/new.mkv") != "absent" {
				t.Errorf("presented from %s: t1 %q, t2 %q", n.Disk, b.on("t1", "Films/new.mkv"), b.on("t2", "Films/new.mkv"))
			}
			for _, data := range b.trashed(other) {
				if data != first {
					t.Errorf("%s's trash holds %q", other, data)
				}
			}
			if got := b.parts("t2"); len(got) != 0 {
				t.Errorf("t2's tmp/ holds %v", got)
			}
			if b.planned() != 0 {
				t.Errorf("%d operations left planned: %v", b.planned(), b.moves())
			}
			for _, m := range b.moves() {
				if strings.Contains(m, "planned") {
					t.Errorf("a move left planned: %v", m)
				}
			}
		})
	}
}
