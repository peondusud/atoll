//go:build loopdev

package loopdev

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/names"
)

// TestTopologyAndImmutable: two filesystems on device-mapper targets of one loop device live on that one disk, and
// one on a loop device of its own on another, as atoll disk add reads them to refuse two filesystems of a pool on
// one disk (R-06); and the folder under a mount point reads immutable, or not, through the clone of its parent's
// mount, whatever the disk mounted over it holds (specs.md §10).
func TestTopologyAndImmutable(t *testing.T) {
	needRoot(t)
	img := filepath.Join(t.TempDir(), "two.img")
	mustDo(t, os.WriteFile(img, nil, 0o600))
	const mb = 256
	mustDo(t, os.Truncate(img, mb<<20))
	loop := must(t, "losetup", "-f", "--show", img)
	t.Cleanup(func() { _ = exec.CommandContext(context.Background(), "losetup", "-d", loop).Run() })
	half := int64(mb<<20) / 512 / 2
	var mapped []string
	for i := range 2 {
		name := fmt.Sprintf("atoll-topology-%d-%d", os.Getpid(), i)
		// No udev here: the node comes from mknodes, as newMapped does.
		must(t, "dmsetup", "create", "--noudevsync", "--noudevrules", name, "--table", fmt.Sprintf("0 %d linear %s %d", half, loop, int64(i)*half))
		must(t, "dmsetup", "mknodes")
		t.Cleanup(func() {
			_ = exec.CommandContext(context.Background(), "dmsetup", "remove", "-f", "--noudevsync", name).Run()
		})
		must(t, "mkfs.ext4", "-q", "/dev/mapper/"+name)
		mapped = append(mapped, "/dev/mapper/"+name)
	}
	other := newVolume(t, names.Ext4)
	at := []string{mountPoint(t), mountPoint(t), mountPoint(t)}
	for _, p := range at {
		mustDo(t, os.MkdirAll(p, 0o755))
	}
	// Immutable before the mount, and mutable again before the folders go.
	must(t, "chattr", "+i", at[0])
	t.Cleanup(func() { _ = exec.CommandContext(context.Background(), "chattr", "-i", at[0]).Run() })
	mount(t, at[0], "mount", mapped[0])
	mount(t, at[1], "mount", mapped[1])
	mountVolume(t, other, "", at[2])

	m, err := disks.NewMachine()
	mustDo(t, err)
	topo, ok := m.(disks.Topology)
	if !ok {
		t.Fatal("the machine cannot tell the topology")
	}
	physical := func(mount string) []string {
		t.Helper()
		seen := m.Mount(mount)
		if seen.Entry == nil {
			t.Fatalf("%s: not mounted: %+v", mount, seen)
		}
		list, err := topo.PhysicalDisks(names.Ext4, seen.Entry, "")
		mustDo(t, err)
		return list
	}
	base := filepath.Base(loop)
	if a, b := physical(at[0]), physical(at[1]); !slices.Equal(a, []string{base}) || !slices.Equal(b, []string{base}) {
		t.Errorf("two targets of %s: %v and %v", loop, a, b)
	}
	if c := physical(at[2]); !slices.Equal(c, []string{filepath.Base(other.device)}) {
		t.Errorf("a loop device of its own, %s: %v", other.device, c)
	}
	for i, want := range []bool{true, false, false} {
		got, err := topo.MountPointImmutable(at[i])
		if err != nil || got != want {
			t.Errorf("%s: immutable %v, %v", at[i], got, err)
		}
	}
}
