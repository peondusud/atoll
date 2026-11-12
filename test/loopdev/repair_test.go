//go:build loopdev

package loopdev

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/integrity"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/names"
)

// physicalAt gives where a byte of a file lies on its device, from the extents filefrag reads (FIEMAP).
func physicalAt(t *testing.T, path string, offset int64) int64 {
	t.Helper()
	must(t, "sync")
	out := must(t, "filefrag", "-v", path)
	block := regexp.MustCompile(`blocks of (\d+) bytes`).FindStringSubmatch(out)
	if block == nil {
		t.Fatalf("filefrag says:\n%s", out)
	}
	size, _ := strconv.ParseInt(block[1], 10, 64)
	want := offset / size
	extent := regexp.MustCompile(`(?m)^\s*\d+:\s+(\d+)\.\.\s*(\d+):\s+(\d+)\.\.\s*\d+:`)
	for _, m := range extent.FindAllStringSubmatch(out, -1) {
		first, _ := strconv.ParseInt(m[1], 10, 64)
		last, _ := strconv.ParseInt(m[2], 10, 64)
		physical, _ := strconv.ParseInt(m[3], 10, 64)
		if want >= first && want <= last {
			return (physical+want-first)*size + offset%size
		}
	}
	t.Fatalf("no extent holds byte %d:\n%s", offset, out)
	return 0
}

// repairDrivers are the filesystems and drivers a chunk repair runs on. btrfs checks its own data, and answers a
// read of a block that rotted with EIO: no rot reaches Atoll there, and a disk that answers EIO is missing (§10).
var repairDrivers = []struct {
	name   string
	fs     names.Filesystem
	driver string
}{
	{"ext4", names.Ext4, ""},
	{"xfs", names.XFS, ""},
	{"ntfs3", names.NTFS, "ntfs3"},
	{"ntfs-3g", names.NTFS, "ntfs-3g"},
	{"lowntfs-3g", names.NTFS, "lowntfs-3g"},
}

// TestRepairOnEachDriver: a chunk that rotted on the device, below the filesystem, of a file whose name takes 255
// bytes, is repaired in place by the scrub, on each filesystem and driver: the file keeps its inode and its time, its
// other chunks are never written, and the chunk it held reaches the trash, in <op>.chunks/<name>/00000001
// (specs.md §16.1, §17).
func TestRepairOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range repairDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newPassBenchWith(t, v, at, "")
			name := strings.Repeat("r", 251) + ".mkv"
			data := make([]byte, 2*integrity.ChunkSize+1000)
			for i := range data {
				data[i] = byte(i*7 + i/4096)
			}
			for _, top := range []string{b.keep, at} {
				mustDo(t, os.WriteFile(filepath.Join(top, "media", name), data, 0o644))
			}
			b.scan(true)
			path := filepath.Join(at, "media", name)
			before, err := os.Stat(path)
			mustDo(t, err)

			// The rot: the first byte of the second chunk, on the device, the volume unmounted. ntfs-3g answers no
			// FIEMAP, so ntfs3 reads where it lies.
			unmountWhenFree(t, at)
			look := tc.driver
			if tc.fs == names.NTFS {
				look = "ntfs3"
			}
			mountVolume(t, v, look, at, driverOptions(t, look)...)
			offset := physicalAt(t, path, integrity.ChunkSize)
			unmountWhenFree(t, at)
			dev, err := os.OpenFile(v.device, os.O_RDWR, 0)
			mustDo(t, err)
			_, err = dev.WriteAt([]byte{data[integrity.ChunkSize] ^ 0xff}, offset)
			mustDo(t, err)
			mustDo(t, dev.Sync())
			mustDo(t, dev.Close())
			mountVolume(t, v, tc.driver, at, options...)
			dropCaches(t)
			rotten, err := os.ReadFile(path)
			mustDo(t, err)
			if bytes.Equal(rotten, data) {
				t.Fatal("the file reads as before: no rot to find")
			}

			_, err = b.db.ExecContext(t.Context(), "UPDATE copies SET verified_ns = NULL")
			mustDo(t, err)
			out, err := jobs.Scrub(t.Context(), b.db, b.config, b.machine, jobs.ScrubOptions{Apply: true, Quarantine: true})
			mustDo(t, err)
			i := slices.IndexFunc(out.Disks, func(d jobs.ScrubDisk) bool { return d.ID == "v" })
			if i < 0 || !slices.Equal(out.Disks[i].Corrupt, []string{name}) || !slices.Equal(out.Disks[i].Repaired, []string{name}) || out.Partial {
				t.Fatalf("%+v", out)
			}

			dropCaches(t)
			wantContent(t, at, name, string(data))
			after, err := os.Stat(path)
			mustDo(t, err)
			if after.Sys().(*syscall.Stat_t).Ino != before.Sys().(*syscall.Stat_t).Ino {
				t.Error("the copy is another file")
			}
			if !after.ModTime().Equal(before.ModTime()) {
				t.Errorf("the time moved: %v, was %v", after.ModTime(), before.ModTime())
			}
			entry := b.text("SELECT CAST(trash_path AS TEXT) FROM trash WHERE reason = 'repaired'")
			saved, err := os.ReadFile(filepath.Join(at, "Atoll", "trash", filepath.FromSlash(entry), "00000001"))
			mustDo(t, err)
			if !bytes.Equal(saved, rotten[integrity.ChunkSize:2*integrity.ChunkSize]) {
				t.Error("the chunk saved is not what rotted")
			}
			if parts, err := os.ReadDir(filepath.Join(at, "Atoll", "tmp")); err == nil && len(parts) != 0 {
				t.Errorf("parts left: %v", parts)
			}
			if got := b.text("SELECT state FROM files"); got != "ok" {
				t.Errorf("the file is %s", got)
			}
		})
	}
}
