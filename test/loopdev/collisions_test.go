//go:build loopdev

package loopdev

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/trash"
)

// queue has the plan queue the copies a pool lacks, as a pass would, without running them.
func (b *passBench) queue() {
	b.t.Helper()
	if _, err := jobs.Replicate(b.t.Context(), b.db, b.config, b.machine, jobs.ReplicateOptions{}, func(*jobs.ReplicateItem) error { return nil }); err != nil {
		b.t.Fatal(err)
	}
}

// outcomeOn gives the outcome of the copy of a path on the disk v among a run's.
func outcomeOn(items []jobs.ReplicateItem, path string) string {
	for _, i := range items {
		if i.Path == path && i.Disk == "v" {
			return i.Outcome
		}
	}
	return ""
}

// TestNoCollidingNameOnEachNTFSDriver: flaws 24 and 25 of conformance/models/README.md on a real volume of each NTFS
// driver, the disk v, beside an ext4 disk k (specs.md §12.4, §15.7). Right before Atoll makes a name on an NTFS disk, it
// lists the folder the name goes in, and each folder on the way, and refuses a name another there has the key of,
// whoever made it (disk-key-look, the maintainer's remedy, 2026-11-25):
//   - a folder films a user made on v since its last walk, which no row says, and a copy of Films/a.mkv: the copy is
//     refused, name-refused, and nothing lands beside films or in it. How the driver itself answers a look at Films there
//     is logged: every driver Atoll writes to finds a name by its bytes (docs/research/ntfs-driver.md);
//   - the same with Films already on v, recorded, and films beside it: the copy into Films is refused;
//   - a copy of Films/a.mkv queued, then a rename of another file into films on k followed on v before it runs: films is
//     Atoll's on v, and the copy is refused, so that v never holds Films and films, both Atoll's.
//
// Atoll made Films beside films, in each.
func TestNoCollidingNameOnEachNTFSDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		if tc.fs != names.NTFS {
			continue
		}
		bench := func(t *testing.T) *passBench {
			t.Helper()
			v := newVolume(t, names.NTFS)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, driverOptions(t, tc.driver)...)
			return newPassBenchWith(t, v, at, "[defaults]\nsettle = \"0s\"\n")
		}
		t.Run(tc.name+"/a user's folder made since the walk", func(t *testing.T) {
			b := bench(t)
			write(t, b.keep, "Films/a.mkv", "the film")
			b.scan(true)
			mustDo(t, os.Mkdir(filepath.Join(b.at, "media", "films"), 0o755))
			_, err := os.Lstat(filepath.Join(b.at, "media", "Films"))
			t.Logf("%s: a look at Films beside films: %v", tc.driver, err)
			items := b.replicate("Films/a.mkv")
			if got := outcomeOn(items, "Films/a.mkv"); got != trash.OutcomeNameRefused {
				t.Errorf("the copy ended %q: %+v", got, items)
			}
			dropCaches(t)
			if got := listed(t, filepath.Join(b.at, "media")); !slices.Equal(got, []string{"films"}) {
				t.Errorf("v holds %v", got)
			}
			if got := listed(t, filepath.Join(b.at, "media", "films")); len(got) != 0 {
				t.Errorf("films holds %v", got)
			}
		})
		t.Run(tc.name+"/a folder on the way", func(t *testing.T) {
			b := bench(t)
			write(t, b.keep, "Films/a.mkv", "the film")
			write(t, b.keep, "Films/b.mkv", "another film")
			write(t, b.at, "Films/b.mkv", "another film")
			b.scan(true)
			mustDo(t, os.Mkdir(filepath.Join(b.at, "media", "films"), 0o755))
			items := b.replicate("Films/a.mkv")
			if got := outcomeOn(items, "Films/a.mkv"); got != trash.OutcomeNameRefused {
				t.Errorf("the copy ended %q: %+v", got, items)
			}
			dropCaches(t)
			if got := listed(t, filepath.Join(b.at, "media", "Films")); !slices.Equal(got, []string{"b.mkv"}) {
				t.Errorf("Films holds %v", got)
			}
		})
		t.Run(tc.name+"/a rename followed before the copy", func(t *testing.T) {
			b := bench(t)
			write(t, b.keep, "Films/a.mkv", "the film")
			write(t, b.keep, "Other/b.mkv", "another film")
			write(t, b.at, "Other/b.mkv", "another film")
			b.scan(true)
			b.queue()
			if got := b.text("SELECT count(*) FROM copies WHERE disk_id = 'v' AND state = 'pending'"); got != "1" {
				t.Fatalf("%s copies queued for v", got)
			}
			on := b.config
			b.config = b.reconfigure("[replication]\nenabled = false\n")
			rename(t, b.keep, "Other/b.mkv", "films/b.mkv")
			b.settle()
			wantContent(t, b.at, "films/b.mkv", "another film")
			b.config = on
			items := b.replicate("")
			if got := outcomeOn(items, "Films/a.mkv"); got != trash.OutcomeNameRefused {
				t.Errorf("the copy ended %q: %+v", got, items)
			}
			dropCaches(t)
			if got := listed(t, filepath.Join(b.at, "media")); slices.Contains(got, "Films") {
				t.Errorf("v holds %v, Films and films both Atoll's", got)
			}
		})
	}
}

