//go:build loopdev

package loopdev

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/trash"
	"github.com/peondusud/atoll/internal/watch"
)

// trashBench is a pool of two real filesystems, each file wanted once, so that the copy on the
// second, the driver under test, is surplus.
type trashBench struct {
	t       *testing.T
	db      *sql.DB
	config  *config.Config
	keep    string
	at      string
	machine disks.Machine
}

func newTrashBench(t *testing.T, v *volume, at string) *trashBench {
	t.Helper()
	kept := newVolume(t, names.Ext4)
	keep := mountPoint(t)
	mountVolume(t, kept, "", keep)
	if err := os.MkdirAll(filepath.Join(keep, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "catalog.db")
	text := fmt.Sprintf("pool_id = \"p\"\ncatalog = %q\n[view]\nmount = %q\n", file, filepath.Join(dir, "pool")) +
		fmt.Sprintf("[[disk]]\nid = \"k\"\nuuid = %q\nmount = %q\nfs = %q\n", kept.uuid, keep, kept.fs) +
		fmt.Sprintf("[[disk]]\nid = \"v\"\nuuid = %q\nmount = %q\nfs = %q\n", v.uuid, at, v.fs) +
		"[[rule]]\nlabel = \"once\"\npath = \"**\"\nreplicas = 0\n"
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
	machine, err := disks.NewMachine()
	if err != nil {
		t.Fatal(err)
	}
	return &trashBench{t: t, db: db, config: c, keep: keep, at: at, machine: machine}
}

func (b *trashBench) scanFull() {
	b.t.Helper()
	if _, err := watch.Scan(b.t.Context(), b.db, b.config, b.machine, watch.Options{Full: true}); err != nil {
		b.t.Fatal(err)
	}
}

func (b *trashBench) session(apply bool) *trash.Session {
	b.t.Helper()
	s, err := trash.Open(b.t.Context(), b.db, b.config, b.machine, apply)
	if err != nil {
		b.t.Fatal(err)
	}
	b.t.Cleanup(s.Close)
	return s
}

func (b *trashBench) prune() map[string]string {
	b.t.Helper()
	s := b.session(true)
	defer s.Close()
	outcomes := map[string]string{}
	if _, err := s.Prune(b.t.Context(), func(i *trash.PruneItem) error {
		outcomes[i.Path] = i.Outcome
		return nil
	}); err != nil {
		b.t.Fatal(err)
	}
	if err := s.Finish(b.t.Context()); err != nil {
		b.t.Fatal(err)
	}
	return outcomes
}

func (b *trashBench) text(query string, args ...any) string {
	b.t.Helper()
	var s sql.NullString
	if err := b.db.QueryRowContext(b.t.Context(), query, args...).Scan(&s); err != nil {
		b.t.Fatal(err)
	}
	return s.String
}

func dropCaches(t *testing.T) {
	t.Helper()
	_ = exec.CommandContext(context.Background(), "sync").Run()
	if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeBoth(t *testing.T, b *trashBench, logical, text string) {
	t.Helper()
	for _, top := range []string{b.keep, b.at} {
		full := filepath.Join(top, "media", logical)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

var trashDrivers = []struct {
	name   string
	fs     names.Filesystem
	driver string
}{
	{"ext4", names.Ext4, ""},
	{"xfs", names.XFS, ""},
	{"btrfs", names.Btrfs, ""},
	{"ntfs3", names.NTFS, "ntfs3"},
	{"ntfs-3g", names.NTFS, "ntfs-3g"},
	{"lowntfs-3g", names.NTFS, "lowntfs-3g"},
}

func driverOptions(t *testing.T, driver string) []string {
	t.Helper()
	if driver == "ntfs3" {
		ntfs3Type(t)
		return ntfs3Options(t)
	}
	return nil
}

// TestPruneOnEachDriver: a copy goes to the trash, comes back and goes for good, on each driver, and
// what was done is still so through a cold cache (specs.md §17.1, §17.4).
func TestPruneOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newTrashBench(t, v, at)
			writeBoth(t, b, "d/a", "content")
			long := strings.Repeat("l", 255)
			writeBoth(t, b, long, "long")
			when := time.Unix(1500000000, 0)
			folder := filepath.Join(at, "media", "d")
			if err := os.Chtimes(folder, when, when); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(folder, "a")
			before, err := os.Stat(source)
			if err != nil {
				t.Fatal(err)
			}
			b.scanFull()
			if got := disks.Confirm(b.machine, &b.config.Disks[1]); got.State != disks.Online {
				t.Fatalf("not confirmed: %+v", got)
			}

			outcomes := b.prune()
			if outcomes["d/a"] != trash.OutcomeTrashed || outcomes[long] != trash.OutcomeTrashed {
				t.Fatalf("%v", outcomes)
			}
			dropCaches(t)
			if _, err := os.Lstat(source); !os.IsNotExist(err) {
				t.Errorf("the source: %v", err)
			}
			target := filepath.Join(at, "Atoll", "trash", filepath.FromSlash(b.text("SELECT CAST(path2 AS TEXT) FROM ops WHERE CAST(path AS TEXT) = 'd/a'")))
			after, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if !after.ModTime().Equal(before.ModTime()) || !os.SameFile(before, after) {
				// SameFile compares device and inode: the move kept them.
				t.Errorf("the file changed on the way: %v, %v", before.ModTime(), after.ModTime())
			}
			folderInfo, err := os.Stat(folder)
			if err != nil {
				t.Fatal(err)
			}
			if !folderInfo.ModTime().Equal(when) {
				t.Errorf("the folder's time: %v", folderInfo.ModTime())
			}

			s := b.session(true)
			got := ""
			if _, err := s.Restore(t.Context(), "d/a", func(i *trash.EntryItem) error { got = i.Outcome; return nil }); err != nil {
				t.Fatal(err)
			}
			if err := s.Finish(t.Context()); err != nil {
				t.Fatal(err)
			}
			s.Close()
			if got != trash.OutcomeRestored {
				t.Fatalf("restore: %q", got)
			}
			dropCaches(t)
			data, err := os.ReadFile(source)
			if err != nil || string(data) != "content" {
				t.Errorf("back: %q, %v", data, err)
			}
			if folderInfo, err = os.Stat(folder); err != nil || !folderInfo.ModTime().Equal(when) {
				t.Errorf("the folder's time, back: %v, %v", folderInfo.ModTime(), err)
			}

			var zero time.Duration
			s = b.session(true)
			purged := 0
			if _, err := s.Purge(t.Context(), &zero, func(i *trash.EntryItem) error {
				if i.Outcome != trash.OutcomePurged {
					t.Errorf("purge: %q", i.Outcome)
				}
				purged++
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			s.Close()
			if purged != 1 {
				t.Errorf("%d purged", purged)
			}
			dropCaches(t)
			names, err := os.ReadDir(filepath.Join(at, "Atoll", "trash"))
			if err != nil || len(names) != 0 {
				t.Errorf("the trash holds %v, %v", names, err)
			}
		})
	}
}

// TestPruneByHandlesPastEveryLimit: 5,025 bytes of path, as §15.8 says the trash needs no check.
func TestPruneByHandlesPastEveryLimit(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newTrashBench(t, v, at)
			name := strings.Repeat("n", 200)
			parts := make([]string, 25)
			for i := range parts {
				parts[i] = name
			}
			for _, top := range []string{b.keep, b.at} {
				if err := os.MkdirAll(filepath.Join(top, "media"), 0o755); err != nil {
					t.Fatal(err)
				}
				deepFile(t, filepath.Join(top, "media"), parts)
			}
			b.scanFull()
			logical := strings.Join(parts, "/") + "/deep"
			if got := b.prune()[logical]; got != trash.OutcomeTrashed {
				t.Errorf("%q", got)
			}
			if got := b.text("SELECT state FROM ops"); got != "done" {
				t.Errorf("the operation is %q", got)
			}
		})
	}
}

