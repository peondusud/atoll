//go:build loopdev

package loopdev

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
)

// The loop-device tests of the five remedies of 2026-11-22 (DONE.md, the TLA+ models of the view and of renames):
// each flaw's case on real drivers, through Atoll's own view where it is the view's.

// trashFiles gives the content of each file in a disk's trash, by its path there.
func trashFiles(t *testing.T, top string) map[string]string {
	t.Helper()
	files := map[string]string{}
	root := filepath.Join(top, "Atoll", "trash")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil
		case err != nil:
			return err
		case d.IsDir():
			return nil
		}
		data, err := os.ReadFile(path)
		rel, _ := filepath.Rel(root, path)
		files[rel] = string(data)
		return err
	})
	mustDo(t, err)
	return files
}

// viewPool is a pool of a disk k (ext4) and a disk v on the volume given, its view writing: Films/a.mkv made on v by a
// user, so that v holds its original, the copy the view presents, and replicated to k.
func viewPool(t *testing.T, v *volume, at string) *passBench {
	t.Helper()
	b := newPassBenchWith(t, v, at, "[defaults]\nsettle = \"0s\"\n")
	write(t, at, "Films/a.mkv", "a film")
	b.scan(false)
	catchUp(b)
	b.scan(false)
	if got := b.text("SELECT group_concat(disk_id || ' ' || created_by, ',') FROM (SELECT * FROM copies ORDER BY disk_id)"); got != "k atoll,v user" {
		t.Fatalf("the copies are %s", got)
	}
	return b
}

// viewOps gives each operation of the view's journal, kind and state, in order.
func (b *passBench) viewOps() string {
	b.t.Helper()
	return b.text("SELECT COALESCE(group_concat(kind || ' ' || state, ','), '') FROM (SELECT * FROM view_ops ORDER BY id)")
}

// TestTheViewsRepriseOfALinkedDeletionOnEachDriver: atoll mount killed between the link and the unlink of a deletion
// through the view, on a driver without RENAME_NOREPLACE, then started again (specs.md §17.1, §17.2, §21.2; flaw 3).
// Its reprise resolves the deletion by the table of §17.2: the copy is no longer at its library name, it is in the
// trash, the deletion is done; and the pass then takes the deletion in, the other copy going to its own trash.
func TestTheViewsRepriseOfALinkedDeletionOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		if tc.driver != "ntfs-3g" && tc.driver != "lowntfs-3g" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := viewPool(t, v, at)
			point := filepath.Join(t.TempDir(), "view")
			mustDo(t, os.Mkdir(point, 0o755))
			view := startViewWith(t, b, point, nil, childWritesEnv+"=1", childKillAtEnv+"=linked")
			view.mounted(t)
			if err := os.Remove(filepath.Join(point, "Films", "a.mkv")); err == nil {
				t.Fatalf("the deletion went through, the view never killed at its link:\n%s", view.said())
			}
			select {
			case <-view.exited:
			case <-time.After(time.Minute):
				t.Fatalf("the view is not dead:\n%s", view.said())
			}
			var exit *exec.ExitError
			if !errors.As(view.err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("the view ended with %v, not killed:\n%s", view.err, view.out.String())
			}
			if got := b.viewOps(); got != "unlink planned" {
				t.Fatalf("the view's journal once killed: %s", got)
			}
			linked := trashFiles(t, at)
			t.Logf("killed at the link: the library %q, the trash %v", readOr(filepath.Join(at, "media", "Films", "a.mkv")), linked)
			if len(linked) != 1 {
				t.Fatalf("the trash of v holds %v at the link", linked)
			}

			// atoll mount started again at the same point: the view left there, whose process died, is detached,
			// and the reprise runs before the view is mounted.
			served, err := serveView(t, b, point, true)
			mustDo(t, err)
			if got := readOr(filepath.Join(at, "media", "Films", "a.mkv")); got != "absent" {
				t.Errorf("the copy is still at its name on v, holding %q", got)
			}
			if got := trashFiles(t, at); len(got) != 1 {
				t.Errorf("the trash of v holds %v", got)
			}
			if got := b.viewOps(); got != "unlink done" {
				t.Errorf("the view's journal after its reprise: %s", got)
			}
			if got := b.text("SELECT count(*) FROM ops WHERE state = 'planned'"); got != "0" {
				t.Errorf("%s operations left planned", got)
			}
			if _, err := os.Stat(filepath.Join(point, "Films", "a.mkv")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the view presents the file deleted: %v", err)
			}
			served.stop()
			mustDo(t, served.wait())

			// The pass takes the deletion in: the copy on k goes to its trash, and the view's journal is taken in.
			b.settle()
			if got := readOr(filepath.Join(b.keep, "media", "Films", "a.mkv")); got != "absent" {
				t.Errorf("k still holds %q", got)
			}
			if got := b.text("SELECT count(*) FROM trash WHERE reason = 'deleted' AND CAST(lpath AS TEXT) = 'Films/a.mkv'"); got != "2" {
				t.Errorf("copies of the file deleted in the trash: %s, want 2", got)
			}
			open, err := catalog.OpenViewOps(t.Context(), b.db)
			mustDo(t, err)
			if len(open) != 0 {
				t.Errorf("not taken in: %+v", open)
			}
		})
	}
}

