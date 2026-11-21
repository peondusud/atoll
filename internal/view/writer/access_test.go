package writer

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/view"
)

// root asks as root does, with every capability: whatever the kernel let through, a disk lets through too.
var root = Caller{Creds: &view.Credentials{Caps: ^view.Caps(0)}}

// The account that runs the tests owns every file of the bench's disks; another is any other account, with no other
// group and no capability, as the kernel would name it.
var (
	me      = uint32(os.Geteuid())
	myGroup = uint32(os.Getegid())
	another = Caller{Owner: Owner{UID: me + 1, GID: myGroup + 1}, Creds: &view.Credentials{}}
)

// presentAs makes the catalog present Films/a.mkv and the folder Films with these modes and this owner, on both disks,
// where the disks hold them as the account that runs the tests made them.
func (b *bench) presentAs(file, folder uint32, uid, gid uint32) {
	b.t.Helper()
	for _, q := range []struct {
		query string
		mode  uint32
	}{
		{"UPDATE copies SET mode = ?, uid = ?, gid = ?", file},
		{"UPDATE entries SET mode = ?, uid = ?, gid = ? WHERE CAST(lpath AS TEXT) = 'Films'", folder},
	} {
		if _, err := b.w.db.ExecContext(b.t.Context(), q.query, q.mode, uid, gid); err != nil {
			b.t.Fatal(err)
		}
	}
}

// chmod gives a path a mode directly on a disk, as a user would there, which no pass has recorded.
func (b *bench) chmod(disk, path string, mode os.FileMode) {
	b.t.Helper()
	if err := os.Chmod(filepath.Join(b.mount[disk], "media", path), mode); err != nil {
		b.t.Fatal(err)
	}
}

// journaled gives how many operations of the view are open.
func (b *bench) journaled() int {
	b.t.Helper()
	open, err := catalog.OpenViewOps(b.t.Context(), b.w.db)
	if err != nil {
		b.t.Fatal(err)
	}
	return len(open)
}

func modeOn(t *testing.T, b *bench, disk, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(filepath.Join(b.mount[disk], "media", path))
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
}

