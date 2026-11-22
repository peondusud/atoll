package watch

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/integrity"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/solver"
)

// anyDevice is the bench with every mount point a disk's own: its device the one disk's, whatever the mount.
type anyDevice struct {
	*bench
	uuid string
}

func (a *anyDevice) Device(names.Filesystem, *disks.MountEntry) disks.DeviceSeen {
	return disks.DeviceSeen{Device: disks.DeviceOK, Identity: &disks.Identity{ID: a.uuid}}
}

func (a *anyDevice) ReadPastCache(fs names.Filesystem, e *disks.MountEntry) disks.DeviceSeen {
	return a.Device(fs, e)
}

// The check once a walk is read knows the folder it walked by each of its marks alone: the mount of the mount point,
// the mount, the device and the inode of the root's path, against the folder the walk held open. Any one that differs
// says that another folder was walked, an interrupted walk, never a disk that lost its files (specs.md §10, invariant 4).
// Only the loop-device tests unmount a disk under a walk; this changes each mark alone.
func TestTheWalkKnowsItsRootByEachOfItsMarks(t *testing.T) {
	b := newBench(t, "t1")
	d := b.config.Disks[0]
	m := &anyDevice{bench: b, uuid: d.UUID}
	opened := heldOpen(t, filepath.Join(d.Mount, d.Root))
	if err := walkedTheDisk(m, &d, &opened); err != nil {
		t.Fatalf("the folder it walked: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(st *fsx.Stat)
	}{
		{"another mount", func(st *fsx.Stat) { st.MountID++ }},
		{"another device", func(st *fsx.Stat) { st.Dev++ }},
		{"another folder", func(st *fsx.Stat) { st.Ino++ }},
	} {
		held := opened
		tc.change(&held)
		if err := walkedTheDisk(m, &d, &held); !errors.Is(err, ErrInterrupted) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	// A root that is a mount of its own below its mount point's, as /dev/shm is below /dev where both are mounts: the
	// top's mount alone, then the root's alone, is not the one the walk held.
	top, errTop := fsx.StatPath("/dev")
	shmPath, errShm := fsx.StatPath("/dev/shm")
	if errTop != nil || errShm != nil || top.MountID == shmPath.MountID {
		t.Logf("no mount of its own at /dev/shm here: %v, %v", errTop, errShm)
		return
	}
	shm := heldOpen(t, "/dev/shm")
	below := d
	below.Mount, below.Root = "/dev", "shm"
	if err := walkedTheDisk(m, &below, &shm); !errors.Is(err, ErrInterrupted) {
		t.Errorf("the mount point on another mount than the folder walked: %v", err)
	}
	held := shm
	held.MountID = top.MountID
	if err := walkedTheDisk(m, &below, &held); !errors.Is(err, ErrInterrupted) {
		t.Errorf("the root on another mount than the folder walked: %v", err)
	}
}

// heldOpen is what a walk holding a folder open finds of it.
func heldOpen(t *testing.T, path string) fsx.Stat {
	t.Helper()
	dir, err := fsx.OpenDir(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	st, err := dir.Info()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// A scan asked for one disk walks that disk alone.
func TestAScanOfOneDiskWalksItAlone(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "a", "x")
	b.write("t2", "b", "y")
	out := b.scan(Options{Disk: "t1"})
	if len(out.Disks) != 1 || out.Disks[0].ID != "t1" || !out.Disks[0].Walked {
		t.Fatalf("%+v", out.Disks)
	}
	if b.count("SELECT count(*) FROM copies WHERE disk_id = 't2'") != 0 {
		t.Error("t2 was walked")
	}
}

// A walk takes the guard's links out of each file's count of names, a file with two names, its own and its link,
// included; a link that went between the listing of the links folder and the look at it is none, and the others are
// counted all the same (specs.md §17).
func TestAWalkLeavesOutTheGuardsLinks(t *testing.T) {
	linked := func(t *testing.T) *bench {
		t.Helper()
		b := newBench(t, "t1")
		b.write("t1", "a", "x")
		b.write("t1", "b", "y")
		links := filepath.Join(b.dirs["t1"], "Atoll", "links")
		if err := os.MkdirAll(links, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, file := range map[string]string{"7": "a", "8": "b"} {
			if err := os.Link(filepath.Join(b.dirs["t1"], "media", file), filepath.Join(links, name)); err != nil {
				t.Fatal(err)
			}
		}
		return b
	}
	b := linked(t)
	b.scan(Options{})
	if got := b.count("SELECT count(*) FROM copies WHERE nlink = 1"); got != 2 {
		t.Errorf("%d copies of one name", got)
	}
	b = linked(t)
	gone := ""
	b.wrap = func(_ string, d fsx.Dir) fsx.Dir {
		return fsx.Fault(d, func(call, name string) error {
			if call == "stat" && (name == "7" || name == "8") && (gone == "" || gone == name) {
				gone = name
				return syscall.ENOENT
			}
			return nil
		})
	}
	b.scan(Options{})
	if one, two := b.count("SELECT count(*) FROM copies WHERE nlink = 1"), b.count("SELECT count(*) FROM copies WHERE nlink = 2"); one != 1 || two != 1 {
		t.Errorf("with the link %s gone meanwhile, %d copies of one name and %d of two", gone, one, two)
	}
}

// A copy whose row lacks its mode, its account or its group, as a catalog from before them does, takes what a walk
// finds of it, each alone, while it is there as its row says (specs.md §9).
func TestARowLackingItsModeOrOwnerTakesThemFromAWalk(t *testing.T) {
	for _, column := range []string{"mode", "uid", "gid"} {
		b := newBench(t, "t1")
		b.write("t1", "a", "x")
		b.scan(Options{})
		if _, err := b.db.ExecContext(t.Context(), "UPDATE copies SET "+column+" = NULL"); err != nil {
			t.Fatal(err)
		}
		b.scan(Options{})
		if got := b.count("SELECT count(*) FROM copies WHERE mode IS NOT NULL AND uid IS NOT NULL AND gid IS NOT NULL"); got != 1 {
			t.Errorf("its %s left out: %d rows whole", column, got)
		}
	}
}

// A copy a look finds at another name says whether it kept its size and its date there, each of them: a rename alone
// keeps both, and one with the file's size or its date changed too keeps them no more as its row says, which a pass
// reads to tell a rename from a rename and an edit (specs.md §15.12).
func TestAMovedCopySaysWhetherItKeptItsDate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, full string)
		same   bool
	}{
		{"a rename alone", func(*testing.T, string) {}, true},
		{"its size changed, its date the same", func(t *testing.T, full string) {
			at := mtimeOf(t, full)
			if err := os.WriteFile(full, []byte("longer"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(full, at, at); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"its date changed, its size the same", func(t *testing.T, full string) {
			at := mtimeOf(t, full).Add(time.Hour)
			if err := os.Chtimes(full, at, at); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		b := newBench(t, "t1")
		b.write("t1", "a", "x")
		b.scan(Options{})
		to := filepath.Join(b.dirs["t1"], "media", "b")
		if err := os.Rename(filepath.Join(b.dirs["t1"], "media", "a"), to); err != nil {
			t.Fatal(err)
		}
		tc.change(t, to)
		d := &b.config.Disks[0]
		r, err := LookAtDisk(t.Context(), b.db, b.config, b, d, disks.Check(b, d), b.now, &Look{})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, seen := range r.Seen {
			if seen.Verdict == solver.VerdictMoved {
				found = true
				if seen.Target != "b" || seen.SameDate != tc.same {
					t.Errorf("%s: %+v", tc.name, seen)
				}
			}
		}
		if !found {
			t.Errorf("%s: no copy moved in %+v", tc.name, r.Seen)
		}
	}
}

func mtimeOf(t *testing.T, full string) time.Time {
	t.Helper()
	info, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}

// A source's root is the folder its first watch went on while its path leads to that folder, by its device and its
// inode, each alone: a root made again, or another put at its path, is another folder.
func TestASourceKnowsItsRootByDeviceAndInode(t *testing.T) {
	root := t.TempDir()
	st := heldOpen(t, root)
	if !sameRoot(root, st.Dev, st.Ino) {
		t.Fatal("the root itself")
	}
	if sameRoot(root, st.Dev+1, st.Ino) || sameRoot(root, st.Dev, st.Ino+1) || sameRoot(root+"/none", st.Dev, st.Ino) {
		t.Error("another device, another inode, or no folder, taken for the root")
	}
}

// A copy's clock says what became of it, and takes what a later look says: found at another name, then that name gone,
// it is gone, its clock started again, as until then its name was known (specs.md §15.12).
func TestACopysClockFollowsWhatTheLooksSay(t *testing.T) {
	b := newBenchWith(t, "[defaults]\nsettle = \"1h\"\n", "t1")
	b.write("t1", "a", "x")
	b.scan(Options{})
	if err := os.Rename(filepath.Join(b.dirs["t1"], "media", "a"), filepath.Join(b.dirs["t1"], "media", "b")); err != nil {
		t.Fatal(err)
	}
	b.scan(Options{})
	kind := func() string {
		t.Helper()
		var k sql.NullString
		if err := b.db.QueryRowContext(t.Context(), "SELECT c.gone_kind FROM copies c JOIN files f ON f.id = c.file_id WHERE f.lpath = CAST('a' AS BLOB)").Scan(&k); err != nil {
			t.Fatal(err)
		}
		return k.String
	}
	if got := kind(); got != "moved" {
		t.Fatalf("renamed: %q", got)
	}
	if err := os.Remove(filepath.Join(b.dirs["t1"], "media", "b")); err != nil {
		t.Fatal(err)
	}
	b.now += int64(time.Minute)
	b.scan(Options{})
	if got := kind(); got != "gone" {
		t.Errorf("its new name gone too: %q", got)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE gone_ns = ?", b.now); got != 1 {
		t.Errorf("the clock did not start again: %d copies gone since now", got)
	}
}

// A copy whose name now holds a folder, or whose folder is now a symbolic link, was replaced by something else, never
// deleted: the file changed, and what was there is kept (specs.md §15.12, invariant 10).
func TestACopyWhoseNameHoldsSomethingElseWasReplaced(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		change     func(t *testing.T, media string)
	}{
		{"a folder at its name", "a", func(t *testing.T, media string) {
			if err := os.Remove(filepath.Join(media, "a")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(media, "a"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"its folder a symbolic link", "d/x", func(t *testing.T, media string) {
			if err := os.RemoveAll(filepath.Join(media, "d")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("elsewhere", filepath.Join(media, "d")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		b := newBench(t, "t1")
		b.write("t1", tc.path, "x")
		b.scan(Options{})
		tc.change(t, filepath.Join(b.dirs["t1"], "media"))
		r := b.scan(Options{}).Disks[0]
		if !slices.Contains(r.Changed, tc.path) || slices.Contains(r.Absent, tc.path) {
			t.Errorf("%s: changed %q, absent %q", tc.name, r.Changed, r.Absent)
		}
	}
}

// A folder found at neither its name nor another is kept with what is below it only while a copy of the disk still
// sits below it, for a look that finds its new name to read the rename by it: an empty folder that went is nothing to
// wait for (specs.md §15.12).
func TestAnEmptyFolderThatWentIsNoneToWaitFor(t *testing.T) {
	b := newBench(t, "t1")
	b.write("t1", "f/x", "x")
	if err := os.Mkdir(filepath.Join(b.dirs["t1"], "media", "e"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.scan(Options{})
	if b.count("SELECT count(*) FROM entries WHERE lpath = CAST('e' AS BLOB) AND btime_ns IS NOT NULL") != 1 {
		t.Skip("no creation time of folders here")
	}
	if err := os.Remove(filepath.Join(b.dirs["t1"], "media", "e")); err != nil {
		t.Fatal(err)
	}
	if r := b.scan(Options{}).Disks[0]; slices.Contains(r.UnfoundFolders, "e") {
		t.Errorf("waits for %q", r.UnfoundFolders)
	}
}

// A scan that reads every copy reads none of a disk that is missing, and those of the disks after it all the same:
// nothing the missing one holds is said verified (invariant 4).
func TestAFullScanReadsNothingOfADiskMissing(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "a", "x")
	b.write("t2", "a", "x")
	b.scan(Options{Full: true})
	verified := func(disk string) int64 {
		t.Helper()
		var at sql.NullInt64
		if err := b.db.QueryRowContext(t.Context(), "SELECT verified_ns FROM copies WHERE disk_id = ?", disk).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at.Int64
	}
	before := verified("t1")
	b.now += int64(time.Hour)
	b.unread = map[string]bool{b.dirs["t1"]: true}
	b.scan(Options{Full: true})
	if verified("t1") != before || verified("t2") != b.now {
		t.Errorf("t1 verified at %d, before at %d; t2 at %d", verified("t1"), before, verified("t2"))
	}
}

// Two files that swapped names each moved to the other's: neither is gone nor replaced, as the file an overwriting
// move leaves nowhere is (specs.md §15.12).
func TestTwoFilesThatSwappedNamesEachMoved(t *testing.T) {
	b := newBench(t, "t1")
	b.write("t1", "a", "x")
	b.write("t1", "b", "yy")
	b.scan(Options{})
	pastTick()
	media := filepath.Join(b.dirs["t1"], "media")
	for _, mv := range [][2]string{{"a", "c"}, {"b", "a"}, {"c", "b"}} {
		if err := os.Rename(filepath.Join(media, mv[0]), filepath.Join(media, mv[1])); err != nil {
			t.Fatal(err)
		}
	}
	r := b.look()
	if len(r.Absent) != 0 || len(r.Changed) != 0 || !slices.Equal(r.Moved, []MovedCopy{{Path: "a", To: "b"}, {Path: "b", To: "a"}}) {
		t.Errorf("%+v", r)
	}
}

// A copy gone from its name that Atoll's own folders hold, the same file, is Atoll's own and never deleted; they are
// asked for the inodes of the copies gone alone, not of those that moved (specs.md §15.12, invariant 11).
func TestACopyGoneIntoAtollsOwnFoldersIsItsOwn(t *testing.T) {
	b := newBench(t, "t1")
	b.write("t1", "a", "x")
	b.write("t1", "b", "yy")
	b.scan(Options{})
	var key string
	var size, mtime int64
	if err := b.db.QueryRowContext(t.Context(), "SELECT c.file_key, c.size, c.mtime_ns FROM copies c JOIN files f ON f.id = c.file_id WHERE f.lpath = ?",
		[]byte("a")).Scan(&key, &size, &mtime); err != nil {
		t.Fatal(err)
	}
	id := func(path string) int64 { return int64(b.count("SELECT id FROM files WHERE lpath = ?", []byte(path))) }
	media := filepath.Join(b.dirs["t1"], "media")
	if err := os.Remove(filepath.Join(media, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(media, "b"), filepath.Join(media, "c")); err != nil {
		t.Fatal(err)
	}
	verdicts, err := disks.CheckAll(t.Context(), b, b.config.Disks)
	if err != nil {
		t.Fatal(err)
	}
	verdictOf := func(r *Report, path string) solver.CopyVerdict {
		for _, seen := range r.Seen {
			if seen.FileID == id(path) {
				return seen.Verdict
			}
		}
		return ""
	}
	var asked []map[string]bool
	look := &Look{Own: func(_ *config.Disk, inodes map[string]bool) (map[string][]solver.FileLook, error) {
		asked = append(asked, inodes)
		return map[string][]solver.FileLook{key: {{Inode: key, Size: size + 1, MtimeNS: mtime}, {Inode: key, Size: size, MtimeNS: mtime}}}, nil
	}}
	r, err := LookAtDisk(t.Context(), b.db, b.config, b, &b.config.Disks[0], verdicts[0], b.now, look)
	if err != nil || verdictOf(r, "a") != solver.VerdictOwn || verdictOf(r, "b") != solver.VerdictMoved || len(asked) != 1 || len(asked[0]) != 1 || !asked[0][key] {
		t.Errorf("%+v, %v; asked %v", r, err, asked)
	}
	r, err = LookAtDisk(t.Context(), b.db, b.config, b, &b.config.Disks[0], verdicts[0], b.now, &Look{})
	if err != nil || verdictOf(r, "a") != solver.VerdictGone {
		t.Errorf("a look that does not ask: %+v, %v", r, err)
	}
	failing := &Look{Own: func(*config.Disk, map[string]bool) (map[string][]solver.FileLook, error) { return nil, syscall.EIO }}
	if _, err := LookAtDisk(t.Context(), b.db, b.config, b, &b.config.Disks[0], verdicts[0], b.now, failing); !errors.Is(err, syscall.EIO) {
		t.Errorf("own folders that fail: %v", err)
	}
}

// A link the guard makes once the walk read its file leaves that file the one name the walk found, never fewer
// (specs.md §17).
func TestALinkTheGuardMakesMeanwhileLeavesAFileItsName(t *testing.T) {
	b := newBench(t, "t1")
	b.write("t1", "a", "x")
	b.write("t1", "b", "y")
	media, links := filepath.Join(b.dirs["t1"], "media"), filepath.Join(b.dirs["t1"], "Atoll", "links")
	if err := os.MkdirAll(links, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, file := range map[string]string{"7": "a", "8": "b"} {
		if err := os.Link(filepath.Join(media, file), filepath.Join(links, name)); err != nil {
			t.Fatal(err)
		}
	}
	first, made := "", false
	b.wrap = func(_ string, d fsx.Dir) fsx.Dir {
		return fsx.Fault(d, func(call, name string) error {
			if call != "stat" || (name != "a" && name != "b") {
				return nil
			}
			switch {
			case first == "":
				first = name
			case name != first && !made:
				made = true
				if err := os.Link(filepath.Join(media, first), filepath.Join(links, "9")); err != nil {
					t.Error(err)
				}
			}
			return nil
		})
	}
	b.scan(Options{})
	if got := b.count("SELECT count(*) FROM copies WHERE nlink = 1"); !made || got != 2 {
		t.Errorf("%d copies of one name, a link made %v", got, made)
	}
}

// deviceGone is the bench with the device of one disk gone away: its mount check finds it missing.
type deviceGone struct {
	*bench
	mount string
}

func (g *deviceGone) Device(fs names.Filesystem, entry *disks.MountEntry) disks.DeviceSeen {
	if entry.MountPoint == g.mount {
		return disks.DeviceSeen{Device: disks.DeviceGoneAway}
	}
	return g.bench.Device(fs, entry)
}

// rootRefused is the bench with the root of one disk that cannot be opened, though the disk passes its check.
type rootRefused struct {
	*bench
	mount string
}

func (r *rootRefused) OpenRoot(mount, root string) (fsx.Dir, error) {
	if mount == r.mount {
		return nil, syscall.EIO
	}
	return r.bench.OpenRoot(mount, root)
}

// HashNew reads nothing of a disk that is missing, nor of one whose root it cannot open, and the copies of the disks
// after it all the same; at its time, it leaves every file for later (specs.md §15.13, invariant 4).
func TestHashNewReadsOnlyWhatItCan(t *testing.T) {
	var b *bench
	verified := func(disk string) bool {
		t.Helper()
		return b.count("SELECT count(*) FROM copies WHERE disk_id = ? AND verified_ns IS NOT NULL", disk) == 1
	}
	hashed := func() int { return b.count("SELECT count(*) FROM files WHERE blake3 IS NOT NULL") }
	fresh := func() {
		b = newBench(t, "t1", "t2")
		b.write("t1", "a", "x")
		b.write("t2", "a", "x")
		b.scan(Options{})
	}
	fresh()
	if out, err := HashNew(t.Context(), b.db, b.config, b, b.now); err != nil || !out.Left || hashed() != 0 {
		t.Errorf("at its time: %+v, %v, %d hashed", out, err, hashed())
	}
	if out, err := HashNew(t.Context(), b.db, b.config, &deviceGone{bench: b, mount: b.dirs["t1"]}, b.now+int64(time.Hour)); err != nil || out.Left || hashed() != 1 || verified("t1") || !verified("t2") {
		t.Errorf("t1 missing: %+v, %v, %d hashed, t1 verified %v, t2 %v", out, err, hashed(), verified("t1"), verified("t2"))
	}
	fresh()
	m := &rootRefused{bench: b, mount: b.dirs["t1"]}
	if out, err := HashNew(t.Context(), b.db, b.config, m, b.now+int64(time.Hour)); err != nil || out.Left || hashed() != 1 || verified("t1") || !verified("t2") {
		t.Errorf("the root of t1 refused: %+v, %v, %d hashed, t1 verified %v, t2 %v", out, err, hashed(), verified("t1"), verified("t2"))
	}
}

// A second to the nanosecond is a second gone by: what waits is committed with the file read at that moment
// (specs.md §15.10).
func TestAShareEndsAtASecondToTheNanosecond(t *testing.T) {
	b := newBench(t, "t1")
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		b.write("t1", name, "x"+name)
	}
	// 500 ms a file: a, b and c are read within a second to the nanosecond, and committed with c; d waits.
	if got := b.killedAt("e", 500_000_000); got != "a,b,c" {
		t.Errorf("killed at e, the catalog holds the hashes of %q, want a, b and c", got)
	}
}

// A folder renamed keeps its row and those below it, and its new name and what is below it get none, folders as
// files, until the files below are decided (specs.md §15.12).
func TestTheFoldersBelowAFolderRenamedGetNoRowYet(t *testing.T) {
	b := newBench(t, "t1")
	b.write("t1", "d/sub/x", "x")
	b.scan(Options{})
	if b.count("SELECT count(*) FROM entries WHERE lpath = CAST('d' AS BLOB) AND btime_ns IS NOT NULL") != 1 {
		t.Skip("no creation time of folders here")
	}
	media := filepath.Join(b.dirs["t1"], "media")
	if err := os.Rename(filepath.Join(media, "d"), filepath.Join(media, "e")); err != nil {
		t.Fatal(err)
	}
	r := b.look()
	if !slices.Contains(r.FolderMoves, FolderMove{From: "d", To: "e"}) {
		t.Fatalf("folder moves %+v", r.FolderMoves)
	}
	for path, want := range map[string]int{"d": 1, "d/sub": 1, "e": 0, "e/sub": 0} {
		if got := b.count("SELECT count(*) FROM entries WHERE lpath = ?", []byte(path)); got != want {
			t.Errorf("%d rows of %s", got, path)
		}
	}
}

// atoll catalog verify compares every disk after one that failed, which it never reads again (specs.md §9, §18).
func TestACompareGoesOnPastAFailedDisk(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "a", "x")
	b.write("t2", "a", "x")
	b.scan(Options{})
	b.config.Disks[0].State = config.DiskFailed
	out, err := Compare(t.Context(), b.db, b.config, b)
	if err != nil || len(out.Disks) != 1 || out.Disks[0].ID != "t2" || !out.Disks[0].Walked {
		t.Errorf("%+v, %v", out, err)
	}
}

// A disk in standby takes a new file's replica, but not when the view may not write to it (specs.md §12.4, §21.2).
func TestAStandbyDiskTheViewCannotWriteIsNoTarget(t *testing.T) {
	b := newBench(t, "t1", "t2", "t3")
	b.write("t1", "show/e1.mkv", "one")
	b.scan(Options{})
	if _, err := b.db.ExecContext(t.Context(), "UPDATE disks SET state = 'standby' WHERE id IN ('t2', 't3')"); err != nil {
		t.Fatal(err)
	}
	free := func(string) (int64, bool) { return 0, false }
	for _, tc := range []struct {
		name     string
		writable func(string) bool
		want     []string
	}{
		{"writable", func(string) bool { return true }, []string{"t2"}},
		{"not writable", func(d string) bool { return d != "t2" }, []string{"t3"}},
	} {
		got, err := ParallelTargets(t.Context(), b.db, b.config, b.now, "show/e2.mkv", "t1", free, tc.writable)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, %v", tc.name, got, err)
		}
	}
}

// heldBelow counts the files this process holds open at or below a folder.
func heldBelow(t *testing.T, folder string) int {
	t.Helper()
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, fd := range fds {
		if target, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name())); err == nil && (target == folder || strings.HasPrefix(target, folder+"/")) {
			n++
		}
	}
	return n
}

// A look limited to names closes every folder it opened on the way to them, and keeps its root open for the next
// name, whichever comes first (specs.md §15.13).
func TestALookAtNamesLeavesNoFolderOpen(t *testing.T) {
	b := newBench(t, "t1")
	b.write("t1", "d/e/x", "x")
	b.write("t1", "y", "y")
	b.scan(Options{})
	verdicts, err := disks.CheckAll(t.Context(), b, b.config.Disks)
	if err != nil {
		t.Fatal(err)
	}
	for _, names := range [][]Named{{{Path: "d/e/x"}, {Path: "y"}}, {{Path: "y"}, {Path: "d/e/x"}}, {{Path: "z"}, {Path: "d/e/x"}}} {
		r, err := LookAtDisk(t.Context(), b.db, b.config, b, &b.config.Disks[0], verdicts[0], b.now, &Look{Names: names})
		if err != nil || len(r.Absent) != 0 || len(r.Unreadable) != 0 {
			t.Errorf("%v: %+v, %v", names, r, err)
		}
		if n := heldBelow(t, b.dirs["t1"]); n != 0 {
			t.Errorf("%v: %d files left open", names, n)
		}
	}
}

// A file of a chunk exactly shares the transaction of the files read with it: only one of more than a chunk has one of
// its own (specs.md §15.10).
func TestAFileOfAChunkSharesItsTransaction(t *testing.T) {
	b := newBench(t, "t1")
	b.write("t1", "a", "xa")
	b.write("t1", "b", strings.Repeat("x", integrity.ChunkSize))
	b.write("t1", "c", "xc")
	if got := b.killedAt("c", 0); got != "" {
		t.Errorf("killed at c, the catalog holds the hashes of %q, want none", got)
	}
}