// readOr gives what a file holds, "absent" when nothing is there.
func readOr(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "absent"
	}
	return string(data)
}

// TestADeletionThroughTheViewEditedAtItsMoveOnEachDriver: a deletion through the view whose copy a user writes into in
// place on its disk between the look right before the move and the move, the fault layer's hook at the move's call
// standing for that instant, on each driver (specs.md §17.1, §17.2, §21.2; flaw 4). The move is resolved at once by
// the table: the edit stays at its path, nothing of it is in the trash, the operation is undone; the next pass takes
// the edit for a modification, given to the other copy.
func TestADeletionThroughTheViewEditedAtItsMoveOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := viewPool(t, v, at)
			const edited = "edited in place, at the move"
			file := filepath.Join(at, "media", "Films", "a.mkv")
			var fired atomic.Bool
			real := b.machine
			b.machine = &faultTop{Machine: real, fail: map[string]func(call, name string) error{at: func(call, name string) error {
				if (call == "rename" || call == "link") && name == "a.mkv" && fired.CompareAndSwap(false, true) {
					if err := os.WriteFile(file, []byte(edited), 0o644); err != nil {
						t.Error(err)
					}
				}
				return nil
			}}}
			point := filepath.Join(t.TempDir(), "view")
			mustDo(t, os.Mkdir(point, 0o755))
			served, err := serveView(t, b, point, true)
			mustDo(t, err)
			err = os.Remove(filepath.Join(point, "Films", "a.mkv"))
			if !fired.Load() {
				t.Fatalf("the deletion never reached its move: %v", err)
			}
			if err == nil {
				t.Error("the deletion said it was made")
			}
			served.stop()
			mustDo(t, served.wait())
			b.machine = real
			dropCaches(t)
			if got := readOr(file); got != edited {
				t.Errorf("the user's edit is not at its path on v: %q", got)
			}
			if got := trashFiles(t, at); len(got) != 0 {
				t.Errorf("the trash of v holds %v", got)
			}
			if got := b.text("SELECT count(*) FROM trash"); got != "0" {
				t.Errorf("%s entries in the trash", got)
			}
			if got := b.text("SELECT count(*) FROM ops WHERE state = 'planned'"); got != "0" {
				t.Errorf("%s operations left planned", got)
			}
			if got := b.viewOps(); got != "unlink undone" {
				t.Errorf("the view's journal: %s", got)
			}
			// Where the edit moved to the trash, it went back, with an op-undone event; where the link's look at the
			// source found it before the unlink, nothing moved, and the operation is undone with none (specs.md §17.1).
			if got := b.text("SELECT count(*) FROM events WHERE kind = 'op-undone'"); (got != "0") != renamesNoReplace(tc.driver) {
				t.Errorf("%s op-undone events", got)
			}
			// The edit is the user's: a modification, which the passes give to the copy on k.
			catchUp(b)
			b.settle()
			wantContent(t, b.keep, "Films/a.mkv", edited)
			wantContent(t, at, "Films/a.mkv", edited)
		})
	}
}

