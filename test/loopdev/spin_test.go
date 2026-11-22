//go:build loopdev

package loopdev

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/names"
)

// TestSpinReadsOfAWholeDisk: what the daemon's check reads of the whole disk below a mount, on the kernels themselves
// (specs.md §10). Its counters read, and move with a write that reaches the disk; whether it has a spindle reads; and a
// loop device, no SCSI disk, gives no answer to CHECK POWER MODE, so its disk counts as spinning, and nothing woke.
func TestSpinReadsOfAWholeDisk(t *testing.T) {
	needRoot(t)
	v := newVolume(t, names.Ext4)
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	m, err := disks.NewMachine()
	mustDo(t, err)
	reader, reads := m.(disks.SpinReader)
	topology, tells := m.(disks.Topology)
	if !reads || !tells {
		t.Fatal("the delivered machine tells nothing of spinning")
	}
	seen := m.Mount(at)
	if seen.Entry == nil {
		t.Fatalf("%s is not mounted", at)
	}
	list, err := topology.PhysicalDisks(names.Ext4, seen.Entry, "")
	mustDo(t, err)
	if want := filepath.Base(v.device); !slices.Equal(list, []string{want}) {
		t.Fatalf("whole disks %q, want %q", list, want)
	}
	name := list[0]
	if _, err := reader.Rotational(name); err != nil {
		t.Errorf("rotational: %v", err)
	}
	before, err := reader.BlockStat(name)
	mustDo(t, err)
	file := filepath.Join(at, "written")
	mustDo(t, os.WriteFile(file, make([]byte, 1<<20), 0o644))
	f, err := os.Open(file)
	mustDo(t, err)
	mustDo(t, f.Sync())
	mustDo(t, f.Close())
	after, err := reader.BlockStat(name)
	mustDo(t, err)
	if after.Completed <= before.Completed {
		t.Errorf("a write and its sync moved no counter: %+v, then %+v", before, after)
	}
	p, err := reader.PowerMode(name)
	if err == nil || p != disks.PowerUnknown {
		t.Errorf("a loop device answered CHECK POWER MODE: %q, %v", p, err)
	}
	if errors.Is(err, disks.ErrNoPowerMode) {
		t.Logf("its sense data said nothing: %v", err)
	}
	asked := 0
	standby := disks.Standby([]disks.WholeDiskLook{{Name: name, Before: &before.Completed, Now: &before.Completed}}, func(string) disks.Power {
		asked++
		p, _ := reader.PowerMode(name)
		return p
	})
	if standby || asked != 1 {
		t.Errorf("standby %v, asked %d times", standby, asked)
	}
}
