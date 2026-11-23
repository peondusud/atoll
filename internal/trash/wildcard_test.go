package trash

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/watch"
)

// TestAPruneLeavesAWildcardNameOnTheTypeNTFS: a copy at a name holding a wildcard character on a disk of the type ntfs,
// which cannot move or remove it without setting its whole volume read-only, is never sent to the trash, even recorded
// surplus as an earlier mount by another driver left it: atoll prune says the name refused, and journals nothing, so
// that nothing is asked of the disk (specs.md §15.8, §17.1). The surplus copy of a name with no wildcard goes, as any.
func TestAPruneLeavesAWildcardNameOnTheTypeNTFS(t *testing.T) {
	b := newBenchOf(t, "ntfs")
	for _, id := range []string{"t1", "t2", "t3"} {
		b.write(id, "Who?/q?.mkv", "x")
	}
	if _, err := watch.Scan(t.Context(), b.db, b.config, b, watch.Options{Full: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.ExecContext(t.Context(), `UPDATE copies SET state = 'surplus' WHERE disk_id = 't3'`); err != nil {
		t.Fatal(err)
	}
	items, summary := b.prune()
	outcomes := map[string]string{}
	for _, i := range items {
		outcomes[i.Disk+" "+i.Path] = i.Outcome
	}
	if outcomes["t3 Who?/q?.mkv"] != OutcomeNameRefused || outcomes["t3 a"] != OutcomeTrashed || summary.DiskLost {
		t.Fatalf("%+v, %+v", items, summary)
	}
	if got := b.count("SELECT count(*) FROM ops WHERE CAST(path AS TEXT) = 'Who?/q?.mkv'"); got != 0 {
		t.Errorf("%d operations journaled", got)
	}
	if _, err := os.Lstat(filepath.Join(b.dirs["t3"], "media", "Who?", "q?.mkv")); err != nil {
		t.Errorf("the copy left: %v", err)
	}
}

// TestATrashEntryAtAWildcardNameOnTheTypeNTFS: an entry whose path holds a wildcard character, which ntfs3 moved to the
// trash, stays there once the disk is mounted with the type ntfs, which cannot remove or move its name without setting
// its volume read-only: a purge journals nothing and leaves it, io-error, and a restore leaves it too, its name refused
// (specs.md §15.8, §17.4). The entry beside it with no wildcard is purged, as any.
func TestATrashEntryAtAWildcardNameOnTheTypeNTFS(t *testing.T) {
	b := newBenchOf(t, "ntfs3")
	for _, id := range []string{"t1", "t2", "t3"} {
		b.write(id, "Who?/q?.mkv", "x")
	}
	if _, err := watch.Scan(t.Context(), b.db, b.config, b, watch.Options{Full: true}); err != nil {
		t.Fatal(err)
	}
	b.prune()
	if got := b.count("SELECT count(*) FROM trash WHERE disk_id = 't3'"); got != 2 {
		t.Fatalf("%d entries in the trash of t3", got)
	}
	b.driver[b.dirs["t3"]] = "ntfs"
	if outcomes := b.restoredBy("Who?", true); len(outcomes) != 1 || outcomes[0] != OutcomeNameRefused {
		t.Errorf("restored: %v", outcomes)
	}
	s, err := Open(t.Context(), b.db, b.config, b, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	outcomes := map[string]string{}
	var zero time.Duration
	if _, err := s.Purge(t.Context(), &zero, func(i *EntryItem) error {
		outcomes[i.Path] = i.Outcome
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if outcomes["Who?/q?.mkv"] != OutcomeIOError || outcomes["a"] != OutcomePurged {
		t.Errorf("purged: %v", outcomes)
	}
	if got := b.count("SELECT count(*) FROM ops WHERE kind = 'unlink' AND CAST(path AS TEXT) = 'Who?/q?.mkv'"); got != 0 {
		t.Errorf("%d purges journaled", got)
	}
	if got := b.count("SELECT count(*) FROM trash WHERE disk_id = 't3'"); got != 1 {
		t.Errorf("%d entries in the trash of t3", got)
	}
}