// The view checks a caller again on the disk an operation reaches, where that disk holds what it acts on otherwise
// than the catalog presents it, as after a chmod made directly there that no pass has recorded (specs.md §21.2): what
// that disk would refuse is refused, with the error Linux would give, before anything is journaled or changed; where
// it holds it as presented, nothing more is asked. Films/a.mkv is presented from t2, its original.
func TestWriterChecksWhatTheDiskHolds(t *testing.T) {
	for _, tc := range []struct {
		name string
		// file and folder are the modes the catalog presents of Films/a.mkv and Films, the bench's account's; disk
		// gives what the disks hold otherwise.
		file, folder uint32
		disk         func(b *bench)
		do           func(b *bench) error
		want         error
		// unchanged is a path whose content stays as it was on t2, "" for none.
		unchanged string
	}{
		{name: "opened to write, as presented", file: 0o666, folder: 0o755, disk: func(*bench) {},
			do: func(b *bench) error {
				return closed(b.w.Open(b.t.Context(), "Films/a.mkv", b.lookup("Films/a.mkv"), another))
			}},
		{name: "opened to write, 0600 on its disk", file: 0o666, folder: 0o755, disk: func(b *bench) { b.chmod("t2", "Films/a.mkv", 0o600) },
			do: func(b *bench) error {
				return closed(b.w.Open(b.t.Context(), "Films/a.mkv", b.lookup("Films/a.mkv"), another))
			},
			want: syscall.EACCES, unchanged: "Films/a.mkv"},
		{name: "opened to write by its owner, 0600 on its disk", file: 0o666, folder: 0o755, disk: func(b *bench) { b.chmod("t2", "Films/a.mkv", 0o600) },
			do: func(b *bench) error {
				return closed(b.w.Open(b.t.Context(), "Films/a.mkv", b.lookup("Films/a.mkv"), Caller{Owner: Owner{UID: me, GID: myGroup}, Creds: &view.Credentials{}}))
			}},
		{name: "setuid on its disk alone", file: 0o666, folder: 0o755, disk: func(b *bench) { b.chmod("t2", "Films/a.mkv", 0o666|os.ModeSetuid) },
			do: func(b *bench) error {
				return closed(b.w.Open(b.t.Context(), "Films/a.mkv", b.lookup("Films/a.mkv"), another))
			},
			want: syscall.EPERM, unchanged: "Films/a.mkv"},
		{name: "truncated by its name", file: 0o666, folder: 0o755, disk: func(b *bench) { b.chmod("t2", "Films/a.mkv", 0o644) },
			do: func(b *bench) error {
				return setattr(b, &ops.UserAttrs{Size: ptr(int64(0))}, Asked{Caller: another})
			},
			want: syscall.EACCES, unchanged: "Films/a.mkv"},
		{name: "truncated through a file open to write", file: 0o666, folder: 0o755, disk: func(b *bench) { b.chmod("t2", "Films/a.mkv", 0o644) },
			do: func(b *bench) error {
				return setattr(b, &ops.UserAttrs{Size: ptr(int64(0))}, Asked{Caller: another, Open: true})
			}},
		{name: "touched", file: 0o666, folder: 0o755, disk: func(b *bench) { b.chmod("t2", "Films/a.mkv", 0o644) },
			do: func(b *bench) error {
				at := fsx.TimeOf(1_700_000_000_000_000_000)
				return setattr(b, &ops.UserAttrs{Atime: &at, Mtime: &at}, Asked{Caller: another, Now: true})
			},
			want: syscall.EACCES},
		{name: "an extended attribute set", file: 0o666, folder: 0o755, disk: func(b *bench) { b.chmod("t2", "Films/a.mkv", 0o644) },
			do: func(b *bench) error {
				return b.w.SetXattr(b.t.Context(), "Films/a.mkv", b.lookup("Films/a.mkv"), "user.DOSATTRIB", []byte("0x20"), 0, another)
			},
			want: syscall.EACCES},
		{name: "made in a folder 0755 on its disk", file: 0o644, folder: 0o777, disk: func(b *bench) { b.chmod("t1", "Films", 0o755) },
			do: func(b *bench) error {
				f, _, err := b.w.Create(b.t.Context(), "Films/new.mkv", 0o644, another)
				return closed(f, err)
			},
			want: syscall.EACCES},
		{name: "a folder made there", file: 0o644, folder: 0o777, disk: func(b *bench) { b.chmod("t1", "Films", 0o755) },
			do: func(b *bench) error {
				_, err := b.w.Mkdir(b.t.Context(), "Films/new", 0o755, another)
				return err
			},
			want: syscall.EACCES},
		{name: "a symbolic link made there", file: 0o644, folder: 0o777, disk: func(b *bench) { b.chmod("t1", "Films", 0o755) },
			do: func(b *bench) error {
				_, err := b.w.Symlink(b.t.Context(), "Films/new", "a.mkv", another)
				return err
			},
			want: syscall.EACCES},
		{name: "made in a folder 0777 as on its disk", file: 0o644, folder: 0o777, disk: func(b *bench) { b.chmod("t1", "Films", 0o777) },
			do: func(b *bench) error {
				f, _, err := b.w.Create(b.t.Context(), "Films/new.mkv", 0o644, another)
				return closed(f, err)
			}},
		{name: "deleted from a folder 0755 on its disk", file: 0o644, folder: 0o777, disk: func(b *bench) { b.chmod("t2", "Films", 0o755) },
			do:   func(b *bench) error { return b.w.Unlink(b.t.Context(), "Films/a.mkv", another) },
			want: syscall.EACCES, unchanged: "Films/a.mkv"},
		{name: "deleted from a folder made sticky on its disk", file: 0o644, folder: 0o777, disk: func(b *bench) { b.chmod("t2", "Films", 0o777|os.ModeSticky) },
			do:   func(b *bench) error { return b.w.Unlink(b.t.Context(), "Films/a.mkv", another) },
			want: syscall.EPERM, unchanged: "Films/a.mkv"},
		{name: "renamed in a folder 0755 on its disk", file: 0o644, folder: 0o777, disk: func(b *bench) { b.chmod("t2", "Films", 0o755) },
			do:   func(b *bench) error { return b.w.Rename(b.t.Context(), "Films/a.mkv", "Films/b.mkv", false, another) },
			want: syscall.EACCES, unchanged: "Films/a.mkv"},
		{name: "a folder renamed, 0755 on its disk", file: 0o644, folder: 0o777,
			disk: func(b *bench) {
				if _, err := b.w.Mkdir(b.t.Context(), "Films/Sub", 0o777, root); err != nil {
					b.t.Fatal(err)
				}
				b.chmod("t1", "Films", 0o755)
			},
			do:   func(b *bench) error { return b.w.Rename(b.t.Context(), "Films/Sub", "Films/Other", false, another) },
			want: syscall.EACCES},
		{name: "hard-linked into a folder 0755 on its disk", file: 0o666, folder: 0o777, disk: func(b *bench) { b.chmod("t2", "Films", 0o755) },
			do: func(b *bench) error {
				_, err := b.w.Link(b.t.Context(), "Films/a.mkv", "Films/b.mkv", another)
				return err
			},
			want: syscall.EACCES},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t, "ext4")
			b.presentAs(tc.file, tc.folder, me, myGroup)
			for _, disk := range []string{"t1", "t2"} {
				b.chmod(disk, "Films/a.mkv", os.FileMode(tc.file))
				b.chmod(disk, "Films", os.FileMode(tc.folder))
			}
			tc.disk(b)
			before, journaled := b.on("t2", "Films/a.mkv"), b.journaled()
			err := tc.do(b)
			switch {
			case tc.want == nil && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("%v, want %v", err, tc.want)
			case tc.want == nil:
				return
			}
			if got := b.journaled(); got != journaled {
				t.Errorf("journaled: %d operations, %d before", got, journaled)
			}
			if tc.unchanged != "" && b.on("t2", tc.unchanged) != before {
				t.Errorf("%s holds %q, %q before", tc.unchanged, b.on("t2", tc.unchanged), before)
			}
			for _, disk := range []string{"t1", "t2"} {
				for _, p := range []string{"Films/new.mkv", "Films/new", "Films/b.mkv", "Films/Other"} {
					if _, err := os.Lstat(filepath.Join(b.mount[disk], "media", p)); err == nil {
						t.Errorf("%s holds %s", disk, p)
					}
				}
			}
			entries, err := catalog.TrashEntries(b.t.Context(), b.w.db)
			if err != nil || len(entries) != 0 {
				t.Errorf("the trash: %+v, %v", entries, err)
			}
		})
	}
}

