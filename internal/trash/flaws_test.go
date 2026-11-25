//go:build flaws

package trash

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
)

// Flaw 28 of conformance/models/README.md (Trash.tla, NoKeptLost: FlawTrashSurplusPruned and FlawTrashSurplusAtOnce),
// open: a surplus copy that atoll prune sends to the trash while a user deletes, directly on their disks, the copies
// that stay, after prune's look at them and before its move, reaches the trash as surplus, and a purge for room takes
// it first, before the retention its deletion would give it: with the link guard off, it is the deleted file's last
// data. Had prune not run, the deletion would have sent it to the trash as deleted, kept for the whole retention
// (specs.md §15.5, §17, §17.3).
func TestFlawASurplusCopySentAsTheOthersGoIsPurgedForRoom(t *testing.T) {
	b := newBench(t)
	// a on t1, t2 and t3, x on each: the level is two copies, t3's is surplus. The user deletes the two that stay,
	// directly on their disks, once prune has looked at them and journaled the move of t3's.
	b.at["journaled"] = func() {
		for _, id := range []string{"t1", "t2"} {
			if err := os.Remove(filepath.Join(b.dirs[id], "media", "a")); err != nil {
				t.Fatal(err)
			}
		}
	}
	items, _ := b.prune()
	if len(items) != 1 || items[0].Outcome != OutcomeTrashed {
		t.Fatalf("%+v", items)
	}
	if got := b.text("SELECT reason FROM trash"); got != "surplus" {
		t.Fatalf("t3's copy reached the trash as %s", got)
	}
	// The trash holds more than trash.max_size, 5 % of 2 bytes: a purge for room takes the surplus first.
	b.capacity = 2
	b.opened().Close()
	s := b.opened()
	var outcomes []string
	if _, err := s.Purge(t.Context(), nil, func(i *EntryItem) error {
		outcomes = append(outcomes, i.Why+" "+i.Outcome)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := b.count("SELECT count(*) FROM trash"); got != 1 {
		t.Errorf("the last data of a, which a user deleted while prune sent its surplus copy to the trash, went for room "+
			"before the retention its deletion gives it: %v", outcomes)
	}
}

// Flaw 28 through atoll balance (specs.md §13.3): its old copy goes by the same path as prune's, trash.Session.Retire,
// one look at the copies that stay (decidePrune), then the move, and reaches the trash as rebalance, which a purge for
// room takes with surplus, first. A user who deletes the copies that stay between the two leaves it the deleted file's
// last data, as above.
func TestFlawARebalancedCopySentAsTheOthersGoIsPurgedForRoom(t *testing.T) {
	b := newBench(t)
	b.at["journaled"] = func() {
		for _, id := range []string{"t1", "t2"} {
			if err := os.Remove(filepath.Join(b.dirs[id], "media", "a")); err != nil {
				t.Fatal(err)
			}
		}
	}
	s := b.opened()
	f, err := catalog.FileAt(t.Context(), b.db, "a")
	if err != nil || f == nil {
		t.Fatalf("%+v, %v", f, err)
	}
	if outcome, err := s.Retire(t.Context(), f, "t3", catalog.ReasonRebalance); err != nil || outcome != OutcomeTrashed {
		t.Fatalf("%s, %v", outcome, err)
	}
	if err := s.Finish(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if got := b.text("SELECT reason FROM trash"); got != catalog.ReasonRebalance {
		t.Fatalf("t3's copy reached the trash as %s", got)
	}
	b.capacity = 2
	b.opened().Close()
	p := b.opened()
	var outcomes []string
	if _, err := p.Purge(t.Context(), nil, func(i *EntryItem) error {
		outcomes = append(outcomes, i.Why+" "+i.Outcome)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := b.count("SELECT count(*) FROM trash"); got != 1 {
		t.Errorf("the last data of a, which a user deleted while balance sent its old copy to the trash, went for room "+
			"before the retention its deletion gives it: %v", outcomes)
	}
}
