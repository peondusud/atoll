package watch

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
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
