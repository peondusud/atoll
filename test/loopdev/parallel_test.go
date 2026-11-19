//go:build loopdev

package loopdev

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/names"
)

// parallelMore runs four copies at once on every disk, whatever kind the kernel gives a loop device: the copies at once
// are what these tests are about, on real filesystems (specs.md §14).
const parallelMore = "[replication]\nmax_parallel_per_disk = 4\n"

// count gives a count the catalog answers.
func (b *passBench) count(query string, args ...any) int64 {
	b.t.Helper()
	var n int64
	if err := b.db.QueryRowContext(b.t.Context(), query, args...).Scan(&n); err != nil {
		b.t.Fatal(err)
	}
	return n
}

// someFiles writes n files of two chunks and a little more below F/, each its own, and gives their bytes by path.
func someFiles(t *testing.T, top string, n int, when time.Time) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for i := range n {
		path := fmt.Sprintf("F/f%02d", i)
		data := bytes.Repeat([]byte(fmt.Sprintf("file %d ", i)), (2*4<<20+5000)/7)
		writeBytes(t, top, path, data, when)
		out[path] = data
	}
	return out
}

// The kind a loop device gives is the kernel's own: KindOf says what /sys/block/<name>/queue/rotational says of it
// (specs.md §14).
func TestTheKindOfALoopDevice(t *testing.T) {
	needRoot(t)
	v := newVolume(t, names.Ext4)
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	machine, err := disks.NewMachine()
	if err != nil {
		t.Fatal(err)
	}
	if !disks.ReadsKinds(machine) {
		t.Fatal("the delivered machine reads no kind")
	}
	raw, err := os.ReadFile(filepath.Join("/sys/block", filepath.Base(v.device), "queue", "rotational"))
	if err != nil {
		t.Fatal(err)
	}
	want := disks.KindSolid
	if strings.TrimSpace(string(raw)) != "0" {
		want = disks.KindRotational
	}
	if got := disks.KindOf(machine, diskOf(v, at)); got != want {
		t.Errorf("%s, the kernel says %q", got, strings.TrimSpace(string(raw)))
	}
}

// Copies at once from ext4 to each driver's disk, read back through a cold cache: each the bytes and the date of its
// source, recorded and verified, with nothing left planned or in tmp/ (specs.md §14).
func TestCopiesAtOnceOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newPassBenchWith(t, v, at, parallelMore)
			when := time.Unix(1500000000, 0)
			files := someFiles(t, b.keep, 8, when)
			b.scan(false)
			items := b.replicate("")
			if len(items) != len(files) {
				t.Fatalf("%+v", items)
			}
			for _, item := range items {
				if item.Outcome != jobs.OutcomeReplicated {
					t.Errorf("%+v", item)
				}
			}
			dropCaches(t)
			for path, data := range files {
				wantCopy(t, at, path, data, when)
			}
			emptyTmp(t, at)
			if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 'v' AND state = 'ok' AND verified_ns IS NOT NULL"); got != int64(len(files)) {
				t.Errorf("%d copies verified", got)
			}
			if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
				t.Errorf("%d operations left planned", got)
			}
		})
	}
}

// A target that fails with copies at once under way, as a USB disk drops: dm-error under a mounted ext4 or XFS
// (CLAUDE.md, "Testing rules"). The disk is lost, every copy on it stops, nothing is recorded that is not on it, and the
// source is never lost for it; once the disk is back, mounted again, the next passes resolve what each copy left and
// make every copy, read back through a cold cache.
func TestATargetFailingWithCopiesAtOnce(t *testing.T) {
	needRoot(t)
	if _, err := exec.LookPath("dmsetup"); err != nil {
		t.Skip("no dmsetup")
	}
	for _, tc := range []struct {
		name string
		fs   names.Filesystem
	}{
		{"ext4", names.Ext4},
		{"xfs", names.XFS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newVolumeSized(t, tc.fs, 512)
			sectors := must(t, "blockdev", "--getsz", v.device)
			name := "atoll-" + strings.ReplaceAll(t.Name(), "/", "-")
			linear := "0 " + sectors + " linear " + v.device + " 0"
			must(t, "dmsetup", "create", "--noudevsync", "--noudevrules", name, "--table", linear)
			must(t, "dmsetup", "mknodes")
			t.Cleanup(func() {
				_ = exec.CommandContext(context.Background(), "dmsetup", "remove", "-f", "--noudevsync", name).Run()
			})
			mapped := &volume{fs: v.fs, device: "/dev/mapper/" + name, uuid: v.uuid}
			at := mountPoint(t)
			mountVolume(t, mapped, "", at)
			b := newPassBenchWith(t, mapped, at, parallelMore)
			when := time.Unix(1500000000, 0)
			files := someFiles(t, b.keep, 12, when)
			b.scan(false)
			// The disk fails once two parts are being written.
			done := make(chan []jobs.ReplicateItem, 1)
			go func() {
				var items []jobs.ReplicateItem
				_, _ = jobs.Replicate(context.Background(), b.db, b.config, b.machine, jobs.ReplicateOptions{Now: true}, func(i *jobs.ReplicateItem) error {
					items = append(items, *i)
					return nil
				})
				done <- items
			}()
			for end := time.Now().Add(30 * time.Second); ; time.Sleep(time.Millisecond) {
				entries, _ := os.ReadDir(filepath.Join(at, "Atoll", "tmp"))
				if len(entries) >= 2 {
					break
				}
				if time.Now().After(end) {
					t.Fatal("no two copies under way")
				}
			}
			must(t, "dmsetup", "suspend", "--noudevsync", name)
			must(t, "dmsetup", "reload", name, "--table", "0 "+sectors+" error")
			must(t, "dmsetup", "resume", "--noudevsync", name)
			items := <-done
			lost := 0
			for _, item := range items {
				if item.Outcome != jobs.OutcomeReplicated {
					lost++
				}
			}
			if lost == 0 {
				t.Errorf("every copy was made on a disk that failed: %+v", items)
			}
			if got := b.text("SELECT state FROM disks WHERE id = 'v'"); got != "missing" {
				t.Errorf("the disk that failed is %s", got)
			}
			if got := b.text("SELECT state FROM disks WHERE id = 'k'"); got == "missing" {
				t.Error("the source was lost for the target's failure")
			}
			// The disk back, as after a remount: the next passes make what is left.
			_ = exec.CommandContext(context.Background(), "umount", "-l", at).Run()
			time.Sleep(200 * time.Millisecond)
			must(t, "dmsetup", "suspend", "--noudevsync", name)
			must(t, "dmsetup", "reload", name, "--table", linear)
			must(t, "dmsetup", "resume", "--noudevsync", name)
			mountVolume(t, mapped, "", at)
			for range 4 {
				b.replicate("")
				if b.count("SELECT count(*) FROM jobs") == 0 {
					break
				}
			}
			dropCaches(t)
			for path, data := range files {
				wantCopy(t, at, path, data, when)
			}
			emptyTmp(t, at)
			if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
				t.Errorf("%d operations left planned", got)
			}
			if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 'v' AND state = 'ok'"); got != int64(len(files)) {
				t.Errorf("%d copies on the disk that came back", got)
			}
		})
	}
}
