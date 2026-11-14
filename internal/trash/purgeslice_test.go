package trash

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

// exec runs a statement on the bench's catalog.
func (b *bench) exec(query string, args ...any) {
	b.t.Helper()
	if _, err := b.db.ExecContext(b.t.Context(), query, args...); err != nil {
		b.t.Fatal(err)
	}
}

// trashed gives a bench whose trash holds one entry for each of a and the paths given, all on the same disk, each of
// one byte and sent there at a time of its own, the first oldest.
func trashed(t *testing.T, paths ...string) *bench {
	t.Helper()
	b := newBench(t)
	b.addFiles(paths...)
	b.prune()
	if got, want := b.count("SELECT count(*) FROM trash"), len(paths)+1; got != want {
		t.Fatalf("%d entries in the trash, want %d", got, want)
	}
	if got := b.count("SELECT count(DISTINCT disk_id) FROM trash"); got != 1 {
		t.Fatalf("the entries are on %d disks", got)
	}
	b.exec("UPDATE trash SET deleted_ns = op_id")
	return b
}

// reached gives what a purge reached, as its operation and why, in the order of the operations.
func reached(items []EntryItem) []string {
	out := make([]string, 0, len(items))
	for i := range items {
		out = append(out, fmt.Sprintf("%d %s %s", items[i].OpID, items[i].Why, items[i].Outcome))
	}
	slices.Sort(out)
	return out
}

// until purges by a slice until a time, and gives what it reached, and whether it stopped there with entries left.
func (b *bench) until(at int64) ([]EntryItem, *Summary, bool) {
	b.t.Helper()
	s, err := Open(b.t.Context(), b.db, b.config, b, true)
	if err != nil {
		b.t.Fatal(err)
	}
	defer s.Close()
	var items []EntryItem
	summary, left, err := s.PurgeUntil(b.t.Context(), at, func(i *EntryItem) error {
		items = append(items, *i)
		return nil
	})
	if err != nil {
		b.t.Fatal(err)
	}
	if err := s.Finish(b.t.Context()); err != nil {
		b.t.Fatal(err)
	}
	return items, summary, left
}

// A purge by slices stops at its time, between two entries, and says that some are left: the next slice goes on
// (specs.md §15.13, §17). The daemon purged the whole trash in one hold of the catalog's lock, two transactions and a
// folder's sync for each entry: the day a mass deletion reached its retention, minutes during which no command ran.
func TestAPurgeBySlicesStopsAtItsTime(t *testing.T) {
	b := trashed(t, "b", "c", "d")
	b.exec("UPDATE trash SET expires_ns = 1")
	// Each entry takes a minute, once journaled: a slice of 30 s ends after the first.
	b.at["journaled"] = func() { b.elapsed += int64(time.Minute) }
	items, summary, left := b.until(b.Now() + 30_000_000_000)
	if len(items) != 1 || items[0].Outcome != OutcomePurged || items[0].Why != "expired" || !left || summary.Bytes != 1 {
		t.Fatalf("the first slice: %+v, %+v, left %v", items, summary, left)
	}
	if got := b.count("SELECT count(*) FROM trash"); got != 3 {
		t.Fatalf("%d entries left", got)
	}
	items, summary, left = b.until(b.Now() + int64(time.Hour))
	if len(items) != 3 || left || summary.Bytes != 3 || summary.Stayed != 0 {
		t.Fatalf("the next slice: %+v, %+v, left %v", items, summary, left)
	}
	if got := b.count("SELECT count(*) FROM trash"); got != 0 {
		t.Errorf("%d entries left", got)
	}
	if got := b.count("SELECT count(*) FROM ops WHERE kind = 'unlink' AND state = 'done'"); got != 4 {
		t.Errorf("%d purges journaled and done, want one for each entry", got)
	}
}

// The slices reach what a purge in one go reaches, for the same reasons: what has reached its time, then what goes
// for room on a disk whose trash holds more than trash.max_size, the oldest first, and nothing else.
func TestAPurgeBySlicesReachesWhatAPurgeReaches(t *testing.T) {
	b := trashed(t, "b", "c", "d", "e")
	// The newest has reached its time, and the trash may hold two bytes: 5 % of 40.
	b.exec("UPDATE trash SET expires_ns = 1 WHERE op_id = (SELECT max(op_id) FROM trash)")
	b.capacity = 40
	said, err := Open(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	var whole []EntryItem
	if _, err := said.Purge(t.Context(), nil, func(i *EntryItem) error {
		i.Outcome = OutcomePurged
		whole = append(whole, *i)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	said.Close()
	if len(whole) != 3 {
		t.Fatalf("a purge in one go would reach %+v, want the one expired and two for room", whole)
	}
	if due, err := PurgeDue(t.Context(), b.db, b.config, b.Now()); err != nil || !due {
		t.Fatalf("a purge is due: %v, %v", due, err)
	}
	items, summary, left := b.until(b.Now() + int64(time.Hour))
	if !slices.Equal(reached(items), reached(whole)) || left || len(summary.Full) != 0 {
		t.Fatalf("the slices reached %v, a purge in one go %v; %+v", reached(items), reached(whole), summary)
	}
	if got := b.count("SELECT count(*) FROM trash"); got != 2 {
		t.Errorf("%d entries left, want the two the trash has room for", got)
	}
	if due, err := PurgeDue(t.Context(), b.db, b.config, b.Now()); err != nil || due {
		t.Errorf("nothing is due any more: %v, %v", due, err)
	}
}

// A trash that holds too much of what never goes for room, as deleted files before their time, is no purge due, and
// stays full once everything due is gone: the alert tells of it (specs.md §17, §23).
func TestATrashThatStaysFull(t *testing.T) {
	b := trashed(t, "b", "c")
	// The catalog records what a disk holds when a session checks its mount.
	b.capacity = 40
	b.opened().Close()
	b.exec("UPDATE trash SET reason = 'deleted'")
	disk := b.text("SELECT DISTINCT disk_id FROM trash")
	if due, err := PurgeDue(t.Context(), b.db, b.config, b.Now()); err != nil || due {
		t.Errorf("due: %v, %v", due, err)
	}
	full, err := FullOncePurged(t.Context(), b.db, b.config, b.Now())
	if err != nil || len(full) != 1 || !full[disk] {
		t.Errorf("full once purged: %v, %v", full, err)
	}
	// With what goes for room, it is back under its limit once purged, and is told of no more.
	b.exec("UPDATE trash SET reason = 'surplus'")
	if full, err := FullOncePurged(t.Context(), b.db, b.config, b.Now()); err != nil || len(full) != 0 {
		t.Errorf("full once purged, with entries that go for room: %v, %v", full, err)
	}
}

// A restore reads the entries of its path and of what is below it, by whole components: a name that only begins as
// the path does stays in the trash (specs.md §17.4).
func TestARestoreReadsItsPathAlone(t *testing.T) {
	b := trashed(t, "d/x", "d.y", "d0")
	outcomes, _ := b.restored("d")
	if !slices.Equal(outcomes, []string{OutcomeRestored}) {
		t.Fatalf("%v", outcomes)
	}
	if got := b.text("SELECT group_concat(CAST(lpath AS TEXT), ',') FROM (SELECT lpath FROM trash ORDER BY lpath)"); got != "a,d.y,d0" {
		t.Errorf("the trash holds %s", got)
	}
}
