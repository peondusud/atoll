//go:build loopdev

package loopdev

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/watch"
)

// scanBench is one real filesystem, with a catalog beside it.
type scanBench struct {
	t      *testing.T
	v      *volume
	at     string
	db     *sql.DB
	config *config.Config
}

func newScan(t *testing.T, v *volume, at string) *scanBench {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "catalog.db")
	text := fmt.Sprintf("pool_id = \"p\"\ncatalog = %q\n[view]\nmount = %q\n[[disk]]\nid = \"t1\"\nuuid = %q\nmount = %q\nfs = %q\n",
		file, filepath.Join(dir, "pool"), v.uuid, at, v.fs)
	c, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	db, err := catalog.OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &scanBench{t: t, v: v, at: at, db: db, config: c}
}

func (b *scanBench) scan(opts watch.Options) *watch.Report {
	b.t.Helper()
	machine, err := disks.NewMachine()
	if err != nil {
		b.t.Fatal(err)
	}
	out, err := watch.Scan(b.t.Context(), b.db, b.config, machine, opts)
	if err != nil {
		b.t.Fatal(err)
	}
	return &out.Disks[0]
}

func (b *scanBench) one(query string, args ...any) sql.NullInt64 {
	b.t.Helper()
	var n sql.NullInt64
	if err := b.db.QueryRowContext(b.t.Context(), query, args...).Scan(&n); err != nil {
		b.t.Fatal(err)
	}
	return n
}

func mkRoot(t *testing.T, at string, files ...string) {
	t.Helper()
	for _, name := range files {
		full := filepath.Join(at, "media", name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestScanOnEachDriver: a file's times and inode, as each driver gives them (specs.md §7,
// docs/research/file-times.md).
func TestScanOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range []struct {
		name   string
		fs     names.Filesystem
		driver string
		mkfs   []string
		birth  bool
	}{
		{"ext4", names.Ext4, "", nil, true},
		{"ext4, 128-byte inodes", names.Ext4, "", []string{"-I", "128"}, false},
		{"xfs", names.XFS, "", nil, true},
		{"btrfs", names.Btrfs, "", nil, true},
		{"ntfs3", names.NTFS, "ntfs3", nil, true},
		{"ntfs-3g", names.NTFS, "ntfs-3g", nil, true},
		{"lowntfs-3g", names.NTFS, "lowntfs-3g", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.driver == "ntfs3" {
				ntfs3Type(t)
			}
			v := newVolume(t, tc.fs, tc.mkfs...)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, optionsFor(t, tc.driver)...)
			start := time.Now().Add(-time.Minute).UnixNano()
			mkRoot(t, at, "Films/a.mkv", "b")
			b := newScan(t, v, at)
			r := b.scan(watch.Options{Full: true})
			if !r.Walked || r.New != 2 || r.Hashed != 2 || r.Folders != 1 {
				t.Fatalf("%+v", r)
			}
			end := time.Now().Add(time.Minute).UnixNano()
			key := []byte("Films/a.mkv")
			birth := b.one("SELECT c.btime_ns FROM copies c JOIN files f ON f.id = c.file_id WHERE f.lpath = ?", key)
			if birth.Valid != tc.birth {
				t.Errorf("creation time: %+v, want it known: %v", birth, tc.birth)
			}
			if tc.birth && (birth.Int64 < start || birth.Int64 > end) {
				t.Errorf("creation time %d, outside %d to %d", birth.Int64, start, end)
			}
			for _, column := range []string{"mtime_ns", "ctime_ns"} {
				got := b.one("SELECT c."+column+" FROM copies c JOIN files f ON f.id = c.file_id WHERE f.lpath = ?", key)
				if !got.Valid || got.Int64 < start || got.Int64 > end {
					t.Errorf("%s: %+v, outside %d to %d", column, got, start, end)
				}
			}
			var st syscall.Stat_t
			if err := syscall.Lstat(filepath.Join(at, "media", "Films/a.mkv"), &st); err != nil {
				t.Fatal(err)
			}
			var fileKey string
			row := b.db.QueryRowContext(t.Context(), "SELECT c.file_key FROM copies c JOIN files f ON f.id = c.file_id WHERE f.lpath = ?", key)
			if err := row.Scan(&fileKey); err != nil {
				t.Fatal(err)
			}
			if fileKey != fmt.Sprintf("%d", st.Ino) {
				t.Errorf("file key %s, inode %d", fileKey, st.Ino)
			}
			if got := b.scan(watch.Options{}); got.Known != 2 || len(got.Changed) != 0 || got.New != 0 {
				t.Errorf("a scan again, on a disk nothing touched, reports it changed: %+v", got)
			}
			// A touch moves the times, and only the record says so.
			when := time.Unix(1600000000, 0)
			if err := os.Chtimes(filepath.Join(at, "media", "b"), when, when); err != nil {
				t.Fatal(err)
			}
			if got := b.scan(watch.Options{}); !slices.Equal(got.Changed, []string{"b"}) {
				t.Errorf("changed: %v", got.Changed)
			}
		})
	}
}

