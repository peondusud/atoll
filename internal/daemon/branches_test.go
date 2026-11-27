package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/watch"
)

// unpluggable is a machine of folder disks whose device can go, as after a USB reset: a disk whose device went is
// found missing for device-gone, a reason a USB reset gives, until it comes back. onDevice, when set, is told of each
// question about a disk's device, before it is answered.
type unpluggable struct {
	*folderDisks
	mu       sync.Mutex
	gone     map[string]bool
	onDevice func(mount string)
}

func (m *unpluggable) unplug(mount string, gone bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gone[mount] = gone
}

func (m *unpluggable) setOnDevice(fn func(mount string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onDevice = fn
}

func (m *unpluggable) Device(fs names.Filesystem, entry *disks.MountEntry) disks.DeviceSeen {
	m.mu.Lock()
	gone, hook := m.gone[entry.MountPoint], m.onDevice
	m.mu.Unlock()
	if hook != nil {
		hook(entry.MountPoint)
	}
	if gone {
		return disks.DeviceSeen{Device: disks.DeviceGoneAway, Note: "unplugged"}
	}
	return m.folderDisks.Device(fs, entry)
}

func (m *unpluggable) ReadPastCache(fs names.Filesystem, entry *disks.MountEntry) disks.DeviceSeen {
	return m.Device(fs, entry)
}

// fakeMergerfs stands for the control file of a mergerfs mounted at the view's mount point: it keeps the branches
// written, and counts the writes. onRead, when set, is called as the branches are read, which a pass does right before
// it decides them.
type fakeMergerfs struct {
	mu       sync.Mutex
	branches string
	writes   int
	onRead   func()
}

func (f *fakeMergerfs) ReadBranches(string) (string, error) {
	f.mu.Lock()
	branches, hook := f.branches, f.onRead
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return branches, nil
}

func (f *fakeMergerfs) WriteBranches(_, branches string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.branches = branches
	f.writes++
	return nil
}

func (f *fakeMergerfs) state() (string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.branches, f.writes
}

func (f *fakeMergerfs) setOnRead(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onRead = fn
}

// branchLoop is the daemon's loop on a bench, as Run makes it, its runtime branches written to control, and an
// /etc/fstab of its own with no entry, so that no check tries a remount for real.
func branchLoop(t *testing.T, c *config.Config, m disks.Machine, control branchIO) *loop {
	t.Helper()
	fstab := filepath.Join(t.TempDir(), "fstab")
	if err := os.WriteFile(fstab, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	opt := &Options{Config: c, Machine: m, Apply: true, Log: slog.New(slog.DiscardHandler), Fstab: fstab, Inotify: true}
	set, err := readSettings(c, opt)
	if err != nil {
		t.Fatal(err)
	}
	grouping, err := watch.NewGrouping(set.debounce.Nanoseconds())
	if err != nil {
		t.Fatal(err)
	}
	l := &loop{
		opt: opt, set: set, log: opt.Log, grouping: grouping, disks: map[string]*diskState{}, first: true, closed: jobs.Closed{},
		watcher: watch.NewWatcher(watch.WatcherOptions{ViewMount: set.viewMount, Now: m.Now, Inotify: true}),
		queues:  newQueues(set.servers, nil), changed: map[string]bool{}, heard: map[string]watch.Heard{}, lines: map[string]jobs.DiskLine{},
		mergerfs: control,
	}
	t.Cleanup(l.watcher.Close)
	return l
}

// hasBranch says whether a list of runtime branches holds the branch of the disk mounted at mount.
func hasBranch(branches, mount string) bool {
	return strings.Contains(":"+branches, ":"+filepath.Join(mount, "media")+"=")
}

// A disk lost and mounted again after a USB reset keeps its mergerfs branch (specs.md §15.13; the maintainer,
// 2026-11-27). The check that finds it missing tries its remount, which brings it back before the pass the loss calls
// for; that pass walks it, as its own mount check finds it there, but the loop still held it missing from that check,
// and the pass took its branch out of mergerfs, about 0.2 s after the remount, until the check after it found the disk
// back: its files were gone from the merged view meanwhile (TestTheDaemonUnderSystemd, in qemu, 2026-11-27). The pass
// checks the disk's mount again right before its branch would go, and spares it. Not when the remount failed, nor
// when the disk is lost again during the pass, after the pass's own check; and a stop of the daemon at that look
// decides nothing.
func TestADiskItsRemountBroughtBackKeepsItsBranch(t *testing.T) {
	for _, tc := range []struct {
		name string
		// back says that the remount brought the disk back before the pass.
		back bool
		// lostAgain loses the disk again during the pass, once its walk is over; stop stops the daemon at the look.
		lostAgain, stop bool
		// wantBranch is whether the disk has its branch after the pass, and wantWrite whether the pass wrote the branches.
		wantBranch, wantWrite bool
	}{
		{name: "the remount brought it back", back: true, wantBranch: true},
		{name: "the remount failed", wantBranch: false, wantWrite: true},
		{name: "lost again during the pass", back: true, lostAgain: true, wantBranch: false, wantWrite: true},
		{name: "a stop at the look", back: true, stop: true, wantBranch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, folders := lightBench(t)
			m := &unpluggable{folderDisks: folders, gone: map[string]bool{}}
			control := &fakeMergerfs{}
			l := branchLoop(t, c, m, control)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			t1, t2 := c.Disks[0].Mount, c.Disks[1].Mount

			// Both disks found and walked: mergerfs has both branches.
			l.check(ctx)
			l.pass(ctx, []string{BecauseStart})
			branches, writes := control.state()
			if !hasBranch(branches, t1) || !hasBranch(branches, t2) {
				t.Fatalf("after the first pass, the branches are %q", branches)
			}

			// t2's device goes, as after a USB reset: the check finds it missing, and its remount, at that check, brings
			// it back, or fails.
			m.unplug(t2, true)
			l.check(ctx)
			if st := l.disks["t2"]; st.state != disks.Missing || !st.reset || !l.gone {
				t.Fatalf("the check found t2 %s, reset %v, gone %v", st.state, st.reset, l.gone)
			}
			if tc.back {
				m.unplug(t2, false)
			}
			switch {
			case tc.lostAgain:
				control.setOnRead(func() { m.unplug(t2, true) })
			case tc.stop:
				// The look at t2's mount, the only question about a device once the branches are read.
				control.setOnRead(func() {
					m.setOnDevice(func(mount string) {
						if mount == t2 {
							cancel()
						}
					})
				})
			}
			l.pass(ctx, []string{BecauseGone})
			control.setOnRead(nil)
			m.setOnDevice(nil)

			after, writesAfter := control.state()
			if got := hasBranch(after, t2); got != tc.wantBranch {
				t.Errorf("t2's branch after the pass: %v, want %v (branches %q, before %q)", got, tc.wantBranch, after, branches)
			}
			if !hasBranch(after, t1) {
				t.Errorf("t1's branch went: %q", after)
			}
			if wrote := writesAfter > writes; wrote != tc.wantWrite {
				t.Errorf("the pass wrote the branches: %v, want %v", wrote, tc.wantWrite)
			}
			if tc.lostAgain && l.disks["t2"].walked {
				t.Error("t2, lost again, is still taken for walked")
			}
			if !tc.back || tc.lostAgain || tc.stop {
				return
			}
			// The next check finds it back; the pass for that walks it, and its branch stays where it is.
			l.check(ctx)
			if st := l.disks["t2"]; st.state == disks.Missing || !l.back {
				t.Fatalf("the next check found t2 %s, back %v", st.state, l.back)
			}
			l.pass(ctx, []string{BecauseBack})
			if last, n := control.state(); !hasBranch(last, t2) || n != writesAfter {
				t.Errorf("after the pass for back, the branches are %q, written %d times more", last, n-writesAfter)
			}
		})
	}
}
