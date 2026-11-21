//go:build loopdev

package loopdev

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/jobs"
)

// faultTop is a machine whose disks' tops, by mount, can make a call fail, as the unit tests' fault layer does
// (internal/fsx): here, a look that finds a folder absent on a real filesystem.
type faultTop struct {
	disks.Machine
	fail map[string]func(call, name string) error
}

func (m *faultTop) OpenTop(mount string) (fsx.Dir, error) {
	dir, err := m.Machine.OpenTop(mount)
	if err != nil || m.fail[mount] == nil {
		return dir, err //nolint:wrapcheck // a test double
	}
	return fsx.Fault(dir, m.fail[mount]), nil
}

// replicateStopping runs a copy of a path that stops at a point, as kill -9 would.
func (b *passBench) replicateStopping(path, point string) {
	b.t.Helper()
	machine := b.machine
	defer func() {
		b.machine = machine
		if err, ok := recover().(error); !ok || !errors.Is(err, errCrash) {
			b.t.Fatalf("it did not stop at %s: %v", point, err)
		}
	}()
	b.machine = &crashAt{Machine: machine, point: point}
	_, _ = jobs.Replicate(b.t.Context(), b.db, b.config, b.machine, jobs.ReplicateOptions{Path: path, Now: true}, func(*jobs.ReplicateItem) error { return nil })
}

// TestAFolderWithNoModelOnEachDriver: under the units' mask, 0027, a folder a copy makes below the root that stands for
// none is 0755 on ext4, XFS, btrfs, ntfs3 and the type ntfs, whose mkdir gave it 0750, a media server running under
// another account reading below it; and killed between its mkdir and the mode it is then given, the reprise gives it
// 0755. ntfs-3g and lowntfs-3g give it what their mount says, 0777, the copy landing all the same; nothing is left
// planned on any driver (specs.md §17.2, §17.4).
func TestAFolderWithNoModelOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newPassBench(t, v, at)
			write(t, b.keep, "p/a", "a film")
			write(t, b.keep, "q/b", "another film")
			b.scan(false)
			// Neither folder reads as there on the source's disk, as for a copy behind at its old path: no model.
			b.machine = &faultTop{Machine: b.machine, fail: map[string]func(string, string) error{b.keep: func(call, name string) error {
				if call == "stat" && (name == "p" || name == "q") {
					return syscall.ENOENT
				}
				return nil
			}}}
			old := syscall.Umask(0o027)
			t.Cleanup(func() { syscall.Umask(old) })
			modeOn := func(folder string) uint32 {
				var st syscall.Stat_t
				mustDo(t, syscall.Lstat(filepath.Join(at, "media", folder), &st))
				return st.Mode & 0o7777
			}
			opState := func(folder string) string {
				return b.text("SELECT group_concat(state) FROM ops WHERE kind = 'mkdir' AND CAST(path AS TEXT) = ?", folder)
			}
			// ntfs-3g and lowntfs-3g, mounted without permissions, give every folder the mode their mount says, 0777
			// here, whatever is asked (measured); the other drivers keep what they are given.
			fuse := tc.driver == "ntfs-3g" || tc.driver == "lowntfs-3g"
			made, wanted := uint32(0o750), uint32(0o755)
			if fuse {
				made, wanted = 0o777, 0o777
			}

			if items := b.replicate("p"); len(items) != 1 || items[0].Outcome != jobs.OutcomeReplicated {
				t.Fatalf("%+v", items)
			}
			t.Logf("%s: a folder made with no model is %04o", tc.name, modeOn("p"))
			if got := modeOn("p"); got != wanted {
				t.Errorf("p is %04o, want %04o", got, wanted)
			}
			if got := opState("p"); got != "done" {
				t.Errorf("p's operation: %q", got)
			}

			b.replicateStopping("q", "mkdir-made")
			t.Logf("%s: as mkdir made it, %04o", tc.name, modeOn("q"))
			if got := modeOn("q"); got != made {
				t.Errorf("q as mkdir made it: %04o, want %04o", got, made)
			}
			if got := opState("q"); got != "planned" {
				t.Fatalf("q's operation: %q", got)
			}
			if items := b.replicate("q"); len(items) != 1 || items[0].Outcome != jobs.OutcomeReplicated {
				t.Errorf("after the reprise: %+v", items)
			}
			t.Logf("%s: after the reprise, %04o", tc.name, modeOn("q"))
			if got := modeOn("q"); got != wanted {
				t.Errorf("q after the reprise: %04o, want %04o", got, wanted)
			}
			if got := opState("q"); got != "done" {
				t.Errorf("q's operation: %q", got)
			}
			if data, err := os.ReadFile(filepath.Join(at, "media", "q", "b")); err != nil || string(data) != "another film" {
				t.Errorf("the copy: %q, %v", data, err)
			}
			if got := b.text("SELECT count(*) FROM ops WHERE state = 'planned'"); got != "0" {
				t.Errorf("%s operations planned", got)
			}
		})
	}
}
