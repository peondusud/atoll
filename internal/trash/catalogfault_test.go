package trash

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
)

// refuse makes the catalog refuse a write from now on, as a catalog on a full or failing disk does: a trigger aborts
// the statement, what is "INSERT ON ops WHEN NEW.kind = 'trash'" for instance.
func (b *bench) refuse(name, what string) {
	b.t.Helper()
	b.exec("CREATE TRIGGER " + name + " BEFORE " + what + " BEGIN SELECT RAISE(ABORT, 'refused by the test'); END")
}

func (b *bench) allow(name string) {
	b.t.Helper()
	b.exec("DROP TRIGGER " + name)
}

func (b *bench) trashThere(id string) bool {
	_, err := os.Lstat(filepath.Join(b.dirs[id], "Atoll", "trash"))
	return err == nil
}

// TestNothingMovesBeforeItIsJournaled: each command whose operation the catalog cannot journal stops with that error,
// and has touched nothing on a disk: a filesystem operation never runs before its ops row is committed (invariant 12).
func TestNothingMovesBeforeItIsJournaled(t *testing.T) {
	t.Run("prune", func(t *testing.T) {
		b := newBench(t)
		b.refuse("no_trash", "INSERT ON ops WHEN NEW.kind = 'trash'")
		s := b.opened()
		if _, err := s.Prune(t.Context(), func(*PruneItem) error { return nil }); err == nil {
			t.Error("a prune went on without its journal")
		}
		if !b.sourceThere() || b.trashThere("t3") || b.count("SELECT count(*) FROM ops") != 0 {
			t.Error("the copy moved, or its trash was made")
		}
	})
	t.Run("restore", func(t *testing.T) {
		b := newBench(t)
		b.prune()
		b.refuse("no_restore", "INSERT ON ops WHEN NEW.kind = 'restore'")
		s := b.opened()
		if _, err := s.Restore(t.Context(), "a", func(*EntryItem) error { return nil }); err == nil {
			t.Error("a restore went on without its journal")
		}
		if b.sourceThere() || b.count("SELECT count(*) FROM trash") != 1 {
			t.Error("the entry left the trash")
		}
	})
	t.Run("a folder of a restore", func(t *testing.T) {
		b := newBench(t)
		b.addFiles("Films/b")
		b.prune()
		if err := os.Remove(filepath.Join(b.dirs["t3"], "media", "Films")); err != nil {
			t.Fatal(err)
		}
		b.refuse("no_mkdir", "INSERT ON ops WHEN NEW.kind = 'mkdir'")
		s := b.opened()
		if _, err := s.Restore(t.Context(), "Films/b", func(*EntryItem) error { return nil }); err == nil {
			t.Error("a folder was made without its journal")
		}
		if b.there("t3", "Films") || b.text("SELECT state FROM ops WHERE kind = 'restore'") != "planned" {
			t.Errorf("the folder there: %v", b.there("t3", "Films"))
		}
	})
	t.Run("purge", func(t *testing.T) {
		b := newBench(t)
		b.prune()
		b.refuse("no_unlink", "INSERT ON ops WHEN NEW.kind = 'unlink'")
		s := b.opened()
		var zero time.Duration
		if _, err := s.Purge(t.Context(), &zero, func(*EntryItem) error { return nil }); err == nil {
			t.Error("a purge went on without its journal")
		}
		if _, err := os.Stat(b.inTrash(1, "a")); err != nil {
			t.Errorf("the entry's file: %v", err)
		}
	})
	t.Run("restore to another disk", func(t *testing.T) {
		b := pruned(t)
		b.refuse("no_create", "INSERT ON ops WHEN NEW.kind = 'create'")
		s := b.opened()
		if _, err := s.RestoreTo(t.Context(), "a", "t1", func(*EntryItem) error { return nil }); err == nil {
			t.Error("a copy went on without its journal")
		}
		if b.partsOn("t1") != 0 || b.read("t1", "a") != "" {
			t.Error("a part or a copy was made")
		}
	})
	t.Run("repair", func(t *testing.T) {
		r := rotted(t)
		r.b.refuse("no_repair", "INSERT ON ops WHEN NEW.kind = 'repair'")
		s := r.b.opened()
		if _, err := s.RepairChunks(t.Context(), r.file, r.copy, []int64{0}, nil, r.healthy, r.chunks); err == nil {
			t.Error("a repair went on without its journal")
		}
		if r.b.read("t3", "a") != "y" || r.b.trashThere("t3") {
			t.Error("the copy was written, or its chunk saved")
		}
	})
	t.Run("link guard", func(t *testing.T) {
		b := newBench(t)
		b.refuse("no_link", "INSERT ON ops WHEN NEW.kind = 'link'")
		s := b.opened()
		if err := s.Guard(t.Context(), "t3", true, nil); err == nil {
			t.Error("a link was made without its journal")
		}
		if _, err := os.Lstat(b.linkOf("t3", 1)); !os.IsNotExist(err) {
			t.Errorf("the link: %v", err)
		}
	})
	t.Run("a folder left empty", func(t *testing.T) {
		b := newBench(t)
		b.addFiles("d/x")
		b.own("t3", "d")
		b.refuse("no_rmdir", "INSERT ON ops WHEN NEW.kind = 'rmdir'")
		s := b.opened()
		if _, err := s.Prune(t.Context(), func(*PruneItem) error { return nil }); err == nil {
			t.Error("a folder was removed without its journal")
		}
		if !b.there("t3", "d") {
			t.Error("the folder went")
		}
	})
}

