package trash

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/watch"
)

// goneAtJournal deletes a's copies on t1 and t2, directly on their disks, once the move of t3's is journaled: after
// the look at the copies that stay, before the move.
func (b *bench) goneAtJournal() {
	b.at["journaled"] = func() {
		for _, id := range []string{"t1", "t2"} {
			if err := os.Remove(filepath.Join(b.dirs[id], "media", "a")); err != nil {
				b.t.Fatal(err)
			}
		}
	}
}

// overFull gives t3's trash more than trash.max_size, 5 % of 2 bytes, as the catalog records it at the next check.
func (b *bench) overFull() {
	b.t.Helper()
	b.capacity = 2
	b.opened().Close()
}

// purgedWhy purges as atoll trash purge does, with no age, and gives each entry's why and outcome.
func (b *bench) purgedWhy() []string {
	b.t.Helper()
	s := b.opened()
	defer s.Close()
	var out []string
	if _, err := s.Purge(b.t.Context(), nil, func(i *EntryItem) error {
		out = append(out, i.Why+" "+i.Outcome)
		return nil
	}); err != nil {
		b.t.Fatal(err)
	}
	return out
}

// prunedSurplus sends t3's surplus copy of a to the trash, as atoll prune does, and checks it went there as surplus.
func (b *bench) prunedSurplus() {
	b.t.Helper()
	items, _ := b.prune()
	if len(items) != 1 || items[0].Outcome != OutcomeTrashed {
		b.t.Fatalf("%+v", items)
	}
	if got := b.text("SELECT reason FROM trash"); got != "surplus" {
		b.t.Fatalf("t3's copy reached the trash as %s", got)
	}
}

// Flaw 28 of conformance/models/README.md (Trash.tla, NoKeptLost: FlawTrashSurplusPruned), fixed with the maintainer's
// remedy, surplus-looked (2026-11-26): a purge for room takes a surplus entry only once a look finds its version at its
// file's path on another disk, as that copy's row says (specs.md §17, §17.3). A surplus copy that prune sent while a
// user deleted, directly on their disks, the copies that stay, after prune's look and before its move, is the deleted
// file's last data: it went for room first, before the retention its deletion would have given it.
func TestASurplusCopySentAsTheOthersGoStaysForItsRetention(t *testing.T) {
	b := newBench(t)
	b.goneAtJournal()
	b.prunedSurplus()
	b.overFull()
	// The catalog still says t1's and t2's copies hold a: the look finds them gone, and the entry stays.
	if got := b.purgedWhy(); !slices.Equal(got, []string{"max-size changed"}) || b.count("SELECT count(*) FROM trash") != 1 {
		t.Fatalf("%v, %d in the trash", got, b.count("SELECT count(*) FROM trash"))
	}
	// A scan since knows them gone: the entry is not reached for room any more, and nothing is looked at.
	if _, err := watch.Scan(t.Context(), b.db, b.config, b, watch.Options{}); err != nil {
		t.Fatal(err)
	}
	looked := 0
	for _, id := range []string{"t1", "t2"} {
		b.fail[b.dirs[id]] = func(call, name string) error {
			if call == "stat" && name == "a" {
				looked++
			}
			return nil
		}
	}
	if got := b.purgedWhy(); len(got) != 0 || looked != 0 || b.count("SELECT count(*) FROM trash") != 1 {
		t.Fatalf("%v, %d looks, %d in the trash", got, looked, b.count("SELECT count(*) FROM trash"))
	}
	// Its retention over, it goes.
	b.elapsed += int64(31 * 24 * time.Hour)
	if got := b.purgedWhy(); !slices.Equal(got, []string{"expired purged"}) {
		t.Errorf("at its time: %v", got)
	}
}

