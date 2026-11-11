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
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/watch"
)

// passBench is a pool of two real filesystems: an ext4 disk, and the driver under test, with a debounce
// short enough to wait for.
type passBench struct {
	t       *testing.T
	db      *sql.DB
	config  *config.Config
	keep    string
	at      string
	machine disks.Machine
}

const shortDebounce = 1100 * time.Millisecond

func newPassBench(t *testing.T, v *volume, at string) *passBench {
	t.Helper()
	kept := newVolume(t, names.Ext4)
	keep := mountPoint(t)
	mountVolume(t, kept, "", keep)
	for _, top := range []string{keep, at} {
		if err := os.MkdirAll(filepath.Join(top, "media"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "catalog.db")
	text := fmt.Sprintf("pool_id = \"p\"\ncatalog = %q\n[view]\nmount = %q\n", file, filepath.Join(dir, "pool")) +
		fmt.Sprintf("[[disk]]\nid = \"k\"\nuuid = %q\nmount = %q\nfs = %q\n", kept.uuid, keep, kept.fs) +
		fmt.Sprintf("[[disk]]\nid = \"v\"\nuuid = %q\nmount = %q\nfs = %q\n", v.uuid, at, v.fs) +
		"[deletes]\ndebounce = \"1s\"\n[deletes.mass_guard]\npercent = \"100%\"\n[modifies.mass_guard]\npercent = \"100%\"\n"
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
	return &passBench{t: t, db: db, config: c, keep: keep, at: at, machine: machine}
}

func (b *passBench) scan(full bool) {
	b.t.Helper()
	if _, err := watch.Scan(b.t.Context(), b.db, b.config, b.machine, watch.Options{Full: full}); err != nil {
		b.t.Fatal(err)
	}
}

func (b *passBench) pass() *jobs.Outcome {
	b.t.Helper()
	out, err := jobs.Run(b.t.Context(), b.db, b.config, b.machine, true)
	if err != nil {
		b.t.Fatal(err)
	}
	return out
}

// settle looks, lets the debounce go by, and looks again.
func (b *passBench) settle() *jobs.Outcome {
	b.t.Helper()
	b.pass()
	time.Sleep(shortDebounce)
	return b.pass()
}

func (b *passBench) text(query string, args ...any) string {
	b.t.Helper()
	var s sql.NullString
	if err := b.db.QueryRowContext(b.t.Context(), query, args...).Scan(&s); err != nil {
		b.t.Fatal(err)
	}
	return s.String
}

func write(t *testing.T, top, logical, text string) {
	t.Helper()
	full := filepath.Join(top, "media", logical)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rename(t *testing.T, top, from, to string) {
	t.Helper()
	full := filepath.Join(top, "media", to)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(top, "media", from), full); err != nil {
		t.Fatal(err)
	}
}

func listed(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	slices.Sort(out)
	return out
}

func wantContent(t *testing.T, top, logical, text string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(top, "media", logical))
	if err != nil || string(got) != text {
		t.Errorf("%q: %q, %v", logical, got, err)
	}
}

// TestRenamesOnEachDriver: a file, a name in new folders, a case-only name and a folder with a file only
// the driver's disk holds, renamed on the other disk, reach each driver, and what was done is still so
// through a cold cache. Each file keeps its inode, its date and its creation time (specs.md §15.12).
func TestRenamesOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newPassBench(t, v, at)
			for _, top := range []string{b.keep, at} {
				write(t, top, "a", "file a")
				write(t, top, "Film.mkv", "film")
				write(t, top, "d/x", "x")
				write(t, top, "n/m", "m")
			}
			write(t, at, "d/y", "y")
			when := time.Unix(1500000000, 0)
			if err := os.Chtimes(filepath.Join(at, "media", "a"), when, when); err != nil {
				t.Fatal(err)
			}
			b.scan(true)
			before := b.text("SELECT c.file_key FROM copies c JOIN files f ON f.id = c.file_id WHERE c.disk_id = 'v' AND f.lpath = CAST('a' AS BLOB)")
			birth := b.text("SELECT c.btime_ns FROM copies c JOIN files f ON f.id = c.file_id WHERE c.disk_id = 'v' AND f.lpath = CAST('a' AS BLOB)")
			rename(t, b.keep, "a", "s/t/b")
			rename(t, b.keep, "Film.mkv", "film.mkv")
			if err := os.Rename(filepath.Join(b.keep, "media", "d"), filepath.Join(b.keep, "media", "e")); err != nil {
				t.Fatal(err)
			}
			out := b.settle()
			if out.Partial {
				t.Fatalf("partial: %+v", out)
			}
			for _, op := range out.Operations {
				if op.Outcome != jobs.OutcomeRenamed {
					t.Errorf("%+v", op)
				}
			}
			folder := slices.ContainsFunc(out.Operations, func(op jobs.Operation) bool { return op.Kind == jobs.KindFolder && op.Path == "d" && op.To == "e" })
			if !folder {
				t.Errorf("no folder operation: %+v", out.Operations)
			}
			dropCaches(t)
			wantContent(t, at, "s/t/b", "file a")
			wantContent(t, at, "film.mkv", "film")
			wantContent(t, at, "e/x", "x")
			wantContent(t, at, "e/y", "y")
			wantContent(t, at, "n/m", "m")
			if got := listed(t, filepath.Join(at, "media")); !slices.Equal(got, []string{"e", "film.mkv", "n", "s"}) {
				t.Errorf("listed %v", got)
			}
			st, err := os.Stat(filepath.Join(at, "media", "s", "t", "b"))
			if err != nil {
				t.Fatal(err)
			}
			if !st.ModTime().Equal(when) {
				t.Errorf("the date moved to %v", st.ModTime())
			}
			row := "SELECT c.file_key FROM copies c JOIN files f ON f.id = c.file_id WHERE c.disk_id = 'v' AND f.lpath = ?"
			if got := b.text(row, []byte("s/t/b")); got != before {
				t.Errorf("inode %s, was %s", got, before)
			}
			if got := b.text("SELECT c.btime_ns FROM copies c JOIN files f ON f.id = c.file_id WHERE c.disk_id = 'v' AND f.lpath = ?", []byte("s/t/b")); got != birth {
				t.Errorf("creation time %s, was %s", got, birth)
			}
			if got := b.text("SELECT count(*) FROM copies WHERE old_lpath IS NOT NULL OR gone_ns IS NOT NULL"); got != "0" {
				t.Errorf("%s rows behind", got)
			}
			if out := b.pass(); len(out.Operations)+len(out.Decisions) != 0 {
				t.Errorf("another pass did more: %+v", out)
			}
		})
	}
}

