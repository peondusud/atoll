//go:build loopdev

package loopdev

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The loop-device tests of the remedies of flaws 18 to 21 (the maintainer, 2026-11-24; conformance/models/README.md):
// Atoll's own view, served over FUSE, renaming files and folders on the bench's ext4 disk k and the driver under test v,
// then the passes taking each operation in. Flaw 22's needs atoll mount stopped short between two disks, which the
// tests of internal/jobs do.

// takenIn runs passes, the debounce going by between them, until no operation of the view is left to take in.
func (b *passBench) takenIn() {
	b.t.Helper()
	for range 8 {
		b.settle()
		if b.text("SELECT count(*) FROM view_ops WHERE taken_ns IS NULL AND state <> 'undone'") == "0" {
			return
		}
	}
	b.t.Errorf("not taken in: %s operations of the view", b.text("SELECT count(*) FROM view_ops WHERE taken_ns IS NULL AND state <> 'undone'"))
}

// viewedPool makes the bench with its guards, the files given on v, the driver under test, alone, scanned, then
// replicated to k by Atoll: v holds each original, which the view acts on, as viewPool makes it.
func viewedPool(t *testing.T, v *volume, at, guards string, files ...string) *passBench {
	t.Helper()
	b := newPassBenchGuarded(t, v, at, guards, "[defaults]\nsettle = \"0s\"\n")
	for i := 0; i < len(files); i += 2 {
		write(t, at, files[i], files[i+1])
	}
	b.scan(false)
	catchUp(b)
	b.scan(false)
	if got := b.text("SELECT count(*) FROM copies WHERE disk_id = 'k' AND state = 'ok'"); got != fmt.Sprint(len(files)/2) {
		t.Fatalf("%s copies on k", got)
	}
	return b
}

// viewPoint serves the view that writes at a new point, and gives it.
func viewPoint(t *testing.T, b *passBench) (string, *served) {
	t.Helper()
	point := filepath.Join(t.TempDir(), "pool")
	mustDo(t, os.MkdirAll(point, 0o755))
	s, err := serveView(t, b, point, true)
	mustDo(t, err)
	return point, s
}

// TestAReplacementThroughTheViewOnEachDriver: under deletes.direct = "restore", a file renamed through the view onto
// another it presents, on v, the driver under test, which holds both originals: the copy replaced there goes to v's
// trash, and is the view's, so the pass sends k's copy of the file replaced to k's trash and renames k's copy of the
// file renamed onto its name, whatever deletes.direct says; as a cold cache reads them, both disks hold the renamed
// file's content at the new name and nothing at the old, and the operation is taken in (specs.md §21.2; flaw 18).
func TestAReplacementThroughTheViewOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := viewedPool(t, v, at, "[deletes]\ndebounce = \"1s\"\ndirect = \"restore\"\n[deletes.mass_guard]\npercent = \"100%\"\n[modifies.mass_guard]\npercent = \"100%\"\n[modifies.day_guard]\npercent = \"100%\"\n",
				"Films/a.mkv", "the film at a", "Films/b.mkv", "the film at b, older")
			point, s := viewPoint(t, b)
			mustDo(t, os.Rename(filepath.Join(point, "Films", "a.mkv"), filepath.Join(point, "Films", "b.mkv")))
			s.stop()
			mustDo(t, s.wait())
			b.takenIn()
			dropCaches(t)
			for _, top := range []string{b.keep, at} {
				wantContent(t, top, "Films/b.mkv", "the film at a")
				if _, err := os.Stat(filepath.Join(top, "media", "Films", "a.mkv")); err == nil {
					t.Errorf("%s still holds Films/a.mkv", top)
				}
			}
			if got := b.text("SELECT count(DISTINCT disk_id) FROM trash"); got != "2" {
				t.Errorf("the copy replaced is in the trash of %s disks", got)
			}
			if got := b.text("SELECT count(*) FROM ops WHERE state = 'planned'"); got != "0" {
				t.Errorf("%s operations left planned", got)
			}
		})
	}
}

