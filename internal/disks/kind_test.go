package disks

import (
	"errors"
	"testing"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/names"
)

// kinds is a machine that tells the whole disks below each mount, and whether each has a spindle.
type kinds struct {
	pastCache
	unmounted bool
	whole     []string
	wholeErr  error
	spindle   map[string]bool // a whole disk missing here gives no answer
}

func (k *kinds) Mount(mount string) MountSeen {
	if k.unmounted {
		return MountSeen{Path: PathOK}
	}
	return k.pastCache.Mount(mount)
}

func (k *kinds) PhysicalDisks(names.Filesystem, *MountEntry, string) ([]string, error) {
	return k.whole, k.wholeErr
}

func (k *kinds) MountPointImmutable(string) (bool, error) { return false, nil }

func (k *kinds) Rotational(name string) (bool, error) {
	spins, ok := k.spindle[name]
	if !ok {
		return false, errors.New("no answer")
	}
	return spins, nil
}

func (k *kinds) BlockStat(string) (BlockStat, error) { return BlockStat{}, errors.New("none") }
func (k *kinds) PowerMode(string) (Power, error)     { return PowerUnknown, errors.New("none") }

// The kind of a disk goes by every whole disk below it, and anything unknown counts as a spindle: one copy at a time
// (specs.md §14, the maintainer, 2026-11-19).
func TestKindOf(t *testing.T) {
	d := &config.Disk{ID: "t1", UUID: "id", Mount: "/m", FS: names.Ext4, Root: "media"}
	for _, tc := range []struct {
		name string
		m    *kinds
		want Kind
	}{
		{"an SSD", &kinds{whole: []string{"sda"}, spindle: map[string]bool{"sda": false}}, KindSolid},
		{"an NVMe drive", &kinds{whole: []string{"nvme0n1"}, spindle: map[string]bool{"nvme0n1": false}}, KindSolid},
		{"a disk with a spindle", &kinds{whole: []string{"sdb"}, spindle: map[string]bool{"sdb": true}}, KindRotational},
		{"two SSDs below one filesystem", &kinds{whole: []string{"sda", "sdc"}, spindle: map[string]bool{"sda": false, "sdc": false}}, KindSolid},
		{"an SSD and a spindle below one filesystem", &kinds{whole: []string{"sda", "sdb"}, spindle: map[string]bool{"sda": false, "sdb": true}}, KindRotational},
		{"a whole disk that gives no answer", &kinds{whole: []string{"sda", "sdx"}, spindle: map[string]bool{"sda": false}}, KindRotational},
		{"no whole disk found", &kinds{spindle: map[string]bool{}}, KindRotational},
		{"whole disks that cannot be told", &kinds{whole: []string{"sda"}, wholeErr: errors.New("sysfs"), spindle: map[string]bool{"sda": false}}, KindRotational},
		{"not mounted", &kinds{unmounted: true, whole: []string{"sda"}, spindle: map[string]bool{"sda": false}}, KindRotational},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !ReadsKinds(tc.m) {
				t.Fatal("the machine reads no kinds")
			}
			if got := KindOf(tc.m, d); got != tc.want {
				t.Errorf("%s, want %s", got, tc.want)
			}
		})
	}
	// A machine that cannot tell, as the conformance build: every disk counts as a spindle, and it reads no kind.
	plain := &pastCache{mode: "rw"}
	if ReadsKinds(plain) {
		t.Error("a machine with no topology reads kinds")
	}
	if got := KindOf(plain, d); got != KindRotational {
		t.Errorf("a machine that cannot tell: %s", got)
	}
}
