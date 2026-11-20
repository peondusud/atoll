package daemon

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/watch"
)

// unpluggedBench is a machine with one disk mounted from /dev/sdb1, or not mounted, whose device names lead where
// nodes says.
type unpluggedBench struct {
	disks.Machine
	mounted bool
	nodes   map[string]fsx.DeviceNode
	err     error
}

func (m *unpluggedBench) Mount(mount string) disks.MountSeen {
	if !m.mounted {
		return disks.MountSeen{Path: disks.PathOK}
	}
	return disks.MountSeen{Path: disks.PathOK, Entry: &disks.MountEntry{
		MountPoint: mount, Root: "/", FSType: "ext4", Source: "/dev/sdb1", MountOptions: []string{"rw"}, SuperOptions: []string{"rw"},
	}}
}

func (m *unpluggedBench) Device(names.Filesystem, *disks.MountEntry) disks.DeviceSeen {
	return disks.DeviceSeen{Device: disks.DeviceOK, Identity: &disks.Identity{ID: "3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b"}}
}

func (m *unpluggedBench) Root(string, string) disks.RootSeen {
	return disks.RootSeen{Folder: disks.FolderOK}
}

func (m *unpluggedBench) Hiberfil(string, *disks.MountEntry) disks.HiberfilSeen {
	return disks.HiberfilSeen{File: disks.HiberfilAbsent}
}

func (m *unpluggedBench) Now() int64 { return time.Now().UnixNano() }

func (m *unpluggedBench) DeviceNode(path string) (fsx.DeviceNode, error) {
	return m.nodes[path], m.err
}

// noNodes is the same machine, which cannot say what a device's name leads to, as the conformance build: only the
// methods of disks.Machine come through.
type noNodes struct{ disks.Machine }

// A disk found online, then not-mounted, as when systemd itself unmounts an idle fstab mount whose device went, is
// one a USB reset took when the device it was mounted from left: then each check tries the remount. Not when the
// device is where it was, as after an administrator's umount, nor when nothing tells, nor for a disk not mounted when
// the daemon started (specs.md §15.13).
func TestADiskNoLongerMountedIsResetWhenItsDeviceLeft(t *testing.T) {
	was := fsx.DeviceNode{There: true, Block: true, Major: 8, Minor: 17, Seq: 41}
	for _, tc := range []struct {
		name     string
		now      fsx.DeviceNode
		err      error
		atStart  bool
		identify bool
		want     bool
	}{
		{name: "its device gone", now: fsx.DeviceNode{}, identify: true, want: true},
		{name: "a disk plugged anew under its name", now: fsx.DeviceNode{There: true, Block: true, Major: 8, Minor: 17, Seq: 57}, identify: true, want: true},
		{name: "another device under its name", now: fsx.DeviceNode{There: true, Block: true, Major: 8, Minor: 33, Seq: 57}, identify: true, want: true},
		{name: "its device where it was", now: was, identify: true, want: false},
		{name: "its device not told", err: errors.New("stat: input/output error"), identify: true, want: false},
		{name: "a machine that tells no device", identify: false, want: false},
		{name: "not mounted when the daemon started", now: fsx.DeviceNode{}, atStart: true, identify: true, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list := []config.Disk{{ID: "u1", UUID: "3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b", Mount: "/mnt/u1", Root: "media", FS: names.Ext4}}
			m := &unpluggedBench{mounted: !tc.atStart, nodes: map[string]fsx.DeviceNode{"/dev/sdb1": was}}
			var machine disks.Machine = m
			if !tc.identify {
				machine = noNodes{m}
			}
			var said bytes.Buffer
			l := &loop{
				opt: &Options{Machine: machine, Config: &config.Config{Disks: list}}, log: slog.New(slog.NewTextHandler(&said, nil)),
				set:     &settings{checkEvery: 30 * time.Second, resetReasons: []disks.Reason{disks.DeviceGone, disks.DeviceUnread, disks.IOError}},
				disks:   map[string]*diskState{},
				watcher: watch.NewWatcher(watch.WatcherOptions{Now: m.Now, Inotify: true}),
			}
			defer l.watcher.Close()
			l.check(t.Context())
			m.mounted, m.nodes["/dev/sdb1"], m.err = false, tc.now, tc.err
			l.check(t.Context())
			st := l.disks["u1"]
			if st.state != disks.Missing {
				t.Fatalf("the disk is %s", st.state)
			}
			if st.reset != tc.want {
				t.Errorf("reset %v, want %v\n%s", st.reset, tc.want, said.String())
			}
			if left := strings.Contains(said.String(), "no longer mounted and its device left"); left != tc.want {
				t.Errorf("said the device left: %v\n%s", left, said.String())
			}
			// Back, the disk forgets it.
			m.mounted, m.nodes["/dev/sdb1"], m.err = true, was, nil
			l.check(t.Context())
			if st.reset {
				t.Error("a disk back is still reset")
			}
		})
	}
}
