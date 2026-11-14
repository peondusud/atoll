//go:build loopdev

package loopdev

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/names"
)

// volume is a filesystem on a loop device, or on another device for what needs one.
type volume struct {
	fs     names.Filesystem
	device string
	uuid   string // as blkid -p reads it, which the code under test does not use
}

// A filesystem of each kind, as mkfs makes it.
var makers = []struct {
	name string
	fs   names.Filesystem
	mb   int64
	mkfs []string
}{
	{"ext4", names.Ext4, 64, []string{"mkfs.ext4", "-q", "-F"}},
	{"xfs", names.XFS, 512, []string{"mkfs.xfs", "-q", "-f"}},
	{"btrfs", names.Btrfs, 512, []string{"mkfs.btrfs", "-q", "-f"}},
	{"ntfs", names.NTFS, 64, []string{"mkntfs", "-F", "-Q", "-q"}},
}

func must(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func attempt(t *testing.T, name string, args ...string) error {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, out)
	}
	return nil
}

// newVolume makes a filesystem on a new loop device, and takes it away at the end.
func newVolume(t *testing.T, fs names.Filesystem, extra ...string) *volume {
	t.Helper()
	return newVolumeSized(t, fs, 0, extra...)
}

// newVolumeSized is newVolume of mb MiB, or of its maker's size with 0.
func newVolumeSized(t *testing.T, fs names.Filesystem, mb int64, extra ...string) *volume {
	t.Helper()
	for _, m := range makers {
		if m.fs != fs {
			continue
		}
		if mb == 0 {
			mb = m.mb
		}
		if _, err := exec.LookPath(m.mkfs[0]); err != nil {
			t.Skipf("no %s", m.mkfs[0])
		}
		img := filepath.Join(t.TempDir(), "disk.img")
		if err := os.WriteFile(img, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(img, mb<<20); err != nil {
			t.Fatal(err)
		}
		args := append(append(append([]string{}, m.mkfs[1:]...), extra...), img)
		must(t, m.mkfs[0], args...)
		device := must(t, "losetup", "-f", "--show", img)
		t.Cleanup(func() { _ = exec.CommandContext(context.Background(), "losetup", "-d", device).Run() })
		return &volume{fs: fs, device: device, uuid: identifierOf(t, device)}
	}
	t.Fatalf("no maker for %s", fs)
	return nil
}

// identifierOf asks blkid, bypassing its cache: the check on the code under test.
func identifierOf(t *testing.T, device string) string {
	t.Helper()
	return must(t, "blkid", "-p", "-o", "value", "-s", "UUID", device)
}

func mountPoint(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "mnt")
}

// mount mounts and takes it away at the end. A FUSE driver needs a moment to let go.
func mount(t *testing.T, at string, command ...string) {
	t.Helper()
	if err := os.MkdirAll(at, 0o755); err != nil {
		t.Fatal(err)
	}
	must(t, command[0], append(append([]string{}, command[1:]...), at)...)
	t.Cleanup(func() {
		_ = exec.CommandContext(context.Background(), "umount", "-l", at).Run()
		time.Sleep(200 * time.Millisecond)
	})
}

// mountVolume mounts a volume, with what the filesystem's driver needs, and makes media/ on it.
func mountVolume(t *testing.T, v *volume, driver string, at string, options ...string) {
	t.Helper()
	opts := strings.Join(options, ",")
	var command []string
	switch driver {
	case "ntfs-3g", "lowntfs-3g":
		if _, err := exec.LookPath(driver); err != nil {
			t.Skipf("no %s", driver)
		}
		command = []string{driver}
		if opts != "" {
			command = append(command, "-o", opts)
		}
		command = append(command, v.device)
	default:
		command = []string{"mount"}
		if driver != "" {
			command = append(command, "-t", driver)
		}
		if opts != "" {
			command = append(command, "-o", opts)
		}
		command = append(command, v.device)
	}
	mount(t, at, command...)
}

func diskOf(v *volume, at string) *config.Disk {
	return &config.Disk{ID: "t1", UUID: v.uuid, Mount: at, FS: v.fs, Root: "media", Role: "data"}
}

func check(t *testing.T, d *config.Disk) disks.Verdict {
	t.Helper()
	observer, err := disks.NewMachine()
	if err != nil {
		t.Fatal(err)
	}
	return disks.Check(observer, d)
}

