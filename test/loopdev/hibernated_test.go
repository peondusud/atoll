//go:build loopdev

package loopdev

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/names"
)

// The contents of a hiberfil.sys that the tests below put at the top of a volume, as docs/research/ntfs-driver.md did:
// under 4 KiB, the magic of a hibernation, and the zeros Windows leaves once it resumed.
var (
	hiberfilShort   = []byte("0123456789")
	hiberfilMagic   = append([]byte("HIBR"), make([]byte, 8<<10-4)...)
	hiberfilResumed = make([]byte, 8<<10)
)

// hibernatedVolume makes an NTFS volume whose top holds hiberfil.sys with that content, and media/, written through
// ntfs-3g and let go of: a volume Windows left hibernated, as far as the drivers tell, each of which gets a fresh one,
// as one that mounts it read-write may mark it dirty.
func hibernatedVolume(t *testing.T, content []byte) *volume {
	t.Helper()
	v := newVolume(t, names.NTFS)
	at := mountPoint(t)
	mountVolume(t, v, "ntfs-3g", at)
	mustDo(t, os.Mkdir(filepath.Join(at, "media"), 0o755))
	if content != nil {
		mustDo(t, os.WriteFile(filepath.Join(at, disks.HiberfilName), content, 0o644))
	}
	unmountFor(t, "ntfs-3g", at)
	return v
}

// mountedAs says how a driver mounted a volume, from the mount table.
func mountedAs(t *testing.T, m disks.Machine, at string) string {
	t.Helper()
	seen := m.Mount(at)
	if seen.Entry == nil {
		t.Fatalf("%s is not mounted: %+v", at, seen)
	}
	return seen.Entry.FSType + " " + strings.Join(seen.Entry.MountOptions, ",") + " " + strings.Join(seen.Entry.SuperOptions, ",")
}

// TestAHibernatedVolumeOnEachDriver: a volume whose top holds a hiberfil.sys that says Windows is hibernated is
// readonly, hibernated, whatever its driver did with it, read-write or read-only, and whatever its driver's options
// say after them; one Windows resumed is online (specs.md §10, §22). Each driver's mount is logged, for what it does
// with such a volume. The check reads hiberfil.sys without changing its access time.
func TestAHibernatedVolumeOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, driver := range []struct {
		name, driver string
		options      func(t *testing.T) []string
		// first is the reason a driver's options give before hibernated, where they give one.
		first disks.Reason
	}{
		{"ntfs3", "ntfs3", ntfs3Options, ""},
		{"ntfs", "ntfs", func(*testing.T) []string { return ntfsOptions }, ""},
		{"ntfs errors=continue", "ntfs", func(*testing.T) []string { return []string{"iocharset=utf8"} }, disks.ErrorsContinue},
		{"ntfs-3g", "ntfs-3g", func(*testing.T) []string { return nil }, ""},
		{"lowntfs-3g", "lowntfs-3g", func(*testing.T) []string { return nil }, ""},
	} {
		for _, content := range []struct {
			name  string
			bytes []byte
			state disks.State
			why   disks.Reason
		}{
			{"10 bytes", hiberfilShort, disks.Readonly, disks.Hibernated},
			{"HIBR", hiberfilMagic, disks.Readonly, disks.Hibernated},
			{"8 KiB of zeros", hiberfilResumed, disks.Online, ""},
			{"none", nil, disks.Online, ""},
		} {
			t.Run(driver.name+" "+content.name, func(t *testing.T) {
				needDriver(t, driver.driver)
				if _, err := exec.LookPath("ntfs-3g"); err != nil {
					t.Skip("no ntfs-3g to make the volume")
				}
				v := hibernatedVolume(t, content.bytes)
				at := mountPoint(t)
				mustDo(t, os.MkdirAll(at, 0o755))
				if err := remount(t, v, driver.driver, at, driver.options(t)); err != nil {
					t.Logf("%s refused to mount it: %v", driver.driver, err)
					if content.state == disks.Online {
						t.Error("a volume Windows did not leave hibernated was refused")
					}
					return
				}
				letGo(t, at)
				m, err := disks.NewMachine()
				mustDo(t, err)
				t.Logf("mounted as %s", mountedAs(t, m, at))
				state, why := content.state, content.why
				if driver.first != "" {
					state, why = disks.Readonly, driver.first
				}
				hiberfil := filepath.Join(at, disks.HiberfilName)
				var before unix.Stat_t
				if content.bytes != nil {
					mustDo(t, unix.Lstat(hiberfil, &before))
				}
				want(t, disks.Check(m, diskOf(v, at)), state, why)
				if content.bytes == nil {
					return
				}
				var after unix.Stat_t
				mustDo(t, unix.Lstat(hiberfil, &after))
				if after.Atim != before.Atim {
					// FUSE passes no O_NOATIME to ntfs-3g, which keeps the access time itself.
					if strings.HasSuffix(driver.driver, "3g") {
						t.Logf("the access time moved from %v to %v", before.Atim, after.Atim)
					} else {
						t.Errorf("the access time moved from %v to %v", before.Atim, after.Atim)
					}
				}
				data, err := os.ReadFile(hiberfil)
				mustDo(t, err)
				if !bytes.Equal(data, content.bytes) {
					t.Error("hiberfil.sys changed")
				}
			})
		}
	}
}

