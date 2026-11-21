package jobs

import (
	"strings"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
)

// A new file the view moved to another disk once a write ran out of space (specs.md §21.2): the pass that follows
// records it on the disk it went to, a user's copy holding every byte written, takes in the view's operations, and
// counts no deletion of what the move left on the full disk, which went to the trash as Atoll's own and was purged.
func TestAPassTakesInAFileTheViewMovedForRoom(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "Films/a.mkv", "x")
	b.write("t2", "Films/a.mkv", "x")
	b.scan(true)
	c, err := config.Parse([]byte(strings.Replace(b.base, "[view]\n", "[view]\nmove_on_enospc = true\n", 1)))
	if err != nil {
		t.Fatal(err)
	}
	saved := b.config
	b.config = c
	w := b.viewOf()
	b.config = saved
	f, _, err := w.Create(t.Context(), "Films/new.mkv", 0o644, viewer)
	if err != nil {
		t.Fatal(err)
	}
	full := f.OnDisk()
	other := map[string]string{"t1": "t2", "t2": "t1"}[full]
	if _, err := f.WriteAt([]byte("first part "), 0); err != nil {
		t.Fatal(err)
	}
	b.fail[b.dirs[full]] = func(call, name string) error {
		if call == "write" && name == "new.mkv" {
			return syscall.ENOSPC
		}
		return nil
	}
	if _, err := f.WriteAt([]byte("second part"), 11); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	delete(b.fail, b.dirs[full])
	if f.OnDisk() != other || b.exists(full, "media/Films/new.mkv") {
		t.Fatalf("the file is on %s, and %s holds it: %v", f.OnDisk(), full, b.exists(full, "media/Films/new.mkv"))
	}
	b.settle()
	if got := b.read(other, "Films/new.mkv"); got != "first part second part" {
		t.Errorf("%s holds %q", other, got)
	}
	if got := b.count("SELECT count(*) FROM copies c JOIN files f ON f.id = c.file_id WHERE f.lpath = CAST('Films/new.mkv' AS BLOB) AND c.disk_id = ? AND c.created_by = 'user'", other); got != 1 {
		t.Errorf("the pass did not record the moved file on %s as the user's: %d", other, got)
	}
	if got := b.count("SELECT count(*) FROM events WHERE kind IN ('copy-gone', 'guard')"); got != 0 {
		t.Errorf("%d deletions or guards counted", got)
	}
	if got := b.count("SELECT count(*) FROM trash"); got != 0 {
		t.Errorf("%d entries left in the trash", got)
	}
	if open := b.openViewOps(); len(open) != 0 {
		t.Errorf("not taken in: %+v", open)
	}
	if got := b.count("SELECT count(*) FROM view_ops WHERE kind = ? AND state = 'done' AND taken_ns IS NOT NULL", catalog.ViewMove); got != 1 {
		t.Errorf("the move taken in: %d", got)
	}
}
