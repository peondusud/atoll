package jobs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// linkedPool gives a pool of two disks with Films/a.mkv on both, read whole, and a view over it that has linked it to
// Films/b.mkv on the disk it presents it from, which it gives with the other disk. Copies wait for no settle, and the
// clock is the real one, as a link moves the change time of what it links, which the disk gives.
func linkedPool(t *testing.T, more string) (b *bench, disk, other string) {
	t.Helper()
	b = newBenchWith(t, "[defaults]\nsettle = \"0s\"\n"+more, "t1", "t2")
	b.now = time.Now().UnixNano()
	b.write("t1", "Films/a.mkv", "a film")
	b.write("t2", "Films/a.mkv", "a film")
	b.scan(true)
	n, err := b.viewOf().Link(t.Context(), "Films/a.mkv", "Films/b.mkv")
	if err != nil {
		t.Fatal(err)
	}
	disk, other = n.Disk, map[string]string{"t1": "t2", "t2": "t1"}[n.Disk]
	if b.inode(disk, "Films/b.mkv") != b.inode(disk, "Films/a.mkv") || b.exists(other, "media/Films/b.mkv") {
		t.Fatalf("the link is not on %s alone", disk)
	}
	return b, disk, other
}

// A hard link through Atoll's own view (specs.md §14, §15.1, §21.2): the pass records the new name as a file of its
// own and replicates it as any other, an independent full copy on the other disk, never a link; then it takes the
// view's operation in, and a pass after it has nothing to do. A deletion of the name it was made from leaves the new
// name, its data and its replica.
func TestAPassTakesInAHardLinkThroughTheView(t *testing.T) {
	b, disk, other := linkedPool(t, "")
	b.settle()
	if got := b.holders("Films/b.mkv"); got != map[string]string{"t1": "t1:user:ok t2:atoll:ok", "t2": "t1:atoll:ok t2:user:ok"}[disk] {
		t.Errorf("the new name's copies: %s", got)
	}
	if b.inode(other, "Films/b.mkv") == b.inode(other, "Films/a.mkv") || b.read(other, "Films/b.mkv") != "a film" {
		t.Errorf("the replica of the new name is no independent copy on %s", other)
	}
	if b.inode(disk, "Films/b.mkv") != b.inode(disk, "Films/a.mkv") {
		t.Errorf("the link on %s was broken", disk)
	}
	if open := b.openViewOps(); len(open) != 0 {
		t.Errorf("not taken in: %+v", open)
	}
	b.now += 60_000_000_000
	if again := b.pass(); len(again.Operations) != 0 || len(again.Decisions) != 0 {
		t.Errorf("a pass after: %+v, %+v", again.Decisions, again.Operations)
	}

	if err := b.viewOf().Unlink(t.Context(), "Films/a.mkv"); err != nil {
		t.Fatal(err)
	}
	b.settle()
	for _, id := range []string{"t1", "t2"} {
		if b.exists(id, "media/Films/a.mkv") || b.read(id, "Films/b.mkv") != "a film" {
			t.Errorf("%s: the name deleted is there, or the other lost its data", id)
		}
	}
	if got := b.count("SELECT count(*) FROM trash WHERE reason = 'deleted' AND lpath = CAST('Films/a.mkv' AS BLOB)"); got != 2 {
		t.Errorf("%d copies of the name deleted in the trash, want 2", got)
	}
	if got := b.holders("Films/b.mkv"); got != map[string]string{"t1": "t1:user:ok t2:atoll:ok", "t2": "t1:atoll:ok t2:user:ok"}[disk] {
		t.Errorf("the new name's copies after the deletion: %s", got)
	}
}

// inodeAt gives the inode of a path, 0 when nothing is there.
func (b *bench) inodeAt(path string) uint64 {
	b.t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		return 0
	}
	st, _ := info.Sys().(*syscall.Stat_t)
	return st.Ino
}

