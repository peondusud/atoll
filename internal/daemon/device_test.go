package daemon

import (
	"errors"
	"maps"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/names"
)

// devices gives each disk's recorded physical disks, by id.
func devices(t *testing.T, l *loop) map[string]string {
	t.Helper()
	db, err := catalog.OpenPool(t.Context(), l.set.catalog, "p")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	list, err := catalog.Disks(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, d := range list {
		out[d.ID] = d.Device
	}
	return out
}

// brokenTopology answers for no disk.
type brokenTopology struct{ *smartBench }

func (brokenTopology) PhysicalDisks(names.Filesystem, *disks.MountEntry, string) ([]string, error) {
	return nil, errors.New("no /sys")
}

// noTopology cannot tell which physical disk a disk lives on.
type noTopology struct{ disks.Machine }

// At each start the daemon records the physical disks of each disk the check found mounted, missing and failed ones
// aside, so that the plan counts two on one as one (specs.md §10, R-06); a later start that finds them apart says
// so, a disk it cannot ask keeps what was found, and a machine that cannot tell records nothing.
func TestLookAtDevices(t *testing.T) {
	l, m := smartLoop(t, true, false)
	// t1 and t2 share sda; t3 is failed, and t4 missing.
	m.devices["/mnt/t2"] = m.devices[l.opt.Config.Disks[0].Mount]
	l.lookAtDevices(t.Context())
	if got := devices(t, l); !maps.Equal(got, map[string]string{"t1": "sda", "t2": "sda", "t3": "", "t4": ""}) {
		t.Errorf("first start: %v", got)
	}
	// Apart again: the next start records it.
	m.devices["/mnt/t2"] = "sdb"
	l.lookAtDevices(t.Context())
	if got := devices(t, l); !maps.Equal(got, map[string]string{"t1": "sda", "t2": "sdb", "t3": "", "t4": ""}) {
		t.Errorf("a later start: %v", got)
	}
	l.opt.Machine = brokenTopology{m}
	l.lookAtDevices(t.Context())
	if got := devices(t, l); got["t2"] != "sdb" {
		t.Errorf("a disk it could not ask: %v", got)
	}
	m.devices["/mnt/t2"] = "sdz"
	l.opt.Machine = noTopology{m}
	l.lookAtDevices(t.Context())
	if got := devices(t, l); got["t2"] != "sdb" {
		t.Errorf("a machine that cannot tell: %v", got)
	}
}
