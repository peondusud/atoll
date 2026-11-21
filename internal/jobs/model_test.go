package jobs

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/trash"
)

// The model of a folder a copy makes (the maintainer, 2026-11-21, "Chercher, sinon attendre"): the same folder on the
// source's disk, or on the other disks present when that disk holds none, as a copy behind at its old path does not; when
// none of them holds it and the catalog says a disk away does, the copy waits (io-error), as §15.12's step 3 does; the
// mode 0755 only when no disk holds it (specs.md §14.2).

// modelPool gives three disks: t1 with a file p/a whose folder a look at t1 does not find, as a copy behind at its old
// path has none there; t2 read-only, so that no copy goes there, holding that folder empty and kept 0700; t3 empty, the
// copy's target.
func modelPool(t *testing.T) *bench {
	t.Helper()
	b := newBench(t, "t1", "t2", "t3")
	b.write("t1", "p/a", "content")
	b.folder("t2", "p", 0o700)
	b.ro[b.dirs["t2"]] = true
	b.scan(false)
	b.noModelOn("t1", "p")
	return b
}

// folder makes a folder of a disk with a mode, whatever the mask.
func (b *bench) folder(id, path string, mode os.FileMode) {
	b.t.Helper()
	full := filepath.Join(b.dirs[id], "media", path)
	if err := os.MkdirAll(full, 0o755); err != nil {
		b.t.Fatal(err)
	}
	if err := os.Chmod(full, mode); err != nil {
		b.t.Fatal(err)
	}
}

// madeLike says that a folder was made like a model: its mode, and its operation's event holding the model's mode and
// owner.
func (b *bench) madeLike(id, folder string, want int64) {
	b.t.Helper()
	if got := b.modeOf(id, folder); got != want {
		b.t.Errorf("%s on %s is %04o, want %04o", folder, id, got, want)
	}
	if got := b.text("SELECT group_concat(state) FROM ops WHERE kind = 'mkdir' AND disk_id = ? AND CAST(path AS TEXT) = ?", id, folder); got != "done" {
		b.t.Errorf("its operation: %q", got)
	}
	got := b.text(`SELECT json_extract(data, '$.mode') || ' ' || json_type(data, '$.uid') || ' ' || json_type(data, '$.gid')
		FROM events WHERE kind = 'mkdir' AND json_extract(data, '$.disk') = ? AND json_extract(data, '$.path') = ?`, id, folder)
	if want := strconv.FormatInt(want, 10) + " integer integer"; got != want {
		b.t.Errorf("its event: %q, want %q", got, want)
	}
}

// waited says that a copy to t3 waited before anything was made there: nothing journaled on t3, no folder, no part, and
// its job queued again, one attempt more.
func (b *bench) waited(items []ReplicateItem, folder string, attempts int) {
	b.t.Helper()
	if len(items) != 1 || items[0].Disk != "t3" || items[0].Outcome != trash.OutcomeIOError {
		b.t.Fatalf("%+v, want one copy to t3 that waits", items)
	}
	if got := b.count("SELECT count(*) FROM ops WHERE disk_id = 't3'"); got != 0 {
		b.t.Errorf("%d operations journaled on t3", got)
	}
	if b.exists("t3", "media/"+folder) {
		b.t.Error("the folder was made on t3")
	}
	if got := b.parts("t3"); len(got) != 0 {
		b.t.Errorf("parts on t3: %v", got)
	}
	if got := b.count("SELECT count(*) FROM jobs WHERE dst_disk = 't3' AND state = 'queued' AND attempts = ?", attempts); got != 1 {
		b.t.Errorf("%d jobs queued with %d attempts", got, attempts)
	}
}

// A copy whose source's disk holds no folder at its parent takes the model of that folder on another disk present: it was
// made 0755, more open than the folder kept 0700 there.
func TestACopysParentIsMadeLikeTheFolderOnAnotherDisk(t *testing.T) {
	b := modelPool(t)
	withUmask(t, 0o027)
	items, _ := b.replicate("p", ReplicateOptions{Now: true})
	if len(items) != 1 || items[0].Disk != "t3" || items[0].Outcome != OutcomeReplicated {
		t.Fatalf("%+v", items)
	}
	b.madeLike("t3", "p", 0o700)
}