// TestRenamesFoundOnEachDriver: each driver says what a rename made on it looks like, a folder with no
// creation time being the one exception: what moved follows, and the old folder stays.
func TestRenamesFoundOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newPassBench(t, v, at)
			for _, top := range []string{b.keep, at} {
				write(t, top, "a", "file a")
				write(t, top, "d/x", "x")
			}
			write(t, b.keep, "d/y", "y")
			b.scan(true)
			rename(t, at, "a", "b")
			if err := os.Rename(filepath.Join(at, "media", "d"), filepath.Join(at, "media", "e")); err != nil {
				t.Fatal(err)
			}
			out := b.settle()
			if out.Partial {
				t.Fatalf("partial: %+v", out)
			}
			dropCaches(t)
			wantContent(t, b.keep, "b", "file a")
			wantContent(t, b.keep, "e/x", "x")
			wantContent(t, b.keep, "e/y", "y")
			if got := listed(t, filepath.Join(b.keep, "media")); !slices.Equal(got, []string{"b", "e"}) {
				t.Errorf("listed %v", got)
			}
		})
	}
}

// TestDeletionsOnEachDriver: gone from one disk with both verified, the other goes to the trash, in each
// direction, and through a cold cache the file is there.
func TestDeletionsOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newPassBench(t, v, at)
			for _, top := range []string{b.keep, at} {
				write(t, top, "a", "file a")
				write(t, top, "c", "file c")
			}
			b.scan(true)
			if err := os.Remove(filepath.Join(b.keep, "media", "a")); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(at, "media", "c")); err != nil {
				t.Fatal(err)
			}
			out := b.settle()
			if out.Partial {
				t.Fatalf("partial: %+v", out)
			}
			dropCaches(t)
			entries, err := catalog.TrashEntries(t.Context(), b.db)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 2 {
				t.Fatalf("%d entries", len(entries))
			}
			for i := range entries {
				top := at
				if entries[i].Disk == "k" {
					top = b.keep
				}
				data, err := os.ReadFile(filepath.Join(top, "Atoll", "trash", filepath.FromSlash(entries[i].TrashPath)))
				if err != nil || !strings.HasPrefix(string(data), "file ") {
					t.Errorf("%+v: %q, %v", entries[i], data, err)
				}
			}
			if got := b.text("SELECT count(*) FROM files"); got != "0" {
				t.Errorf("%s files left", got)
			}
		})
	}
}

