//go:build loopdev

package loopdev

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"testing"

	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/names"
)

// physicalOffset gives where the first byte of a file lies on its device, as filefrag reads it.
func physicalOffset(t *testing.T, path string) int64 {
	t.Helper()
	must(t, "sync")
	out := must(t, "filefrag", "-v", path)
	block := regexp.MustCompile(`blocks of (\d+) bytes`).FindStringSubmatch(out)
	first := regexp.MustCompile(`(?m)^\s*0:\s+0\.\.\s*\d+:\s+(\d+)\.\.`).FindStringSubmatch(out)
	if block == nil || first == nil {
		t.Fatalf("filefrag says:\n%s", out)
	}
	size, _ := strconv.ParseInt(block[1], 10, 64)
	start, _ := strconv.ParseInt(first[1], 10, 64)
	return start * size
}

// TestScrubFindsRot: a byte changed below the filesystem, on the device itself, leaves the copy's size and times as
// they were. The scrub reads it, finds a chunk that does not match, reads it again past the cache, calls the copy
// corrupt, and moves it to the trash, as the other copy, read whole first, is healthy; the next pass makes the copy
// again from that one (specs.md §16). Without integrity.auto_repair, as with it the scrub repairs the copy in place
// instead, which TestRepairOnEachDriver covers.
func TestScrubFindsRot(t *testing.T) {
	needRoot(t)
	v := newVolume(t, names.Ext4)
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	// No settle, so that the pass after the scrub makes the copy again at once.
	b := newPassBenchWith(t, v, at, "[defaults]\nsettle = \"0s\"\n[integrity]\nauto_repair = false\n")
	content := bytes.Repeat([]byte("a film "), 900_000) // past one chunk of 4 MiB
	for _, top := range []string{b.keep, at} {
		mustDo(t, os.WriteFile(filepath.Join(top, "media", "a.mkv"), content, 0o644))
	}
	b.scan(true)
	if got := b.text("SELECT count(*) FROM copies WHERE verified_ns IS NOT NULL"); got != "2" {
		t.Fatalf("verified copies: %s", got)
	}

	// The rot: one byte of the first chunk, on the device, then every cache dropped.
	offset := physicalOffset(t, filepath.Join(at, "media", "a.mkv"))
	dev, err := os.OpenFile(v.device, os.O_RDWR, 0)
	mustDo(t, err)
	_, err = dev.WriteAt([]byte("A"), offset+2)
	mustDo(t, err)
	mustDo(t, dev.Sync())
	mustDo(t, dev.Close())
	must(t, "sh", "-c", "echo 3 > /proc/sys/vm/drop_caches")
	data, err := os.ReadFile(filepath.Join(at, "media", "a.mkv"))
	mustDo(t, err)
	if bytes.Equal(data, content) {
		t.Fatal("the file reads as before: no rot to find")
	}

	// Due now, as a month would make them: verified long ago. Never verified, a copy that differs is another version
	// of its file, a conflict, and no rot (specs.md §16).
	_, err = b.db.ExecContext(t.Context(), "UPDATE copies SET verified_ns = 1")
	mustDo(t, err)
	out, err := jobs.Scrub(t.Context(), b.db, b.config, b.machine, jobs.ScrubOptions{Apply: true, Quarantine: true})
	mustDo(t, err)
	var k, vd *jobs.ScrubDisk
	for i := range out.Disks {
		switch out.Disks[i].ID {
		case "k":
			k = &out.Disks[i]
		case "v":
			vd = &out.Disks[i]
		}
	}
	if k == nil || vd == nil || k.Read != 1 || !slices.Equal(vd.Corrupt, []string{"a.mkv"}) || !slices.Equal(vd.Quarantined, []string{"a.mkv"}) {
		t.Fatalf("%+v", out)
	}
	if _, err := os.Stat(filepath.Join(at, "media", "a.mkv")); !os.IsNotExist(err) {
		t.Errorf("the corrupt copy is still in media/: %v", err)
	}
	if got := b.text("SELECT reason FROM trash WHERE disk_id = 'v'"); got != "quarantined" {
		t.Errorf("in the trash for %q", got)
	}
	b.settle()
	again, err := os.ReadFile(filepath.Join(at, "media", "a.mkv"))
	if err != nil || !bytes.Equal(again, content) {
		t.Errorf("made again: %d bytes, %v", len(again), err)
	}
}