func deepFile(t *testing.T, root string, parts []string) {
	t.Helper()
	dir, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range parts {
		if err := unix.Mkdirat(dir, name, 0o755); err != nil {
			t.Fatal(err)
		}
		next, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_ = unix.Close(dir)
		dir = next
	}
	defer func() { _ = unix.Close(dir) }()
	file, err := unix.Openat(dir, "deep", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_ = unix.Close(file)
}

// TestRestoreStaysOnItsDisk: a folder on the way that is another mount is not entered, as nothing
// confirmed it (specs.md §17.1). The path goes one folder past it, so that a restore that entered it
// would make a folder there, where the kernel's EXDEV would refuse only the move.
func TestRestoreStaysOnItsDisk(t *testing.T) {
	needRoot(t)
	v := newVolume(t, names.Ext4)
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	b := newTrashBench(t, v, at)
	writeBoth(t, b, "d/sub/a", "content")
	b.scanFull()
	if got := b.prune()["d/sub/a"]; got != trash.OutcomeTrashed {
		t.Fatalf("%q", got)
	}
	other := filepath.Join(at, "media", "d")
	must(t, "mount", "-t", "tmpfs", "tmpfs", other)
	t.Cleanup(func() { _ = exec.CommandContext(context.Background(), "umount", "-l", other).Run() })
	s := b.session(true)
	got := ""
	if _, err := s.Restore(t.Context(), "d/sub/a", func(i *trash.EntryItem) error { got = i.Outcome; return nil }); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if got != trash.OutcomeIOError {
		t.Errorf("%q", got)
	}
	names, err := os.ReadDir(other)
	if err != nil || len(names) != 0 {
		t.Errorf("the other mount holds %v, %v", names, err)
	}
	if n := b.text("SELECT count(*) FROM ops WHERE kind IN ('restore', 'mkdir')"); n != "0" {
		t.Errorf("%s operations journaled", n)
	}
}

// TestATrashOnAnotherMount: the same on the way to the trash. The operation is over at once, so that
// nothing piles up from one run to the next.
func TestATrashOnAnotherMount(t *testing.T) {
	needRoot(t)
	v := newVolume(t, names.Ext4)
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	b := newTrashBench(t, v, at)
	writeBoth(t, b, "a", "content")
	b.scanFull()
	other := filepath.Join(at, "Atoll", "trash")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	must(t, "mount", "-t", "tmpfs", "tmpfs", other)
	t.Cleanup(func() { _ = exec.CommandContext(context.Background(), "umount", "-l", other).Run() })
	for range 2 {
		if got := b.prune()["a"]; got != trash.OutcomeIOError {
			t.Fatalf("%q", got)
		}
	}
	names, err := os.ReadDir(other)
	if err != nil || len(names) != 0 {
		t.Errorf("the other mount holds %v, %v", names, err)
	}
	if n := b.text("SELECT count(*) FROM ops WHERE state = 'undone'"); n != "2" {
		t.Errorf("%s undone", n)
	}
	if n := b.text("SELECT count(*) FROM ops WHERE state = 'planned'"); n != "0" {
		t.Errorf("%s left planned", n)
	}
	if _, err := os.Lstat(filepath.Join(at, "media", "a")); err != nil {
		t.Errorf("the copy: %v", err)
	}
}

// TestAnotherFilesystemAtTheMountPath: its top has the inode of the top of any ext4, so only its device
// and mount tell, and the session stops writing through a handle that still answers (specs.md §10).
func TestAnotherFilesystemAtTheMountPath(t *testing.T) {
	needRoot(t)
	v := newVolume(t, names.Ext4)
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	b := newTrashBench(t, v, at)
	writeBoth(t, b, "a", "one")
	writeBoth(t, b, "b", "two")
	b.scanFull()
	other := newVolume(t, names.Ext4)
	s := b.session(true)
	var outcomes []string
	if _, err := s.Prune(t.Context(), func(i *trash.PruneItem) error {
		outcomes = append(outcomes, i.Outcome)
		if len(outcomes) == 1 {
			must(t, "umount", "-l", at)
			mountVolume(t, other, "", at)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if len(outcomes) != 2 || outcomes[0] != trash.OutcomeTrashed || outcomes[1] != trash.OutcomeIOError {
		t.Errorf("%v", outcomes)
	}
	if _, err := os.Lstat(filepath.Join(at, "Atoll")); !os.IsNotExist(err) {
		t.Errorf("it wrote to what stands at the mount path now: %v", err)
	}
	if n := b.text("SELECT count(*) FROM ops WHERE state = 'planned'"); n != "1" {
		t.Errorf("%s left planned", n)
	}
}

// TestPruneOfARefusedName: written before the disk was mounted with windows_names, it cannot reach
// the trash, and stays (specs.md §17, §17.1).
func TestPruneOfARefusedName(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers[3:] {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			first := mountPoint(t)
			mountVolume(t, v, tc.driver, first, options...)
			if err := os.MkdirAll(filepath.Join(first, "media"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(first, "media", "a:b.mkv"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			must(t, "umount", first)
			time.Sleep(time.Second)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, append(options, "windows_names")...)
			b := newTrashBench(t, v, at)
			if err := os.WriteFile(filepath.Join(b.keep, "media", "a:b.mkv"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			b.scanFull()
			if got := b.prune()["a:b.mkv"]; got != trash.OutcomeNameRefused {
				t.Fatalf("%q", got)
			}
			if got := b.text("SELECT state FROM ops"); got != "undone" {
				t.Errorf("the operation is %q", got)
			}
			dropCaches(t)
			data, err := os.ReadFile(filepath.Join(at, "media", "a:b.mkv"))
			if err != nil || string(data) != "x" {
				t.Errorf("the copy: %q, %v", data, err)
			}
			names, err := os.ReadDir(filepath.Join(at, "Atoll", "trash"))
			if err != nil || len(names) != 0 {
				t.Errorf("the trash holds %v, %v", names, err)
			}
		})
	}
}

type mappedVolume struct {
	*volume
	name, sectors, real string
}

func newMapped(t *testing.T, fs names.Filesystem, extra ...string) *mappedVolume {
	t.Helper()
	v := newVolume(t, fs, extra...)
	sectors := must(t, "blockdev", "--getsz", v.device)
	name := "atoll-" + strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	must(t, "dmsetup", "create", "--noudevsync", "--noudevrules", name, "--table", "0 "+sectors+" linear "+v.device+" 0")
	must(t, "dmsetup", "mknodes")
	t.Cleanup(func() {
		_ = exec.CommandContext(context.Background(), "dmsetup", "remove", "-f", "--noudevsync", name).Run()
	})
	return &mappedVolume{volume: &volume{fs: v.fs, device: "/dev/mapper/" + name, uuid: v.uuid}, name: name, sectors: sectors, real: v.device}
}

func (m *mappedVolume) table(t *testing.T, table string) {
	t.Helper()
	// --nolockfs: suspend otherwise syncs the filesystem first, and a move answered on a live device
	// would always be durable by the time the device dies, which is what this is here to break.
	must(t, "dmsetup", "suspend", "--noudevsync", "--nolockfs", m.name)
	must(t, "dmsetup", "reload", m.name, "--table", table)
	must(t, "dmsetup", "resume", "--noudevsync", m.name)
}

func (m *mappedVolume) fail(t *testing.T) {
	t.Helper()
	m.table(t, "0 "+m.sectors+" error")
}

func (m *mappedVolume) heal(t *testing.T) {
	t.Helper()
	m.table(t, "0 "+m.sectors+" linear "+m.real+" 0")
}

// failing writes in the middle of an operation, once it has reached a point.
type failingAt struct {
	disks.Machine
	point string
	fire  func()
	fired bool
}

func (f *failingAt) Checkpoint(point string) {
	if point == f.point && !f.fired {
		f.fired = true
		f.fire()
	}
}

var failingDrivers = []struct {
	name   string
	fs     names.Filesystem
	driver string
}{
	{"ext4", names.Ext4, ""},
	{"xfs", names.XFS, ""},
	{"ntfs3", names.NTFS, "ntfs3"},
	{"ntfs-3g", names.NTFS, "ntfs-3g"},
}

// TestConfirmationOfAFailingDevice: the mount check still answers from the cache, the read past it
// does not, and nothing is written, not even a line of the journal (specs.md §10).
func TestConfirmationOfAFailingDevice(t *testing.T) {
	needRoot(t)
	for _, tc := range failingDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			m := newMapped(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, m.volume, tc.driver, at, options...)
			b := newTrashBench(t, m.volume, at)
			writeBoth(t, b, "a", "content")
			b.scanFull()
			must(t, "sync")
			if got := disks.Confirm(b.machine, &b.config.Disks[1]); got.State != disks.Online {
				t.Fatalf("healthy: %+v", got)
			}
			m.fail(t)
			d := &b.config.Disks[1]
			if got := disks.Check(b.machine, d); got.State != disks.Online {
				t.Logf("the cached check saw it already: %+v", got)
			}
			got := disks.Confirm(b.machine, d)
			if got.State != disks.Missing || got.Reason != disks.DeviceUnread {
				t.Errorf("confirmation: %+v", got)
			}
			if outcome := b.prune()["a"]; outcome != "disk-unavailable" {
				t.Errorf("%q", outcome)
			}
			if got := b.text("SELECT count(*) FROM ops"); got != "0" {
				t.Errorf("%s operations on a dead disk", got)
			}
			// A scan finds it missing too, whether its walk fails or the cache answers the walk and the confirmation
			// does not: nothing it walked is recorded (specs.md §15.10).
			out, err := watch.Scan(t.Context(), b.db, b.config, b.machine, watch.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if r := out.Disks[1]; r.Verdict.State != disks.Missing {
				t.Errorf("scan: %+v", r.Verdict)
			}
			if got := b.text("SELECT count(*) FROM copies WHERE disk_id = 'v' AND gone_kind IS NOT NULL"); got != "0" {
				t.Errorf("%s clocks started on a dead disk", got)
			}
			m.heal(t)
		})
	}
}

// TestADeviceThatFailsDuringAnOperation: whatever a driver answered while the device was dead, once
// it is back the reprise finds the file in one place, and the catalog says the same place (specs.md
// §17.1, §17.2). A driver that said done on the strength of the move alone is what this catches: XFS
// answers a move on a dead device with success, so the point that matters is the one where the folders
// are made and the move is still to come.
func TestADeviceThatFailsDuringAnOperation(t *testing.T) {
	needRoot(t)
	for _, tc := range failingDrivers {
		for _, point := range []string{"journaled", "trash-folders", "moved", "target-synced"} {
			t.Run(tc.name+"/"+point, func(t *testing.T) {
				deviceFailure(t, tc.fs, tc.driver, point, 1, func(*trashBench) {}, func(b *trashBench) string { return b.prune()["a"] })
			})
		}
	}
}

// TestADeviceThatFailsDuringARestore: the same on the way back.
func TestADeviceThatFailsDuringARestore(t *testing.T) {
	needRoot(t)
	for _, tc := range failingDrivers {
		for _, point := range []string{"journaled", "moved", "target-synced"} {
			t.Run(tc.name+"/"+point, func(t *testing.T) {
				deviceFailure(t, tc.fs, tc.driver, point, 2,
					func(b *trashBench) {
						if got := b.prune()["a"]; got != trash.OutcomeTrashed {
							t.Fatalf("%q", got)
						}
					},
					func(b *trashBench) string {
						s := b.session(true)
						defer s.Close()
						outcome := ""
						if _, err := s.Restore(t.Context(), "a", func(i *trash.EntryItem) error { outcome = i.Outcome; return nil }); err != nil {
							t.Fatal(err)
						}
						if err := s.Finish(t.Context()); err != nil {
							t.Fatal(err)
						}
						return outcome
					})
			})
		}
	}
}

// deviceFailure kills the device when an operation reaches a point, brings it back, lets a run resolve
// what was left, and requires the disk and the catalog to agree. op is the id of that operation.
func deviceFailure(t *testing.T, fs names.Filesystem, driver, point string, op int, prepare func(*trashBench), run func(*trashBench) string) {
	t.Helper()
	options := driverOptions(t, driver)
	m := newMapped(t, fs)
	at := mountPoint(t)
	mountVolume(t, m.volume, driver, at, options...)
	b := newTrashBench(t, m.volume, at)
	writeBoth(t, b, "a", "content")
	b.scanFull()
	prepare(b)
	must(t, "sync")
	real := b.machine
	b.machine = &failingAt{Machine: real, point: point, fire: func() { m.fail(t) }}
	outcome := run(b)
	state := b.text("SELECT state FROM ops WHERE id = ?", op)
	t.Logf("while it was dead: %s, %s", outcome, state)
	if state == "done" && (point == "journaled" || point == "trash-folders") {
		t.Error("done, with the device dead before the move")
	}

	_ = exec.CommandContext(context.Background(), "umount", "-l", at).Run()
	time.Sleep(time.Second)
	if strings.HasSuffix(driver, "3g") {
		_ = exec.CommandContext(context.Background(), "pkill", "-9", "-f", driver+" "+m.device+" ").Run()
		time.Sleep(time.Second)
	}
	m.heal(t)
	// Dirty, ntfs3 refuses to mount without force.
	if driver == "ntfs3" {
		options = append(options, "force")
	}
	if err := remount(t, m.volume, driver, at, options); err != nil {
		t.Skipf("the volume did not mount again: %v", err)
	}
	b.machine = real
	after := run(b)

	source := filepath.Join(at, "media", "a")
	dropCaches(t)
	// ntfs3 keeps no journal of its own: writes lost with the device can leave a name and its record
	// that disagree, which it answers with ESTALE or EINVAL (measured). Atoll concludes nothing from
	// that, and only a repair can say more.
	if driver == "ntfs3" {
		inTrash := filepath.Join(at, "Atoll", "trash", filepath.FromSlash(b.text("SELECT CAST(path2 AS TEXT) FROM ops WHERE id = 1")))
		for _, path := range []string{source, inTrash} {
			file, err := os.Open(path)
			if err != nil && !os.IsNotExist(err) {
				t.Skipf("a name and its record disagree (%v), and the operation is %s", err, b.text("SELECT state FROM ops WHERE id = ?", op))
			}
			if err == nil {
				_ = file.Close()
			}
		}
		if concludesNothing(t, b.text("SELECT state FROM ops WHERE id = ?", op), source, inTrash) {
			return
		}
	}
	_, sourceErr := os.Lstat(source)
	entries, err := catalog.TrashEntries(t.Context(), b.db)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.text("SELECT count(*) FROM ops WHERE state = 'planned'"); got != "0" {
		t.Errorf("%s operations left planned", got)
	}
	copies := b.text("SELECT count(*) FROM copies WHERE disk_id = 'v'")
	switch {
	case sourceErr == nil:
		// It is at its path: the catalog must say so, and hold no entry.
		if copies != "1" || len(entries) != 0 {
			t.Errorf("at its path, with %s rows and %d entries (%q)", copies, len(entries), after)
		}
	case os.IsNotExist(sourceErr):
		// It is in the trash: the entry must lead to it.
		if copies != "0" || len(entries) != 1 {
			t.Fatalf("gone, with %s rows and %d entries (%q)", copies, len(entries), after)
		}
		data, err := os.ReadFile(filepath.Join(at, "Atoll", "trash", filepath.FromSlash(entries[0].TrashPath)))
		if err != nil || string(data) != "content" {
			t.Errorf("the entry leads nowhere: %q, %v", data, err)
		}
	default:
		t.Fatal(sourceErr)
	}
}

func remount(t *testing.T, v *volume, driver, at string, options []string) error {
	t.Helper()
	opts := strings.Join(options, ",")
	var command []string
	switch driver {
	case "ntfs-3g", "lowntfs-3g":
		command = []string{driver}
	case "":
		command = []string{"mount"}
	default:
		command = []string{"mount", "-t", driver}
	}
	if opts != "" {
		command = append(command, "-o", opts)
	}
	command = append(command, v.device, at)
	return attempt(t, command[0], command[1:]...)
}