func want(t *testing.T, got disks.Verdict, state disks.State, reason disks.Reason) {
	t.Helper()
	if got.State != state || got.Reason != reason {
		t.Errorf("got %s %q (%s), want %s %q", got.State, got.Reason, got.Message, state, reason)
	}
}

func needRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root: sudo make test-loopdev")
	}
}

// TestDiskIdentity: each driver, with a real filesystem. What Atoll reads must be what blkid -p
// reads, and the disk must be online.
func TestDiskIdentity(t *testing.T) {
	needRoot(t)
	for _, tc := range []struct {
		name   string
		fs     names.Filesystem
		driver string
		opts   []string
	}{
		{"ext4", names.Ext4, "", nil},
		{"xfs", names.XFS, "", nil},
		{"btrfs", names.Btrfs, "", nil},
		{"ntfs-3g", names.NTFS, "ntfs-3g", nil},
		{"lowntfs-3g", names.NTFS, "lowntfs-3g", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, tc.opts...)
			if err := os.Mkdir(filepath.Join(at, "media"), 0o755); err != nil {
				t.Fatal(err)
			}
			d := diskOf(v, at)
			want(t, check(t, d), disks.Online, "")
			// Nothing was written to it: the top holds only what this test put there.
			d.UUID = "7a2b4c6d-8e0f-4a1b-9c2d-3e4f5a6b7c8d"
			want(t, check(t, d), disks.Missing, disks.WrongIdentifier)
		})
	}
	t.Run("ntfs3", func(t *testing.T) {
		v := newVolume(t, names.NTFS)
		at := mountPoint(t)
		mountVolume(t, v, ntfs3Type(t), at, ntfs3Options(t)...)
		if err := os.Mkdir(filepath.Join(at, "media"), 0o755); err != nil {
			t.Fatal(err)
		}
		want(t, check(t, diskOf(v, at)), disks.Online, "")
	})
}

// ntfs3Type skips where the kernel has no ntfs3.
func ntfs3Type(t *testing.T) string {
	t.Helper()
	_ = exec.CommandContext(context.Background(), "modprobe", "ntfs3").Run()
	data, err := os.ReadFile("/proc/filesystems")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "ntfs3") {
		t.Skip("no ntfs3 in this kernel")
	}
	return "ntfs3"
}

