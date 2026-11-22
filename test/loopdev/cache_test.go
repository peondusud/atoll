//go:build loopdev

package loopdev

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/names"
)

// TestTheCacheOnRealDisks: the SSD cache on real filesystems (specs.md §19). With Atoll's own view, a file written
// through it lands on the cache disk, ext4, and nowhere else. Its residency over and replication.window open, the passes
// copy it to both data disks, ext4 and XFS, then send its copy on the cache to that disk's trash and purge it at once:
// the cache no longer holds it, its trash is empty, each data disk holds it, read through a cold cache, one of them as
// the user's copy, and the view presents it as before, now from a data disk. Then break glass: the view stopped, plain
// mergerfs from atoll mergerfs-config's line, the cache disk a branch of it, presents the same.
func TestTheCacheOnRealDisks(t *testing.T) {
	needRoot(t)
	needMergerfs(t)
	vols := map[string]*volume{"c": newVolume(t, names.Ext4), "k": newVolume(t, names.Ext4), "v": newVolume(t, names.XFS)}
	tops := map[string]string{}
	for _, id := range []string{"c", "k", "v"} {
		tops[id] = mountPoint(t)
		mountVolume(t, vols[id], "", tops[id])
		mustDo(t, os.MkdirAll(filepath.Join(tops[id], "media"), 0o755))
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "catalog.db")
	point := filepath.Join(dir, "pool")
	mustDo(t, os.MkdirAll(point, 0o755))
	hour := time.Now().UTC().Hour()
	text := fmt.Sprintf("pool_id = \"p\"\ntimezone = \"UTC\"\ncatalog = %q\n[view]\nengine = \"atoll\"\nmount = %q\n", file, point)
	for _, id := range []string{"c", "k", "v"} {
		text += fmt.Sprintf("[[disk]]\nid = %q\nuuid = %q\nmount = %q\nfs = %q\n", id, vols[id].uuid, tops[id], vols[id].fs)
		if id == "c" {
			text += "role = \"cache\"\n"
		}
	}
	text += benchGuards + "[defaults]\nsettle = \"0s\"\n" +
		fmt.Sprintf("[replication]\nwindow = \"%02d:00-%02d:00\"\n", hour, (hour+2)%24) +
		"[cache]\nenabled = true\nmin_age = \"0s\"\n"
	c, err := config.Parse([]byte(text))
	mustDo(t, err)
	_, err = catalog.Create(t.Context(), file, "p")
	mustDo(t, err)
	db, err := catalog.OpenPool(t.Context(), file, "p")
	mustDo(t, err)
	t.Cleanup(func() { _ = db.Close() })
	machine, err := disks.NewMachine()
	mustDo(t, err)
	b := &passBench{t: t, db: db, config: c, configText: text, catalog: file, keep: tops["k"], at: tops["v"], machine: machine}
	b.scan(false)

	served, err := serveView(t, b, point, true)
	mustDo(t, err)
	mustDo(t, os.MkdirAll(filepath.Join(point, "Shows", "S01"), 0o755))
	mustDo(t, os.WriteFile(filepath.Join(point, "Shows", "S01", "e01.mkv"), []byte("an episode"), 0o644))
	on := func(id string) string {
		data, err := os.ReadFile(filepath.Join(tops[id], "media", "Shows", "S01", "e01.mkv"))
		if err != nil {
			return "absent"
		}
		return string(data)
	}
	if on("c") != "an episode" || on("k") != "absent" || on("v") != "absent" {
		t.Fatalf("written through the view: c %q, k %q, v %q", on("c"), on("k"), on("v"))
	}

	for range 5 {
		b.settle()
		if on("c") == "absent" {
			break
		}
	}
	dropCaches(t)
	if on("c") != "absent" || on("k") != "an episode" || on("v") != "an episode" {
		t.Fatalf("after the passes: c %q, k %q, v %q", on("c"), on("k"), on("v"))
	}
	// The user's part goes to the copy first seen, then by position: either data disk, as the copies ran.
	if got := b.text("SELECT group_concat(disk_id || ':' || state, ' ') FROM (SELECT disk_id, state FROM copies ORDER BY disk_id)"); got != "k:ok v:ok" {
		t.Errorf("copies %s", got)
	}
	if got := b.text("SELECT group_concat(created_by, ' ') FROM (SELECT created_by FROM copies ORDER BY first_seen_ns, disk_id)"); got != "user atoll" {
		t.Errorf("the copies by first sight are %s, want the first the user's", got)
	}
	if got := b.text("SELECT count(*) FROM trash"); got != "0" {
		t.Errorf("%s entries in the trash", got)
	}
	if got := b.text("SELECT group_concat(kind || ':' || state, ' ') FROM (SELECT kind, state FROM ops WHERE disk_id = 'c' ORDER BY id)"); got != "trash:done unlink:done" {
		t.Errorf("journal of the cache: %s", got)
	}
	entries, err := os.ReadDir(filepath.Join(tops["c"], "Atoll", "trash"))
	if err == nil && len(entries) != 0 {
		t.Errorf("the cache's trash holds %d entries", len(entries))
	}
	time.Sleep(1100 * time.Millisecond) // past the kernel's caches of the view
	if data, err := os.ReadFile(filepath.Join(point, "Shows", "S01", "e01.mkv")); err != nil || string(data) != "an episode" {
		t.Errorf("through the view: %q, %v", data, err)
	}
	before := described(t, point)

	line, _ := fstabLine(b)
	served.stop()
	mustDo(t, served.wait())
	mountView(t, line, point)
	if merged := described(t, point); !maps.Equal(merged, before) {
		t.Errorf("break glass: mergerfs presents\n%v\nwhere the view presented\n%v", merged, before)
	}
}