// A change of mode, owner or times is checked on the disk as Linux checks it there: the owner there, or CAP_FOWNER,
// changes the mode and gives the times, CAP_CHOWN alone gives a file away; and setgid is taken off a mode where the
// caller is not in the group the disk holds, as Linux takes it off (specs.md §21.2).
func TestWriterChangesAsTheDiskAllows(t *testing.T) {
	b := newBench(t, "ext4")
	// The catalog says that another owns Films/a.mkv, setuid; the disk, the account that runs the tests.
	b.presentAs(0o4644, 0o755, another.UID, another.GID)
	b.chmod("t2", "Films/a.mkv", 0o644|os.ModeSetuid)
	ctx := t.Context()
	journaled := b.journaled()
	for _, tc := range []struct {
		name string
		a    *ops.UserAttrs
		how  Asked
		want error
	}{
		{"a mode", &ops.UserAttrs{Mode: ptr(uint32(0o600))}, Asked{Caller: another}, syscall.EPERM},
		{"times given", &ops.UserAttrs{Mtime: ptr(fsx.TimeOf(1_700_000_000_000_000_000))}, Asked{Caller: another}, syscall.EPERM},
		{"an owner", &ops.UserAttrs{UID: ptr(another.UID)}, Asked{Caller: another}, syscall.EPERM},
		{"a group", &ops.UserAttrs{GID: ptr(another.GID)}, Asked{Caller: another}, syscall.EPERM},
		{"a mode that only takes setuid off", &ops.UserAttrs{Mode: ptr(uint32(0o644))}, Asked{Caller: another}, nil},
	} {
		if _, err := b.w.Setattr(ctx, "Films/a.mkv", b.lookup("Films/a.mkv"), tc.a, tc.how); !errors.Is(err, tc.want) && (tc.want != nil || err != nil) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	if got := modeOn(t, b, "t2", "Films/a.mkv"); got != 0o644 {
		t.Errorf("the mode on the disk: %v", got)
	}
	if got := b.journaled(); got != journaled+1 {
		t.Errorf("journaled: %d operations, %d before", got, journaled)
	}

	// Its owner on the disk, in neither its group there nor holding CAP_FSETID, asks setgid: it is taken off.
	b = newBench(t, "ext4")
	owner := Caller{Owner: Owner{UID: me, GID: myGroup + 1}, Creds: &view.Credentials{Groups: []uint32{myGroup + 2}}}
	b.presentAs(0o644, 0o755, me, myGroup+2)
	n, err := b.w.Setattr(ctx, "Films/a.mkv", b.lookup("Films/a.mkv"), &ops.UserAttrs{Mode: ptr(uint32(0o2644))}, Asked{Caller: owner})
	if err != nil {
		t.Fatal(err)
	}
	if got := modeOn(t, b, "t2", "Films/a.mkv"); got != 0o644 || n.Mode != 0o644 {
		t.Errorf("setgid of a group it is not in on the disk: %v, presented %#o", got, n.Mode)
	}
	owner.Creds = &view.Credentials{Groups: []uint32{myGroup}}
	if _, err := b.w.Setattr(ctx, "Films/a.mkv", b.lookup("Films/a.mkv"), &ops.UserAttrs{Mode: ptr(uint32(0o2644))}, Asked{Caller: owner}); err != nil {
		t.Fatal(err)
	}
	if got := modeOn(t, b, "t2", "Films/a.mkv"); got != 0o644|os.ModeSetgid {
		t.Errorf("setgid of a group it is in on the disk: %v", got)
	}
}

// A hard link of a file that fs.protected_hardlinks would keep the caller from linking on its disk, 0600 of another
// there, is refused, where the catalog presents it 0666 (specs.md §21.2).
func TestWriterLinksAsTheDiskAllows(t *testing.T) {
	if !protectedHardlinks() {
		t.Skip("fs.protected_hardlinks is off here")
	}
	b := newBench(t, "ext4")
	b.presentAs(0o666, 0o777, me, myGroup)
	b.chmod("t1", "Films", 0o777)
	b.chmod("t2", "Films", 0o777)
	b.chmod("t2", "Films/a.mkv", 0o600)
	if _, err := b.w.Link(t.Context(), "Films/a.mkv", "Films/b.mkv", another); !errors.Is(err, syscall.EPERM) {
		t.Errorf("%v, want EPERM", err)
	}
	if _, err := os.Lstat(filepath.Join(b.mount["t2"], "media", "Films", "b.mkv")); err == nil {
		t.Error("the link was made")
	}
}

func ptr[T any](v T) *T { return &v }

// closed closes a file a test opened, and gives what opening it said.
func closed(f *File, err error) error {
	if f != nil {
		_ = f.Close()
	}
	return err
}

func setattr(b *bench, a *ops.UserAttrs, how Asked) error {
	_, err := b.w.Setattr(b.t.Context(), "Films/a.mkv", b.lookup("Films/a.mkv"), a, how)
	return err
}
