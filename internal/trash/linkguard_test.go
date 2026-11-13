package trash

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/watch"
)

// guarded runs the link guard on a disk in a session of its own, and gives what it sent to the trash and whether the
// session lost a disk.
func (b *bench) guarded(id string, on bool) ([]LinkItem, bool) {
	b.t.Helper()
	s := b.opened()
	defer s.Close()
	var items []LinkItem
	if err := s.Guard(b.t.Context(), id, on, func(i *LinkItem) error {
		items = append(items, *i)
		return nil
	}); err != nil {
		b.t.Fatal(err)
	}
	return items, s.Lost()
}

func (b *bench) linkOf(id string, fileID int64) string {
	return filepath.Join(b.dirs[id], "Atoll", "links", strconv.FormatInt(fileID, 10))
}

// inodeOf gives what a name leads to, 0 when it is not there.
func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return info.Sys().(*syscall.Stat_t).Ino //nolint:forcetypeassert // Linux only
}

// statOf looks at a file as a walk does.
func statOf(t *testing.T, path string) fsx.Stat {
	t.Helper()
	dir, err := fsx.OpenDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	st, err := dir.Stat(filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// TestTheGuardLinksEachCopy: a link at links/<file id>, to the copy's own inode, journaled; the folder private; the
// copy's row still what the disk holds, as the link moved its change time; and nothing more at the next run.
func TestTheGuardLinksEachCopy(t *testing.T) {
	b := newBench(t)
	if items, lost := b.guarded("t3", true); len(items) != 0 || lost {
		t.Fatalf("%+v, lost %v", items, lost)
	}
	media := filepath.Join(b.dirs["t3"], "media", "a")
	if got, want := inodeOf(t, b.linkOf("t3", 1)), inodeOf(t, media); got != want || got == 0 {
		t.Fatalf("the link leads to %d, the copy is %d", got, want)
	}
	if got := b.text("SELECT kind || ' ' || link || ' ' || state || ' ' || CAST(path AS TEXT) FROM ops"); got != "link 1 done a" {
		t.Errorf("the journal says %q", got)
	}
	info, err := os.Stat(filepath.Dir(b.linkOf("t3", 1)))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the links folder: %v, %v", info.Mode(), err)
	}
	row, err := catalog.CopyOf(t.Context(), b.db, "t3", "a", strconv.FormatUint(inodeOf(t, media), 10))
	if err != nil {
		t.Fatal(err)
	}
	st := statOf(t, media)
	found, err := watch.FileFrom("a", &st)
	if err != nil {
		t.Fatal(err)
	}
	if !watch.Unchanged(&found, &row) {
		t.Errorf("the copy reads as changed: row %+v, disk %+v", row, found)
	}
	if found.Nlink != 2 {
		t.Errorf("the disk shows %d names", found.Nlink)
	}
	if items, _ := b.guarded("t3", true); len(items) != 0 || b.count("SELECT count(*) FROM ops") != 1 {
		t.Errorf("a second run: %+v, %d operations", items, b.count("SELECT count(*) FROM ops"))
	}
}

// lostItsName makes t3's link, then takes the copy away as a deletion through the merged view does, and its row as the
// pass that saw it does.
func (b *bench) lostItsName() uint64 {
	b.t.Helper()
	if _, lost := b.guarded("t3", true); lost {
		b.t.Fatal("lost")
	}
	inode := inodeOf(b.t, b.linkOf("t3", 1))
	if err := os.Remove(filepath.Join(b.dirs["t3"], "media", "a")); err != nil {
		b.t.Fatal(err)
	}
	if _, err := b.db.ExecContext(b.t.Context(), "DELETE FROM copies WHERE disk_id = 't3'"); err != nil {
		b.t.Fatal(err)
	}
	return inode
}

// TestTheGuardSendsWhatLostItsName: the data the link kept goes to the trash at the file's path, deleted, as an
// operation kind = trash with the link; nothing else moves, and the entry is there to restore.
func TestTheGuardSendsWhatLostItsName(t *testing.T) {
	b := newBench(t)
	inode := b.lostItsName()
	items, lost := b.guarded("t3", true)
	if lost || len(items) != 1 || items[0] != (LinkItem{Disk: "t3", Path: "a", Reason: solver.LinkDeleted, Outcome: OutcomeTrashed}) {
		t.Fatalf("%+v, lost %v", items, lost)
	}
	if got := inodeOf(t, b.inTrash(2, "a")); got != inode {
		t.Errorf("the trash holds %d, the link held %d", got, inode)
	}
	if inodeOf(t, b.linkOf("t3", 1)) != 0 {
		t.Error("the link is still there")
	}
	if got := b.text("SELECT kind || ' ' || link || ' ' || state FROM ops WHERE id = 2"); got != "trash 1 done" {
		t.Errorf("the journal says %q", got)
	}
	if got := b.text("SELECT reason || ' ' || size || ' ' || CAST(lpath AS TEXT) FROM trash"); got != "deleted 1 a" {
		t.Errorf("the entry is %q", got)
	}
	if b.count("SELECT count(*) FROM copies") != 2 || b.count("SELECT count(*) FROM files") != 1 {
		t.Error("a row went with the link")
	}
}

// TestALinkWhoseDataHasItsName: data whose name in the root is still there, as a file a rule now ignores leaves it,
// only loses its link: nothing goes to the trash.
func TestALinkWhoseDataHasItsName(t *testing.T) {
	b := newBench(t)
	if _, lost := b.guarded("t3", true); lost {
		t.Fatal("lost")
	}
	if _, err := b.db.ExecContext(t.Context(), "DELETE FROM copies WHERE disk_id = 't3'"); err != nil {
		t.Fatal(err)
	}
	items, lost := b.guarded("t3", true)
	if lost || len(items) != 0 || b.count("SELECT count(*) FROM trash") != 0 {
		t.Fatalf("%+v, lost %v", items, lost)
	}
	if inodeOf(t, b.linkOf("t3", 1)) != 0 || inodeOf(t, filepath.Join(b.dirs["t3"], "media", "a")) == 0 {
		t.Error("the link should go, and the file stay")
	}
	if got := b.text("SELECT group_concat(kind || ' ' || state, ', ') FROM ops"); got != "link done, unlink done" {
		t.Errorf("the journal says %q", got)
	}
}

// TestALinkThatIsTheLastNameStays: the guard off removes a link that leads to its copy, but a copy removed since the
// last look leaves its link the last name of its data, which is never removed: it stays, undone, for the next pass to
// send to the trash once its walk finds the copy gone.
func TestALinkThatIsTheLastNameStays(t *testing.T) {
	b := newBench(t)
	if _, lost := b.guarded("t3", true); lost {
		t.Fatal("lost")
	}
	inode := inodeOf(t, b.linkOf("t3", 1))
	if err := os.Remove(filepath.Join(b.dirs["t3"], "media", "a")); err != nil {
		t.Fatal(err)
	}
	if items, lost := b.guarded("t3", false); lost || len(items) != 0 {
		t.Fatalf("%+v, lost %v", items, lost)
	}
	if got := inodeOf(t, b.linkOf("t3", 1)); got != inode {
		t.Errorf("the data's last name went: %d", got)
	}
	if got := b.text("SELECT group_concat(kind || ' ' || state, ', ') FROM ops"); got != "link done, unlink undone" {
		t.Errorf("the journal says %q", got)
	}
}

// TestTheGuardOff: each link goes, journaled, and the folder with them; the copy's row follows the change time its
// removal moved.
func TestTheGuardOff(t *testing.T) {
	b := newBench(t)
	if _, lost := b.guarded("t3", true); lost {
		t.Fatal("lost")
	}
	if items, lost := b.guarded("t3", false); lost || len(items) != 0 {
		t.Fatalf("%+v, lost %v", items, lost)
	}
	if _, err := os.Lstat(filepath.Dir(b.linkOf("t3", 1))); !os.IsNotExist(err) {
		t.Errorf("the links folder stays: %v", err)
	}
	media := filepath.Join(b.dirs["t3"], "media", "a")
	row, err := catalog.CopyOf(t.Context(), b.db, "t3", "a", strconv.FormatUint(inodeOf(t, media), 10))
	if err != nil {
		t.Fatal(err)
	}
	st := statOf(t, media)
	found, err := watch.FileFrom("a", &st)
	if err != nil {
		t.Fatal(err)
	}
	if !watch.Unchanged(&found, &row) {
		t.Errorf("the copy reads as changed: row %+v, disk %+v", row, found)
	}
}

// TestAPurgeTakesTheLinkFirst: an entry Atoll sent to the trash itself still has its link until the next pass; a purge
// before it removes the link first, so that nothing of the purged data stays behind it.
func TestAPurgeTakesTheLinkFirst(t *testing.T) {
	b := newBench(t)
	if _, lost := b.guarded("t3", true); lost {
		t.Fatal("lost")
	}
	inode := inodeOf(t, b.linkOf("t3", 1))
	if items, _ := b.prune(); len(items) != 1 || items[0].Outcome != OutcomeTrashed {
		t.Fatalf("%+v", items)
	}
	if got := inodeOf(t, b.inTrash(2, "a")); got != inode {
		t.Fatalf("the trash holds %d, the link %d", got, inode)
	}
	if outcomes, _ := b.purgedAll(); len(outcomes) != 1 || outcomes[0] != OutcomePurged {
		t.Fatalf("%v", outcomes)
	}
	if inodeOf(t, b.linkOf("t3", 1)) != 0 || inodeOf(t, b.inTrash(2, "a")) != 0 {
		t.Error("something of the purged data stays")
	}
	if got := b.text("SELECT group_concat(kind || ' ' || COALESCE(link, '-') || ' ' || state, ', ') FROM ops"); got != "link 1 done, trash - done, unlink 1 done, unlink - done" {
		t.Errorf("the journal says %q", got)
	}
	if items, _ := b.guarded("t3", true); len(items) != 0 {
		t.Errorf("the next run sends %+v to the trash", items)
	}
}

// TestAFullTrashTakesNoMoreLinks: once a move to a disk's trash found no room, no other link goes there in that
// session, as each would be journaled and undone for nothing (specs.md §17.1).
func TestAFullTrashTakesNoMoreLinks(t *testing.T) {
	b := newBench(t)
	b.write("t3", "b", "y")
	if _, err := watch.Scan(t.Context(), b.db, b.config, b, watch.Options{Full: true}); err != nil {
		t.Fatal(err)
	}
	b.lostItsName()
	if err := os.Remove(filepath.Join(b.dirs["t3"], "media", "b")); err != nil {
		t.Fatal(err)
	}
	b.failOn("t3", "mkdir", syscall.ENOSPC)
	items, lost := b.guarded("t3", true)
	if lost || len(items) != 1 || items[0].Outcome != OutcomeIOError {
		t.Fatalf("%+v, lost %v", items, lost)
	}
	if got := b.count("SELECT count(*) FROM ops WHERE kind = 'trash'"); got != 1 {
		t.Errorf("%d moves to the trash journaled", got)
	}
}

// TestTheGuardFails: a disk that fails is lost, with what it was doing left planned, and the next run's reprise and
// guard end where a run with no failure would have; a trash with no room leaves the link where it is, undone.
func TestTheGuardFails(t *testing.T) {
	for _, c := range []struct {
		name    string
		before  func(b *bench)
		on      bool
		call    string
		err     error
		lost    bool
		planned int
		// after says what the run that follows, with no failure, must leave.
		link, trashed bool
	}{
		{name: "a link that fails", on: true, call: "link", err: syscall.EIO, lost: true, planned: 1, link: true},
		{name: "a link refused", on: true, call: "link", err: syscall.EPERM, link: true},
		{name: "a move to the trash that fails", before: func(b *bench) { b.lostItsName() }, on: true, call: "rename", err: syscall.EIO, lost: true, planned: 1, trashed: true},
		{name: "a trash with no room", before: func(b *bench) { b.lostItsName() }, on: true, call: "mkdir", err: syscall.ENOSPC, trashed: true},
		{name: "a removal that fails", before: func(b *bench) { b.guarded("t3", true) }, call: "unlink", err: syscall.EIO, lost: true, planned: 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newBench(t)
			if c.before != nil {
				c.before(b)
			}
			b.failOn("t3", c.call, c.err)
			_, lost := b.guarded("t3", c.on)
			if lost != c.lost {
				t.Errorf("lost %v", lost)
			}
			if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != c.planned {
				t.Errorf("%d operations planned", got)
			}
			delete(b.fail, b.dirs["t3"])
			if _, lost := b.guarded("t3", c.on); lost {
				t.Fatal("lost again")
			}
			if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
				t.Errorf("%d operations still planned", got)
			}
			if there := inodeOf(t, b.linkOf("t3", 1)) != 0; there != c.link {
				t.Errorf("the link there: %v", there)
			}
			if got := b.count("SELECT count(*) FROM trash t JOIN ops o ON o.id = t.op_id WHERE o.state = 'done'"); (got == 1) != c.trashed {
				t.Errorf("%d entries", got)
			}
		})
	}
}
