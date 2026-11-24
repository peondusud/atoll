package jobs

import (
	"bytes"
	"errors"
	"log/slog"
	"maps"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/names"
)

// topology is the bench with the physical disks below each mount, as /sys tells them, and the mounts it cannot tell of.
type topology struct {
	*bench
	physical map[string][]string
	unknown  map[string]bool
}

func (t *topology) PhysicalDisks(_ names.Filesystem, entry *disks.MountEntry, _ string) ([]string, error) {
	if t.unknown[entry.MountPoint] {
		return nil, errors.New("no /sys")
	}
	return t.physical[entry.MountPoint], nil
}

func (t *topology) MountPointImmutable(string) (bool, error) { return false, nil }

// devicesOf gives each disk's recorded physical disks, by id.
func (b *bench) devicesOf() map[string]string {
	b.t.Helper()
	list, err := catalog.Disks(b.t.Context(), b.db)
	if err != nil {
		b.t.Fatal(err)
	}
	out := map[string]string{}
	for i := range list {
		out[list[i].ID] = list[i].Device
	}
	return out
}

// A start records the physical disks each disk lives on that its check found mounted, a failed disk aside, and one whose
// mount it cannot read, and the disks after these all the same, so that the plan counts two disks on one as one
// (specs.md §10, R-06); a later start records what changed, on every disk after one that did not; a disk it cannot ask
// keeps what was found, and a machine that cannot tell records nothing.
func TestAStartRecordsThePhysicalDisksOfEachDisk(t *testing.T) {
	b := newBench(t, "t1", "t2", "t3", "t4", "t5")
	b.scan(false)
	b.config.Disks[0].State = config.DiskFailed
	b.gone[b.dirs["t3"]] = true
	m := &topology{bench: b, unknown: map[string]bool{}, physical: map[string][]string{
		b.dirs["t1"]: {"sda"}, b.dirs["t2"]: {"sdb"}, b.dirs["t3"]: {"sdc"}, b.dirs["t4"]: {"sdd"}, b.dirs["t5"]: {"sdd", "sde"},
	}}
	mounted := func(i int) bool { return i != 1 }
	RecordDevices(t.Context(), b.db, b.config, m, mounted)
	if got, want := b.devicesOf(), map[string]string{"t1": "", "t2": "", "t3": "", "t4": "sdd", "t5": "sdd,sde"}; !maps.Equal(got, want) {
		t.Errorf("first start: %v, want %v", got, want)
	}
	// t4 as it was, t5 moved to another disk: its row follows.
	m.physical[b.dirs["t5"]] = []string{"sde"}
	RecordDevices(t.Context(), b.db, b.config, m, mounted)
	if got, want := b.devicesOf(), map[string]string{"t1": "", "t2": "", "t3": "", "t4": "sdd", "t5": "sde"}; !maps.Equal(got, want) {
		t.Errorf("t5 moved: %v, want %v", got, want)
	}
	// t4 cannot be asked, and keeps what was found; t5 moves back all the same.
	m.unknown[b.dirs["t4"]] = true
	m.physical[b.dirs["t4"]] = []string{"sdz"}
	m.physical[b.dirs["t5"]] = []string{"sdd", "sde"}
	RecordDevices(t.Context(), b.db, b.config, m, mounted)
	if got, want := b.devicesOf(), map[string]string{"t1": "", "t2": "", "t3": "", "t4": "sdd", "t5": "sdd,sde"}; !maps.Equal(got, want) {
		t.Errorf("t4 not asked: %v, want %v", got, want)
	}
	// A machine with no topology records nothing.
	m.physical[b.dirs["t5"]] = []string{"sdy"}
	RecordDevices(t.Context(), b.db, b.config, b, mounted)
	if got := b.devicesOf()["t5"]; got != "sdd,sde" {
		t.Errorf("no topology: t5 %q", got)
	}
}

// A catalog that cannot record a disk's physical disks says so, for each disk, and a start goes on (specs.md §10).
func TestAStartSaysWhatItCouldNotRecord(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.scan(false)
	m := &topology{bench: b, physical: map[string][]string{b.dirs["t1"]: {"sda"}, b.dirs["t2"]: {"sdb"}}}
	var logged bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	defer slog.SetDefault(old)
	RecordDevices(t.Context(), b.db, b.config, m, func(int) bool { return true })
	if strings.Contains(logged.String(), "not recorded") {
		t.Errorf("recorded, and said not to be: %s", logged.String())
	}
	if _, err := b.db.ExecContext(t.Context(), "CREATE TRIGGER refuse BEFORE UPDATE OF device ON disks BEGIN SELECT RAISE(ABORT, 'refused'); END"); err != nil {
		t.Fatal(err)
	}
	m.physical[b.dirs["t1"]], m.physical[b.dirs["t2"]] = []string{"sdc"}, []string{"sdd"}
	logged.Reset()
	RecordDevices(t.Context(), b.db, b.config, m, func(int) bool { return true })
	if n := strings.Count(logged.String(), "the physical disks of a disk are not recorded"); n != 2 {
		t.Errorf("%d warnings: %s", n, logged.String())
	}
}