// With the disk that holds the folder away, as the catalog says, the copy waits for it, as a folder made with no model
// would stay open for good; it waits before anything is copied or journaled, so that a disk away for days costs a look a
// run, not a copy. Back, the copy takes its model.
func TestACopyWaitsForTheDiskAwayThatHoldsItsParent(t *testing.T) {
	b := modelPool(t)
	b.gone[b.dirs["t2"]] = true
	items, _ := b.replicate("p", ReplicateOptions{Now: true})
	b.waited(items, "p", 1)
	// Still away: it waits again.
	items, _ = b.replicate("p", ReplicateOptions{Now: true})
	b.waited(items, "p", 2)
	b.gone[b.dirs["t2"]] = false
	items, _ = b.replicate("p", ReplicateOptions{Now: true})
	if len(items) != 1 || items[0].Disk != "t3" || items[0].Outcome != OutcomeReplicated {
		t.Fatalf("back: %+v", items)
	}
	b.madeLike("t3", "p", 0o700)
}

// A look at the folder on another disk that fails makes the copy wait, as nothing can be said of the mode it should have:
// a disk that fails so is lost, never the target; another error leaves it in service. Back to normal, the copy takes its
// model.
func TestACopyWaitsWhenALookAtAnotherDiskFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		lost bool
	}{{"EIO", syscall.EIO, true}, {"ENODEV", syscall.ENODEV, true}, {"EACCES", syscall.EACCES, false}} {
		t.Run(tc.name, func(t *testing.T) {
			b := modelPool(t)
			armed := true
			b.fail[b.dirs["t2"]] = func(call, name string) error {
				if armed && call == "stat" && name == "p" {
					return tc.err
				}
				return nil
			}
			items, _ := b.replicate("p", ReplicateOptions{Now: true})
			b.waited(items, "p", 1)
			if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got == "missing" {
				t.Error("the target is lost for an error of another disk")
			}
			if got := b.text("SELECT state FROM disks WHERE id = 't2'"); (got == "missing") != tc.lost {
				t.Errorf("the disk whose look failed is %s", got)
			}
			armed = false
			b.exec("UPDATE disks SET state = 'readonly' WHERE id = 't2'")
			items, _ = b.replicate("p", ReplicateOptions{Now: true})
			if len(items) != 1 || items[0].Outcome != OutcomeReplicated {
				t.Fatalf("back: %+v", items)
			}
			b.madeLike("t3", "p", 0o700)
		})
	}
}

// The model is looked for again once the copy is verified, under the lock of the target's names: a look that fails then
// leaves the part for the job to resume, the operation undone, and the disk that failed lost, never the target.
func TestACopyWaitsWhenALookAtAnotherDiskFailsLate(t *testing.T) {
	b := modelPool(t)
	late := false
	b.at["verified"] = func() { late = true }
	b.fail[b.dirs["t2"]] = func(call, name string) error {
		if late && call == "stat" && name == "p" {
			return syscall.EIO
		}
		return nil
	}
	items, _ := b.replicate("p", ReplicateOptions{Now: true})
	if len(items) != 1 || items[0].Outcome != trash.OutcomeIOError {
		t.Fatalf("%+v", items)
	}
	if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got == "missing" {
		t.Error("the target is lost for an error of another disk")
	}
	if got := b.text("SELECT state FROM disks WHERE id = 't2'"); got != "missing" {
		t.Errorf("the disk whose look failed is %s", got)
	}
	if got := b.text("SELECT group_concat(kind || ' ' || state) FROM ops WHERE disk_id = 't3'"); got != "create undone" {
		t.Errorf("on t3: %q", got)
	}
	if b.exists("t3", "media/p") {
		t.Error("the folder was made on t3")
	}
	if got := b.parts("t3"); len(got) != 1 {
		t.Errorf("parts on t3: %v, want the one the job resumes", got)
	}
	b.fail[b.dirs["t2"]] = nil
	b.exec("UPDATE disks SET state = 'readonly' WHERE id = 't2'")
	items, _ = b.replicate("p", ReplicateOptions{Now: true})
	if len(items) != 1 || items[0].Outcome != OutcomeReplicated {
		t.Fatalf("back: %+v", items)
	}
	b.madeLike("t3", "p", 0o700)
	if got := b.parts("t3"); len(got) != 0 {
		t.Errorf("parts left on t3: %v", got)
	}
}

