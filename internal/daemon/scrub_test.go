package daemon

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/names"
)

// The daemon's scrub takes a corrupt copy an earlier scrub left in the root once in each stretch of integrity.window,
// whichever slice reaches it, and brings a pass once it repaired or moved one, whose alerts end the file's and which
// makes the copy again (specs.md §15.13, §16). Taken at each slice, a copy that cannot go read its other copy whole
// every 30 s all night.
func TestTheDaemonTakesACorruptCopyAgainOnceAStretch(t *testing.T) {
	c, m, _ := upsBench(t, "[integrity]\nauto_repair = false\n[[rule]]\nlabel = \"all\"\npath = \"**\"\nmode = \"immediate\"\n")
	file, _ := c.Effective()["catalog"].(string)
	db, err := catalog.OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for range 2 {
		if _, err := jobs.Run(t.Context(), db, c, m, true); err != nil {
			t.Fatal(err)
		}
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	// rot gives a copy other bytes of the same size, its row the times the write gave, as rot that no row tells.
	rot := func(i int, text string) {
		t.Helper()
		path := filepath.Join(c.Disks[i].Mount, "media", "a")
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		st, _ := info.Sys().(*syscall.Stat_t)
		exec("UPDATE copies SET mtime_ns = ?, ctime_ns = ?, verified_ns = ? WHERE disk_id = ?", info.ModTime().UnixNano(), st.Ctim.Nano(), time.Now().UnixNano(),
			c.Disks[i].ID)
	}
	// The copy on t2 found corrupt by a scrub that could not move it; the one on t1 rotten too, though its row says ok.
	rot(1, "y")
	exec("UPDATE copies SET state = 'corrupt', verified_ns = NULL WHERE disk_id = 't2'")
	exec("UPDATE files SET state = 'corrupt'")
	rot(0, "z")
	var log bytes.Buffer
	woken := &wokenDisks{Machine: m, woken: map[string]int{}}
	l := &loop{opt: &Options{Machine: woken, Config: c, Apply: true}, set: &settings{catalog: file, scrubBudget: time.Hour},
		log: slog.New(slog.NewTextHandler(&log, nil))}
	// Taking it opens both disks, which confirms each, a read of its head past the cache: nothing else is due.
	taken := func(slice string, want int, pass bool) {
		t.Helper()
		if got := strings.Count(log.String(), " again=1 "); got != want || woken.times(c.Disks[0].Mount) != want || woken.times(c.Disks[1].Mount) != want {
			t.Errorf("%s: taken %d times in all, the disks opened %d and %d times, want %d:\n%s", slice, got, woken.times(c.Disks[0].Mount),
				woken.times(c.Disks[1].Mount), want, log.String())
		}
		if (l.passDue != nil) != pass {
			t.Errorf("%s: a pass due %v", slice, l.passDue)
		}
	}
	l.scrub(t.Context(), 1)
	taken("the first slice", 1, false)
	l.scrub(t.Context(), 1)
	taken("another slice of the same stretch", 1, false)
	l.scrub(t.Context(), 2)
	taken("the next stretch", 2, false)
	// Its other copy healthy again, the next stretch moves it to the trash, and brings a pass.
	rot(0, "x")
	l.scrub(t.Context(), 3)
	taken("once it can go", 3, true)
	if _, err := os.Lstat(filepath.Join(c.Disks[1].Mount, "media", "a")); !os.IsNotExist(err) {
		t.Errorf("the corrupt copy is still in the root: %v", err)
	}
	var state string
	if err := db.QueryRowContext(t.Context(), "SELECT state FROM files").Scan(&state); err != nil || state == "corrupt" {
		t.Errorf("the file is %s, %v", state, err)
	}
}

// wokenDisks counts, by mount, the reads of a disk's head past the cache: what confirms a disk, and wakes it.
type wokenDisks struct {
	disks.Machine
	mu    sync.Mutex
	woken map[string]int
}

func (m *wokenDisks) ReadPastCache(fs names.Filesystem, entry *disks.MountEntry) disks.DeviceSeen {
	m.mu.Lock()
	m.woken[entry.MountPoint]++
	m.mu.Unlock()
	return m.Machine.ReadPastCache(fs, entry)
}

func (m *wokenDisks) times(mount string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.woken[mount]
}