// The same through atoll balance, whose old copy goes by prune's own path, trash.Session.Retire, one look at the copies
// that stay then the move, and reaches the trash as rebalance, which a purge for room takes with surplus (specs.md
// §13.3, §17; the maintainer, 2026-11-26).
func TestARebalancedCopySentAsTheOthersGoStaysForItsRetention(t *testing.T) {
	b := newBench(t)
	b.goneAtJournal()
	s := b.opened()
	if outcome, err := s.Retire(t.Context(), b.fileA(), "t3", catalog.ReasonRebalance); err != nil || outcome != OutcomeTrashed {
		t.Fatalf("%s, %v", outcome, err)
	}
	if err := s.Finish(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if got := b.text("SELECT reason FROM trash"); got != catalog.ReasonRebalance {
		t.Fatalf("t3's copy reached the trash as %s", got)
	}
	b.overFull()
	if got := b.purgedWhy(); !slices.Equal(got, []string{"max-size changed"}) || b.count("SELECT count(*) FROM trash") != 1 {
		t.Errorf("%v, %d in the trash", got, b.count("SELECT count(*) FROM trash"))
	}
}

// A surplus entry whose version a copy on another disk holds, as its row says, goes for room, as before; a rebalance
// entry too.
func TestASurplusEntryGoesForRoomWhenACopyHoldsIt(t *testing.T) {
	b := newBench(t)
	b.prunedSurplus()
	b.overFull()
	if got := b.purgedWhy(); !slices.Equal(got, []string{"max-size purged"}) || b.count("SELECT count(*) FROM trash") != 0 {
		t.Errorf("%v, %d in the trash", got, b.count("SELECT count(*) FROM trash"))
	}
}

// The look before a purge for room, on the disks of the copies that hold the entry's version: one found as its row says
// is enough; a disk that fails the look is lost (invariant 4), one missing is not looked at, one mounted read-only is;
// and a run stopped at the look, or right after it, journals nothing, so the next one looks again.
func TestTheLookBeforeAPurgeForRoom(t *testing.T) {
	stop := errors.New("stopped")
	eio := func(b *bench, ids ...string) {
		for _, id := range ids {
			b.fail[b.dirs[id]] = func(call, name string) error {
				if call == "stat" && name == "a" {
					return syscall.EIO
				}
				return nil
			}
		}
	}
	for _, tc := range []struct {
		name  string
		dress func(b *bench)
		want  string
		left  int
	}{
		{"EIO on both", func(b *bench) { eio(b, "t1", "t2") }, "max-size changed", 1},
		{"EIO on one, the other holds", func(b *bench) { eio(b, "t1") }, "max-size purged", 0},
		{"both missing", func(b *bench) { b.unread[b.dirs["t1"]], b.unread[b.dirs["t2"]] = true, true }, "max-size changed", 1},
		{"both read-only", func(b *bench) { b.ro[b.dirs["t1"]], b.ro[b.dirs["t2"]] = true, true }, "max-size purged", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t)
			b.prunedSurplus()
			b.overFull()
			tc.dress(b)
			if got := b.purgedWhy(); !slices.Equal(got, []string{tc.want}) || b.count("SELECT count(*) FROM trash") != tc.left {
				t.Errorf("%v, %d in the trash", got, b.count("SELECT count(*) FROM trash"))
			}
			if tc.name == "EIO on both" {
				if got := b.text("SELECT group_concat(state, ' ') FROM (SELECT state FROM disks WHERE id IN ('t1', 't2') ORDER BY id)"); got != "missing missing" {
					t.Errorf("the disks that failed the look are %s", got)
				}
			}
		})
	}
	for _, tc := range []struct {
		name string
		arm  func(b *bench)
	}{
		{"at the look", func(b *bench) {
			b.fail[b.dirs["t1"]] = func(call, name string) error {
				if call == "stat" && name == "a" {
					panic(stop) //nolint:forbidigo // what stands for kill -9
				}
				return nil
			}
		}},
		{"right after it", func(b *bench) { b.at["looked"] = func() { panic(stop) } }}, //nolint:forbidigo // kill -9
	} {
		t.Run("stopped "+tc.name, func(t *testing.T) {
			b := newBench(t)
			b.prunedSurplus()
			b.overFull()
			tc.arm(b)
			func() {
				defer func() {
					if err, ok := recover().(error); !ok || !errors.Is(err, stop) {
						t.Fatalf("it did not stop: %v", err)
					}
				}()
				b.purgedWhy()
			}()
			delete(b.fail, b.dirs["t1"])
			delete(b.at, "looked")
			if got := b.count("SELECT count(*) FROM ops WHERE kind = 'unlink'"); got != 0 || b.count("SELECT count(*) FROM trash") != 1 {
				t.Fatalf("%d purges journaled, %d in the trash", got, b.count("SELECT count(*) FROM trash"))
			}
			if got := b.purgedWhy(); !slices.Equal(got, []string{"max-size purged"}) {
				t.Errorf("the next run: %v", got)
			}
		})
	}
}