// TestADeviceThatFailsDuringARename: whatever a driver answered while the device was dead, once it is
// back the next pass finds the copy at one name, and the catalog says that name (specs.md §15.12, §17.2).
// To another folder, ntfs3 can be left with both names leading to the file, which has one link: the
// reprise then concludes nothing, and the operation stays planned until a repair of the volume.
func TestADeviceThatFailsDuringARename(t *testing.T) {
	needRoot(t)
	for _, tc := range failingDrivers {
		for _, to := range []string{"b", "s/b"} {
			for _, point := range []string{"journaled", "moved", "target-synced"} {
				t.Run(tc.name+"/"+to+"/"+point, func(t *testing.T) {
					options := driverOptions(t, tc.driver)
					m := newMapped(t, tc.fs)
					at := mountPoint(t)
					mountVolume(t, m.volume, tc.driver, at, options...)
					b := newPassBench(t, m.volume, at)
					write(t, b.keep, "a", "content")
					write(t, at, "a", "content")
					b.scan(false)
					must(t, "sync")
					rename(t, b.keep, "a", to)
					b.pass()
					time.Sleep(shortDebounce)
					real := b.machine
					b.machine = &failingAt{Machine: real, point: point, fire: func() { m.fail(t) }}
					out := b.pass()
					t.Logf("while it was dead: %+v", out.Operations)
					state := b.text("SELECT COALESCE(max(state), '') FROM ops WHERE kind = 'rename'")
					if state == "done" && point == "journaled" {
						t.Error("done, with the device dead before the move")
					}
					bringBack(t, m, tc.driver, at, options)
					b.machine = real
					after := b.settle()
					dropCaches(t)
					source, target := filepath.Join(at, "media", "a"), filepath.Join(at, "media", filepath.FromSlash(to))
					if tc.driver == "ntfs3" {
						skipWhereNamesDisagree(t, source, target)
						if concludesNothing(t, b.text("SELECT COALESCE(max(state), '') FROM ops WHERE kind = 'rename'"), source, target) {
							return
						}
					}
					renameLeft(t, b, after, source, target)
				})
			}
		}
	}
}

