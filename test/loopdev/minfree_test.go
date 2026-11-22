//go:build loopdev

package loopdev

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/jobs"
)

// fillTo leaves a mount with room bytes free, as its filesystem says it, by a file of the user's own at its top, synced.
func fillTo(t *testing.T, at string, room int64) {
	t.Helper()
	_, free, err := fsx.Space(at)
	if err != nil {
		t.Fatal(err)
	}
	size := free - room
	if size <= 0 {
		t.Fatalf("%s has %d bytes free, not more than %d", at, free, room)
	}
	f, err := os.OpenFile(filepath.Join(at, "filler"), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := unix.Fallocate(int(f.Fd()), 0, 0, size); err != nil {
		// A driver without fallocate: the bytes written, as a program writes them.
		block := make([]byte, 1<<20)
		for left := size; left > 0; left -= int64(len(block)) {
			if _, err := f.Write(block[:min(int64(len(block)), left)]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	unix.Sync()
}

// A copy checks min_free as it writes, by its target's free space as each driver says it (specs.md §14.2;
// conformance/models/README.md, flaw 12): once the copy of a file of four chunks is journaled, a file of the user's own
// leaves its target 6 MiB past min_free, 32 MiB, room the plan counted on and the copy needs 16 MiB of, and enough for
// the copy itself. The copy stops no-room before it takes the disk past min_free, its part gone and nothing at the
// file's name; the user's file removed, the next run makes the copy, read back through a cold cache.
func TestACopyStopsAtMinFreeOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolumeSized(t, tc.fs, 256)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newPassBenchWith(t, v, at, "[placement]\nmin_free = [\"32MiB\"]\n")
			when := time.Unix(1500000000, 0)
			data := bytes.Repeat([]byte("atoll min_free "), (16<<20)/15+1)[:16<<20]
			writeBytes(t, b.keep, "big.mkv", data, when)
			b.scan(false)
			b.machine = &stoppingAt{Machine: b.machine, at: map[string]func(){
				"journaled": func() { fillTo(t, at, 38<<20) },
			}}
			items := b.replicate("")
			if len(items) != 1 || items[0].Outcome != jobs.OutcomeNoRoom {
				t.Fatalf("with 6 MiB past min_free: %+v", items)
			}
			if _, err := os.Lstat(filepath.Join(at, "media", "big.mkv")); !os.IsNotExist(err) {
				t.Errorf("the copy is at its name: %v", err)
			}
			emptyTmp(t, at)
			if _, free, err := fsx.Space(at); err != nil || free < 32<<20 {
				t.Errorf("the target has %d bytes free, past min_free: %v", free, err)
			}
			if got := b.count("SELECT count(*) FROM ops WHERE kind = 'create' AND state = 'undone'"); got != 1 {
				t.Errorf("%d operations undone", got)
			}

			if err := os.Remove(filepath.Join(at, "filler")); err != nil {
				t.Fatal(err)
			}
			unix.Sync()
			items = b.replicate("")
			if len(items) != 1 || items[0].Outcome != jobs.OutcomeReplicated {
				t.Fatalf("with room again: %+v", items)
			}
			dropCaches(t)
			wantCopy(t, at, "big.mkv", data, when)
			emptyTmp(t, at)
			if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
				t.Errorf("%d operations left planned", got)
			}
		})
	}
}