// TestAHibernatedVolumeIsReadOncePerMount: the daemon's machine reads hiberfil.sys once for each mount, so that its
// check every 30 s reads nothing of it again, and once more when the disk is mounted anew, however the kernel numbers
// the new mount; a machine of its own reads it at once (specs.md §10).
func TestAHibernatedVolumeIsReadOncePerMount(t *testing.T) {
	needRoot(t)
	driver, options := ntfs3Type(t), ntfs3Options(t)
	v := newVolume(t, names.NTFS)
	at := mountPoint(t)
	mountVolume(t, v, driver, at, options...)
	mustDo(t, os.Mkdir(filepath.Join(at, "media"), 0o755))
	hiberfil := filepath.Join(at, disks.HiberfilName)
	mustDo(t, os.WriteFile(hiberfil, hiberfilShort, 0o644))
	must(t, "umount", at)
	mustDo(t, remount(t, v, driver, at, options))
	d := diskOf(v, at)
	daemon, err := disks.NewMachine()
	mustDo(t, err)
	want(t, disks.Check(daemon, d), disks.Readonly, disks.Hibernated)

	// Windows shut down fully, as far as this mount shows: the same mount, so the same answer to the same machine.
	mustDo(t, os.WriteFile(hiberfil, hiberfilResumed, 0o644))
	must(t, "sync")
	want(t, disks.Check(daemon, d), disks.Readonly, disks.Hibernated)
	other, err := disks.NewMachine()
	mustDo(t, err)
	want(t, disks.Check(other, d), disks.Online, "")

	// Nothing of it is read again: its pages dropped, the next check of the daemon's machine reads nothing from the
	// device, where a new machine does.
	dropPages(t, hiberfil)
	reads := readsOf(t, v.device)
	want(t, disks.Check(daemon, d), disks.Readonly, disks.Hibernated)
	if got := readsOf(t, v.device); got != reads {
		t.Errorf("a check of the same mount read the device: %d reads, then %d", reads, got)
	}
	third, err := disks.NewMachine()
	mustDo(t, err)
	want(t, disks.Check(third, d), disks.Online, "")
	if got := readsOf(t, v.device); got == reads {
		t.Log("a new machine read hiberfil.sys from the page cache: the measure above may be blind")
	}

	// Mounted anew, with no check in between: the unique mount id tells, where the id of the table may be the same.
	before := daemon.Mount(at).Entry.ID
	must(t, "umount", at)
	mustDo(t, remount(t, v, driver, at, options))
	t.Logf("the id of the mount table: %d, then %d", before, daemon.Mount(at).Entry.ID)
	want(t, disks.Check(daemon, d), disks.Online, "")

	// And once a check found nothing mounted there, on a kernel that numbers mounts as it does.
	mustDo(t, os.WriteFile(hiberfil, hiberfilMagic, 0o644))
	must(t, "umount", at)
	want(t, disks.Check(daemon, d), disks.Missing, disks.NotMounted)
	mustDo(t, remount(t, v, driver, at, options))
	want(t, disks.Check(daemon, d), disks.Readonly, disks.Hibernated)
}