// TestAFolderRenameThroughTheViewRepriseOnEachDriver: a folder renamed through the view whose rename fails on both
// disks, EIO, stays planned, its journal naming the disk of its last try, v, and that folder's inode there; a user then
// makes a folder at the new name directly on v. The view's next start finds there a folder of another inode, and calls
// the rename undone: through the view, the file below the old name is there (specs.md §21.2; flaw 19). Without the
// fault, the rename goes through on both, the journal naming k and its folder, and the passes take it in.
func TestAFolderRenameThroughTheViewRepriseOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := viewedPool(t, v, at, benchGuards, "Shows/D/e1.mkv", "the episode")
			armed := true
			fail := func(call, name string) error {
				if armed && (call == "rename" || call == "plainrename") && name == "E" {
					return syscall.EIO
				}
				return nil
			}
			machine := b.machine
			b.machine = &faultTop{Machine: machine, fail: map[string]func(string, string) error{b.keep: fail, at: fail}}
			point, s := viewPoint(t, b)
			if err := os.Rename(filepath.Join(point, "Shows", "D"), filepath.Join(point, "Shows", "E")); !errors.Is(err, syscall.EIO) {
				t.Errorf("the rename through the view: %v, want EIO", err)
			}
			s.stop()
			mustDo(t, s.wait())
			armed = false
			b.machine = machine
			if got := b.text("SELECT disk_id || ' ' || state FROM view_ops WHERE folder = 1"); got != "v planned" {
				t.Fatalf("the journal of the folder's rename: %q", got)
			}
			mustDo(t, os.Mkdir(filepath.Join(at, "media", "Shows", "E"), 0o755))
			point, s = viewPoint(t, b)
			if got := b.text("SELECT state FROM view_ops WHERE folder = 1"); got != "undone" {
				t.Errorf("the reprise calls the rename %s, want undone", got)
			}
			eventually(t, "the file below the old name through the view", func() bool {
				_, err := os.Stat(filepath.Join(point, "Shows", "D", "e1.mkv"))
				return err == nil
			})
			s.stop()
			mustDo(t, s.wait())

			// Without the fault, the rename back from E is no question; D renamed to F goes through on both.
			mustDo(t, os.Remove(filepath.Join(at, "media", "Shows", "E")))
			b.takenIn()
			point, s = viewPoint(t, b)
			mustDo(t, os.Rename(filepath.Join(point, "Shows", "D"), filepath.Join(point, "Shows", "F")))
			s.stop()
			mustDo(t, s.wait())
			if got := b.text("SELECT disk_id || ' ' || state || ' ' || (file_key IS NOT NULL) FROM view_ops WHERE folder = 1 AND lpath2 = CAST('Shows/F' AS BLOB)"); got != "k done 1" {
				t.Errorf("the journal of the rename that went through: %q", got)
			}
			b.takenIn()
			dropCaches(t)
			for _, top := range []string{b.keep, at} {
				wantContent(t, top, "Shows/F/e1.mkv", "the episode")
			}
		})
	}
}

// TestARenameThroughTheViewUndoneDirectlyOnEachDriver: a file renamed through the view on v, its original, which a
// user moves back directly there before a pass takes the rename in: the pass takes it in as it is, and through the
// view the file is at its old name (specs.md §21.2; flaw 20).
func TestARenameThroughTheViewUndoneDirectlyOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := viewedPool(t, v, at, benchGuards, "Films/a.mkv", "the film")
			point, s := viewPoint(t, b)
			mustDo(t, os.Rename(filepath.Join(point, "Films", "a.mkv"), filepath.Join(point, "Films", "b.mkv")))
			disk := b.text("SELECT disk_id FROM view_ops")
			top := map[string]string{"k": b.keep, "v": at}[disk]
			rename(t, top, "Films/b.mkv", "Films/a.mkv")
			b.takenIn()
			eventually(t, "the file at its old name through the view", func() bool {
				_, err := os.Stat(filepath.Join(point, "Films", "a.mkv"))
				return err == nil
			})
			s.stop()
			mustDo(t, s.wait())
			dropCaches(t)
			for _, top := range []string{b.keep, at} {
				wantContent(t, top, "Films/a.mkv", "the film")
			}
		})
	}
}

// TestAFolderRenamedThroughTheViewOntoAUsersFolderOnEachDriver: a user makes Shows/E directly on v, which no pass
// recorded; the view renames Shows/D to Shows/E, refused on v, made on k. The folder's job for v cannot run, its new name
// taken; the copy below follows one by one into the user's folder, and the old folder, left empty on v, makes the
// rename one a user undid directly, taken in as it is: as a cold cache reads it, both disks hold the file at Shows/E,
// and nothing is left planned (specs.md §21.2; flaw 21).
func TestAFolderRenamedThroughTheViewOntoAUsersFolderOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := viewedPool(t, v, at, benchGuards, "Shows/D/e1.mkv", "the episode")
			mustDo(t, os.Mkdir(filepath.Join(at, "media", "Shows", "E"), 0o755))
			point, s := viewPoint(t, b)
			mustDo(t, os.Rename(filepath.Join(point, "Shows", "D"), filepath.Join(point, "Shows", "E")))
			s.stop()
			mustDo(t, s.wait())
			b.takenIn()
			dropCaches(t)
			for _, top := range []string{b.keep, at} {
				wantContent(t, top, "Shows/E/e1.mkv", "the episode")
			}
			if got := b.text("SELECT count(*) FROM ops WHERE state = 'planned'"); got != "0" {
				t.Errorf("%s operations left planned", got)
			}
		})
	}
}
