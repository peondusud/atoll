package trash

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
)

// fileA reads the file a, with its copies.
func (b *bench) fileA() *catalog.FileRow {
	b.t.Helper()
	f, err := catalog.FileAt(b.t.Context(), b.db, "a")
	if err != nil || f == nil {
		b.t.Fatalf("%v", err)
	}
	return f
}

// What a session says of a disk it does not know, and of one it knows before and after it opened it.
func TestWhatASessionSaysOfADisk(t *testing.T) {
	b := newBench(t)
	s := b.opened()
	if s.TimePrecision("t9") != 0 || s.MountOwner("t9") || s.Readable("t9") || s.Writable("t9") || s.Opened("t9") {
		t.Error("a disk the session does not know is said to be something")
	}
	if h, err := s.Handle(t.Context(), "t9"); h != nil || err != nil {
		t.Errorf("a handle on a disk the session does not know: %v, %v", h, err)
	}
	if s.MountOwner("t3") || s.Opened("t3") || !s.Readable("t3") || !s.Writable("t3") {
		t.Error("t3, before it is opened")
	}
	if ok, err := s.Confirms(t.Context(), "t3", errors.New("a read failed")); ok || err != nil {
		t.Errorf("a disk not opened confirms: %v, %v", ok, err)
	}
	if h, err := s.Handle(t.Context(), "t3"); h == nil || err != nil || !s.Opened("t3") {
		t.Fatalf("t3: %v, %v", h, err)
	}
	if ok, err := s.Confirms(t.Context(), "t3", errors.New("a read failed")); !ok || err != nil || s.Lost() {
		t.Errorf("a disk that is there: %v, %v", ok, err)
	}
	b.exec("UPDATE disks SET time_precision_ns = 100 WHERE id = 't2'")
	if got := b.opened().TimePrecision("t2"); got != 100 {
		t.Errorf("the precision t2's row says: %d", got)
	}
	// What a caller says moved, and a disk it lost.
	if s.Changed() {
		t.Error("changed before anything moved")
	}
	s.MarkChanged()
	if !s.Changed() {
		t.Error("not changed once told")
	}
	if err := s.Settle(t.Context()); err != nil {
		t.Error(err)
	}
	if err := s.Lose(t.Context(), "t3", errors.New("gone")); err != nil || s.Writable("t3") || s.Opened("t3") || !s.Lost() || b.diskState("t3") != "missing" {
		t.Errorf("lost: %v, t3 is %s", err, b.diskState("t3"))
	}
}

// A session that only reads confirms each disk it reads through, as one that writes does, and writes to none: an
// operation a crash left stays planned, and tmp/ keeps what it holds (specs.md §17.2).
func TestASessionThatOnlyReads(t *testing.T) {
	b := newBench(t)
	b.plan("a")
	tmp := filepath.Join(b.dirs["t3"], "Atoll", "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "1.part"), []byte("p"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.confirm = map[string]int{}
	s, err := OpenToRead(t.Context(), b.db, b.config, b)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if h, err := s.Handle(t.Context(), "t3"); h == nil || err != nil || b.confirm[b.dirs["t3"]] != 1 {
		t.Errorf("a handle: %v, %v, confirmed %d times", h, err, b.confirm[b.dirs["t3"]])
	}
	if b.text("SELECT state FROM ops") != "planned" || !b.sourceThere() || s.Lost() {
		t.Error("a session that only reads resolved what a crash left")
	}
	if _, err := os.Stat(filepath.Join(tmp, "1.part")); err != nil {
		t.Errorf("tmp/: %v", err)
	}
}

// A read that failed on a disk that no longer confirms is the disk's: Confirms records it lost, and the session writes
// to it no more (invariant 4, specs.md §10, §16). So is one whose mount path now leads elsewhere.
func TestADiskThatNoLongerConfirms(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(b *bench)
	}{
		{"its head unreadable", func(b *bench) { b.unread[b.dirs["t3"]] = true }},
		{"another mount at its path", func(b *bench) {
			if err := b.moveAway(); err != nil {
				b.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t)
			s := b.opened()
			if h, err := s.Handle(t.Context(), "t3"); h == nil || err != nil {
				t.Fatalf("%v, %v", h, err)
			}
			tc.break_(b)
			if ok, err := s.Confirms(t.Context(), "t3", syscall.EIO); ok || err != nil {
				t.Errorf("%v, %v", ok, err)
			}
			if !s.Lost() || s.Writable("t3") || s.Opened("t3") || b.diskState("t3") != "missing" {
				t.Errorf("lost %v, writable %v, t3 is %s", s.Lost(), s.Writable("t3"), b.diskState("t3"))
			}
		})
	}
}