// ntfs3Options gives what §22 recommends: ads=0, where the kernel knows it. Linux 6.12 refuses it.
func ntfs3Options(t *testing.T) []string {
	t.Helper()
	v := newVolume(t, names.NTFS)
	at := mountPoint(t)
	if err := os.MkdirAll(at, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := attempt(t, "mount", "-t", "ntfs3", "-o", "ads=0", v.device, at); err != nil {
		return nil
	}
	_ = exec.CommandContext(context.Background(), "umount", at).Run()
	return []string{"ads=0"}
}

// TestMountShapes: what a mount path can look like (docs/research/disk-identity.md).
func TestMountShapes(t *testing.T) {
	needRoot(t)
	v := newVolume(t, names.Ext4)
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	if err := os.MkdirAll(filepath.Join(at, "media", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := diskOf(v, at)
	want(t, check(t, d), disks.Online, "")

	t.Run("nothing mounted", func(t *testing.T) {
		empty := mountPoint(t)
		if err := os.MkdirAll(filepath.Join(empty, "media"), 0o755); err != nil {
			t.Fatal(err)
		}
		other := *d
		other.Mount = empty
		want(t, check(t, &other), disks.Missing, disks.NotMounted)
		entries, err := os.ReadDir(empty)
		if err != nil || len(entries) != 1 {
			t.Errorf("written to an empty mount point: %v %v", entries, err)
		}
	})
	t.Run("no such path", func(t *testing.T) {
		other := *d
		other.Mount = mountPoint(t)
		want(t, check(t, &other), disks.Missing, disks.NotMounted)
	})
	t.Run("whole bind", func(t *testing.T) {
		bind := mountPoint(t)
		mount(t, bind, "mount", "--bind", at)
		other := *d
		other.Mount = bind
		want(t, check(t, &other), disks.Online, "")
	})
	t.Run("bind of a folder", func(t *testing.T) {
		bind := mountPoint(t)
		mount(t, bind, "mount", "--bind", filepath.Join(at, "media"))
		other := *d
		other.Mount = bind
		want(t, check(t, &other), disks.Missing, disks.PartialMount)
	})
	t.Run("read-only bind", func(t *testing.T) {
		bind := mountPoint(t)
		mount(t, bind, "mount", "--bind", at)
		must(t, "mount", "-o", "remount,bind,ro", bind)
		other := *d
		other.Mount = bind
		want(t, check(t, &other), disks.Readonly, disks.ReadOnlyMount)
	})
	t.Run("a symlink to the mount", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(at, link); err != nil {
			t.Fatal(err)
		}
		other := *d
		other.Mount = link
		want(t, check(t, &other), disks.Missing, disks.NotMounted)
	})
	t.Run("a path through a symlinked folder", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(filepath.Dir(at), link); err != nil {
			t.Fatal(err)
		}
		other := *d
		other.Mount = filepath.Join(link, filepath.Base(at))
		want(t, check(t, &other), disks.Missing, disks.NotMounted)
	})
	t.Run("the root mounted over", func(t *testing.T) {
		mount(t, filepath.Join(at, "media", "sub"), "mount", "-t", "tmpfs", "tmpfs")
		other := *d
		other.Root = "media/sub"
		want(t, check(t, &other), disks.Missing, disks.NoRoot)
	})
	t.Run("no root", func(t *testing.T) {
		other := *d
		other.Root = "absent"
		want(t, check(t, &other), disks.Missing, disks.NoRoot)
		if _, err := os.Stat(filepath.Join(at, "absent")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the check created the root: %v", err)
		}
	})
	t.Run("a tmpfs over the disk", func(t *testing.T) {
		over := mountPoint(t)
		fresh := newVolume(t, names.Ext4)
		mountVolume(t, fresh, "", over)
		mount(t, over, "mount", "-t", "tmpfs", "tmpfs")
		want(t, check(t, diskOf(fresh, over)), disks.Missing, disks.WrongFilesystem)
	})
	t.Run("the same disk mounted again over its old mount", func(t *testing.T) {
		over := mountPoint(t)
		fresh := newVolume(t, names.Ext4)
		mountVolume(t, fresh, "", over)
		if err := os.Mkdir(filepath.Join(over, "media"), 0o755); err != nil {
			t.Fatal(err)
		}
		mountVolume(t, fresh, "", over)
		want(t, check(t, diskOf(fresh, over)), disks.Online, "")
	})
	t.Run("a read-only mount", func(t *testing.T) {
		ro := mountPoint(t)
		fresh := newVolume(t, names.Ext4)
		mountVolume(t, fresh, "", ro)
		if err := os.Mkdir(filepath.Join(ro, "media"), 0o755); err != nil {
			t.Fatal(err)
		}
		must(t, "mount", "-o", "remount,ro", ro)
		want(t, check(t, diskOf(fresh, ro)), disks.Readonly, disks.ReadOnlyMount)
	})
	t.Run("a name with a space and a tab", func(t *testing.T) {
		odd := filepath.Join(t.TempDir(), "x y\tz")
		fresh := newVolume(t, names.Ext4)
		mountVolume(t, fresh, "", odd)
		if err := os.Mkdir(filepath.Join(odd, "media"), 0o755); err != nil {
			t.Fatal(err)
		}
		want(t, check(t, diskOf(fresh, odd)), disks.Online, "")
	})
}

// TestAnExt4WithoutAJournal: mkfs.ext4 -O ^has_journal makes a disk readonly, which disk add refuses, as a name made
// there is not durable once its folder is synced (specs.md §10, §17.1). With its journal, the same disk is online.
func TestAnExt4WithoutAJournal(t *testing.T) {
	needRoot(t)
	for _, tc := range []struct {
		name  string
		mkfs  []string
		state disks.State
		why   disks.Reason
	}{
		{"without", []string{"-O", "^has_journal"}, disks.Readonly, disks.NoJournal},
		{"with", nil, disks.Online, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newVolume(t, names.Ext4, tc.mkfs...)
			at := mountPoint(t)
			mountVolume(t, v, "", at)
			if err := os.Mkdir(filepath.Join(at, "media"), 0o755); err != nil {
				t.Fatal(err)
			}
			want(t, check(t, diskOf(v, at)), tc.state, tc.why)
		})
	}
}

