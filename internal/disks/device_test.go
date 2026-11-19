package disks

import (
	"testing"

	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/vectors"
)

// Whether the device a disk was mounted from left, for a disk found online, then not-mounted: systemd unmounts an
// idle fstab mount whose device goes, and only a device that left makes that a USB reset (specs.md §15.13).
func TestDeviceLeftVectors(t *testing.T) {
	type node struct {
		There bool   `json:"there"`
		Block bool   `json:"block"`
		Major uint32 `json:"major"`
		Minor uint32 `json:"minor"`
		Seq   uint64 `json:"seq"`
	}
	type vector struct {
		Before node   `json:"before"`
		Now    node   `json:"now"`
		Left   bool   `json:"left"`
		Note   string `json:"note"`
	}
	of := func(n node) fsx.DeviceNode {
		return fsx.DeviceNode{There: n.There, Block: n.Block, Major: n.Major, Minor: n.Minor, Seq: n.Seq}
	}
	for i, v := range vectors.Load[vector](t, "device_left.json") {
		if got := DeviceLeft(of(v.Before), of(v.Now)); got != v.Left {
			t.Errorf("case %d (%s): left %v, want %v", i, v.Note, got, v.Left)
		}
	}
}

// The mount check gives the device a disk that is not missing was mounted from, as the mount table names it, and
// none for a missing disk (specs.md §15.13).
func TestACheckGivesTheSourceOfADiskFound(t *testing.T) {
	d, a := answersFor(&checkCase{})
	a.seen.Entry.Source = "/dev/sdb1"
	if got := Check(a, d); got.State == Missing || got.Source != "/dev/sdb1" {
		t.Errorf("online: %+v", got)
	}
	a.root = RootSeen{Folder: FolderAbsent}
	if got := Check(a, d); got.State != Missing || got.Source != "" {
		t.Errorf("missing: %+v", got)
	}
}