// Among the disks present, the source's comes first, then those whose copy of the file is at its path, by position, then
// any other by position: the folder the file sits in, as §15.12 renames a copy behind into it, before one that only has
// its name.
func TestACopysModelIsTheFolderTheFileSitsIn(t *testing.T) {
	b := newBenchWith(t, "[defaults]\nreplicas = 2\n", "t1", "t2", "t3", "t4")
	b.write("t1", "p/a", "content")
	b.write("t3", "p/a", "content")
	b.folder("t2", "p", 0o750)
	b.folder("t3", "p", 0o700)
	b.ro[b.dirs["t2"]] = true
	b.scan(true)
	b.noModelOn("t1", "p")
	items, _ := b.replicate("p", ReplicateOptions{Now: true})
	if len(items) != 1 || items[0].Disk != "t4" || items[0].Source != "t1" || items[0].Outcome != OutcomeReplicated {
		t.Fatalf("%+v", items)
	}
	b.madeLike("t4", "p", 0o700)
}

// A disk present that the catalog says holds the folder, and that a look finds without it, does not hold it: the folder
// is made with no model, 0755 whatever the mask, as no disk holds it.
func TestACopyDoesNotWaitForADiskPresentWithoutTheFolder(t *testing.T) {
	b := modelPool(t)
	b.noModelOn("t2", "p")
	withUmask(t, 0o027)
	items, _ := b.replicate("p", ReplicateOptions{Now: true})
	if len(items) != 1 || items[0].Outcome != OutcomeReplicated {
		t.Fatalf("%+v", items)
	}
	b.madeOpen("t3", "p", 0o755)
}

// A disk failed is never read again, and a disk the configuration no longer lists is retired: neither is away, and
// neither makes a copy wait for good, its file one copy short for as long. The folder is made with no model.
func TestACopyDoesNotWaitForAFailedOrRetiredDisk(t *testing.T) {
	for _, tc := range []string{"failed", "retired"} {
		t.Run(tc, func(t *testing.T) {
			b := modelPool(t)
			switch tc {
			case "failed":
				b.base = strings.Replace(b.base, "id = \"t2\"\n", "id = \"t2\"\nstate = \"failed\"\n", 1)
			case "retired":
				start := strings.Index(b.base, "[[disk]]\nid = \"t2\"\n")
				end := strings.Index(b.base, "[[disk]]\nid = \"t3\"\n")
				b.base = b.base[:start] + b.base[end:]
			}
			b.reconfigure("")
			withUmask(t, 0o027)
			items, _ := b.replicate("p", ReplicateOptions{Now: true})
			if len(items) != 1 || items[0].Disk != "t3" || items[0].Outcome != OutcomeReplicated {
				t.Fatalf("%+v", items)
			}
			b.madeOpen("t3", "p", 0o755)
		})
	}
}

// The decision's own case, end to end: the user renames a file into a folder kept 0700 on one disk; the other copy sits
// behind at its old path, its rename held back; a third copy read from it waits while the disk that holds the folder is
// away, and takes that folder's mode once it is back.
func TestACopyFromACopyBehindTakesTheModelOfTheFolderItWasRenamedInto(t *testing.T) {
	b := newBench(t, "t1", "t2", "t3")
	b.write("t1", "a", "content")
	b.write("t2", "a", "content")
	b.scan(true)
	b.folder("t2", "P", 0o700)
	b.rename("t2", "a", "P/a")
	// t1 read-only: its copy cannot follow, and stays behind.
	b.ro[b.dirs["t1"]] = true
	b.settle()
	if got := b.text("SELECT CAST(f.lpath AS TEXT) || ' ' || COALESCE(CAST(c.old_lpath AS TEXT), '-') FROM copies c JOIN files f ON f.id = c.file_id WHERE c.disk_id = 't1'"); got != "P/a a" {
		t.Fatalf("t1's copy: %q, want behind", got)
	}
	b.ro[b.dirs["t1"]] = false
	b.gone[b.dirs["t2"]] = true
	b.reconfigure("[defaults]\nreplicas = 2\n")
	items, _ := b.replicate("P", ReplicateOptions{Now: true})
	if len(items) != 1 || items[0].Source != "t1" {
		t.Fatalf("%+v", items)
	}
	b.waited(items, "P", 1)
	b.gone[b.dirs["t2"]] = false
	items, _ = b.replicate("P", ReplicateOptions{Now: true})
	if len(items) != 1 || items[0].Disk != "t3" || items[0].Source != "t1" || items[0].Outcome != OutcomeReplicated {
		t.Fatalf("back: %+v", items)
	}
	b.madeLike("t3", "P", 0o700)
}
