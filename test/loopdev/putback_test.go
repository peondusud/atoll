//go:build loopdev

package loopdev

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/peondusud/atoll/internal/names"
)

// The loop-device tests of the remedies of flaws 13 and 14 (the maintainer, 2026-11-23; conformance/models/README.md):
// what a rename of Atoll's own took that is not the copy or the folder journaled, put back on each driver.

// TestAnExchangeBackOnEachDriver: two names swapped on the bench's ext4 disk, which the pass gets round on the disk
// under test by an exchange of the two names, and a program that saves a file at one of them right before the exchange,
// atomically, a new name renamed onto it. ext4, XFS and btrfs exchange the two names; the look after finds the file
// saved at the other name, and the two are exchanged back at once, synced: as a cold cache reads them, the file saved is
// at the name it was saved at, the copy at its own, and both renames are undone (specs.md §15.12; flaw 13). The NTFS
// drivers refuse the exchange, which TestASwapOnEachDriver covers.
func TestAnExchangeBackOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		if tc.fs == names.NTFS {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newPassBench(t, v, at)
			for _, top := range []string{b.keep, at} {
				write(t, top, "a", "file a")
				write(t, top, "d/b", "file b")
			}
			b.scan(true)
			rename(t, b.keep, "a", "t")
			rename(t, b.keep, "d/b", "a")
			rename(t, b.keep, "t", "d/b")
			const saved = "a file a program saved at d/b"
			exchanges := 0
			b.machine = &faultTop{Machine: b.machine, fail: map[string]func(string, string) error{at: func(call, _ string) error {
				if call == "exchange" {
					if exchanges++; exchanges == 1 {
						write(t, at, "d/b.new", saved)
						rename(t, at, "d/b.new", "d/b")
					}
				}
				return nil
			}}}
			b.settle()
			if exchanges != 2 {
				t.Fatalf("%d exchanges, want the exchange and the one back", exchanges)
			}
			dropCaches(t)
			wantContent(t, at, "d/b", saved)
			wantContent(t, at, "a", "file a")
			if got := b.text("SELECT count(*) FROM ops WHERE kind = 'rename' AND disk_id = 'v' AND state = 'undone'"); got != "2" {
				t.Errorf("%s renames undone, want both", got)
			}
			if got := b.text("SELECT count(*) FROM ops WHERE state = 'planned'"); got != "0" {
				t.Errorf("%s operations left planned", got)
			}
		})
	}
}

// TestAFolderPutBackOnEachDriver: a folder renamed on the bench's ext4 disk, whose job renames it on the disk under test
// as one operation, and a program there that renames the folder away and makes a new one at its name, with a file in
// it, right before the job's rename, as a tool that rotates a folder does. The job's rename moves the new folder; the
// look after finds it at the new name, not the folder journaled, and puts it back at the old name, synced: as a cold
// cache reads them, the new folder is at its name with its file, nothing of it at the name the job gave, and nothing
// is left planned, on every driver, the plain rename of ntfs-3g and lowntfs-3g included (specs.md §15.12; flaw 14).
func TestAFolderPutBackOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newPassBench(t, v, at)
			for _, top := range []string{b.keep, at} {
				write(t, top, "D/x", "file x")
			}
			b.scan(true)
			rename(t, b.keep, "D", "E")
			const made = "a file of the new folder"
			rotated := false
			b.machine = &faultTop{Machine: b.machine, fail: map[string]func(string, string) error{at: func(call, name string) error {
				if (call == "rename" || call == "plainrename") && name == "E" && !rotated {
					rotated = true
					rename(t, at, "D", "F")
					mustDo(t, os.Mkdir(filepath.Join(at, "media", "D"), 0o755))
					write(t, at, "D/new", made)
				}
				return nil
			}}}
			b.settle()
			if !rotated {
				b.settle()
			}
			if !rotated {
				t.Fatal("no folder job reached the disk")
			}
			dropCaches(t)
			wantContent(t, at, "D/new", made)
			if _, err := os.Stat(filepath.Join(at, "media", "E", "new")); err == nil {
				t.Errorf("the new folder is at E, a name the folder's job gave it")
			}
			if got := b.text("SELECT count(*) FROM ops WHERE kind = 'rename' AND disk_id = 'v' AND state = 'undone'"); got != "1" {
				t.Errorf("%s renames undone, want the folder's", got)
			}
			if got := b.text("SELECT count(*) FROM ops WHERE state = 'planned'"); got != "0" {
				t.Errorf("%s operations left planned", got)
			}
		})
	}
}
