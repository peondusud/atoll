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
