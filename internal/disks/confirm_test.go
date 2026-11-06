package disks

import (
	"testing"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
)

// pastCache is a machine whose cached answers say the disk is fine, and whose read past the cache says
// what a test wants.
type pastCache struct {
	past DeviceSeen
	mode string
}

func (a *pastCache) Mount(mount string) MountSeen {
	return MountSeen{Path: PathOK, Entry: &MountEntry{
		MountPoint: mount, Root: "/", FSType: "ext4", Source: "/dev/x", MountOptions: []string{a.mode}, SuperOptions: []string{a.mode},
	}}
}

func (a *pastCache) Device(names.Filesystem, *MountEntry) DeviceSeen {
	return DeviceSeen{Device: DeviceOK, Identity: &Identity{ID: "id"}}
}

func (a *pastCache) Root(string, string) RootSeen { return RootSeen{Folder: FolderOK} }
func (a *pastCache) Now() int64                   { return 0 }
func (a *pastCache) Space(string) (capacity, free int64, err error) {
	return 0, 0, nil
}
func (a *pastCache) OpenRoot(string, string) (fsx.Dir, error) { return nil, nil }
func (a *pastCache) OpenTop(string) (fsx.Dir, error)          { return nil, nil }
func (a *pastCache) Checkpoint(string)                        {}
func (a *pastCache) ReadPastCache(names.Filesystem, *MountEntry) DeviceSeen {
	return a.past
}

func TestConfirm(t *testing.T) {
	d := &config.Disk{ID: "t1", UUID: "id", Mount: "/m", FS: names.Ext4, Root: "media"}
	good := DeviceSeen{Device: DeviceOK, Identity: &Identity{ID: "id"}}
	for _, tc := range []struct {
		name   string
		past   DeviceSeen
		mode   string
		state  State
		reason Reason
	}{
		{"healthy", good, "rw", Online, ""},
		{"a device that does not answer", DeviceSeen{Device: DeviceUnreadable, Note: "EIO"}, "rw", Missing, DeviceUnread},
		{"a device that went", DeviceSeen{Device: DeviceGoneAway}, "rw", Missing, DeviceGone},
		{"no filesystem any more", DeviceSeen{Device: DeviceOK}, "rw", Missing, WrongFilesystem},
		{"another disk", DeviceSeen{Device: DeviceOK, Identity: &Identity{ID: "other"}}, "rw", Missing, WrongIdentifier},
		{"read-only is still read-only", good, "ro", Readonly, ReadOnlyMount},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Confirm(&pastCache{past: tc.past, mode: tc.mode}, d)
			if got.State != tc.state || got.Reason != tc.reason {
				t.Errorf("%+v, want %s %s", got, tc.state, tc.reason)
			}
		})
	}
	t.Run("the check comes first", func(t *testing.T) {
		other := &config.Disk{ID: "t1", UUID: "not-id", Mount: "/m", FS: names.Ext4, Root: "media"}
		got := Confirm(&pastCache{past: good, mode: "rw"}, other)
		if got.State != Missing || got.Reason != WrongIdentifier {
			t.Errorf("%+v", got)
		}
	})
}
