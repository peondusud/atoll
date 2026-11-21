package jobs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
)

// A file that takes a copy's name between the last look and the move to the trash is moved there in its place: it is
// not the copy, so it goes back to its name, untouched, the operation undone with an event that says so, and the copy
// it replaced is a deletion like any other for the next passes (specs.md §17.1, §17.2). Nothing of the user's is left
// in the trash in its place.
func TestAFileThatTookTheNameGoesBackFromTheTrash(t *testing.T) {
	for _, driver := range []struct {
		name string
		fail func(call, name string) error
	}{{"ext4", nil}, {"ntfs-3g", func(call, _ string) error {
		// It refuses a rename that replaces nothing, whatever the name: only a link tells (specs.md §17.1).
		if call == "rename" {
			return syscall.EINVAL
		}
		return nil
	}}} {
		t.Run(driver.name, func(t *testing.T) {
			b := pool(t)
			b.remove("t1", "a")
			b.pass()
			b.now += 30_000_000_000
			full := filepath.Join(b.dirs["t2"], "media", "a")
			swapped := false
			b.fail[b.dirs["t2"]] = func(call, _ string) error {
				// Right before the move, someone else's file takes the name, as an atomic save does: written beside it,
				// then renamed over it, so that it is another inode, with another content.
				if !swapped && (call == "rename" || call == "link") {
					swapped = true
					if err := os.WriteFile(full+".new", []byte("theirs"), 0o644); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(full+".new", full); err != nil {
						t.Fatal(err)
					}
				}
				if driver.fail != nil {
					return driver.fail(call, "")
				}
				return nil
			}
			out, err := Run(t.Context(), b.db, b.config, b, true)
			if err != nil {
				t.Fatal(err)
			}
			if !swapped {
				t.Fatalf("the move never came: %+v", out.Operations)
			}
			delete(b.fail, b.dirs["t2"])
			if got := b.read("t2", "a"); got != "theirs" {
				t.Errorf("the file that took the name holds %q", got)
			}
			if got := b.count("SELECT count(*) FROM ops WHERE kind = ? AND state = 'undone'", catalog.OpTrash); got != 1 {
				t.Errorf("%d operations undone", got)
			}
			if got := b.count("SELECT count(*) FROM events WHERE kind = 'op-undone'"); got != 1 {
				t.Errorf("%d events op-undone", got)
			}
			if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
				t.Errorf("%d operations left planned", got)
			}
			trash := b.tree("t2", "Atoll/trash", 2)
			if len(trash) != 0 {
				t.Errorf("the trash holds %q", trash)
			}
		})
	}
}
