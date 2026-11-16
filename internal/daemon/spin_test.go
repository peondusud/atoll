package daemon

import (
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/watch"
)

// spinDisks is a machine of folder disks, each on a whole disk of its own, sd-<id>, which it says asleep or spinning
// as a test sets it, and whose counters move as a test says.
type spinDisks struct {
	*folderDisks
	mu      sync.Mutex
	asleep  map[string]bool
	counts  map[string]int64
	moving  map[string]bool
	refuses map[string]bool
	asked   map[string]int
}

func newSpinDisks(m *folderDisks) *spinDisks {
	return &spinDisks{folderDisks: m, asleep: map[string]bool{}, counts: map[string]int64{}, moving: map[string]bool{}, refuses: map[string]bool{}, asked: map[string]int{}}
}

func (m *spinDisks) PhysicalDisks(_ names.Filesystem, entry *disks.MountEntry, _ string) ([]string, error) {
	return []string{"sd-" + filepath.Base(entry.MountPoint)}, nil
}

func (m *spinDisks) MountPointImmutable(string) (bool, error) { return false, nil }

func (m *spinDisks) Rotational(string) (bool, error) { return true, nil }

func (m *spinDisks) BlockStat(name string) (disks.BlockStat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.moving[name] {
		m.counts[name]++
	}
	return disks.BlockStat{Completed: m.counts[name]}, nil
}

func (m *spinDisks) PowerMode(name string) (disks.Power, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.asked[name]++
	switch {
	case m.refuses[name]:
		return disks.PowerUnknown, errors.New("invalid command operation code")
	case m.asleep[name]:
		return disks.PowerStandby, nil
	}
	return disks.PowerSpinning, nil
}

func (m *spinDisks) set(f func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f()
}

func (m *spinDisks) askedOf(name string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.asked[name]
}

// waitStates waits until the catalog holds these states of the disks.
func waitStates(t *testing.T, c *config.Config, want map[string]string) {
	t.Helper()
	db, err := catalog.OpenPool(t.Context(), c.Effective()["catalog"].(string), "p")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var got map[string]string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		got = map[string]string{}
		list, err := catalog.Disks(t.Context(), db)
		if err != nil {
			continue
		}
		same := true
		for _, d := range list {
			got[d.ID] = d.State
			same = same && want[d.ID] == d.State
		}
		if same && len(got) == len(want) {
			return
		}
	}
	t.Fatalf("the catalog holds %v, want %v", got, want)
}

// The daemon's check tells a disk in standby from one online, and records it in the catalog, where the view reads it
// (specs.md §10, §15.13). A whole disk whose counters move spins, and is asked nothing; one whose counters stand still
// is asked its power mode. Asleep, its disk is in standby; once its counters move, online again.
func TestTheCheckRecordsADiskInStandby(t *testing.T) {
	c, folders := lightBench(t)
	m := newSpinDisks(folders)
	m.set(func() { m.moving["sd-t1"], m.asleep["sd-t2"] = true, true })
	runLight(t, c, m)
	waitStates(t, c, map[string]string{"t1": "online", "t2": "standby"})
	if n := m.askedOf("sd-t1"); n > 1 {
		t.Errorf("a disk whose counters move was asked %d times", n)
	}
	if m.askedOf("sd-t2") == 0 {
		t.Error("a disk whose counters stand still was not asked")
	}
	m.set(func() { m.moving["sd-t2"] = true })
	waitStates(t, c, map[string]string{"t1": "online", "t2": "online"})
}

// A whole disk that gives no answer is asked no more until the configuration is read again: a USB bridge that refuses
// pass-through refuses it each time, and one that hangs would hold every check. Its disk counts as online.
func TestADiskThatGivesNoAnswerIsAskedNoMore(t *testing.T) {
	c, folders := lightBench(t)
	m := newSpinDisks(folders)
	m.set(func() { m.refuses["sd-t1"], m.asleep["sd-t1"] = true, true })
	grouping, err := watch.NewGrouping(0)
	if err != nil {
		t.Fatal(err)
	}
	l := &loop{
		opt: &Options{Machine: m, Config: c, Load: func() (*config.Config, error) { return c, nil }}, log: slog.New(slog.DiscardHandler),
		set:      &settings{checkEvery: 30 * time.Second, disksByID: map[string]*config.Disk{"t1": &c.Disks[0], "t2": &c.Disks[1]}},
		disks:    map[string]*diskState{},
		grouping: grouping,
		watcher:  watch.NewWatcher(watch.WatcherOptions{Now: m.Now, Inotify: true}),
	}
	defer l.watcher.Close()
	for range 3 {
		l.check(t.Context())
	}
	if n := m.askedOf("sd-t1"); n != 1 {
		t.Errorf("asked %d times", n)
	}
	if st := l.disks["t1"]; st == nil || st.standby {
		t.Errorf("the disk is %+v", st)
	}
	l.reload()
	l.check(t.Context())
	if n := m.askedOf("sd-t1"); n != 2 {
		t.Errorf("after the configuration was read again, asked %d times", n)
	}
}
