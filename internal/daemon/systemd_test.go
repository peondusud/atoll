package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/vectors"
)

// The name of the mount unit systemd gives a mount point, as systemd-escape --path --suffix=mount writes it and
// systemd-fstab-generator names the unit of an /etc/fstab entry (specs.md §15.13).
func TestMountUnitVectors(t *testing.T) {
	type vector struct {
		Path  string `json:"path"`
		Unit  string `json:"unit"`
		Error bool   `json:"error"`
		Note  string `json:"note"`
	}
	for i, v := range vectors.Load[vector](t, "mount_unit.json") {
		got, err := mountUnit(v.Path)
		switch {
		case v.Error && err == nil:
			t.Errorf("case %d, %q: %q, want an error", i, v.Path, got)
		case !v.Error && err != nil:
			t.Errorf("case %d, %q: %v, want %q", i, v.Path, err, v.Unit)
		case !v.Error && got != v.Unit:
			t.Errorf("case %d, %q: %q, want %q", i, v.Path, got, v.Unit)
		}
	}
}

// What systemctl show writes, a property a line, read whole; one it left out is empty.
func TestParseShown(t *testing.T) {
	got := parseShown([]byte("Job=\nWhat=/dev/disk/by-uuid/4A3F=1\nLoadState=loaded\nActiveState=failed\nResult=exit-code\n" +
		"FragmentPath=/run/systemd/generator/mnt-disk1.mount\nnoise\n"))
	want := unitShown{load: "loaded", active: "failed", result: "exit-code", fragment: "/run/systemd/generator/mnt-disk1.mount",
		what: "/dev/disk/by-uuid/4A3F=1"}
	if got != want {
		t.Errorf("%+v, want %+v", got, want)
	}
	if got := parseShown([]byte("Job=42\n")); got.job != "42" || got.load != "" {
		t.Errorf("%+v", got)
	}
}

// systemdBench is a disk to remount at a mount point with a space, its fstab entry, and a systemd of the test's own:
// a systemctl that says what its files hold of the disk's mount unit, and starts it as its files say, and umount and
// mount, which tell systemctl's files that the mount is gone. Each writes what it was asked to calls.
type systemdBench struct {
	t    *testing.T
	dir  string
	log  *syncBuffer
	l    *loop
	disk *config.Disk
}

const benchUnit = `mnt-My\x20Disk.mount`