// A disk a look found missing is let go of, and recorded missing by a session that writes only.
func TestMarkMissing(t *testing.T) {
	b := newBench(t)
	s := b.opened()
	if err := s.MarkMissing(t.Context(), "t9"); err != nil {
		t.Error(err)
	}
	if _, err := s.Handle(t.Context(), "t2"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkMissing(t.Context(), "t2"); err != nil {
		t.Fatal(err)
	}
	if s.Opened("t2") || s.Readable("t2") || b.diskState("t2") != "missing" {
		t.Errorf("opened %v, readable %v, t2 is %s", s.Opened("t2"), s.Readable("t2"), b.diskState("t2"))
	}
	if h, err := s.Handle(t.Context(), "t2"); h != nil || err != nil {
		t.Errorf("a disk found missing opened again: %v, %v", h, err)
	}
	dry, err := Open(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	defer dry.Close()
	if err := dry.MarkMissing(t.Context(), "t1"); err != nil || dry.Readable("t1") || b.diskState("t1") != "online" {
		t.Errorf("a dry run: %v, readable %v, t1 is %s", err, dry.Readable("t1"), b.diskState("t1"))
	}
}

// A disk whose mount path leads nowhere when the session opens it is lost, never an empty one (invariant 4): its copy
// stays, and is no surplus to send.
func TestADiskThatCannotBeOpenedIsLost(t *testing.T) {
	b := newBench(t)
	if err := os.Rename(b.dirs["t3"], b.dirs["t3"]+".away"); err != nil {
		t.Fatal(err)
	}
	items, summary := b.prune()
	if len(items) != 1 || items[0].Outcome != outcomeUnavailable || !summary.DiskLost || b.diskState("t3") != "missing" {
		t.Fatalf("%+v, %+v, t3 is %s", items, summary, b.diskState("t3"))
	}
	if err := os.Rename(b.dirs["t3"]+".away", b.dirs["t3"]); err != nil {
		t.Fatal(err)
	}
	if items, _ := b.prune(); len(items) != 1 || items[0].Outcome != OutcomeTrashed {
		t.Errorf("once back: %+v", items)
	}
}

// Retire sends a surplus copy to the trash for a reason of the caller's, when prune would and only then; a run that
// may not write says it would; a disk with no surplus copy of the file says nothing (specs.md §13.3, §17.3).
func TestRetire(t *testing.T) {
	b := newBench(t)
	dry, err := Open(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := dry.Retire(t.Context(), b.fileA(), "t3", catalog.ReasonRebalance); err != nil || got != "to-trash" {
		t.Errorf("a dry run: %q, %v", got, err)
	}
	dry.Close()
	if !b.sourceThere() || b.count("SELECT count(*) FROM ops") != 0 {
		t.Error("a dry run moved the copy")
	}
	s := b.opened()
	if got, err := s.Retire(t.Context(), b.fileA(), "t1", catalog.ReasonRebalance); err != nil || got != "" {
		t.Errorf("no surplus copy there: %q, %v", got, err)
	}
	if got, err := s.Retire(t.Context(), b.fileA(), "t3", catalog.ReasonRebalance); err != nil || got != OutcomeTrashed {
		t.Errorf("%q, %v", got, err)
	}
	if b.sourceThere() || b.text("SELECT reason FROM trash") != "rebalance" {
		t.Error("the copy did not go to the trash, for that reason")
	}
}

// The copy a cache disk held goes to its trash, then is purged from there at once, which gives its space back; a purge
// that fails leaves it in the trash, for its retention or trash.max_size (specs.md §19). A disk that may only be read
// keeps it.
func TestTransfer(t *testing.T) {
	b := newBench(t)
	s := b.opened()
	f := b.fileA()
	if got, err := s.Transfer(t.Context(), f, copyOn(f, "t3")); err != nil || got != OutcomeTrashed {
		t.Fatalf("%q, %v", got, err)
	}
	if b.sourceThere() || b.count("SELECT count(*) FROM trash") != 0 {
		t.Error("the copy stays, at its place or in the trash")
	}
	if got := b.text("SELECT group_concat(kind || ' ' || state, ', ') FROM ops"); got != "trash done, unlink done" {
		t.Errorf("the journal says %s", got)
	}
	b = newBench(t)
	b.failOn("t3", "unlink", syscall.EACCES)
	s = b.opened()
	f = b.fileA()
	if got, err := s.Transfer(t.Context(), f, copyOn(f, "t3")); err != nil || got != OutcomeTrashed {
		t.Fatalf("a purge that fails: %q, %v", got, err)
	}
	if b.sourceThere() || b.count("SELECT count(*) FROM trash") != 1 {
		t.Error("the copy is not in the trash")
	}
	b = newBench(t)
	b.ro[b.dirs["t3"]] = true
	s = b.opened()
	f = b.fileA()
	if got, err := s.Transfer(t.Context(), f, copyOn(f, "t3")); err != nil || got != outcomeUnavailable || !b.sourceThere() {
		t.Errorf("a disk that may only be read: %q, %v", got, err)
	}
}

// A copy that has not followed a rename yet waits for it: it is not what its row says, and prune leaves it (specs.md
// §17.3).
func TestAPruneOfACopyBehindARename(t *testing.T) {
	b := newBench(t)
	b.exec("UPDATE copies SET old_lpath = x'7a' WHERE disk_id = 't3'")
	items, summary := b.prune()
	if len(items) != 1 || items[0].Outcome != OutcomeChanged || summary.Stayed != 1 {
		t.Fatalf("%+v, %+v", items, summary)
	}
	if !b.sourceThere() || b.count("SELECT count(*) FROM ops") != 0 {
		t.Error("the copy moved")
	}
}

// A copy the configuration now wants is no surplus, whatever the last scan marked: the level counts the larger of the
// file's row and what the configuration says now (specs.md §17.3).
func TestAPruneOfACopyNowWanted(t *testing.T) {
	b := newBench(t)
	b.config = b.configWith("[defaults]\nreplicas = 2\n")
	items, _ := b.prune()
	if len(items) != 1 || items[0].Outcome != "below-level" {
		t.Fatalf("%+v", items)
	}
	if !b.sourceThere() {
		t.Error("a copy now wanted went")
	}
}

// configWith gives the bench's configuration with more text after it.
func (b *bench) configWith(more string) *config.Config {
	b.t.Helper()
	top := filepath.Dir(b.dirs["t1"])
	var text strings.Builder
	fmt.Fprintf(&text, "pool_id = \"p\"\ncatalog = %q\n%s[view]\nmount = %q\n", filepath.Join(top, "catalog.db"), more, filepath.Join(top, "pool"))
	for i, id := range []string{"t1", "t2", "t3"} {
		fmt.Fprintf(&text, "[[disk]]\nid = %q\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3%d\"\nmount = %q\nfs = \"ext4\"\n", id, i, b.dirs[id])
	}
	c, err := config.Parse([]byte(text.String()))
	if err != nil {
		b.t.Fatal(err)
	}
	return c
}

// What a disk's tmp/ holds that no operation names is not Atoll's to remove; a tmp/ that cannot be read leaves its
// parts for the next time, and a disk that fails there is lost (specs.md §14.2).
func TestWhatTmpHoldsOfNoOperation(t *testing.T) {
	b := newBench(t)
	tmp := filepath.Join(b.dirs["t3"], "Atoll", "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "stranger"), []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.fail[b.dirs["t3"]] = func(call, name string) error {
		if call == "opendir" && name == "tmp" {
			return syscall.EACCES
		}
		return nil
	}
	if items, summary := b.prune(); len(items) != 1 || items[0].Outcome != OutcomeTrashed || summary.DiskLost {
		t.Fatalf("a tmp/ that cannot be read: %+v, %+v", items, summary)
	}
	delete(b.fail, b.dirs["t3"])
	b.opened().Close()
	if _, err := os.Stat(filepath.Join(tmp, "stranger")); err != nil {
		t.Errorf("what no operation names: %v", err)
	}
	b = newBench(t)
	b.fail[b.dirs["t3"]] = func(call, name string) error {
		if call == "opendir" && name == "tmp" {
			return syscall.EIO
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Join(b.dirs["t3"], "Atoll", "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if items, summary := b.prune(); len(items) != 1 || items[0].Outcome != outcomeUnavailable || !summary.DiskLost {
		t.Errorf("a tmp/ on a disk that fails: %+v, %+v", items, summary)
	}
}

// failTmp makes t3's tmp/ fail with EIO when a session opens the disk, which looks there first (specs.md §14.2).
func (b *bench) failTmp() {
	b.t.Helper()
	if err := os.MkdirAll(filepath.Join(b.dirs["t3"], "Atoll", "tmp"), 0o700); err != nil {
		b.t.Fatal(err)
	}
	b.fail[b.dirs["t3"]] = func(call, name string) error {
		if call == "opendir" && name == "tmp" {
			return syscall.EIO
		}
		return nil
	}
}

// A disk lost while a session opens it, as when its tmp/ answers EIO, takes no write in that session: whoever asked
// for the handle gets none, as for a disk that does not confirm (invariant 4, specs.md §17.1). The handle went back
// to the caller all the same, and a transfer or a send to the trash that opened the disk first then journaled and
// moved a copy on a disk the session had just recorded missing; a repair wrote its chunks there.
func TestADiskLostWhileOpenedTakesNoWrite(t *testing.T) {
	t.Run("handle", func(t *testing.T) {
		b := newBench(t)
		b.failTmp()
		s := b.opened()
		if h, err := s.Handle(t.Context(), "t3"); h != nil || err != nil || !s.Lost() || s.Opened("t3") {
			t.Errorf("a handle on a disk lost as it opened: %v, %v, lost %v", h, err, s.Lost())
		}
	})
	t.Run("transfer", func(t *testing.T) {
		b := newBench(t)
		b.failTmp()
		s := b.opened()
		f := b.fileA()
		if got, err := s.Transfer(t.Context(), f, copyOn(f, "t3")); err != nil || got != outcomeUnavailable {
			t.Errorf("%q, %v", got, err)
		}
		if !b.sourceThere() || b.count("SELECT count(*) FROM ops") != 0 || b.diskState("t3") != "missing" {
			t.Errorf("the copy there %v, %d operations, t3 %s", b.sourceThere(), b.count("SELECT count(*) FROM ops"), b.diskState("t3"))
		}
	})
	t.Run("send", func(t *testing.T) {
		b := newBench(t)
		b.failTmp()
		s := b.opened()
		f := b.fileA()
		if got, err := s.Send(t.Context(), f, copyOn(f, "t3"), "deleted"); err != nil || got != outcomeUnavailable {
			t.Errorf("%q, %v", got, err)
		}
		if !b.sourceThere() || b.count("SELECT count(*) FROM ops") != 0 {
			t.Error("a copy moved on a disk lost")
		}
	})
	t.Run("repair", func(t *testing.T) {
		r := rotted(t)
		r.b.failTmp()
		s := r.b.opened()
		if got, err := s.RepairChunks(t.Context(), r.file, r.copy, []int64{0}, nil, r.healthy, r.chunks); err != nil || got != outcomeUnavailable {
			t.Errorf("%q, %v", got, err)
		}
		if r.b.read("t3", "a") != "y" || r.b.count("SELECT count(*) FROM ops") != 0 {
			t.Error("a copy was written on a disk lost")
		}
	})
}