// The purge by age looks at nothing: what reached its time goes, whatever the other disks hold, and wakes none of them.
func TestAPurgeByAgeLooksAtNothing(t *testing.T) {
	b := newBench(t)
	b.goneAtJournal()
	b.prunedSurplus()
	b.overFull()
	b.elapsed += int64(31 * 24 * time.Hour)
	looked := 0
	for _, id := range []string{"t1", "t2"} {
		b.fail[b.dirs[id]] = func(call, name string) error {
			if call == "stat" && name == "a" {
				looked++
			}
			return nil
		}
	}
	if got := b.purgedWhy(); !slices.Equal(got, []string{"expired purged"}) || looked != 0 {
		t.Errorf("%v, %d looks", got, looked)
	}
}

// The SSD cache's transfer (Trash.tla, FlawTrashSurplusAtOnce): its purge at once looks at the data copies; one it finds
// gone, or a stop before its look, leaves the copy in the cache's trash, surplus, which a purge for room then takes only
// once a look finds its version on a data disk (specs.md §17, §19).
func TestATransfersEntryStaysForItsRetention(t *testing.T) {
	t.Run("its look finds the data copies gone", func(t *testing.T) {
		b := newBench(t)
		b.goneAtJournal()
		s := b.opened()
		f := b.fileA()
		if got, err := s.Transfer(t.Context(), f, copyOn(f, "t3")); err != nil || got != OutcomeChanged {
			t.Fatalf("%q, %v", got, err)
		}
		s.Close()
		b.overFull()
		if got := b.purgedWhy(); !slices.Equal(got, []string{"max-size changed"}) || b.count("SELECT count(*) FROM trash") != 1 {
			t.Errorf("%v, %d in the trash", got, b.count("SELECT count(*) FROM trash"))
		}
	})
	t.Run("stopped before its look", func(t *testing.T) {
		b := newBench(t)
		stop := errors.New("stopped")
		// The data copies deleted, then the run stopped as kill -9 stops it, the copy moved and its operation planned.
		b.at["source-synced"] = func() {
			for _, id := range []string{"t1", "t2"} {
				if err := os.Remove(filepath.Join(b.dirs[id], "media", "a")); err != nil {
					t.Fatal(err)
				}
			}
			panic(stop) //nolint:forbidigo // what stands for kill -9
		}
		func() {
			defer func() {
				if err, ok := recover().(error); !ok || !errors.Is(err, stop) {
					t.Fatalf("it did not stop: %v", err)
				}
			}()
			s := b.opened()
			f := b.fileA()
			_, _ = s.Transfer(t.Context(), f, copyOn(f, "t3"))
		}()
		// The next run's reprise finishes the move: the copy is in the trash, surplus, its purge never done.
		b.opened().Close()
		if got := b.text("SELECT reason FROM trash"); got != "surplus" {
			t.Fatalf("the reprise left %s", got)
		}
		b.overFull()
		if got := b.purgedWhy(); !slices.Equal(got, []string{"max-size changed"}) || b.count("SELECT count(*) FROM trash") != 1 {
			t.Errorf("%v, %d in the trash", got, b.count("SELECT count(*) FROM trash"))
		}
	})
}
