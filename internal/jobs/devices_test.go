package jobs

import (
	"errors"
	"maps"
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
// mount it cannot read, so that the plan counts two disks on one as one (specs.md §10, R-06); a later start records what
// changed, on every disk after one that did not; a disk it cannot ask keeps what was found, and a machine that cannot
// tell records nothing.
func TestAStartRecordsThePhysicalDisksOfEachDisk(t *testing.T) {
	b := newBench(t, "t1", "t2", "t3", "t4", "t5")
	b.scan(false)
	b.config.Disks[2].State = config.DiskFailed
	b.gone[b.dirs["t5"]] = true
	m := &topology{bench: b, unknown: map[string]bool{}, physical: map[string][]string{
		b.dirs["t1"]: {"sda"}, b.dirs["t2"]: {"sda", "sdb"}, b.dirs["t3"]: {"sdc"}, b.dirs["t4"]: {"sdd"}, b.dirs["t5"]: {"sde"},
	}}
	mounted := func(i int) bool { return i != 3 }
	RecordDevices(t.Context(), b.db, b.config, m, mounted)
	if got, want := b.devicesOf(), map[string]string{"t1": "sda", "t2": "sda,sdb", "t3": "", "t4": "", "t5": ""}; !maps.Equal(got, want) {
		t.Errorf("first start: %v, want %v", got, want)
	}
	// t1 as it was, t2 moved to another disk: its row follows.
	m.physical[b.dirs["t2"]] = []string{"sdb"}
	RecordDevices(t.Context(), b.db, b.config, m, mounted)
	if got, want := b.devicesOf(), map[string]string{"t1": "sda", "t2": "sdb", "t3": "", "t4": "", "t5": ""}; !maps.Equal(got, want) {
		t.Errorf("t2 moved: %v, want %v", got, want)
	}
	// t1 cannot be asked, and keeps what was found; t2 moves back all the same.
	m.unknown[b.dirs["t1"]] = true
	m.physical[b.dirs["t1"]] = []string{"sdz"}
	m.physical[b.dirs["t2"]] = []string{"sda", "sdb"}
	RecordDevices(t.Context(), b.db, b.config, m, mounted)
	if got, want := b.devicesOf(), map[string]string{"t1": "sda", "t2": "sda,sdb", "t3": "", "t4": "", "t5": ""}; !maps.Equal(got, want) {
		t.Errorf("t1 not asked: %v, want %v", got, want)
	}
	// A machine with no topology records nothing.
	m.physical[b.dirs["t2"]] = []string{"sdy"}
	RecordDevices(t.Context(), b.db, b.config, b, mounted)
	if got := b.devicesOf()["t2"]; got != "sda,sdb" {
		t.Errorf("no topology: t2 %q", got)
	}
}