// TestCreationTimeOfNtfs3GIsWhatNtfs3Reads: the attribute and statx must agree, or the original
// would depend on the driver.
func TestCreationTimeOfNtfs3GIsWhatNtfs3Reads(t *testing.T) {
	needRoot(t)
	kind := ntfs3Type(t)
	v := newVolume(t, names.NTFS)
	at := mountPoint(t)
	mountVolume(t, v, "ntfs-3g", at)
	mkRoot(t, at, "a")
	first, err := fsx.OpenDir(filepath.Join(at, "media"))
	if err != nil {
		t.Fatal(err)
	}
	seen, err := first.Stat("a")
	if err != nil {
		t.Fatal(err)
	}
	if !seen.HasBirth {
		t.Fatal("no creation time through the attribute")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	must(t, "umount", at)
	time.Sleep(500 * time.Millisecond)
	mount(t, at, "mount", "-t", kind, "-o", strings.Join(append(ntfs3Options(t), "ro"), ","), v.device)
	second, err := fsx.OpenDir(filepath.Join(at, "media"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	again, err := second.Stat("a")
	if err != nil {
		t.Fatal(err)
	}
	if !again.HasBirth || again.Birth != seen.Birth {
		t.Errorf("ntfs-3g gave %+v, ntfs3 %+v", seen.Birth, again.Birth)
	}
}

// TestScanStaysOnItsDisk: what is mounted under the root is not entered, and special files are
// recorded, not opened.
func TestScanStaysOnItsDisk(t *testing.T) {
	needRoot(t)
	v := newVolume(t, names.Ext4)
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	mkRoot(t, at, "a")
	inner := filepath.Join(at, "media", "inner")
	if err := os.Mkdir(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	mount(t, inner, "mount", "-t", "tmpfs", "tmpfs")
	if err := os.WriteFile(filepath.Join(inner, "not-ours"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(at, "media", "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	name := "\xff.mkv"
	if err := os.WriteFile(filepath.Join(at, "media", name), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := newScan(t, v, at)
	r := b.scan(watch.Options{Full: true})
	if !slices.Equal(r.OtherMounts, []string{"inner"}) || r.Files != 2 || r.Hashed != 2 || r.Ignored != 1 {
		t.Errorf("%+v", r)
	}
	if got := b.one("SELECT count(*) FROM files WHERE lpath = ?", []byte("inner/not-ours")); got.Int64 != 0 {
		t.Error("a file of another mount was recorded")
	}
	if got := b.one("SELECT count(*) FROM files WHERE lpath = ? AND lpath_key IS NULL", []byte(name)); got.Int64 != 1 {
		t.Error("a name that is not UTF-8 is not there as it is")
	}
}

// TestScanOfAFailingDisk: dm-error, after the caches are gone. Whatever a check or a walk finds,
// the disk is missing, and no copy is said to be absent (invariant 4).
func TestScanOfAFailingDisk(t *testing.T) {
	needRoot(t)
	if _, err := exec.LookPath("dmsetup"); err != nil {
		t.Skip("no dmsetup")
	}
	for _, tc := range []struct {
		name   string
		fs     names.Filesystem
		driver string
	}{
		{"ext4", names.Ext4, ""},
		{"xfs", names.XFS, ""},
		{"ntfs3", names.NTFS, "ntfs3"},
		{"ntfs-3g", names.NTFS, "ntfs-3g"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.driver == "ntfs3" {
				ntfs3Type(t)
			}
			v := newVolume(t, tc.fs)
			sectors := must(t, "blockdev", "--getsz", v.device)
			name := "atoll-" + strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
			must(t, "dmsetup", "create", "--noudevsync", "--noudevrules", name, "--table", "0 "+sectors+" linear "+v.device+" 0")
			must(t, "dmsetup", "mknodes")
			t.Cleanup(func() {
				_ = exec.CommandContext(context.Background(), "dmsetup", "remove", "-f", "--noudevsync", name).Run()
			})
			mapped := &volume{fs: v.fs, device: "/dev/mapper/" + name, uuid: v.uuid}
			at := mountPoint(t)
			mountVolume(t, mapped, tc.driver, at, optionsFor(t, tc.driver)...)
			mkRoot(t, at, "a", "sub/b")
			must(t, "sync")
			b := newScan(t, mapped, at)
			if r := b.scan(watch.Options{}); r.New != 2 {
				t.Fatalf("the first scan: %+v", r)
			}
			must(t, "dmsetup", "suspend", "--noudevsync", name)
			must(t, "dmsetup", "reload", name, "--table", "0 "+sectors+" error")
			must(t, "dmsetup", "resume", "--noudevsync", name)
			_ = exec.CommandContext(context.Background(), "sync").Run()
			if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0o644); err != nil {
				t.Fatal(err)
			}
			r := b.scan(watch.Options{})
			if r.Verdict.State != disks.Missing || r.Walked {
				t.Errorf("%+v", r)
			}
			if len(r.Absent) != 0 || r.Known != 0 {
				t.Errorf("concluded from a disk that does not answer: %+v", r)
			}
			if got := b.one("SELECT count(*) FROM copies"); got.Int64 != 2 {
				t.Errorf("%d copies", got.Int64)
			}
		})
	}
}
