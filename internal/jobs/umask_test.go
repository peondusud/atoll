package jobs

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

// noModelOn makes a folder of a disk read as absent to a look by its name, as one no disk holds any more: a folder
// made like it has no model (specs.md §15.12, step 3). What goes through it by a handle still finds it.
func (b *bench) noModelOn(id, folder string) {
	b.fail[b.dirs[id]] = func(call, name string) error {
		if call == "stat" && name == folder {
			return syscall.ENOENT
		}
		return nil
	}
}

// madeOpen says what a folder made with no model must be: 0755 whatever the mask, with the setgid bit its parent passes
// on, its operation's event holding the mode 0755 and no owner, and its entry the mode it has (the maintainer,
// 2026-11-20).
func (b *bench) madeOpen(id, folder string, want int64) {
	b.t.Helper()
	if got := b.modeOf(id, folder); got != want {
		b.t.Errorf("%s on %s is %04o, want %04o", folder, id, got, want)
	}
	if got := b.text("SELECT group_concat(state) FROM ops WHERE kind = 'mkdir' AND disk_id = ? AND CAST(path AS TEXT) = ?", id, folder); got != "done" {
		b.t.Errorf("its operation: %q", got)
	}
	got := b.text(`SELECT json_extract(data, '$.mode') || ' ' || (json_type(data, '$.uid') IS NULL) || (json_type(data, '$.gid') IS NULL)
		FROM events WHERE kind = 'mkdir' AND json_extract(data, '$.disk') = ? AND json_extract(data, '$.path') = ?`, id, folder)
	if got != "493 11" {
		b.t.Errorf("its event holds mode, no uid, no gid: %q, want %q", got, "493 11")
	}
	if got := b.count("SELECT mode FROM entries WHERE disk_id = ? AND CAST(lpath AS TEXT) = ?", id, folder); int64(got) != want {
		b.t.Errorf("its entry says %04o", got)
	}
}

// A parent of a copy that the source's disk does not hold, as a copy behind at its old path has none, is made 0755
// whatever the mask: under the units' 0027 it was 0750, closed to a media server running under another account, and what
// landed below it left that server's library with nothing said. Only the permission bits are set: the setgid bit a
// parent passes on stays, as mkdir gives it whatever the mask.
func TestACopysParentWithNoModelIsOpenWhateverTheUmask(t *testing.T) {
	for _, tc := range []struct {
		name   string
		parent os.FileMode
		want   int64
	}{{"under a plain folder", 0o755, 0o755}, {"under a setgid folder", 0o755 | os.ModeSetgid, 0o2755}} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t, "t1", "t2")
			b.write("t1", "p/a", "content")
			if err := os.Chmod(filepath.Join(b.dirs["t2"], "media"), tc.parent); err != nil {
				t.Fatal(err)
			}
			b.scan(false)
			b.noModelOn("t1", "p")
			withUmask(t, 0o027)
			items, _ := b.replicate("p", ReplicateOptions{Now: true})
			if len(items) != 1 || items[0].Outcome != OutcomeReplicated {
				t.Fatalf("%+v", items)
			}
			b.madeOpen("t2", "p", tc.want)
		})
	}
}

// A parent of a rename that no disk holds is made 0755 whatever the mask (specs.md §15.12, step 3).
func TestARenamesParentWithNoModelIsOpenWhateverTheUmask(t *testing.T) {
	b := pool(t)
	b.rename("t1", "a", "P/a")
	b.pass()
	b.now += 30_000_000_000
	b.noModelOn("t1", "P")
	withUmask(t, 0o027)
	out := b.pass()
	if got := outcomeOf(out, KindRename, "a"); got != "renamed" {
		t.Fatalf("outcome %q (%+v)", got, out.Operations)
	}
	b.madeOpen("t2", "P", 0o755)
}

// A folder with a model takes the model's mode, whatever the mask, and its event the model's owner too.
func TestACopysParentWithAModelKeepsItsModeWhateverTheUmask(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "p/a", "content")
	if err := os.Chmod(filepath.Join(b.dirs["t1"], "media", "p"), 0o705); err != nil {
		t.Fatal(err)
	}
	b.scan(false)
	withUmask(t, 0o027)
	items, _ := b.replicate("p", ReplicateOptions{Now: true})
	if len(items) != 1 || items[0].Outcome != OutcomeReplicated {
		t.Fatalf("%+v", items)
	}
	if got := b.modeOf("t2", "p"); got != 0o705 {
		t.Errorf("%04o", got)
	}
	got := b.text(`SELECT json_extract(data, '$.mode') || ' ' || (json_type(data, '$.uid') IS NOT NULL) || (json_type(data, '$.gid') IS NOT NULL)
		FROM events WHERE kind = 'mkdir'`)
	if got != "453 11" {
		t.Errorf("its event: %q", got)
	}
}