// TestNameVariants: what changes name rules makes a disk readonly (specs.md §10).
func TestNameVariants(t *testing.T) {
	needRoot(t)
	withMedia := func(t *testing.T, at string) {
		t.Helper()
		if err := os.Mkdir(filepath.Join(at, "media"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("ext4 casefold", func(t *testing.T) {
		v := newVolume(t, names.Ext4, "-O", "casefold")
		at := mountPoint(t)
		mountVolume(t, v, "", at)
		withMedia(t, at)
		want(t, check(t, diskOf(v, at)), disks.Readonly, disks.Casefold)
	})
	t.Run("xfs ascii-ci", func(t *testing.T) {
		v := newVolume(t, names.XFS, "-m", "crc=0", "-n", "version=ci")
		at := mountPoint(t)
		if err := attempt(t, "mount", "--version"); err != nil {
			t.Skip(err)
		}
		if err := os.MkdirAll(at, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := attempt(t, "mount", v.device, at); err != nil {
			t.Skipf("this kernel does not mount it: %v", err)
		}
		t.Cleanup(func() { _ = exec.CommandContext(context.Background(), "umount", "-l", at).Run() })
		withMedia(t, at)
		want(t, check(t, diskOf(v, at)), disks.Readonly, disks.ASCIICI)
	})
	t.Run("lowntfs-3g ignore_case", func(t *testing.T) {
		v := newVolume(t, names.NTFS)
		at := mountPoint(t)
		mountVolume(t, v, "lowntfs-3g", at, "ignore_case")
		withMedia(t, at)
		want(t, check(t, diskOf(v, at)), disks.Readonly, disks.IgnoreCase)
	})
	t.Run("ntfs3 nocase", func(t *testing.T) {
		kind := ntfs3Type(t)
		v := newVolume(t, names.NTFS)
		at := mountPoint(t)
		mountVolume(t, v, kind, at, append(ntfs3Options(t), "nocase")...)
		withMedia(t, at)
		want(t, check(t, diskOf(v, at)), disks.Readonly, disks.NoCase)
	})
	t.Run("ntfs3 iocharset", func(t *testing.T) {
		kind := ntfs3Type(t)
		v := newVolume(t, names.NTFS)
		at := mountPoint(t)
		if err := os.MkdirAll(at, 0o755); err != nil {
			t.Fatal(err)
		}
		args := []string{"-t", kind, "-o", strings.Join(append(ntfs3Options(t), "iocharset=iso8859-1"), ","), v.device, at}
		if err := attempt(t, "mount", args...); err != nil {
			t.Skipf("this kernel has no such character set: %v", err)
		}
		t.Cleanup(func() { _ = exec.CommandContext(context.Background(), "umount", "-l", at).Run() })
		withMedia(t, at)
		want(t, check(t, diskOf(v, at)), disks.Readonly, disks.IOCharset)
	})
	t.Run("ntfs3 ads", func(t *testing.T) {
		kind := ntfs3Type(t)
		if ntfs3Options(t) == nil {
			t.Skip("this kernel has no ads")
		}
		v := newVolume(t, names.NTFS)
		at := mountPoint(t)
		mountVolume(t, v, kind, at)
		withMedia(t, at)
		want(t, check(t, diskOf(v, at)), disks.Readonly, disks.ADS)
	})
	t.Run("the ntfs driver", func(t *testing.T) {
		_ = exec.CommandContext(context.Background(), "modprobe", "ntfs").Run()
		data, err := os.ReadFile("/proc/filesystems")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "\tntfs\n") {
			t.Skip("no driver of the type ntfs in this kernel")
		}
		v := newVolume(t, names.NTFS)
		at := mountPoint(t)
		if err := os.MkdirAll(at, 0o755); err != nil {
			t.Fatal(err)
		}
		must(t, "mount", "-i", "-t", "ntfs", v.device, at)
		t.Cleanup(func() { _ = exec.CommandContext(context.Background(), "umount", "-l", at).Run() })
		withMedia(t, at)
		want(t, check(t, diskOf(v, at)), disks.Readonly, disks.UnsupportedDriver)
	})
}

// TestDeviceFailures: the mount stays in the table, and a check must not read the disk as healthy
// once what it asks is no longer cached. An error is never an absent file (invariant 4).
func TestDeviceFailures(t *testing.T) {
	needRoot(t)
	if _, err := exec.LookPath("dmsetup"); err != nil {
		t.Skip("no dmsetup")
	}
	for _, tc := range []struct {
		name   string
		fs     names.Filesystem
		driver string
	}{
		{"ext4", names.Ext4, ""},
		{"xfs", names.XFS, ""},
		{"ntfs3", names.NTFS, "ntfs3"},
		{"ntfs-3g", names.NTFS, "ntfs-3g"},
	} {
		t.Run("dm-error/"+tc.name, func(t *testing.T) {
			if tc.driver == "ntfs3" {
				ntfs3Type(t)
			}
			v := newVolume(t, tc.fs)
			sectors := must(t, "blockdev", "--getsz", v.device)
			name := "atoll-" + strings.ReplaceAll(t.Name(), "/", "-")
			must(t, "dmsetup", "create", "--noudevsync", "--noudevrules", name, "--table", "0 "+sectors+" linear "+v.device+" 0")
			must(t, "dmsetup", "mknodes")
			t.Cleanup(func() {
				_ = exec.CommandContext(context.Background(), "dmsetup", "remove", "-f", "--noudevsync", name).Run()
			})
			mapped := &volume{fs: v.fs, device: "/dev/mapper/" + name, uuid: v.uuid}
			at := mountPoint(t)
			mountVolume(t, mapped, tc.driver, at, optionsFor(t, tc.driver)...)
			if err := os.Mkdir(filepath.Join(at, "media"), 0o755); err != nil {
				t.Fatal(err)
			}
			must(t, "sync")
			d := diskOf(mapped, at)
			want(t, check(t, d), disks.Online, "")

			must(t, "dmsetup", "suspend", "--noudevsync", name)
			must(t, "dmsetup", "reload", name, "--table", "0 "+sectors+" error")
			must(t, "dmsetup", "resume", "--noudevsync", name)
			_ = exec.CommandContext(context.Background(), "sync").Run()
			if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0o644); err != nil {
				t.Fatal(err)
			}
			got := check(t, d)
			if got.State != disks.Missing {
				t.Errorf("got %s %q (%s), want missing", got.State, got.Reason, got.Message)
			}
			if got.Reason == disks.NoRoot || got.Reason == disks.NotMounted {
				t.Errorf("an I/O error was read as %q: %s", got.Reason, got.Message)
			}
		})
	}
}

func optionsFor(t *testing.T, driver string) []string {
	t.Helper()
	if driver == "ntfs3" {
		return ntfs3Options(t)
	}
	return nil
}

// TestSourceNameChanged: the name in the mount table may lead elsewhere by now, even to a device
// that holds the same filesystem. It must not vouch for this mount.
func TestSourceNameChanged(t *testing.T) {
	needRoot(t)
	v := newVolume(t, names.Ext4)
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	if err := os.Mkdir(filepath.Join(at, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := diskOf(v, at)
	want(t, check(t, d), disks.Online, "")
	must(t, "sync")
	img := filepath.Join(t.TempDir(), "clone.img")
	must(t, "dd", "if="+v.device, "of="+img, "bs=1M", "status=none")
	clone := must(t, "losetup", "-f", "--show", img)
	t.Cleanup(func() { _ = exec.CommandContext(context.Background(), "losetup", "-d", clone).Run() })
	if got := identifierOf(t, clone); got != v.uuid {
		t.Fatalf("the clone holds %s, want %s", got, v.uuid)
	}
	saved := v.device + ".saved"
	if err := os.Rename(v.device, saved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(v.device); _ = os.Rename(saved, v.device) })

	if err := os.Symlink(clone, v.device); err != nil {
		t.Fatal(err)
	}
	want(t, check(t, d), disks.Missing, disks.DeviceGone)
	if err := os.Remove(v.device); err != nil {
		t.Fatal(err)
	}
	head, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(v.device, head, 0o600); err != nil {
		t.Fatal(err)
	}
	want(t, check(t, d), disks.Missing, disks.DeviceGone)
	if err := os.Remove(v.device); err != nil {
		t.Fatal(err)
	}
	want(t, check(t, d), disks.Missing, disks.DeviceGone)
	if err := os.Rename(saved, v.device); err != nil {
		t.Fatal(err)
	}
	want(t, check(t, d), disks.Online, "")
}

// TestSourceNameChangedBtrfs: a btrfs entry holds an anonymous number, so nothing can be compared,
// and only the kind of file stands between a stale name and the bytes it leads to.
func TestSourceNameChangedBtrfs(t *testing.T) {
	needRoot(t)
	v := newVolume(t, names.Btrfs)
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	if err := os.Mkdir(filepath.Join(at, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := diskOf(v, at)
	want(t, check(t, d), disks.Online, "")
	must(t, "sync")
	copied := filepath.Join(t.TempDir(), "head")
	must(t, "dd", "if="+v.device, "of="+copied, "bs=4096", "count=32", "status=none")
	head, err := os.ReadFile(copied)
	if err != nil {
		t.Fatal(err)
	}
	saved := v.device + ".saved"
	if err := os.Rename(v.device, saved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(v.device); _ = os.Rename(saved, v.device) })
	if err := os.WriteFile(v.device, head, 0o600); err != nil {
		t.Fatal(err)
	}
	want(t, check(t, d), disks.Missing, disks.DeviceGone)
}

// TestNoPrivilege: a user who may not read the device cannot confirm a disk, and that is said as
// it is, not as an absent disk.
func TestNoPrivilege(t *testing.T) {
	needRoot(t)
	v := newVolume(t, names.Ext4)
	// Not under t.TempDir, whose folders that user may not enter.
	at, err := os.MkdirTemp("/", "atoll-no-privilege-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(at, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(at) })
	mountVolume(t, v, "", at)
	if err := os.Mkdir(filepath.Join(at, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := diskOf(v, at)
	want(t, check(t, d), disks.Online, "")
	if err := syscall.Seteuid(65534); err != nil {
		t.Fatal(err)
	}
	got := check(t, d)
	if err := syscall.Seteuid(0); err != nil {
		t.Fatal(err)
	}
	want(t, got, disks.Missing, disks.DeviceUnread)
}

// TestDaemonDies: with the FUSE daemon gone, the path gives ENOTCONN, which says nothing about
// what the disk holds.
func TestDaemonDies(t *testing.T) {
	needRoot(t)
	v := newVolume(t, names.NTFS)
	at := mountPoint(t)
	mountVolume(t, v, "ntfs-3g", at)
	if err := os.Mkdir(filepath.Join(at, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := diskOf(v, at)
	want(t, check(t, d), disks.Online, "")
	must(t, "pkill", "-9", "-f", "ntfs-3g "+v.device+" ")
	time.Sleep(500 * time.Millisecond)
	want(t, check(t, d), disks.Missing, disks.IOError)
}

// TestDeviceRemoved: as a USB disk unplugged, with scsi_debug. The mount stays in the table and
// statx keeps answering: the device that is gone is what notices it.
func TestDeviceRemoved(t *testing.T) {
	needRoot(t)
	if err := attempt(t, "modprobe", "scsi_debug", "dev_size_mb=64"); err != nil {
		t.Skipf("no scsi_debug: %v", err)
	}
	t.Cleanup(func() {
		time.Sleep(time.Second)
		_ = exec.CommandContext(context.Background(), "modprobe", "-r", "scsi_debug").Run()
	})
	time.Sleep(2 * time.Second)
	links, err := filepath.Glob("/sys/bus/pseudo/drivers/scsi_debug/adapter*/host*/target*/*:*/block/*")
	if err != nil || len(links) == 0 {
		t.Fatalf("no scsi_debug disk: %v", err)
	}
	name := filepath.Base(links[0])
	device := "/dev/" + name
	must(t, "mkfs.ext4", "-q", "-F", device)
	v := &volume{fs: names.Ext4, device: device, uuid: identifierOf(t, device)}
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	if err := os.Mkdir(filepath.Join(at, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := diskOf(v, at)
	want(t, check(t, d), disks.Online, "")
	if err := os.WriteFile("/sys/block/"+name+"/device/delete", []byte("1"), 0o200); err != nil {
		t.Fatal(err)
	}
	for range 50 {
		if _, err := os.Stat(device); err != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	want(t, check(t, d), disks.Missing, disks.DeviceGone)
}