// forceBack unmounts a disk of a kernel driver at once, as a USB reset leaves it, and mounts it again: a mount of its
// own, which the view's check must find and open again.
func forceBack(t *testing.T, v *volume, driver, at string, options []string) {
	t.Helper()
	must(t, "umount", "-l", at)
	if err := remount(t, v, driver, at, options); err != nil {
		t.Fatalf("the volume did not mount again: %v", err)
	}
	got := check(t, diskOf(v, at))
	t.Logf("mounted again: %s; its check %s %s %s", findmnt(at), got.State, got.Reason, got.Message)
}

// findmnt says what is mounted at a path, every mount stacked there.
func findmnt(at string) string {
	out, err := exec.CommandContext(context.Background(), "findmnt", "-n", "-o", "FSTYPE,SOURCE,OPTIONS", at).CombinedOutput()
	if err != nil {
		return err.Error() + ": " + strings.TrimSpace(string(out))
	}
	return strings.ReplaceAll(strings.TrimSpace(string(out)), "\n", "; ")
}

// TestTheViewsRecheckAfterADiskLostOnEachDriver: a deletion through the view meets EIO at its move, a disk that cannot
// be relied on, the fault layer's hook at the move's call standing for it, and stays planned; the disk is then
// unmounted at once and mounted again, as after a USB reset, while the view runs, before its next check. The view's
// check finds it there again and resolves the operation, with no restart: nothing moved, so it is undone, the copy at
// its name (specs.md §17.1, §17.2, §21.2; flaw 5). The file can then be deleted through the view, and a pass takes
// that in. TestTheViewsRecheckOnADiskStillFailing is the case of a check while the disk fails, mounted.
func TestTheViewsRecheckAfterADiskLostOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := viewPool(t, v, at)
			var lost atomic.Bool
			real := b.machine
			b.machine = &faultTop{Machine: real, fail: map[string]func(call, name string) error{at: func(call, name string) error {
				if (call == "rename" || call == "link") && name == "a.mkv" && lost.CompareAndSwap(false, true) {
					return syscall.EIO
				}
				return nil
			}}}
			point := filepath.Join(t.TempDir(), "view")
			mustDo(t, os.Mkdir(point, 0o755))
			served, err := serveViewEvery(t, b, point, true, 3*time.Second)
			mustDo(t, err)
			in := filepath.Join(point, "Films", "a.mkv")
			if err := os.Remove(in); err == nil {
				t.Fatal("the deletion went through a disk that failed")
			}
			if got := b.viewOps(); got != "unlink planned" {
				t.Fatalf("the view's journal once the disk failed: %s", got)
			}
			if renamesNoReplace(tc.driver) {
				forceBack(t, v, tc.driver, at, options)
			} else {
				// ntfs-3g mounted again at once, its old process killed, while the view runs there with an operation it
				// left planned, mounted, then read /dev/fuse with EPERM ("fuse: reading device: Operation not
				// permitted") and unmounted itself, in qemu, where the same remount works with the view running and
				// nothing planned (DONE.md, 2026-11-23). The disk stays mounted here: never lost, it is not there again,
				// and the view's check leaves the deletion planned for ten checks; its next start resolves it.
				time.Sleep(10 * 3 * time.Second)
				if got := b.viewOps(); got != "unlink planned" {
					t.Fatalf("the view's journal, its disk never lost: %s, want the deletion planned", got)
				}
				served.stop()
				mustDo(t, served.wait())
				served, err = serveViewEvery(t, b, point, true, 3*time.Second)
				mustDo(t, err)
			}
			waitFor(t, "the deletion resolved by the view's check", func() bool { return !strings.Contains(b.viewOps(), "planned") })
			if got := b.viewOps(); got != "unlink undone" {
				t.Errorf("the view's journal once the disk is back: %s", got)
			}
			if got := readOr(filepath.Join(at, "media", "Films", "a.mkv")); got != "a film" {
				t.Errorf("v holds %q at the copy's name", got)
			}
			if got := trashFiles(t, at); len(got) != 0 {
				t.Errorf("the trash of v holds %v", got)
			}
			if got := b.text("SELECT count(*) FROM ops WHERE state = 'planned'"); got != "0" {
				t.Errorf("%s operations left planned", got)
			}
			// What follows at that path: the file deleted through the view, now that its disk is back.
			waitFor(t, "a deletion through the view", func() bool { return os.Remove(in) == nil })
			served.stop()
			mustDo(t, served.wait())
			b.machine = real
			b.settle()
			for _, top := range []string{b.keep, at} {
				if got := readOr(filepath.Join(top, "media", "Films", "a.mkv")); got != "absent" {
					t.Errorf("%s still holds %q", top, got)
				}
			}
			if got := b.text("SELECT count(*) FROM trash WHERE reason = 'deleted' AND CAST(lpath AS TEXT) = 'Films/a.mkv'"); got != "2" {
				t.Errorf("copies of the file deleted in the trash: %s, want 2", got)
			}
			open, err := catalog.OpenViewOps(t.Context(), b.db)
			mustDo(t, err)
			if len(open) != 0 {
				t.Errorf("not taken in: %+v", open)
			}
		})
	}
}

