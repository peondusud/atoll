package ops

import (
	"errors"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
)

// openAs opens a disk on a folder with a filesystem of the configuration, an NTFS one as ntfs3 mounts it.
func openAs(t *testing.T, mount string, fs names.Filesystem) *Disk {
	t.Helper()
	m := &onDisk{}
	if fs == names.NTFS {
		m.driver = "ntfs3"
	}
	d, err := Open(m, &config.Disk{ID: "t1", Mount: mount, Root: "media", Workdir: "Atoll", FS: fs}, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d
}

// Right before Atoll makes a name on an NTFS disk, it lists the folder the name goes in and refuses the name when
// another there has its key, whoever made that one, before anything is written: a folder, a copy's or a restore's name,
// a rename's of a file or a folder, and what the view makes or renames (specs.md §15.7; conformance/models/README.md,
// flaws 24 and 25, the maintainer's remedy disk-key-look, 2026-11-25). A case-only rename does not collide with its own
// old name. The looks before refuse a name on the way that collides, and say so. On ext4, which holds both names,
// each is made.
func TestEachNameMadeOnNTFSIsLookedAtByItsKey(t *testing.T) {
	like := func(string) (*fsx.Stat, error) { return &fsx.Stat{Mode: 0o755}, nil }
	for _, tc := range []struct {
		what    string
		before  []string // files below the mount
		do      func(t *testing.T, d *Disk, mount string) error
		made    string // what it makes when it may
		refused bool   // on NTFS
	}{
		{"a folder beside one of its key", []string{"media/films/x"}, func(_ *testing.T, d *Disk, _ string) error {
			_, err := d.MakeRootFolder("Films", nil, func(string) error { return nil })
			return err
		}, "media/Films", true},
		{"a copy's name beside one of its key", []string{"media/a.mkv"}, func(t *testing.T, d *Disk, mount string) error {
			op := &Op{ID: 1, Disk: "t1", Kind: catalog.OpCreate, Path: "A.MKV", Path2: "1.part"}
			part, err := d.MakePart(op)
			if err != nil {
				t.Fatal(err)
			}
			_ = part.Close()
			op.FileKey = keyAt(t, mount, "Atoll/tmp/1.part")
			_, err = d.PlacePart(op, "A.MKV", nil)
			return err
		}, "media/A.MKV", true},
		{"a restore beside one of its key", []string{"media/a.mkv", "Atoll/trash/2026-01-01/2/A.MKV"}, func(t *testing.T, d *Disk, mount string) error {
			op := &Op{ID: 2, Disk: "t1", Kind: catalog.OpRestore, Path: "A.MKV", Path2: "2026-01-01/2/A.MKV", FileKey: keyAt(t, mount, "Atoll/trash/2026-01-01/2/A.MKV")}
			_, err := d.FromTrash(op)
			return err
		}, "media/A.MKV", true},
		{"a rename beside a name of its key", []string{"media/b.mkv", "media/a.mkv"}, func(t *testing.T, d *Disk, mount string) error {
			row := rowAt(t, mount, "b.mkv")
			_, _, err := d.RenameFile(&Op{ID: 3, Disk: "t1", Kind: catalog.OpRename, Path: "b.mkv", Path2: "A.mkv", FileKey: row.FileKey.String}, row)
			return err
		}, "media/A.mkv", true},
		{"a case-only rename", []string{"media/a.mkv"}, func(t *testing.T, d *Disk, mount string) error {
			row := rowAt(t, mount, "a.mkv")
			_, _, err := d.RenameFile(&Op{ID: 4, Disk: "t1", Kind: catalog.OpRename, Path: "a.mkv", Path2: "A.mkv", FileKey: row.FileKey.String}, row)
			return err
		}, "media/A.mkv", false},
		{"a folder's case-only rename", []string{"media/films/x"}, func(t *testing.T, d *Disk, mount string) error {
			return d.RenameFolder(&Op{ID: 5, Disk: "t1", Kind: catalog.OpRename, Path: "films", Path2: "Films", FileKey: keyAt(t, mount, "media/films")})
		}, "media/Films/x", false},
		{"a folder's rename beside one of its key", []string{"media/Other/x", "media/films/y"}, func(t *testing.T, d *Disk, mount string) error {
			return d.RenameFolder(&Op{ID: 6, Disk: "t1", Kind: catalog.OpRename, Path: "Other", Path2: "Films", FileKey: keyAt(t, mount, "media/Other")})
		}, "media/Films/x", true},
		{"the view's new file in a folder of another's key", []string{"media/films/x"}, func(_ *testing.T, d *Disk, _ string) error {
			f, _, err := d.CreateUserFile("Films/a.mkv", 0o644, 0, 0, like)
			if err == nil {
				_ = f.Close()
			}
			return err
		}, "media/Films/a.mkv", true},
		{"the view's new file beside one of its key", []string{"media/films/a.mkv"}, func(_ *testing.T, d *Disk, _ string) error {
			f, _, err := d.CreateUserFile("films/A.mkv", 0o644, 0, 0, like)
			if err == nil {
				_ = f.Close()
			}
			return err
		}, "media/films/A.mkv", true},
		{"the view's new folder", []string{"media/films/x"}, func(_ *testing.T, d *Disk, _ string) error {
			_, err := d.MakeUserFolder("Films", 0o755, 0, 0, like)
			return err
		}, "media/Films", true},
		{"the view's new link", []string{"media/films/x"}, func(_ *testing.T, d *Disk, _ string) error {
			_, err := d.MakeUserSymlink("Films", "x", 0, 0, like)
			return err
		}, "media/Films", true},
		{"the view's rename", []string{"media/a.mkv", "media/B.mkv"}, func(t *testing.T, d *Disk, mount string) error {
			return d.RenameUser("a.mkv", "b.mkv", false, keyAt(t, mount, "media/a.mkv"), like)
		}, "media/b.mkv", true},
		{"the view's case-only rename", []string{"media/a.mkv"}, func(t *testing.T, d *Disk, mount string) error {
			return d.RenameUser("a.mkv", "A.mkv", false, keyAt(t, mount, "media/a.mkv"), like)
		}, "media/A.mkv", false},
		{"the view's link", []string{"media/a.mkv", "media/B.mkv"}, func(t *testing.T, d *Disk, mount string) error {
			_, err := d.LinkUser("a.mkv", "b.mkv", keyAt(t, mount, "media/a.mkv"), like)
			return err
		}, "media/b.mkv", true},
	} {
		for _, fs := range []names.Filesystem{names.NTFS, names.Ext4} {
			t.Run(tc.what+"/"+string(fs), func(t *testing.T) {
				mount := newMount(t)
				for _, f := range tc.before {
					put(t, mount, f, "x")
				}
				err := tc.do(t, openAs(t, mount, fs), mount)
				refused := tc.refused && fs == names.NTFS
				switch {
				case refused && !errors.Is(err, ErrNameCollides):
					t.Errorf("%v, want the name refused for its key", err)
				case refused && !errors.Is(err, ErrNameRefused):
					t.Errorf("%v is no name refused", err)
				case !refused && err != nil:
					t.Errorf("%v, want it made", err)
				}
				if absent(mount, tc.made) != refused {
					t.Errorf("%s made: %v", tc.made, !absent(mount, tc.made))
				}
			})
		}
	}
}

// The looks before a write refuse a name on the way, or at the end, that collides on an NTFS disk, and say which: a
// copy's look (TargetCollides), and the look of a rename or a restore (ErrNameCollides), but for the name a case-only
// rename leaves.
func TestTheLooksOnNTFSSeeTheKeys(t *testing.T) {
	mount := newMount(t)
	put(t, mount, "media/films/x", "x")
	put(t, mount, "media/Films2/a.mkv", "a")
	d := openAs(t, mount, names.NTFS)
	for _, tc := range []struct{ path, want string }{
		{"Films/a.mkv", TargetCollides},
		{"films/y", TargetFree},
		{"films/X", TargetCollides},
		{"Films2/A.MKV", TargetCollides},
		{"Films2/a.mkv", TargetTaken},
		{"FILMS2/b", TargetCollides},
	} {
		if got, _, err := d.LookAtTarget(tc.path); err != nil || got != tc.want {
			t.Errorf("LookAtTarget(%s) = %q, %v; want %q", tc.path, got, err, tc.want)
		}
	}
	for _, tc := range []struct {
		path, from string
		collides   bool
	}{
		{"Films/a.mkv", "", true},
		{"Films", "films", false},
		{"Films", "Other", true},
		{"films/X", "films/x", false},
		{"films/X", "films/y", true},
		{"Films2/A.MKV", "a", true},
	} {
		if _, _, err := d.LookAt(tc.path, tc.from); errors.Is(err, ErrNameCollides) != tc.collides || err != nil && !tc.collides {
			t.Errorf("LookAt(%s, %s): %v", tc.path, tc.from, err)
		}
	}
	ext4 := openAs(t, mount, names.Ext4)
	if got, _, err := ext4.LookAtTarget("Films/a.mkv"); err != nil || got != TargetFree {
		t.Errorf("on ext4: %q, %v", got, err)
	}
}