// renameLeft requires what a rename that a device failure interrupted must leave once it is resolved: the
// file at its target, nothing at its source, and nothing planned or behind.
func renameLeft(t *testing.T, b *passBench, after *jobs.Outcome, source, target string) {
	t.Helper()
	if got := b.text("SELECT count(*) FROM ops WHERE state = 'planned'"); got != "0" {
		t.Errorf("%s operations left planned: %+v", got, after)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "content" {
		t.Errorf("at the target: %q, %v (%+v)", data, err, after)
	}
	if _, err := os.Lstat(source); !os.IsNotExist(err) {
		t.Errorf("at the source: %v", err)
	}
	if got := b.text("SELECT count(*) FROM copies WHERE old_lpath IS NOT NULL"); got != "0" {
		t.Errorf("%s rows behind: %+v", got, after)
	}
}

// namesDisagree says whether two names lead to one file that has a single link: one of them is a name its
// record no longer holds (measured on ntfs3).
func namesDisagree(a, b string) bool {
	var sa, sb syscall.Stat_t
	return syscall.Lstat(a, &sa) == nil && syscall.Lstat(b, &sb) == nil && sa.Ino == sb.Ino && sa.Nlink == 1
}

// concludesNothing says whether two names lead to one file that has a single link: a name its record no
// longer holds, which ntfs3 leaves when a device fails between the syncs of a move's two folders
// (measured). The reprise must then have concluded nothing, and the content be whole at both names.
func concludesNothing(t *testing.T, state, a, b string) bool {
	t.Helper()
	if !namesDisagree(a, b) {
		return false
	}
	t.Logf("a name and its record disagree: %s and %s", a, b)
	if state != "planned" {
		t.Errorf("the operation is %q, on names that disagree", state)
	}
	for _, path := range []string{a, b} {
		if data, err := os.ReadFile(path); err != nil || string(data) != "content" {
			t.Errorf("%s: %q, %v", path, data, err)
		}
	}
	return true
}

// TestADeviceThatFailsDuringAFolderRename: the same for a folder renamed whole, with a file that only the
// driver's disk holds. On NTFS a folder's own record holds its name, as a file's does (specs.md §15.12).
func TestADeviceThatFailsDuringAFolderRename(t *testing.T) {
	needRoot(t)
	for _, tc := range failingDrivers {
		for _, point := range []string{"journaled", "moved", "target-synced"} {
			t.Run(tc.name+"/"+point, func(t *testing.T) {
				options := driverOptions(t, tc.driver)
				m := newMapped(t, tc.fs)
				at := mountPoint(t)
				mountVolume(t, m.volume, tc.driver, at, options...)
				b := newPassBench(t, m.volume, at)
				write(t, b.keep, "d/x", "x")
				write(t, at, "d/x", "x")
				write(t, at, "d/y", "y")
				b.scan(false)
				must(t, "sync")
				if err := os.Rename(filepath.Join(b.keep, "media", "d"), filepath.Join(b.keep, "media", "e")); err != nil {
					t.Fatal(err)
				}
				b.pass()
				time.Sleep(shortDebounce)
				real := b.machine
				b.machine = &failingAt{Machine: real, point: point, fire: func() { m.fail(t) }}
				out := b.pass()
				t.Logf("while it was dead: %+v", out.Operations)
				state := b.text("SELECT COALESCE(max(state), '') FROM ops WHERE kind = 'rename'")
				if state == "done" && point == "journaled" {
					t.Error("done, with the device dead before the move")
				}
				bringBack(t, m, tc.driver, at, options)
				b.machine = real
				after := b.settle()
				dropCaches(t)
				old, renamed := filepath.Join(at, "media", "d"), filepath.Join(at, "media", "e")
				if tc.driver == "ntfs3" {
					skipWhereNamesDisagree(t, old, renamed, filepath.Join(renamed, "x"), filepath.Join(renamed, "y"))
				}
				if got := b.text("SELECT count(*) FROM ops WHERE state = 'planned'"); got != "0" {
					t.Errorf("%s operations left planned: %+v", got, after)
				}
				wantContent(t, at, "e/x", "x")
				wantContent(t, at, "e/y", "y")
				if _, err := os.Lstat(old); !os.IsNotExist(err) {
					t.Errorf("the old folder: %v (%+v)", err, after)
				}
				if got := b.text("SELECT count(*) FROM copies WHERE old_lpath IS NOT NULL"); got != "0" {
					t.Errorf("%s rows behind: %+v", got, after)
				}
			})
		}
	}
}

// bringBack does what a user does after a USB drop: what is left of the mount goes, the device comes back,
// and the volume is mounted again. One that does not mount skips the test.
func bringBack(t *testing.T, m *mappedVolume, driver, at string, options []string) {
	t.Helper()
	_ = exec.CommandContext(context.Background(), "umount", "-l", at).Run()
	time.Sleep(time.Second)
	if strings.HasSuffix(driver, "3g") {
		_ = exec.CommandContext(context.Background(), "pkill", "-9", "-f", driver+" "+m.device+" ").Run()
		time.Sleep(time.Second)
	}
	m.heal(t)
	// Dirty, ntfs3 refuses to mount without force.
	if driver == "ntfs3" {
		options = append(slices.Clone(options), "force")
	}
	if err := remount(t, m.volume, driver, at, options); err != nil {
		t.Skipf("the volume did not mount again: %v", err)
	}
}

// skipWhereNamesDisagree: ntfs3 keeps no journal of its own, and writes lost with the device can leave a
// name and its record that disagree, which it answers with ESTALE or EINVAL (measured). Atoll concludes
// nothing from that, and only a repair can say more.
func skipWhereNamesDisagree(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil && !os.IsNotExist(err) {
			t.Skipf("a name and its record disagree: %v", err)
		}
		if err == nil {
			_ = file.Close()
		}
	}
}