// TestTheViewsRecheckOnADiskStillFailing: as TestTheViewsRecheckAfterADiskLostOnEachDriver, but the disk stays mounted
// while its device fails (dm-error), for ten of the view's checks. While it fails it is not there again, and what its
// page cache still answers says nothing of what reached it (invariant 4, §10): the deletion stays planned, as the check
// resolves only a disk found lost and back on a mount of its own, confirmed past the page cache (the maintainer,
// 2026-11-23; specs.md §21.2). It once was resolved done from the cache, within 40 to 350 ms on ext4, ntfs3 and
// ntfs-3g, and once the disk was back the copy was at its name. The view is then stopped, the disk brought back and the
// view started again, whose reprise resolves the deletion by what the disk holds, and the pass takes it in, the
// library and the catalog agreeing. Stopped first, as the remount of a kernel driver under the view's handles gives
// back the very superblock they keep alive, aborted, which no USB reset does.
func TestTheViewsRecheckOnADiskStillFailing(t *testing.T) {
	needRoot(t)
	for _, tc := range failingDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			m := newMapped(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, m.volume, tc.driver, at, options...)
			b := viewPool(t, m.volume, at)
			must(t, "sync")
			real := b.machine
			b.machine = &failingAt{Machine: real, point: "trash-folders", fire: func() { m.fail(t) }}
			point := filepath.Join(t.TempDir(), "view")
			mustDo(t, os.Mkdir(point, 0o755))
			served, err := serveView(t, b, point, true)
			mustDo(t, err)
			if err := os.Remove(filepath.Join(point, "Films", "a.mkv")); err == nil {
				t.Fatal("the deletion went through a disk that failed")
			}
			time.Sleep(time.Second) // ten checks, the disk failing and still mounted
			failing := b.viewOps()
			served.stop()
			mustDo(t, served.wait())
			if failing != "unlink planned" {
				t.Errorf("the view's journal while the disk failed: %s, want the deletion planned", failing)
			}
			bringBack(t, m, tc.driver, at, options)
			b.machine = real
			served, err = serveView(t, b, point, true)
			mustDo(t, err)
			served.stop()
			mustDo(t, served.wait())
			dropCaches(t)
			library := readOr(filepath.Join(at, "media", "Films", "a.mkv"))
			state := b.viewOps()
			t.Logf("while the disk failed: %s; once back and the view started again: %s, the library %q, the trash %v",
				failing, state, library, trashFiles(t, at))
			if strings.Contains(state, "planned") {
				t.Fatalf("the view's journal once its reprise ran: %s", state)
			}
			if (state == "unlink done") != (library == "absent") {
				t.Errorf("the view's journal says %s, and v holds %q at the copy's name", state, library)
			}
			b.settle()
			open, err := catalog.OpenViewOps(t.Context(), b.db)
			mustDo(t, err)
			if len(open) != 0 {
				t.Errorf("not taken in: %+v", open)
			}
			want := map[string]string{"unlink done": "absent", "unlink undone": "a film"}[state]
			for _, top := range []string{b.keep, at} {
				if got := readOr(filepath.Join(top, "media", "Films", "a.mkv")); got != want {
					t.Errorf("%s holds %q, the deletion %s", top, got, state)
				}
			}
		})
	}
}

