package ops

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
)

// The check before each operation knows the disk's top by its mount, its device and its folder, each alone: another
// mount at the disk's place, or the same mount with another filesystem below it, or another folder there, is the disk
// lost, whatever the other two say (specs.md §10, invariant 3).
func TestVerifyKnowsTheTopByEachOfItsMarks(t *testing.T) {
	mount := newMount(t)
	if err := openAt(t, mount, nil).Verify(); err != nil {
		t.Fatalf("the disk it opened: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(d *Disk)
	}{
		{"another mount", func(d *Disk) { d.mountID++ }},
		{"another device", func(d *Disk) { d.dev++ }},
		{"another folder", func(d *Disk) { d.ino++ }},
	} {
		d := openAt(t, mount, nil)
		tc.change(d)
		if err := d.Verify(); !errors.Is(err, ErrDiskLost) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

// refusing fails every call of a kind with EIO, and counts them.
type refusing struct {
	call  string
	calls int
}

func (r *refusing) fail(call, _ string) error {
	if call == r.call {
		r.calls++
		return syscall.EIO
	}
	return nil
}

// A folder that already has the mode Atoll gives it is not given it again: a chmod moves its change time, which a look
// compares, and asks the disk a write it does not need, which a disk that fails turns into an error.
func TestAFolderWithItsModeIsLeftAsItIs(t *testing.T) {
	t.Run("the folder of the parts, private already", func(t *testing.T) {
		mount := newMount(t)
		if err := os.MkdirAll(filepath.Join(mount, "Atoll", "tmp"), 0o700); err != nil {
			t.Fatal(err)
		}
		r := &refusing{call: "setmode"}
		part, err := openAt(t, mount, r.fail).MakePart(&Op{ID: 5, Disk: "t1", Kind: catalog.OpCreate, Path: "a", Path2: "5.part"})
		if err != nil || r.calls != 0 {
			t.Fatalf("%d chmods: %v", r.calls, err)
		}
		_ = part.Close()
	})
	t.Run("a folder below the root, made with its mode", func(t *testing.T) {
		old := syscall.Umask(0o022)
		defer syscall.Umask(old)
		mount := newMount(t)
		r := &refusing{call: "setmode"}
		if _, err := openAt(t, mount, r.fail).MakeRootFolder("New", nil); err != nil || r.calls != 0 {
			t.Fatalf("%d chmods: %v", r.calls, err)
		}
	})
	// The units' umask, 0027, leaves a new folder 0750: it is opened to FolderMode, for a media server under another
	// account (specs.md §17.4).
	t.Run("a folder below the root, made under the units' umask", func(t *testing.T) {
		old := syscall.Umask(0o027)
		defer syscall.Umask(old)
		mount := newMount(t)
		if _, err := openAt(t, mount, nil).MakeRootFolder("New", nil); err != nil {
			t.Fatal(err)
		}
		if got := statAt(t, mount, "media/New").Mode & 0o777; got != FolderMode {
			t.Errorf("the folder is %o", got)
		}
		// A disk that fails that chmod is the disk lost, never a folder left closed with nothing said.
		chmod := &refusing{call: "setmode"}
		if _, err := openAt(t, mount, chmod.fail).MakeRootFolder("Newer", nil); !errors.Is(err, ErrDiskLost) || chmod.calls != 1 {
			t.Errorf("%d chmods: %v", chmod.calls, err)
		}
	})
	// A folder the reprise finds where its operation made one, wider than the mode its event holds, takes that mode
	// exactly, its other bits as they were (specs.md §17.4).
	t.Run("a folder the reprise finds open to all", func(t *testing.T) {
		p := newPool(t, nil)
		op, err := catalog.BeginMkdir(t.Context(), p.db, "t1", "New", LikeOf(nil), repriseNow)
		if err != nil {
			t.Fatal(err)
		}
		full := filepath.Join(p.mount, "media", "New")
		if err := os.Mkdir(full, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Chmod(full, 0o1777); err != nil {
			t.Fatal(err)
		}
		if _, err := p.recover(); err != nil || p.opState(op.ID) != "done" {
			t.Fatalf("%v, the operation %s", err, p.opState(op.ID))
		}
		if got := statAt(t, p.mount, "media/New").Mode; got&0o777 != FolderMode || got&0o1000 == 0 {
			t.Errorf("the folder is %o", got)
		}
	})
}

// A source looked at again is the file read only when each of its inode, size, modification time and change time is
// what the read found: any one that differs alone says it changed (specs.md §14.2).
func TestASourceLookedAtAgainKnowsEachChange(t *testing.T) {
	mount := newMount(t)
	put(t, mount, "media/Films/a", "film")
	put(t, mount, "Atoll/trash/2026-01-01/7/b", "old")
	d := openAt(t, mount, nil)
	for _, tc := range []struct {
		name   string
		change func(st *fsx.Stat)
		still  bool
	}{
		{"nothing", func(*fsx.Stat) {}, true},
		{"the inode", func(st *fsx.Stat) { st.Ino++ }, false},
		{"the size", func(st *fsx.Stat) { st.Size++ }, false},
		{"the modification time", func(st *fsx.Stat) { st.Mtime.Nsec++ }, false},
		{"the change time", func(st *fsx.Stat) { st.Ctime.Nsec++ }, false},
	} {
		read := statAt(t, mount, "media/Films/a")
		tc.change(&read)
		if still, err := d.SourceStill("Films/a", &read); err != nil || still != tc.still {
			t.Errorf("a copy's source, %s changed: %v, %v", tc.name, still, err)
		}
		read = statAt(t, mount, "Atoll/trash/2026-01-01/7/b")
		tc.change(&read)
		if still, err := d.TrashSourceStill("2026-01-01/7/b", &read); err != nil || still != tc.still {
			t.Errorf("an entry of the trash, %s changed: %v, %v", tc.name, still, err)
		}
	}
}

// Each folder a move to the trash makes on the way stands for a folder of the copy's path, and takes its mode: the
// operation's own folder the root's, then each parent of the copy its own, so that a private folder stays private in
// the trash (specs.md §17.1).
func TestTheFoldersOfTheTrashAreLikeThoseOfTheCopy(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	mount := newMount(t)
	put(t, mount, "media/Films/Sub/a", "film")
	modes := map[string]os.FileMode{"media": 0o751, "media/Films": 0o700, "media/Films/Sub": 0o710}
	for rel, mode := range modes {
		if err := os.Chmod(filepath.Join(mount, filepath.FromSlash(rel)), mode); err != nil {
			t.Fatal(err)
		}
	}
	row := rowAt(t, mount, "Films/Sub/a")
	op := &Op{ID: 7, Disk: "t1", Kind: catalog.OpTrash, Path: "Films/Sub/a", Path2: "2026-01-01/7/Films/Sub/a", FileKey: row.FileKey.String}
	if _, err := openAt(t, mount, nil).ToTrash(op, row); err != nil {
		t.Fatal(err)
	}
	for trashed, like := range map[string]string{
		"Atoll/trash/2026-01-01/7": "media", "Atoll/trash/2026-01-01/7/Films": "media/Films", "Atoll/trash/2026-01-01/7/Films/Sub": "media/Films/Sub",
	} {
		if got := statAt(t, mount, trashed).Mode & 0o777; got != uint32(modes[like]) {
			t.Errorf("%s is %o, where %s is %o", trashed, got, like, modes[like])
		}
	}
}

// failingOn fails every call of a kind on one name with EIO, and counts them.
type failingOn struct {
	call, name string
	calls      int
}

func (f *failingOn) fail(call, name string) error {
	if call == f.call && name == f.name {
		f.calls++
		return syscall.EIO
	}
	return nil
}

// A part placed at its name gives back what recording it gave, and only once it is recorded goes on: the file synced,
// its folder synced, and given back its time (specs.md §14.2).
func TestAPartPlacedIsRecordedThenDurable(t *testing.T) {
	setUp := func(t *testing.T) (string, *Op, fsx.Time) {
		t.Helper()
		mount := newMount(t)
		put(t, mount, "Atoll/tmp/5.part", "film")
		put(t, mount, "media/Films/other", "x")
		at := time.Unix(1000, 0)
		if err := os.Chtimes(filepath.Join(mount, "media", "Films"), at, at); err != nil {
			t.Fatal(err)
		}
		op := &Op{ID: 5, Disk: "t1", Kind: catalog.OpCreate, Path: "Films/a", Path2: "5.part", FileKey: keyAt(t, mount, "Atoll/tmp/5.part")}
		return mount, op, statAt(t, mount, "media/Films").Mtime
	}
	t.Run("recorded", func(t *testing.T) {
		mount, op, before := setUp(t)
		if _, err := openAt(t, mount, nil).PlacePart(op, "Films/a", func(*fsx.Stat) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if got := statAt(t, mount, "media/Films").Mtime; got != before {
			t.Errorf("the folder's time went from %v to %v", before, got)
		}
	})
	t.Run("not recorded", func(t *testing.T) {
		mount, op, _ := setUp(t)
		stop := errors.New("the catalog cannot record it")
		syncs := &refusing{call: "syncentry"}
		if _, err := openAt(t, mount, syncs.fail).PlacePart(op, "Films/a", func(*fsx.Stat) error { return stop }); !errors.Is(err, stop) {
			t.Errorf("what recording gave is lost: %v", err)
		}
		if syncs.calls != 0 {
			t.Error("it went on once the part could not be recorded")
		}
	})
}

// What removes Atoll's parts makes it durable, and says when it cannot: a sync of their folder that fails is the disk
// lost (specs.md §14.2, §17.1).
func TestThePartsRemovedAreDurable(t *testing.T) {
	t.Run("one part dropped", func(t *testing.T) {
		mount := newMount(t)
		put(t, mount, "Atoll/tmp/6.part", "half")
		sync := &failingOn{call: "sync", name: "tmp"}
		if err := openAt(t, mount, sync.fail).DropPart("6.part"); !errors.Is(err, ErrDiskLost) || sync.calls != 1 {
			t.Errorf("%d syncs: %v", sync.calls, err)
		}
	})
	t.Run("the parts cleaned", func(t *testing.T) {
		mount := newMount(t)
		for _, name := range []string{"3.part", "7.part", "9.part"} {
			put(t, mount, "Atoll/tmp/"+name, name)
		}
		sync := &failingOn{call: "sync", name: "tmp"}
		err := openAt(t, mount, sync.fail).CleanTmp(map[string]string{"3.part": "done", "7.part": "undone", "9.part": "planned"}, nil)
		if !errors.Is(err, ErrDiskLost) || sync.calls != 1 {
			t.Errorf("%d syncs: %v", sync.calls, err)
		}
		if !absent(mount, "Atoll/tmp/3.part") || !absent(mount, "Atoll/tmp/7.part") || !holding(mount, "Atoll/tmp/9.part", "9.part") {
			t.Error("not every part an operation done or undone names is gone, or one left planned went")
		}
	})
}

// A change time is given as the catalog holds it, and one a look cannot give as nothing valid.
func TestTimeOf(t *testing.T) {
	if got := TimeOf(fsx.Time{Sec: 5, Nsec: 7}); !got.Valid || got.Int64 != 5*int64(time.Second)+7 {
		t.Errorf("a time: %+v", got)
	}
	if got := TimeOf(fsx.Time{Sec: 5, Nsec: 1000000000}); got.Valid {
		t.Errorf("nanoseconds past a second: %+v", got)
	}
}

// A copy edited in place since its row was written is not what the row says: the look right before a move to the
// trash finds it, and nothing moves (specs.md §17.1, step 4).
func TestACopyEditedSinceItsRowStaysWhereItIs(t *testing.T) {
	mount := newMount(t)
	put(t, mount, "media/Films/a", "film")
	row := rowAt(t, mount, "Films/a")
	writeInPlace(t, filepath.Join(mount, "media", "Films", "a"), "another film")
	op := &Op{ID: 7, Disk: "t1", Kind: catalog.OpTrash, Path: "Films/a", Path2: "2026-01-01/7/Films/a", FileKey: row.FileKey.String}
	if _, err := openAt(t, mount, nil).ToTrash(op, row); !errors.Is(err, ErrGone) {
		t.Errorf("an edited copy was taken for what its row says: %v", err)
	}
	if !holding(mount, "media/Films/a", "another film") || !absent(mount, "Atoll/trash/2026-01-01/7/Films/a") {
		t.Error("the edited copy moved")
	}
}

// shortNames answers for the short name of each entry it is told of, in capitals, as every NTFS driver does, and lists
// none (docs/research/ntfs-driver.md).
type shortNames struct {
	fsx.Dir
	alias map[string]string
}

func (s *shortNames) Unwrap() fsx.Dir { return s.Dir }

func (s *shortNames) Stat(name string) (fsx.Stat, error) {
	if long, ok := s.alias[name]; ok {
		name = long
	}
	return s.Dir.Stat(name) //nolint:wrapcheck // a test double
}

func (s *shortNames) OpenDir(name string) (fsx.Dir, error) {
	d, err := s.Dir.OpenDir(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	return &shortNames{Dir: d, alias: s.alias}, nil
}

type shortNamesMachine struct {
	onDisk
	alias map[string]string
}

func (m *shortNamesMachine) OpenTop(mount string) (fsx.Dir, error) {
	top, err := m.onDisk.OpenTop(mount)
	if err != nil {
		return nil, err
	}
	return &shortNames{Dir: top, alias: m.alias}, nil
}

// On NTFS, a name of the short form that the folder does not list, and that a look finds, at that name or in capitals,
// is the short name of another entry, which a copy is never made at; a name of that form the folder lists is an entry
// of its own. Only a name of that form is looked for, as reading a folder costs: no other name, and no name on another
// type, has the folder read (specs.md §15.8).
func TestTheShortNameOfAnotherEntryIsRefused(t *testing.T) {
	mount := newMount(t)
	put(t, mount, "media/Films/Long name.mkv", "film")
	put(t, mount, "media/Films/AB~1", "an entry of its own")
	alias := map[string]string{"LONGNA~1.MKV": "Long name.mkv"}
	open := func(t *testing.T, fs names.Filesystem, fail func(call, name string) error) *Disk {
		t.Helper()
		d, err := Open(&shortNamesMachine{onDisk: onDisk{fail: fail, driver: "ntfs3"}, alias: alias},
			&config.Disk{ID: "t1", Mount: mount, Root: "media", Workdir: "Atoll", FS: fs}, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(d.Close)
		return d
	}
	ntfs := open(t, names.NTFS, nil)
	for _, tc := range []struct{ path, want string }{
		{"Films/LONGNA~1.MKV", TargetRefused},
		{"Films/longna~1.mkv", TargetRefused},
		{"Films/AB~1", TargetTaken},
		{"Films/CD~1", TargetFree},
	} {
		if got, _, err := ntfs.LookAtTarget(tc.path); err != nil || got != tc.want {
			t.Errorf("%s: %q, %v", tc.path, got, err)
		}
	}
	unread := &refusing{call: "names"}
	for _, tc := range []struct {
		name string
		fs   names.Filesystem
		path string
	}{{"a long name on NTFS", names.NTFS, "Films/Long name.mkv"}, {"a short one on ext4", names.Ext4, "Films/LONGNA~1.MKV"}} {
		if _, _, err := open(t, tc.fs, unread.fail).LookAtTarget(tc.path); err != nil || unread.calls != 0 {
			t.Errorf("%s: the folder read %d times: %v", tc.name, unread.calls, err)
		}
	}
}

// The folders of a place of the trash left empty go, down to its date, those further down never made skipped; the
// trash itself stays, and so does a folder that holds something (specs.md §17.1).
func TestCleanTrashGoesDownToTheDate(t *testing.T) {
	mount := newMount(t)
	for _, folder := range []string{"Atoll/trash/2026-01-01/7", "Atoll/trash/2026-01-02/8/Films"} {
		if err := os.MkdirAll(filepath.Join(mount, filepath.FromSlash(folder)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	put(t, mount, "Atoll/trash/2026-01-02/9/b", "an entry")
	d := openAt(t, mount, nil)
	d.CleanTrash("2026-01-01/7/Films/Sub/a")
	d.CleanTrash("2026-01-02/8/Films/a")
	if !absent(mount, "Atoll/trash/2026-01-01") || !absent(mount, "Atoll/trash/2026-01-02/8") || absent(mount, "Atoll/trash") ||
		!holding(mount, "Atoll/trash/2026-01-02/9/b", "an entry") {
		t.Error("the trash is not as it should be")
	}
	// Its only date gone, the trash is left, empty.
	alone := newMount(t)
	if err := os.MkdirAll(filepath.Join(alone, "Atoll", "trash", "2026-01-03", "7"), 0o700); err != nil {
		t.Fatal(err)
	}
	openAt(t, alone, nil).CleanTrash("2026-01-03/7/a")
	if !absent(alone, "Atoll/trash/2026-01-03") || absent(alone, "Atoll/trash") {
		t.Error("the trash went, or its date stays")
	}
}