// Killed between its mkdir and the mode it is then given, a folder with no model is left as the mask made it, its
// operation planned; the reprise gives it 0755 from what its event holds, as the process that resumes has the same mask.
// One that holds anything by then is not given it, as a folder just made holds nothing, and may not be the one made.
func TestAFolderWithNoModelTakesItsModeAtTheReprise(t *testing.T) {
	for _, tc := range []struct {
		name string
		held bool
		want int64
	}{{"empty", false, 0o755}, {"holding a file by then", true, 0o750}} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t, "t1", "t2")
			b.write("t1", "p/a", "content")
			b.scan(false)
			b.noModelOn("t1", "p")
			withUmask(t, 0o027)
			b.replicateStopping("p", "mkdir-made")
			if got := b.modeOf("t2", "p"); got != 0o750 {
				t.Errorf("as mkdir made it: %04o", got)
			}
			if got := b.text("SELECT state FROM ops WHERE kind = 'mkdir'"); got != "planned" {
				t.Fatalf("its operation: %q", got)
			}
			if tc.held {
				if err := os.WriteFile(filepath.Join(b.dirs["t2"], "media", "p", "b"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			b.replicate("p", ReplicateOptions{Now: true})
			if got := b.modeOf("t2", "p"); got != tc.want {
				t.Errorf("after the reprise: %04o, want %04o", got, tc.want)
			}
			if got := b.text("SELECT state FROM ops WHERE kind = 'mkdir'"); got != "done" {
				t.Errorf("its operation: %q", got)
			}
			if got := b.count("SELECT mode FROM entries WHERE disk_id = 't2' AND CAST(lpath AS TEXT) = 'p'"); int64(got) != tc.want {
				t.Errorf("its entry says %04o", got)
			}
		})
	}
}

// A driver that refuses the mode, as ntfs-3g without permissions does, leaves the folder as mkdir made it: the folder
// and its copy are there, with a warning.
func TestAFolderWhoseModeIsRefused(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "p/a", "content")
	b.scan(false)
	b.noModelOn("t1", "p")
	b.fail[b.dirs["t2"]] = func(call, name string) error {
		if call == "setmode" && name == "p" {
			return syscall.EPERM
		}
		return nil
	}
	withUmask(t, 0o027)
	items, _ := b.replicate("p", ReplicateOptions{Now: true})
	if len(items) != 1 || items[0].Outcome != OutcomeReplicated {
		t.Fatalf("%+v", items)
	}
	if got := b.modeOf("t2", "p"); got != 0o750 {
		t.Errorf("%04o", got)
	}
	if got := b.text("SELECT state FROM ops WHERE kind = 'mkdir'"); got != "done" {
		t.Errorf("its operation: %q", got)
	}
}

// A disk that fails as the folder is given its mode keeps the operation planned, for the reprise to give it.
func TestADiskLostAsAFolderIsGivenItsMode(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "p/a", "content")
	b.scan(false)
	b.noModelOn("t1", "p")
	b.fail[b.dirs["t2"]] = func(call, name string) error {
		if call == "setmode" && name == "p" {
			return syscall.EIO
		}
		return nil
	}
	withUmask(t, 0o027)
	if items, _ := b.replicate("p", ReplicateOptions{Now: true}); len(items) != 1 || items[0].Outcome == OutcomeReplicated {
		t.Fatalf("%+v", items)
	}
	if got := b.text("SELECT state FROM ops WHERE kind = 'mkdir'"); got != "planned" {
		t.Fatalf("its operation: %q", got)
	}
	b.fail[b.dirs["t2"]] = nil
	b.exec("UPDATE disks SET state = 'online'")
	b.replicate("p", ReplicateOptions{Now: true})
	if got := b.modeOf("t2", "p"); got != 0o755 {
		t.Errorf("after the reprise: %04o", got)
	}
	if got := b.text("SELECT state FROM ops WHERE kind = 'mkdir'"); got != "done" {
		t.Errorf("its operation: %q", got)
	}
}
