//go:build loopdev

package loopdev

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/names"
)

// TestTheTimePrecisionOfEachFilesystem: the head of what mkfs makes says how finely its driver holds a time, and the
// driver holds a time set to the nanosecond to exactly that, rounded down (specs.md §9, §10). The ext4 bytes of
// conformance/vectors/time_precision.json are what mkfs.ext4 writes.
func TestTheTimePrecisionOfEachFilesystem(t *testing.T) {
	needRoot(t)
	for _, tc := range []struct {
		name, driver string
		fs           names.Filesystem
		mkfs         []string
		want         int64
		ext4         string // the revision at 1100 and the inode size at 1112, in hexadecimal
	}{
		{"ext4", "", names.Ext4, nil, 1, "01000000 0001"},
		{"ext4 with 128-byte inodes", "", names.Ext4, []string{"-I", "128"}, 1_000_000_000, "01000000 8000"},
		{"xfs", "", names.XFS, nil, 1, ""},
		{"btrfs", "", names.Btrfs, nil, 1, ""},
		{"ntfs3", "ntfs3", names.NTFS, nil, 100, ""},
		{"ntfs-3g", "ntfs-3g", names.NTFS, nil, 100, ""},
		{"lowntfs-3g", "lowntfs-3g", names.NTFS, nil, 100, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs, tc.mkfs...)
			head, err := fsx.ReadBlockHead(v.device, disks.HeadSize)
			if err != nil {
				t.Fatal(err)
			}
			if tc.ext4 != "" {
				if got := hex.EncodeToString(head.Data[1100:1104]) + " " + hex.EncodeToString(head.Data[1112:1114]); got != tc.ext4 {
					t.Errorf("mkfs.ext4 wrote %s", got)
				}
			}
			id, err := disks.ReadIdentity(tc.fs, head.Data)
			if err != nil || id.TimePrecisionNS != tc.want {
				t.Fatalf("%+v, %v, want %d ns", id, err, tc.want)
			}
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			file := filepath.Join(at, "f")
			if err := os.WriteFile(file, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			const set = int64(1_600_000_000_123_456_789)
			if err := os.Chtimes(file, time.Unix(0, set), time.Unix(0, set)); err != nil {
				t.Fatal(err)
			}
			dropCaches(t)
			st, err := os.Stat(file)
			if err != nil {
				t.Fatal(err)
			}
			if held := st.ModTime().UnixNano(); held != set/tc.want*tc.want {
				t.Errorf("the driver holds %d of %d", held, set)
			}
		})
	}
}

// crashAt stops a pass at a point, as kill -9 would: nothing after it runs.
type crashAt struct {
	disks.Machine
	point string
}

var errCrash = errors.New("stopped")

func (c *crashAt) Checkpoint(point string) {
	if point == c.point {
		panic(errCrash) //nolint:forbidigo // what stands for kill -9
	}
}

func (b *passBench) crashAt(point string) {
	b.t.Helper()
	machine := b.machine
	defer func() {
		b.machine = machine
		if err, ok := recover().(error); !ok || !errors.Is(err, errCrash) {
			b.t.Fatalf("it did not stop at %s: %v", point, err)
		}
	}()
	b.machine = &crashAt{Machine: machine, point: point}
	_, _ = jobs.Run(b.t.Context(), b.db, b.config, b.machine, true)
}

// TestARepriseOfATimeOnEachDriver: a pass killed once a time is set on a copy, before that is recorded. The reprise
// finds what the driver holds of the time, which on NTFS is that time to 100 ns, finishes the operation with no
// warning, and the pass that follows has nothing left to apply (specs.md §9, §17.2).
func TestARepriseOfATimeOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newPassBench(t, v, at)
			writeBytes(t, b.keep, "big", threeChunks(), time.Unix(1500000000, 0))
			b.scan(false)
			if _, err := jobs.Replicate(b.t.Context(), b.db, b.config, b.machine, jobs.ReplicateOptions{}, func(*jobs.ReplicateItem) error { return nil }); err != nil {
				t.Fatal(err)
			}
			b.pass()
			touched := time.Unix(0, 1_600_000_000_123_456_789)
			if err := os.Chtimes(filepath.Join(b.keep, "media", "big"), touched, touched); err != nil {
				t.Fatal(err)
			}
			b.machine = &later{Machine: b.machine, by: 11 * time.Minute}
			b.crashAt("times-set")
			out := b.pass()
			if got := b.text("SELECT group_concat(state, ',') FROM (SELECT state FROM ops WHERE kind = 'utimens' ORDER BY id)"); got != "done" {
				t.Errorf("the operations are %s", got)
			}
			if got := b.text("SELECT count(*) FROM events WHERE kind = 'op-undone'"); got != "0" {
				t.Errorf("%s warnings", got)
			}
			if !slices.Equal(out.Decisions, []jobs.Decision{{Path: "big", Kind: "metadata"}}) || len(out.Operations) != 0 || out.Partial {
				t.Errorf("%+v, %+v, partial %v", out.Decisions, out.Operations, out.Partial)
			}
			dropCaches(t)
			st, err := os.Stat(filepath.Join(at, "media", "big"))
			if err != nil {
				t.Fatal(err)
			}
			held := st.ModTime().UnixNano()
			if gap := touched.UnixNano() - held; gap < 0 || gap >= 100 {
				t.Errorf("the copy holds %d, for %d", held, touched.UnixNano())
			}
			if got := b.text("SELECT mtime_ns FROM copies WHERE disk_id = 'v'"); got != strconv.FormatInt(held, 10) {
				t.Errorf("the row says %s, the disk holds %d", got, held)
			}
			if out := b.pass(); len(out.Decisions)+len(out.Operations) != 0 {
				t.Errorf("then %+v, %+v", out.Decisions, out.Operations)
			}
		})
	}
}
