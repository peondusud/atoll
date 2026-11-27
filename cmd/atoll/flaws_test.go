//go:build flaws

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/view"
	"github.com/peondusud/atoll/internal/view/writer"
)

// cachePool is a pool of three disks, t1 the SSD cache, with Atoll's own view, whose new files land on t1 and whose
// transfer runs at the bench's time: its catalog made by init, a pass run.
func cachePool(t *testing.T) *poolBench {
	t.Helper()
	// The window holds the hour of the system's clock, which it is read by, in UTC.
	hour := time.Now().UTC().Hour()
	b := newPoolBenchViewing(t, 3, "engine = \"atoll\"\n", fmt.Sprintf("[defaults]\nsettle = \"0s\"\n[cache]\nenabled = true\n"+
		"min_age = \"0s\"\n[replication]\nwindow = \"%02d:00-%02d:00\"\n[modifies.mass_guard]\npercent = \"100%%\"\n[modifies.day_guard]\npercent = \"100%%\"\n", hour, (hour+2)%24))
	// The bench's clock is the real one, as a copy is due from the change time its disk gives it.
	b.machine.now = time.Now().UnixNano()
	text, err := os.ReadFile(b.configPath)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(text), "pool_id = \"p\"\n", "pool_id = \"p\"\ntimezone = \"UTC\"\n", 1)
	s = strings.Replace(s, "fs = \"ext4\"\n", "fs = \"ext4\"\nrole = \"cache\"\n", 1)
	if err := os.WriteFile(b.configPath, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	b.ok(exitOK, "init")
	// A pass records each disk's room, which the view reads to place a new file.
	b.ok(exitOK, "daemon", "--until-idle")
	return b
}

// mounted is Atoll's own view as atoll mount serves it: over the catalog it opens once, as it starts, and the disks.
func (b *poolBench) mounted() *writer.Writer {
	b.t.Helper()
	c, err := load(b.configPath)
	if err != nil {
		b.t.Fatal(err)
	}
	db, err := catalog.OpenPool(context.Background(), catalogPath(c), c.PoolID)
	if err != nil {
		b.t.Fatal(err)
	}
	b.t.Cleanup(func() { _ = db.Close() })
	roots, handles := view.NewDiskRoots(), writer.NewHandles()
	b.t.Cleanup(roots.Close)
	b.t.Cleanup(handles.Close)
	for i := range c.Disks {
		d := &c.Disks[i]
		root, err := b.machine.OpenRoot(d.Mount, d.Root)
		if err != nil {
			b.t.Fatal(err)
		}
		roots.Set(d.ID, root)
		h, err := ops.Open(b.machine, d, 0)
		if err != nil {
			b.t.Fatal(err)
		}
		handles.Set(d.ID, h)
	}
	settings, err := c.Cache()
	if err != nil {
		b.t.Fatal(err)
	}
	tree := view.NewTree(db, roots, view.Owner{}, b.machine.Now()).WithCache(settings.InView)
	w, err := writer.New(db, c, b.machine, tree, handles, slog.New(slog.DiscardHandler))
	if err != nil {
		b.t.Fatal(err)
	}
	return w
}

// Flaw 35 of the TLA+ models (conformance/models/README.md, Rebuild.tla): atoll catalog rebuild while Atoll's own view
// holds a file open to write. The new catalog, made from the disks, knows nothing of what the view holds open, and the
// view goes on journaling in the old one, which it opened at its start: the SSD cache's transfer, which never moves a
// copy the view holds open (§19, §21.2, Held open), moves the file and purges it under the program, and what the
// program writes after is lost. It passes once the flaw is fixed, whatever the remedy, the rebuild refused or not: the
// file stays on the cache while the view holds it, and the late write reaches the library.
func TestFlawARebuildKeepsWhatTheViewHoldsOpen(t *testing.T) {
	b := cachePool(t)
	w := b.mounted()
	who := writer.Caller{Owner: writer.Owner{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}, Creds: &view.Credentials{Caps: ^view.Caps(0)}}
	f, _, err := w.Create(t.Context(), "new.mkv", 0o644, who)
	if err != nil {
		t.Fatal(err)
	}
	if f.Disk != "t1" {
		t.Fatalf("the new file landed on %s", f.Disk)
	}
	if _, err := f.WriteAt([]byte("episode"), 0); err != nil {
		t.Fatal(err)
	}
	// The rebuild, while the view runs; refused or not.
	_, _, _ = b.atoll("catalog", "rebuild", "--yes")
	for range 3 {
		b.ok(exitOK, "daemon", "--until-idle")
		b.machine.advance(time.Minute)
	}
	if b.read(1, "new.mkv") == "absent" {
		t.Errorf("the file left the cache while the view held it open: t2 %q, t3 %q", b.read(2, "new.mkv"), b.read(3, "new.mkv"))
	}
	const late = "a late write"
	if _, err := f.WriteAt([]byte(late), 0); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	for range 4 {
		b.ok(exitOK, "daemon", "--until-idle")
		b.machine.advance(time.Minute)
	}
	found := false
	for disk := 1; disk <= 3; disk++ {
		found = found || strings.HasPrefix(b.read(disk, "new.mkv"), late)
	}
	if !found {
		t.Errorf("the late write is in no copy: t1 %q, t2 %q, t3 %q; the trash of t1 holds %v", b.read(1, "new.mkv"),
			b.read(2, "new.mkv"), b.read(3, "new.mkv"), trashOf(b.mounts[0]))
	}
}

// trashOf lists what a disk's trash holds, its paths below Atoll/trash/.
func trashOf(mount string) []string {
	var out []string
	top := filepath.Join(mount, "Atoll", "trash")
	_ = filepath.WalkDir(top, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(top, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}