// TestAFolderAUserMadeAtAMkdirsNameOnEachDriver: a pass killed once the mkdir of a copy's missing parent is journaled,
// before the mkdir (mkdir-journaled), or once the folder is made, before its inode is journaled (mkdir-unkeyed); a
// user then makes a folder of their own at that name, 0700, the one Atoll made removed first. The reprise leaves it as
// the user made it, the operation undone and no row saying it is Atoll's, and the copy goes into it (specs.md §17.2,
// §17.4; flaw 6).
func TestAFolderAUserMadeAtAMkdirsNameOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		for _, point := range []string{"mkdir-journaled", "mkdir-unkeyed"} {
			t.Run(tc.name+"/"+point, func(t *testing.T) {
				options := driverOptions(t, tc.driver)
				v := newVolume(t, tc.fs)
				at := mountPoint(t)
				mountVolume(t, v, tc.driver, at, options...)
				b := newPassBench(t, v, at)
				write(t, b.keep, "p/a", "content")
				mustDo(t, os.Chmod(filepath.Join(b.keep, "media", "p"), 0o755))
				b.scan(false)
				real := b.machine
				stop := errors.New("stopped")
				b.machine = &stoppingAt{Machine: real, at: map[string]func(){
					point: func() { panic(stop) }, //nolint:forbidigo // what stands for kill -9
				}}
				func() {
					defer func() {
						if err, ok := recover().(error); !ok || !errors.Is(err, stop) {
							t.Fatalf("it did not stop at %s: %v", point, err)
						}
					}()
					b.replicate("p/a")
				}()
				b.machine = real
				if got := b.text("SELECT state FROM ops WHERE kind = 'mkdir'"); got != "planned" {
					t.Fatalf("the operation mkdir: %q", got)
				}
				mine := filepath.Join(at, "media", "p")
				_, err := os.Lstat(mine)
				switch {
				case point == "mkdir-unkeyed" && err != nil:
					t.Fatalf("no folder made before its inode was journaled: %v", err)
				case point == "mkdir-unkeyed":
					mustDo(t, os.Remove(mine))
				case err == nil:
					t.Fatal("a folder made before the mkdir")
				}
				mustDo(t, os.Mkdir(mine, 0o700))
				mustDo(t, os.Chmod(mine, 0o700))
				var made syscall.Stat_t
				mustDo(t, syscall.Lstat(mine, &made))
				b.machine = &later{Machine: real, by: 11 * time.Minute}
				b.pass()
				b.replicate("p/a")
				b.machine = real
				dropCaches(t)
				var now syscall.Stat_t
				mustDo(t, syscall.Lstat(mine, &now))
				if now.Ino != made.Ino || now.Mode&0o7777 != made.Mode&0o7777 {
					t.Errorf("the user's folder: inode %d, mode %04o, made %d, %04o", now.Ino, now.Mode&0o7777, made.Ino, made.Mode&0o7777)
				}
				if got := b.text("SELECT group_concat(state) FROM ops WHERE kind = 'mkdir'"); got != "undone" {
					t.Errorf("the operation mkdir: %q", got)
				}
				if got := b.text("SELECT count(*) FROM entries WHERE disk_id = 'v' AND CAST(lpath AS TEXT) = 'p' AND created_by = 'atoll'"); got != "0" {
					t.Errorf("the user's folder is recorded as Atoll's own (%s rows)", got)
				}
				wantContent(t, at, "p/a", "content")
				if got := b.text("SELECT count(*) FROM ops WHERE state = 'planned'"); got != "0" {
					t.Errorf("%s operations left planned", got)
				}
			})
		}
	}
}