// TestAMoveTheCatalogCannotRecordIsLeftToTheReprise: a move done whose end the catalog cannot record stops the command
// with that error, its operation planned, as a crash right then leaves it; the next run's reprise finishes it (specs.md
// §17.2).
func TestAMoveTheCatalogCannotRecordIsLeftToTheReprise(t *testing.T) {
	const done = "UPDATE OF state ON ops WHEN NEW.state = 'done' AND OLD.kind = "
	t.Run("trash", func(t *testing.T) {
		b := newBench(t)
		b.refuse("no_done", done+"'trash'")
		s := b.opened()
		if _, err := s.Prune(t.Context(), func(*PruneItem) error { return nil }); err == nil {
			t.Error("a move went on without its record")
		}
		s.Close()
		if b.sourceThere() || b.text("SELECT state FROM ops") != "planned" {
			t.Error("the copy is still at its place, or its operation is not planned")
		}
		b.allow("no_done")
		b.opened().Close()
		if b.text("SELECT state FROM ops") != "done" || b.count("SELECT count(*) FROM copies WHERE disk_id = 't3'") != 0 {
			t.Errorf("after the reprise: %s", b.text("SELECT state FROM ops"))
		}
	})
	t.Run("restore", func(t *testing.T) {
		b := newBench(t)
		b.prune()
		b.refuse("no_done", done+"'restore'")
		s := b.opened()
		if _, err := s.Restore(t.Context(), "a", func(*EntryItem) error { return nil }); err == nil {
			t.Error("a restore went on without its record")
		}
		s.Close()
		if !b.sourceThere() || b.text("SELECT state FROM ops WHERE kind = 'restore'") != "planned" {
			t.Error("the copy is not back, or its operation is not planned")
		}
		b.allow("no_done")
		b.opened().Close()
		if b.text("SELECT state FROM ops WHERE kind = 'restore'") != "done" || b.count("SELECT count(*) FROM trash") != 0 {
			t.Error("the reprise did not finish it")
		}
	})
	t.Run("purge", func(t *testing.T) {
		b := newBench(t)
		b.prune()
		b.refuse("no_done", done+"'unlink'")
		s := b.opened()
		var zero time.Duration
		if _, err := s.Purge(t.Context(), &zero, func(*EntryItem) error { return nil }); err == nil {
			t.Error("a purge went on without its record")
		}
		s.Close()
		if _, err := os.Stat(b.inTrash(1, "a")); !os.IsNotExist(err) || b.text("SELECT state FROM ops WHERE kind = 'unlink'") != "planned" {
			t.Errorf("the entry's file: %v", err)
		}
		b.allow("no_done")
		b.opened().Close()
		if b.text("SELECT state FROM ops WHERE kind = 'unlink'") != "done" || b.count("SELECT count(*) FROM trash") != 0 {
			t.Error("the reprise did not finish it")
		}
	})
}

// TestACatalogThatCannotBeReadStopsTheCommand: what the trash cannot read of the catalog stops it with that error, and
// nothing is purged, restored or counted as nothing.
func TestACatalogThatCannotBeReadStopsTheCommand(t *testing.T) {
	b := newBench(t)
	b.prune()
	b.exec("UPDATE trash SET expires_ns = 1")
	s := b.opened()
	b.exec("ALTER TABLE trash RENAME TO trash_gone")
	var zero time.Duration
	if _, err := s.Purge(t.Context(), &zero, func(*EntryItem) error { return nil }); err == nil {
		t.Error("purge")
	}
	if _, _, err := s.PurgeUntil(t.Context(), b.Now()+1, func(*EntryItem) error { return nil }); err == nil {
		t.Error("purge by slices")
	}
	if _, err := s.Restore(t.Context(), "a", func(*EntryItem) error { return nil }); err == nil {
		t.Error("restore")
	}
	if due, err := PurgeDue(t.Context(), b.db, b.config, b.Now()); err == nil {
		t.Errorf("a purge due: %v", due)
	}
	if full, err := FullOncePurged(t.Context(), b.db, b.config, b.Now()); err == nil {
		t.Errorf("full: %v", full)
	}
	b.exec("ALTER TABLE trash_gone RENAME TO trash")
	if _, err := os.Stat(b.inTrash(1, "a")); err != nil || b.sourceThere() {
		t.Errorf("the entry's file: %v", err)
	}
	// What the purge for room reads of the disks.
	b.exec("UPDATE trash SET expires_ns = 9223372036854775807")
	b.exec("ALTER TABLE disks RENAME TO disks_gone")
	if _, _, err := s.PurgeUntil(t.Context(), b.Now()+1, func(*EntryItem) error { return nil }); err == nil {
		t.Error("purge by slices, for room")
	}
	if _, err := PurgeDue(t.Context(), b.db, b.config, b.Now()); err == nil {
		t.Error("a purge due, for room")
	}
	if _, err := s.Purge(t.Context(), &zero, func(*EntryItem) error { return nil }); err == nil {
		t.Error("purge, of the disks")
	}
	b.exec("ALTER TABLE disks_gone RENAME TO disks")
	if f, err := catalog.FileAt(t.Context(), b.db, "a"); err != nil || f == nil {
		t.Errorf("the catalog after: %v", err)
	}
}