// TestWhatClearsAHibernation: ntfsfix and ntfs-3g's remove_hiberfile, run on a volume Windows left hibernated, each
// logged for what it does with hiberfil.sys; the check, on the mount after it, says what the file then says (specs.md
// §22).
func TestWhatClearsAHibernation(t *testing.T) {
	needRoot(t)
	if _, err := exec.LookPath("ntfs-3g"); err != nil {
		t.Skip("no ntfs-3g")
	}
	t.Run("ntfsfix", func(t *testing.T) {
		if _, err := exec.LookPath("ntfsfix"); err != nil {
			t.Skip("no ntfsfix")
		}
		v := hibernatedVolume(t, hiberfilMagic)
		out, err := exec.CommandContext(t.Context(), "ntfsfix", v.device).CombinedOutput()
		t.Logf("ntfsfix: %v\n%s", err, out)
		at := mountPoint(t)
		mustDo(t, os.MkdirAll(at, 0o755))
		mustDo(t, remount(t, v, "ntfs-3g", at, []string{"ro"}))
		letGo(t, at)
		data, err := os.ReadFile(filepath.Join(at, disks.HiberfilName))
		switch {
		case os.IsNotExist(err):
			t.Log("ntfsfix removed hiberfil.sys")
			want(t, check(t, diskOf(v, at)), disks.Readonly, disks.ReadOnlyMount)
		case err != nil:
			t.Fatal(err)
		case bytes.Equal(data, hiberfilMagic):
			t.Log("ntfsfix left hiberfil.sys as it was")
			want(t, check(t, diskOf(v, at)), disks.Readonly, disks.Hibernated)
		default:
			t.Logf("ntfsfix changed hiberfil.sys: %d bytes, starting %q", len(data), data[:min(len(data), 4)])
		}
	})
	t.Run("remove_hiberfile", func(t *testing.T) {
		v := hibernatedVolume(t, hiberfilMagic)
		at := mountPoint(t)
		mustDo(t, os.MkdirAll(at, 0o755))
		if err := remount(t, v, "ntfs-3g", at, []string{"remove_hiberfile"}); err != nil {
			t.Fatalf("ntfs-3g with remove_hiberfile: %v", err)
		}
		letGo(t, at)
		m, err := disks.NewMachine()
		mustDo(t, err)
		t.Logf("mounted as %s", mountedAs(t, m, at))
		if _, err := os.Lstat(filepath.Join(at, disks.HiberfilName)); !os.IsNotExist(err) {
			t.Errorf("hiberfil.sys is still there: %v", err)
		}
		want(t, disks.Check(m, diskOf(v, at)), disks.Online, "")
	})
}

// dropPages makes the kernel forget what it holds of a file's content, so that the next read of it reaches the device.
func dropPages(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	mustDo(t, err)
	defer func() { _ = f.Close() }()
	mustDo(t, unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED))
}

// readsOf gives the reads a device completed, the first field of its stat in /sys.
func readsOf(t *testing.T, device string) int64 {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("/sys/block", filepath.Base(device), "stat"))
	mustDo(t, err)
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		t.Fatalf("%s: %q", device, data)
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	mustDo(t, err)
	return n
}

// letGo unmounts what remount mounted, at the end of a test, as mount does for what it mounts.
func letGo(t *testing.T, at string) {
	t.Helper()
	t.Cleanup(func() {
		_ = exec.CommandContext(context.Background(), "umount", "-l", at).Run()
		time.Sleep(200 * time.Millisecond)
	})
}