// TestWhatAKeyLookCostsOnEachNTFSDriver measures, with -atoll.scale names in one folder of a real volume of each NTFS
// driver, what disk-key-look adds to a copy there: the look before a copy, which lists that folder, its cache warm and
// cold, beside the copy of a file of 1 MiB into it (specs.md §15.7, §26). In qemu without KVM every instruction is
// emulated: the times say how they compare, not what a machine takes.
func TestWhatAKeyLookCostsOnEachNTFSDriver(t *testing.T) {
	needRoot(t)
	if *scaleFiles <= 0 {
		t.Skip("-atoll.scale is not set")
	}
	n := *scaleFiles
	for _, tc := range trashDrivers {
		if tc.fs != names.NTFS {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			v := newVolume(t, names.NTFS)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, driverOptions(t, tc.driver)...)
			b := newPassBenchWith(t, v, at, "[defaults]\nsettle = \"0s\"\n")
			big := filepath.Join(at, "media", "big")
			mustDo(t, os.MkdirAll(big, 0o755))
			for i := range n {
				mustDo(t, os.WriteFile(filepath.Join(big, fmt.Sprintf("film %06d.mkv", i)), nil, 0o644))
			}
			handle, err := ops.Open(b.machine, &b.config.Disks[1], 0)
			mustDo(t, err)
			defer handle.Close()
			mustDo(t, handle.Verify())
			look := func() time.Duration {
				began := time.Now()
				if target, _, err := handle.LookAtTarget("big/new.mkv"); err != nil || target != ops.TargetFree {
					t.Fatalf("%q, %v", target, err)
				}
				return time.Since(began)
			}
			dropCaches(t)
			cold := look()
			const tries = 20
			var warm time.Duration
			for range tries {
				warm += look()
			}
			writeBytes(t, b.keep, "big/new.mkv", make([]byte, 1<<20), time.Now().Add(-time.Hour))
			b.scan(true)
			began := time.Now()
			items := b.replicate("big/new.mkv")
			copied := time.Since(began)
			if got := outcomeOn(items, "big/new.mkv"); got != jobs.OutcomeReplicated {
				t.Fatalf("the copy ended %q", got)
			}
			t.Logf("%s, %d names in the folder: a look %v cold, %v warm; the copy of 1 MiB there, its two looks included, %v",
				tc.driver, n, cold.Round(time.Microsecond), (warm / tries).Round(time.Microsecond), copied.Round(time.Millisecond))
		})
	}
}