// links gives the inode each link of the guard on a disk leads to, by the id of its file.
func (b *bench) links(id string) map[string]uint64 {
	b.t.Helper()
	out := map[string]uint64{}
	folder := filepath.Join(b.dirs[id], "Atoll", "links")
	entries, err := os.ReadDir(folder)
	if err != nil {
		return out
	}
	for _, e := range entries {
		out[e.Name()] = b.inodeAt(filepath.Join(folder, e.Name()))
	}
	return out
}

// The link guard with a file of two names on a disk (specs.md §17): each name, a file of its own, has its link there,
// both leading to the one file, and none counts among a copy's names; once the name it was made from is deleted through
// the view, its link goes, and the other name keeps its link and its data.
func TestTheLinkGuardWithAFileOfTwoNames(t *testing.T) {
	b, disk, other := linkedPool(t, "[trash]\nlink_guard = true\n")
	b.settle()
	idA := b.text("SELECT CAST(id AS TEXT) FROM files WHERE lpath = CAST('Films/a.mkv' AS BLOB)")
	idB := b.text("SELECT CAST(id AS TEXT) FROM files WHERE lpath = CAST('Films/b.mkv' AS BLOB)")
	if got := b.links(disk); len(got) != 2 || got[idA] != b.inode(disk, "Films/a.mkv") || got[idB] != b.inode(disk, "Films/a.mkv") {
		t.Errorf("the links on %s: %v", disk, got)
	}
	if got := b.links(other); len(got) != 2 || got[idA] != b.inode(other, "Films/a.mkv") || got[idB] != b.inode(other, "Films/b.mkv") {
		t.Errorf("the links on %s: %v", other, got)
	}
	if got := b.text("SELECT group_concat(nlink, ' ') FROM (SELECT c.nlink FROM copies c JOIN files f ON f.id = c.file_id WHERE c.disk_id = ? ORDER BY f.lpath)", disk); got != "2 2" {
		t.Errorf("the names of the copies on %s: %s, want both names and not the guard's", disk, got)
	}
	if err := b.viewOf().Unlink(t.Context(), "Films/a.mkv"); err != nil {
		t.Fatal(err)
	}
	b.settle()
	for _, id := range []string{"t1", "t2"} {
		got := b.links(id)
		if _, ok := got[idA]; ok || got[idB] != b.inode(id, "Films/b.mkv") || b.read(id, "Films/b.mkv") != "a film" {
			t.Errorf("%s after the deletion: links %v", id, got)
		}
	}
	if got := b.count("SELECT count(*) FROM trash"); got != 2 {
		t.Errorf("%d entries in the trash, want the two copies of the name deleted", got)
	}
}

// A file of two names on the SSD cache (specs.md §19): each name leaves it as a file of its own, with its copies on the
// data disks, independent of each other, and the cache then holds neither name.
func TestAFileOfTwoNamesLeavesTheCache(t *testing.T) {
	b := cacheBench(t, "[cache]\nenabled = true\nmin_age = \"0s\"\n"+windowOpen)
	b.write("c", "show/e1.mkv", "episode")
	b.scan(false)
	n, err := b.viewOf().Link(t.Context(), "show/e1.mkv", "show/e1-again.mkv")
	if err != nil || n.Disk != "c" {
		t.Fatalf("the link: %+v, %v", n, err)
	}
	for range 4 {
		b.pass()
		b.now += 60_000_000_000
	}
	for _, p := range []string{"show/e1.mkv", "show/e1-again.mkv"} {
		if b.exists("c", "media/"+p) {
			t.Errorf("the cache still holds %s: %s", p, b.holders(p))
		}
		for _, id := range []string{"t1", "t2"} {
			if b.read(id, p) != "episode" {
				t.Errorf("%s: %s holds %q", id, p, b.read(id, p))
			}
		}
	}
	for _, id := range []string{"t1", "t2"} {
		if b.inode(id, "show/e1.mkv") == b.inode(id, "show/e1-again.mkv") {
			t.Errorf("%s: the two names are linked there", id)
		}
	}
	if got := b.count("SELECT count(*) FROM trash"); got != 0 {
		t.Errorf("%d entries left in the trash", got)
	}
	if open := b.openViewOps(); len(open) != 0 {
		t.Errorf("not taken in: %+v", open)
	}
}