// renamesNoReplace says whether a driver renames with RENAME_NOREPLACE, so that a pass's rename of a copy is one call
// whose look after it can find what arrived is not the copy; ntfs-3g and lowntfs-3g link, then unlink.
func renamesNoReplace(driver string) bool {
	return driver != "ntfs-3g" && driver != "lowntfs-3g"
}

// TestAFileSavedAtTheOldNameGoesBackOnEachDriver: a pass takes a user's rename to the other copy; a program saves a
// file at the copy's old name between the look right before Atoll's rename and the rename, the fault layer's hook at
// the rename's call standing for that instant, as no real mount holds the window open (specs.md §15.12, step 4,
// §17.2; flaw 7). Where the driver renames with RENAME_NOREPLACE, the file saved arrives at the new name, and goes back
// to the old one at once (put-back), also when the pass is killed right after (put-back's crash point); where it links,
// the look at the source finds the file saved and the link goes. Either way the file saved is at its old name, and at
// no name Atoll gave it.
func TestAFileSavedAtTheOldNameGoesBackOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		for _, killed := range []bool{false, true} {
			name := tc.name + "/saved"
			if killed {
				name = tc.name + "/killed-at-put-back"
			}
			t.Run(name, func(t *testing.T) {
				options := driverOptions(t, tc.driver)
				if killed && !renamesNoReplace(tc.driver) {
					t.Skip("link, then unlink: the look at the source finds the file saved, and nothing is put back")
				}
				v := newVolume(t, tc.fs)
				at := mountPoint(t)
				mountVolume(t, v, tc.driver, at, options...)
				b := newPassBench(t, v, at)
				for _, top := range []string{b.keep, at} {
					write(t, top, "a", "content")
				}
				b.scan(true)
				rename(t, b.keep, "a", "b")
				old := filepath.Join(at, "media", "a")
				const saved = "a new file, saved at the old name"
				var fired atomic.Bool
				real := b.machine
				var machine = &faultTop{Machine: real, fail: map[string]func(call, name string) error{at: func(call, name string) error {
					if call == "rename" && name == "b" && fired.CompareAndSwap(false, true) {
						// An atomic save at the copy's old name, right before Atoll's move.
						if err := os.WriteFile(old+".new", []byte(saved), 0o644); err != nil {
							t.Error(err)
						}
						if err := os.Rename(old+".new", old); err != nil {
							t.Error(err)
						}
					}
					return nil
				}}}
				b.machine = machine
				if killed {
					stop := errors.New("stopped")
					b.machine = &stoppingAt{Machine: machine, at: map[string]func(){
						"put-back": func() { panic(stop) }, //nolint:forbidigo // what stands for kill -9
					}}
					func() {
						defer func() {
							if err, ok := recover().(error); !ok || !errors.Is(err, stop) {
								t.Fatalf("it did not stop at put-back: %v", err)
							}
						}()
						b.settle()
					}()
				} else {
					b.settle()
				}
				b.machine = real
				if !fired.Load() {
					t.Fatal("no rename reached v")
				}
				if killed {
					b.settle()
				}
				dropCaches(t)
				if got := readOr(old); got != saved {
					t.Errorf("the file saved at a on v is not there (%q), and b holds %q", got, readOr(filepath.Join(at, "media", "b")))
				}
				if got := readOr(filepath.Join(at, "media", "b")); got == saved {
					t.Error("the file saved at a is at b too")
				}
				if got := b.text("SELECT count(*) FROM ops WHERE kind = 'rename' AND state = 'planned'"); got != "0" {
					t.Errorf("%s renames left planned", got)
				}
				wantContent(t, b.keep, "b", "content")
			})
		}
	}
}
