//go:build loopdev

package loopdev

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/names"
)

// TestAnEditInTheSecondOfTheLastChangeOnACoarseExt4: an ext4 made with 128-byte inodes holds a time to the second
// (specs.md §10). A copy Atoll places there records the change time its look finds; an edit in place made in that
// second, keeping the size and the modification time, leaves that very change time, so that no look tells it (flaw 31
// of the TLA+ models). The scrub finds another content with the times its row has: its file is a conflict, the edit
// kept, nothing written (specs.md §16). The edit falls in that second when the replication ends soon enough after its
// record: the test tries again with another file when it did not.
func TestAnEditInTheSecondOfTheLastChangeOnACoarseExt4(t *testing.T) {
	needRoot(t)
	v := newVolume(t, names.Ext4, "-I", "128")
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	b := newPassBenchWith(t, v, at, "")
	content := bytes.Repeat([]byte("a film "), 1000)
	edit := bytes.Repeat([]byte("A FILM "), 1000)
	var name string
	for attempt := 0; attempt < 10 && name == ""; attempt++ {
		n := fmt.Sprintf("a%d.mkv", attempt)
		mustDo(t, os.WriteFile(filepath.Join(b.keep, "media", n), content, 0o644))
		b.scan(true)
		_, err := jobs.Replicate(t.Context(), b.db, b.config, b.machine, jobs.ReplicateOptions{Path: n, Now: true},
			func(*jobs.ReplicateItem) error { return nil })
		mustDo(t, err)
		path := filepath.Join(at, "media", n)
		before, err := os.Stat(path)
		mustDo(t, err)
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		mustDo(t, err)
		_, err = f.WriteAt(edit, 0)
		mustDo(t, err)
		mustDo(t, f.Close())
		mustDo(t, os.Chtimes(path, before.ModTime(), before.ModTime()))
		var st syscall.Stat_t
		mustDo(t, syscall.Stat(path, &st))
		row := b.text("SELECT ctime_ns FROM copies WHERE disk_id = 'v' AND file_id = (SELECT id FROM files WHERE CAST(lpath AS TEXT) = '" + n + "')")
		if row == strconv.FormatInt(st.Ctim.Nano(), 10) {
			name = n
		}
	}
	if name == "" {
		t.Fatal("no edit fell in the second of its copy's last change in 10 tries")
	}
	if got := b.text("SELECT time_precision_ns FROM disks WHERE id = 'v'"); got != "1000000000" {
		t.Fatalf("the disk's precision: %s", got)
	}

	// Due now, as a month would make them.
	_, err := b.db.ExecContext(t.Context(), "UPDATE copies SET verified_ns = 1")
	mustDo(t, err)
	out, err := jobs.Scrub(t.Context(), b.db, b.config, b.machine, jobs.ScrubOptions{Apply: true, Quarantine: true})
	mustDo(t, err)
	i := slices.IndexFunc(out.Disks, func(d jobs.ScrubDisk) bool { return d.ID == "v" })
	if i < 0 || !slices.Equal(out.Disks[i].Conflict, []string{name}) || len(out.Disks[i].Corrupt) != 0 || len(out.Disks[i].Repaired) != 0 {
		t.Fatalf("%+v", out)
	}
	dropCaches(t)
	wantContent(t, at, name, string(edit))
	wantContent(t, b.keep, name, string(content))
	if got := b.text("SELECT state FROM files WHERE CAST(lpath AS TEXT) = '" + name + "'"); got != "conflict" {
		t.Errorf("the file is %s", got)
	}
	if got := b.text("SELECT count(*) FROM trash"); got != "0" {
		t.Errorf("%s entries in the trash", got)
	}
}
