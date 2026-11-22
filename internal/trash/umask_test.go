package trash

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// withUmask sets the process's mask until the test ends, as the units' UMask=0027 does (deploy/atoll.service). The
// mask is the whole process's: a test that sets it is never parallel, and no parallel test runs while it does.
func withUmask(t *testing.T, mask int) {
	t.Helper()
	old := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(old) })
}

// noModelInTrash makes a folder of an operation's folder in the trash of a disk read as absent to a look by its name:
// a folder made like it has none (specs.md §17.4). The trash's own folders, and what goes through them by a handle,
// are as they are.
func (b *bench) noModelInTrash(id, op, folder string) {
	last := ""
	b.fail[b.dirs[id]] = func(call, name string) error {
		switch {
		case call == "opendir":
			last = name
		case call == "stat" && name == folder && last == op:
			return syscall.ENOENT
		}
		return nil
	}
}

func (b *bench) modeOf(id, path string) uint32 {
	b.t.Helper()
	var st syscall.Stat_t
	if err := syscall.Lstat(filepath.Join(b.dirs[id], "media", path), &st); err != nil {
		b.t.Fatal(err)
	}
	return st.Mode & 0o7777
}

// madeOpen says what a folder made with no model must be: 0755 whatever the mask, its operation's event holding the
// mode 0755 and no owner (the maintainer, 2026-11-20).
func (b *bench) madeOpen(id, folder string) {
	b.t.Helper()
	if got := b.modeOf(id, folder); got != 0o755 {
		b.t.Errorf("%s on %s is %04o, want 0755", folder, id, got)
	}
	if got := b.text("SELECT group_concat(state) FROM ops WHERE kind = 'mkdir'"); got != "done" {
		b.t.Errorf("its operation: %q", got)
	}
	got := b.text(`SELECT json_extract(data, '$.disk') || ' ' || json_extract(data, '$.mode') || ' ' ||
		(json_type(data, '$.uid') IS NULL) || (json_type(data, '$.gid') IS NULL) FROM events WHERE kind = 'mkdir'`)
	if want := id + " 493 11"; got != want {
		b.t.Errorf("its event: %q, want %q", got, want)
	}
}

// A folder a restore makes, which the trash holds no folder for, is 0755 whatever the mask: under the units' 0027 it
// was 0750, closed to a media server running under another account (specs.md §17.4).
func TestARestoredFolderWithNoModelIsOpenWhateverTheUmask(t *testing.T) {
	b := newBench(t)
	b.addFiles("p/x")
	b.prune()
	if err := os.Remove(filepath.Join(b.dirs["t3"], "media", "p")); err != nil {
		t.Fatal(err)
	}
	b.noModelInTrash("t3", "2", "p")
	withUmask(t, 0o027)
	if outcomes, _ := b.restored("p/x"); len(outcomes) != 1 || outcomes[0] != OutcomeRestored {
		t.Fatalf("%+v", outcomes)
	}
	b.madeOpen("t3", "p")
}

// So is one a restore to another disk makes, with no folder in the trash it comes from (specs.md §17.4).
func TestAFolderRestoredToAnotherDiskWithNoModelIsOpenWhateverTheUmask(t *testing.T) {
	b := newBench(t)
	b.addFiles("p/x")
	b.prune()
	for _, name := range []string{"p/x", "p"} {
		if err := os.Remove(filepath.Join(b.dirs["t1"], "media", name)); err != nil {
			t.Fatal(err)
		}
	}
	b.noModelInTrash("t3", "2", "p")
	withUmask(t, 0o027)
	if outcomes, _ := b.restoredTo("p/x", "t1", true); len(outcomes) != 1 || outcomes[0] != OutcomeRestored {
		t.Fatalf("%+v", outcomes)
	}
	b.madeOpen("t1", "p")
}