func newSystemdBench(t *testing.T) *systemdBench {
	t.Helper()
	dir := t.TempDir()
	b := &systemdBench{t: t, dir: dir, log: &syncBuffer{}, disk: &config.Disk{ID: "u1", Mount: "/mnt/My Disk"}}
	scripts := map[string]string{
		"systemctl": `#!/bin/sh
d=$(dirname "$0")
echo "systemctl $*" >>"$d/calls"
verb=
for a in "$@"; do case $a in show|start) verb=$a ;; esac; done
case $verb in
show)
	echo "Job=$(cat "$d/job" 2>/dev/null)"
	echo "LoadState=$(cat "$d/load")"
	echo "ActiveState=$(cat "$d/active")"
	echo "Result=$(cat "$d/result" 2>/dev/null)"
	echo "FragmentPath=$(cat "$d/fragment")"
	echo "What=$(cat "$d/what")"
	;;
start)
	if [ -e "$d/refuse" ]; then
		echo "Failed to start: Interactive authentication required." >&2
		exit 1
	fi
	cp "$d/outcome" "$d/active"
	;;
esac
`,
		"umount": "#!/bin/sh\nd=$(dirname \"$0\")\necho \"umount $*\" >>\"$d/calls\"\necho inactive >\"$d/active\"\n",
		"mount":  "#!/bin/sh\nd=$(dirname \"$0\")\necho \"mount $*\" >>\"$d/calls\"\n",
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	fstab := filepath.Join(dir, "fstab")
	device := filepath.Join(dir, "device")
	booted := filepath.Join(dir, "booted")
	if err := os.Mkdir(booted, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"fstab":  "# the pool\nUUID=4A3F /mnt/My\\040Disk ntfs-3g defaults,nofail 0 0\n",
		"device": "", "load": "loaded", "active": "active", "outcome": "active", "what": device,
		"fragment": "/run/systemd/generator/" + benchUnit,
	} {
		b.set(name, text)
	}
	b.l = &loop{
		opt: &Options{Apply: true}, log: slog.New(slog.NewTextHandler(b.log, nil)),
		set: &settings{remount: true, fstab: fstab, remountWait: 10 * time.Second,
			systemd: systemdWay{fstab: fstab, booted: booted, systemctl: filepath.Join(dir, "systemctl")}},
	}
	return b
}

func (b *systemdBench) set(name, text string) {
	b.t.Helper()
	if err := os.WriteFile(filepath.Join(b.dir, name), []byte(text), 0o644); err != nil {
		b.t.Fatal(err)
	}
}

// calls gives what each command was asked, in order, and forgets it.
func (b *systemdBench) calls() []string {
	b.t.Helper()
	data, err := os.ReadFile(filepath.Join(b.dir, "calls"))
	if err != nil && !os.IsNotExist(err) {
		b.t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(b.dir, "calls"))
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func (b *systemdBench) remount() []string {
	b.t.Helper()
	b.l.remount(b.t.Context(), b.disk)
	return b.calls()
}

func asked(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// Where systemd is PID 1 and has a unit of the disk's fstab entry, the remount detaches the dead mount, then has systemd
// start that unit, and waits for it: what the mount starts, as ntfs-3g's FUSE process, is then the unit's and not the
// daemon's service's, whose stop would end it (specs.md §15.13). It never runs mount itself.
func TestTheRemountStartsTheMountUnit(t *testing.T) {
	b := newSystemdBench(t)
	calls := b.remount()
	umount := slices.Index(calls, "umount -l /mnt/My Disk")
	start := slices.Index(calls, "systemctl --no-block --no-ask-password start -- "+benchUnit)
	if umount < 0 || start < umount {
		t.Fatalf("umount -l, then the unit started: %q", calls)
	}
	if n := asked(calls, "mount "); n != 0 {
		t.Errorf("mount run %d times: %q", n, calls)
	}
	if n := asked(calls[start+1:], "systemctl show"); n == 0 {
		t.Errorf("the unit's state never read once started: %q", calls)
	}
	if said := b.log.String(); !strings.Contains(said, `msg="remounted after a reset"`) || !strings.Contains(said, "unit="+benchUnit) {
		t.Errorf("the log:\n%s", said)
	}
}

// A start refused, as without Atoll's polkit rule, leaves the remount to mount, as before; the log says why once, not
// at each check.
func TestTheRemountMountsItselfWhenSystemdRefuses(t *testing.T) {
	b := newSystemdBench(t)
	b.set("refuse", "")
	for range 2 {
		calls := b.remount()
		if n := asked(calls, "systemctl --no-block --no-ask-password start -- "+benchUnit); n != 1 {
			t.Errorf("the unit asked %d times: %q", n, calls)
		}
		if n := asked(calls, "mount -T "+b.l.set.fstab+" /mnt/My Disk"); n != 1 {
			t.Errorf("mount run %d times: %q", n, calls)
		}
	}
	said := b.log.String()
	if n := strings.Count(said, "systemd refused to start the mount unit"); n != 1 {
		t.Errorf("why said %d times:\n%s", n, said)
	}
}

// Where systemd is not PID 1, as in a container, where systemd has no unit of that mount point, where its unit's name
// is past what Atoll computes, and where the table read is not the one systemd reads, the remount runs mount, as
// before, and says why once.
func TestTheRemountMountsItselfWithoutAUnit(t *testing.T) {
	for _, tc := range []struct {
		name, why string
		change    func(b *systemdBench)
		systemctl bool
	}{
		{"no systemd", "systemd is not the service manager", func(b *systemdBench) { b.l.set.systemd.booted = filepath.Join(b.dir, "none") }, false},
		{"no unit", "systemd has no unit for the mount point", func(b *systemdBench) { b.set("load", "not-found"); b.set("fragment", "") }, true},
		{"a unit from the mount table alone", "systemd has no unit for the mount point", func(b *systemdBench) { b.set("fragment", "") }, true},
		{"no systemctl", "systemctl cannot say", func(b *systemdBench) { b.l.set.systemd.systemctl = filepath.Join(b.dir, "none") }, false},
		{"a name systemd hashes", "no mount unit name", func(b *systemdBench) {
			b.disk.Mount = "/mnt/" + strings.Repeat("a", 246)
			b.set("fstab", "UUID=4A3F "+b.disk.Mount+" ext4 defaults 0 0\n")
		}, false},
		{"another table", "not the one systemd reads", func(b *systemdBench) { b.l.set.systemd.fstab = "/etc/fstab" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newSystemdBench(t)
			tc.change(b)
			for range 2 {
				calls := b.remount()
				if n := asked(calls, "mount -T "+b.l.set.fstab+" "+b.disk.Mount); n != 1 {
					t.Errorf("mount run %d times: %q", n, calls)
				}
				if n := asked(calls, "systemctl --no-block"); n != 0 {
					t.Errorf("a unit started: %q", calls)
				}
				if n := asked(calls, "systemctl"); (n > 0) != tc.systemctl {
					t.Errorf("systemctl asked %d times: %q", n, calls)
				}
			}
			said := b.log.String()
			if n := strings.Count(said, tc.why); n != 1 {
				t.Errorf("%q said %d times:\n%s", tc.why, n, said)
			}
		})
	}
}

// A unit that systemd started and that failed, as on a device that answers nothing, is not mounted by hand after it:
// the FUSE process would be the service's again. The next check tries again.
func TestAMountUnitThatFailedIsNotMountedByHand(t *testing.T) {
	b := newSystemdBench(t)
	b.set("outcome", "failed")
	b.set("result", "exit-code")
	calls := b.remount()
	if n := asked(calls, "mount "); n != 0 {
		t.Errorf("mount run %d times: %q", n, calls)
	}
	if said := b.log.String(); !strings.Contains(said, `msg="the remount failed"`) || !strings.Contains(said, "exit-code") {
		t.Errorf("the log:\n%s", said)
	}
}

// A device that is not back yet is waited for: the unit is not started, which would hold the daemon until systemd
// gives up on the device, 90 s later, nor mounted by hand.
func TestTheRemountWaitsForItsDevice(t *testing.T) {
	b := newSystemdBench(t)
	if err := os.Remove(filepath.Join(b.dir, "device")); err != nil {
		t.Fatal(err)
	}
	calls := b.remount()
	if n := asked(calls, "systemctl --no-block") + asked(calls, "mount "); n != 0 {
		t.Errorf("started or mounted: %q", calls)
	}
	if said := b.log.String(); !strings.Contains(said, "its device is not there") {
		t.Errorf("the log:\n%s", said)
	}
}

// A unit still active once the dead mount is detached, as systemd reads the mount table on its own time, is waited for:
// a start then would do nothing, and the log would say the disk remounted.
func TestTheRemountWaitsForSystemdToSeeTheUnmount(t *testing.T) {
	b := newSystemdBench(t)
	// umount that systemd hears late.
	script := "#!/bin/sh\nd=$(dirname \"$0\")\necho \"umount $*\" >>\"$d/calls\"\n" +
		"(sleep 0.5; echo inactive >\"$d/active\") >/dev/null 2>&1 &\n"
	if err := os.WriteFile(filepath.Join(b.dir, "umount"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	calls := b.remount()
	start := slices.Index(calls, "systemctl --no-block --no-ask-password start -- "+benchUnit)
	umount := slices.Index(calls, "umount -l /mnt/My Disk")
	if start < 0 || asked(calls[umount:start], "systemctl show") < 2 {
		t.Errorf("not waited for: %q", calls)
	}
}

// With no fstab entry for the mount point, nothing is tried, as before (specs.md §15.13).
func TestNoRemountWithoutAnFstabEntry(t *testing.T) {
	b := newSystemdBench(t)
	b.set("fstab", "UUID=4A3F /mnt/other ext4 defaults 0 0\n")
	if calls := b.remount(); len(calls) != 1 || calls[0] != "" {
		t.Errorf("%q", calls)
	}
}
