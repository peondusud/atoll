package ops

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/watch"
)

// overMount is a machine whose disk shows another mount at every folder of one name, once over says so: one mounted over
// the links folder, which hides what that folder holds.
type overMount struct {
	*onDisk
	over string
}

func (m *overMount) OpenTop(mount string) (fsx.Dir, error) {
	top, err := m.onDisk.OpenTop(mount)
	if err != nil {
		return nil, err
	}
	return &mountedOver{Dir: top, m: m}, nil
}

type mountedOver struct {
	fsx.Dir
	m *overMount
}

func (d *mountedOver) Unwrap() fsx.Dir { return d.Dir }

func (d *mountedOver) OpenDir(name string) (fsx.Dir, error) {
	below, err := d.Dir.OpenDir(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	if name == d.m.over {
		return &anotherMount{Dir: below}, nil
	}
	return &mountedOver{Dir: below, m: d.m}, nil
}

type anotherMount struct{ fsx.Dir }

func (a *anotherMount) Unwrap() fsx.Dir { return a.Dir }

func (a *anotherMount) Info() (fsx.Stat, error) {
	st, err := a.Dir.Info()
	st.MountID++
	return st, err //nolint:wrapcheck // a test double
}

// linkedDisk is a disk whose file a has three names: its own, the guard's link links/1, and an entry of the trash, as a
// copy Atoll sent there before a purge; b has its own name alone. Without the links folder, a has two.
type linkedDisk struct {
	mount string
	m     *overMount
	d     *Disk
	key   string
	row   catalog.KnownCopy
}

const (
	linkedEntry = "2026-01-01/2/a"
	movedEntry  = "2026-01-01/3/a"
)

func newLinkedDisk(t *testing.T, withLinks bool) *linkedDisk {
	t.Helper()
	mount := t.TempDir()
	media, entry := filepath.Join(mount, "media"), filepath.Join(mount, "Atoll", "trash", filepath.FromSlash(linkedEntry))
	for _, dir := range []string{media, filepath.Dir(entry)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(media, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(filepath.Join(media, "a"), entry); err != nil {
		t.Fatal(err)
	}
	if withLinks {
		if err := os.Mkdir(filepath.Join(mount, "Atoll", "links"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(media, "a"), filepath.Join(mount, "Atoll", "links", "1")); err != nil {
			t.Fatal(err)
		}
	}
	dir, err := fsx.OpenDir(media)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	st, err := dir.Stat("a")
	if err != nil {
		t.Fatal(err)
	}
	found, err := watch.FileFrom("a", &st)
	if err != nil {
		t.Fatal(err)
	}
	m := &overMount{onDisk: &onDisk{}}
	d, err := Open(m, &config.Disk{ID: "t1", Mount: mount, Root: "media", Workdir: "Atoll"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return &linkedDisk{mount: mount, m: m, d: d, key: found.FileKey, row: catalog.KnownCopy{
		Path: "a", Size: sql.NullInt64{Int64: found.Size, Valid: true}, MtimeNS: sql.NullInt64{Int64: found.MtimeNS, Valid: true},
		CtimeNS: sql.NullInt64{Int64: found.CtimeNS, Valid: true}, FileKey: sql.NullString{String: found.FileKey, Valid: true},
	}}
}

func (l *linkedDisk) there(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(filepath.Join(l.mount, filepath.FromSlash(path)))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

// A links folder that cannot be read is never one with no link: the guard's links are taken out of every count of a
// file's names (specs.md §17), and one left in would make a purge go on while the purged data stays behind its link,
// which the next pass sends back to the trash. A disk's error there is the disk's, lost, the operation left for later
// (invariant 4); any other says nothing, so nothing is purged nor linked on it, and a move that went through is left to
// be resolved as a reprise would, never undone as one refused before anything moved. Nothing of a failed read is kept:
// the next look reads the folder again. An absent folder holds no link.
func TestALinksFolderItCannotReadIsNoFolderWithNoLink(t *testing.T) {
	names := func(n uint32) string { return fmt.Sprintf("%d names", n) }
	calls := []struct {
		name string
		// links says that the call needs the links folder open, which it is before the folder fails.
		links bool
		// do makes the call, and gives what it counts of a's names, or of the links that lead to it.
		do func(t *testing.T, l *linkedDisk, opened *LinksFolder) (string, error)
		// want is what it gives once the folder reads, with the folder and without it.
		want, absent string
		// untouched is what a call that fails leaves there.
		untouched []string
	}{
		{name: "a look", want: names(2), absent: names(2), do: func(t *testing.T, l *linkedDisk, _ *LinksFolder) (string, error) {
			t.Helper()
			st, _, err := l.d.StatRoot("a")
			return names(st.Nlink), err
		}},
		{name: "the links of an entry of the trash", want: "[1]", absent: "[]", do: func(t *testing.T, l *linkedDisk, _ *LinksFolder) (string, error) {
			t.Helper()
			ids, err := l.d.OwnLinksOf(linkedEntry, l.key)
			return fmt.Sprint(ids), err
		}},
		{name: "a purge", want: names(2), absent: names(2), untouched: []string{"Atoll/trash/" + linkedEntry},
			do: func(t *testing.T, l *linkedDisk, _ *LinksFolder) (string, error) {
				t.Helper()
				purged, err := l.d.PurgeEntry(&Op{ID: 2, Disk: "t1", Kind: catalog.OpUnlink, Path: "a", Path2: linkedEntry, FileKey: l.key})
				if err == nil && l.there(t, "Atoll/trash/"+linkedEntry) {
					t.Error("the entry stays")
				}
				return names(purged.Nlink), err
			}},
		{name: "a move to the trash", want: names(2), absent: names(2), do: func(t *testing.T, l *linkedDisk, _ *LinksFolder) (string, error) {
			t.Helper()
			if !l.there(t, "media/a") {
				// A move that went through before what failed: a look at where it went counts what a reprise would.
				st, _, err := l.d.stat(l.d.trashPlace(movedEntry))
				return names(st.Nlink), err
			}
			moved, err := l.d.ToTrash(&Op{ID: 3, Disk: "t1", Kind: catalog.OpTrash, Path: "a", Path2: movedEntry, FileKey: l.key}, &l.row)
			if err != nil {
				return "", err
			}
			return names(moved.Stat.Nlink), nil
		}},
		{name: "a link", links: true, want: "made", absent: "made", untouched: []string{"media/b"},
			do: func(t *testing.T, l *linkedDisk, opened *LinksFolder) (string, error) {
				t.Helper()
				op := &Op{ID: 4, Disk: "t1", Kind: catalog.OpLink, Path: "b", FileKey: inodeKey(t, filepath.Join(l.mount, "media", "b")), Link: 2}
				if _, err := l.d.MakeLink(op, opened); err != nil {
					return "", err
				}
				if !l.there(t, "Atoll/links/2") {
					return "none made", nil
				}
				return "made", nil
			}},
	}
	for _, tc := range []struct {
		name string
		fail func(m *overMount)
		lost bool
	}{
		{"read on a disk that fails", func(m *overMount) { m.fail = failing("names", "links", syscall.EIO) }, true},
		{"opened on a disk gone", func(m *overMount) { m.fail = failing("opendir", "links", syscall.ENOTCONN) }, true},
		{"read with no right to", func(m *overMount) { m.fail = failing("names", "links", syscall.EACCES) }, false},
		{"a link in it that cannot be looked at", func(m *overMount) { m.fail = failing("stat", "1", syscall.EACCES) }, false},
		{"opened with no file left to open", func(m *overMount) { m.fail = failing("opendir", "links", syscall.EMFILE) }, false},
		{"another mount over it", func(m *overMount) { m.over = "links" }, false},
	} {
		for _, c := range calls {
			t.Run(tc.name+"/"+c.name, func(t *testing.T) {
				l := newLinkedDisk(t, true)
				var opened *LinksFolder
				if c.links {
					var err error
					if opened, err = l.d.OpenLinks(false); err != nil || opened == nil {
						t.Fatalf("the links folder: %v", err)
					}
					defer opened.Close()
				}
				tc.fail(l.m)
				got, err := c.do(t, l, opened)
				switch {
				case tc.lost && !errors.Is(err, ErrDiskLost):
					t.Errorf("not the disk lost: %s, %v", got, err)
				case !tc.lost && (err == nil || errors.Is(err, ErrDiskLost)):
					t.Errorf("not a doubt: %s, %v", got, err)
				case errors.Is(err, ErrOtherMount):
					t.Errorf("read as refused before anything moved: %v", err)
				}
				for _, path := range c.untouched {
					if !l.there(t, path) {
						t.Errorf("%s went", path)
					}
				}
				if c.links && l.there(t, "Atoll/links/2") {
					t.Error("a link was made")
				}
				// Nothing of the failed read is kept: once the folder reads, the same handle counts what it holds.
				l.m.fail, l.m.over = nil, ""
				if got, err := c.do(t, l, opened); err != nil || got != c.want {
					t.Errorf("once it reads: %s, %v, want %s", got, err, c.want)
				}
			})
		}
	}
	for _, c := range calls {
		t.Run("absent/"+c.name, func(t *testing.T) {
			l := newLinkedDisk(t, false)
			var opened *LinksFolder
			if c.links {
				var err error
				if opened, err = l.d.OpenLinks(true); err != nil || opened == nil {
					t.Fatalf("the links folder: %v", err)
				}
				defer opened.Close()
			}
			if got, err := c.do(t, l, opened); err != nil || got != c.absent {
				t.Errorf("%s, %v, want %s", got, err, c.absent)
			}
		})
	}
}

// failing fails one call about one name with an error.
func failing(call, name string, err error) func(string, string) error {
	return func(c, n string) error {
		if c == call && n == name {
			return err
		}
		return nil
	}
}

func inodeKey(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return strconv.FormatUint(info.Sys().(*syscall.Stat_t).Ino, 10) //nolint:forcetypeassert // Linux only
}

// link gives a name below a mount a second name, its folders made on the way.
func link(t *testing.T, mount, from, to string) {
	t.Helper()
	full := filepath.Join(mount, filepath.FromSlash(to))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(mount, filepath.FromSlash(from)), full); err != nil {
		t.Fatal(err)
	}
}

// The names a look counts leave out the guard's own links alone: a regular file of the links folder named by a file id.
// Anything else there, a file a user put there with another name, is a name like any other (specs.md §17). A file with
// one name has no link of the guard, and its look reads nothing of the links folder, which may not even be readable.
func TestALookLeavesOutTheGuardsLinksAlone(t *testing.T) {
	mount := newMount(t)
	put(t, mount, "media/Films/a", "film")
	put(t, mount, "media/Films/b", "film")
	link(t, mount, "media/Films/a", "Atoll/links/7")
	link(t, mount, "media/Films/b", "Atoll/links/notes")
	unread := &refusing{call: "names"}
	d := openAt(t, mount, unread.fail)
	put(t, mount, "media/Films/c", "film")
	if st, there, err := d.StatRoot("Films/c"); err != nil || !there || st.Nlink != 1 || unread.calls != 0 {
		t.Fatalf("a file with one name: %d names, %d reads of the links folder, %v", st.Nlink, unread.calls, err)
	}
	d = openAt(t, mount, nil)
	for path, want := range map[string]uint32{"Films/a": 1, "Films/b": 2} {
		if st, _, err := d.StatRoot(path); err != nil || st.Nlink != want {
			t.Errorf("%s: %d names, %v", path, st.Nlink, err)
		}
	}
}

// What the guard counts of its links is what the links folder holds when it was last read: a read of it anew forgets
// the links gone since, and no count ever says a file found at a name has none.
func TestTheGuardCountsWhatItLastRead(t *testing.T) {
	mount := newMount(t)
	put(t, mount, "media/Films/a", "film")
	link(t, mount, "media/Films/a", "media/Films/b")
	link(t, mount, "media/Films/a", "Atoll/links/7")
	link(t, mount, "media/Films/a", "Atoll/links/8")
	d := openAt(t, mount, nil)
	if st, _, err := d.StatRoot("Films/a"); err != nil || st.Nlink != 2 {
		t.Fatalf("a and b, with the links 7 and 8: %d names, %v", st.Nlink, err)
	}
	for _, name := range []string{"7", "8"} {
		if err := os.Remove(filepath.Join(mount, "Atoll", "links", name)); err != nil {
			t.Fatal(err)
		}
	}
	if st, _, err := d.StatRoot("Films/a"); err != nil || st.Nlink < 1 {
		t.Errorf("the links gone, not read again: %d names, %v", st.Nlink, err)
	}
	if _, err := d.Links(); err != nil {
		t.Fatal(err)
	}
	if st, _, err := d.StatRoot("Films/a"); err != nil || st.Nlink != 2 {
		t.Errorf("the links read again: %d names, %v", st.Nlink, err)
	}
}

// The highest file id the links folder names counts the largest a catalog gives, and nothing past it.
func TestTheHighestLinkIsOneACatalogGives(t *testing.T) {
	mount := newMount(t)
	put(t, mount, "media/a", "film")
	link(t, mount, "media/a", fmt.Sprintf("Atoll/links/%d", catalog.MaxID))
	link(t, mount, "media/a", fmt.Sprintf("Atoll/links/%d", int64(catalog.MaxID)+1))
	if got, err := openAt(t, mount, nil).HighestLinkID(); err != nil || got != catalog.MaxID {
		t.Errorf("%d, %v", got, err)
	}
}

// Making a link, and removing one, give the change time the file had just before, as each moves it for every name of
// the file, and a name that recorded another holds a change nobody has looked at (specs.md §17).
func TestALinkSaysTheChangeTimeBeforeIt(t *testing.T) {
	mount := newMount(t)
	put(t, mount, "media/Films/a", "film")
	put(t, mount, "media/Films/other", "x")
	before := statAt(t, mount, "media/Films/a").Ctime
	d := openAt(t, mount, nil)
	folder, err := d.OpenLinks(true)
	if err != nil {
		t.Fatal(err)
	}
	defer folder.Close()
	key := keyAt(t, mount, "media/Films/a")
	links, err := d.MakeLink(&Op{ID: 7, Disk: "t1", Kind: catalog.OpLink, Path: "Films/a", Link: 42, FileKey: key}, folder)
	if err != nil || links == nil || links.PriorNS != TimeOf(before) {
		t.Fatalf("made: %+v, %v", links, err)
	}
	linked := statAt(t, mount, "media/Films/a").Ctime
	prior, err := d.DropLink(&Op{ID: 8, Disk: "t1", Kind: catalog.OpUnlink, Path: "Films/a", Link: 42, FileKey: key}, folder)
	if err != nil || prior != TimeOf(linked) || !absent(mount, "Atoll/links/42") {
		t.Errorf("removed: %+v, %v", prior, err)
	}
}

// A link the guard sends to the trash lands in folders that stand for the root and the folders of its path, each like
// the one it stands for while that one is there, and private, the file's owner's, once it is gone (specs.md §17.1).
func TestTheFoldersOfALinkInTheTrashAreLikeThoseOfItsPath(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	mount := newMount(t)
	put(t, mount, "media/Films/Sub/x", "x")
	put(t, mount, "media/elsewhere", "film")
	link(t, mount, "media/elsewhere", "Atoll/links/42")
	if err := os.Remove(filepath.Join(mount, "media", "elsewhere")); err != nil {
		t.Fatal(err)
	}
	modes := map[string]os.FileMode{"media": 0o751, "media/Films": 0o750, "media/Films/Sub": 0o710}
	for rel, mode := range modes {
		if err := os.Chmod(filepath.Join(mount, filepath.FromSlash(rel)), mode); err != nil {
			t.Fatal(err)
		}
	}
	op := &Op{ID: 9, Disk: "t1", Kind: catalog.OpTrash, Path: "Films/Sub/Gone/a", Path2: "2026-01-01/9/Films/Sub/Gone/a", Link: 42,
		FileKey: keyAt(t, mount, "Atoll/links/42")}
	if _, err := openAt(t, mount, nil).LinkToTrash(op); err != nil {
		t.Fatal(err)
	}
	const place = "Atoll/trash/2026-01-01/9"
	for trashed, want := range map[string]uint32{
		place: 0o751, place + "/Films": 0o750, place + "/Films/Sub": 0o710, place + "/Films/Sub/Gone": 0o700,
	} {
		if got := statAt(t, mount, trashed).Mode & 0o777; got != want {
			t.Errorf("%s is %o, not %o", trashed, got, want)
		}
	}
	if !holding(mount, place+"/Films/Sub/Gone/a", "film") {
		t.Error("the link's data is not in the trash")
	}
}

// A link the guard removes is forgotten alone: what the guard counts of the others that lead to the same data stays.
func TestALinkRemovedIsForgottenAlone(t *testing.T) {
	mount := newMount(t)
	put(t, mount, "media/Films/a", "film")
	for _, id := range []string{"41", "42", "43"} {
		link(t, mount, "media/Films/a", "Atoll/links/"+id)
	}
	d := openAt(t, mount, nil)
	if _, err := d.Links(); err != nil {
		t.Fatal(err)
	}
	folder, err := d.OpenLinks(false)
	if err != nil {
		t.Fatal(err)
	}
	defer folder.Close()
	key := keyAt(t, mount, "media/Films/a")
	if _, err := d.DropLink(&Op{ID: 8, Disk: "t1", Kind: catalog.OpUnlink, Path: "Films/a", Link: 42, FileKey: key}, folder); err != nil {
		t.Fatal(err)
	}
	if st, _, err := d.StatRoot("Films/a"); err != nil || st.Nlink != 1 {
		t.Errorf("the links 41 and 43 left: %d names, %v", st.Nlink, err)
	}
}
