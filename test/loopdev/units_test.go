//go:build loopdev

package loopdev

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/vectors"
	"github.com/peondusud/atoll/internal/view"
	"github.com/peondusud/atoll/internal/view/fusefs"
)

// The tests of this file start deploy/'s units as they are, through systemd, as a release's .deb installs them: the
// hardening of each, its calls, its sockets, its namespaces, with its account and its capabilities (docs/threat-model.md,
// G18). They run only in the second run of test/qemu's guest, where systemd is PID 1 (test/qemu/README.md): they install
// the units in /run/systemd/system, and use /etc/atoll, /var/lib/atoll and /run/atoll, as a machine's own Atoll would.

// atollBinary is where deploy/'s units start atoll, and where test/qemu/run.sh puts the one it built.
const atollBinary = "/usr/local/bin/atoll"

// The account the units run as, as the .deb's postinst makes it, and another one, as a media server's.
const (
	unitUID  = 990
	otherUID = 991
)

// needSystemd skips unless this is the guest's second run, where systemd is PID 1 and atoll is at atollBinary; there,
// a missing one is a broken guest.
func needSystemd(t *testing.T) {
	t.Helper()
	needRoot(t)
	if os.Getenv("ATOLL_QEMU_SYSTEMD") == "" {
		t.Skip("only in the second run of test/qemu's guest, under systemd: it installs deploy/'s units as they are")
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		t.Fatalf("systemd is not PID 1 in the guest's second run: %v", err)
	}
	if _, err := os.Stat(atollBinary); err != nil {
		t.Fatalf("no %s in the guest: make qemu-build builds it, and test/qemu/run.sh puts it there: %v", atollBinary, err)
	}
}

// theAccount adds the account atoll, and its group, until the test ends.
func theAccount(t *testing.T) {
	t.Helper()
	for file, line := range map[string]string{
		"/etc/passwd": "atoll:x:" + strconv.Itoa(unitUID) + ":" + strconv.Itoa(unitUID) + "::/nonexistent:/usr/sbin/nologin\n",
		"/etc/group":  "atoll:x:" + strconv.Itoa(unitUID) + ":\n",
	} {
		data, err := os.ReadFile(file)
		mustDo(t, err)
		t.Cleanup(func() { _ = os.WriteFile(file, data, 0o644) })
		mustDo(t, os.WriteFile(file, append(slices.Clone(data), line...), 0o644))
	}
}

// unit is one of deploy/'s units, installed as it is.
type unit struct {
	t    *testing.T
	name string
	// log is the file the service's standard output and error go to.
	log string
}

// installUnit puts a unit of deploy/ in /run/systemd/system, byte for byte, with a drop-in of what the guest needs: no
// default dependencies, as the guest starts nothing of sysinit.target, whose tmpfiles would empty /tmp; its output to
// a file, as the guest has no journal; no restart, so that a failure stays one; and a call out of its filter ending the
// process, where the unit says EPERM, so that a call that was not measured cannot go by unseen.
func installUnit(t *testing.T, name string) *unit {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "deploy", name))
	mustDo(t, err)
	u := &unit{t: t, name: name, log: filepath.Join(t.TempDir(), name+".log")}
	file := filepath.Join("/run/systemd/system", name)
	dropin := file + ".d"
	mustDo(t, os.WriteFile(file, data, 0o644))
	mustDo(t, os.MkdirAll(dropin, 0o755))
	mustDo(t, os.WriteFile(filepath.Join(dropin, "test.conf"), []byte("[Unit]\nDefaultDependencies=no\n[Service]\n"+
		"StandardOutput=append:"+u.log+"\nStandardError=inherit\nRestart=no\nSystemCallErrorNumber=kill\n"), 0o644))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("%s\n%s said:\n%s", u.status(), name, u.said())
			if out, err := exec.CommandContext(context.Background(), "dmesg").CombinedOutput(); err == nil {
				t.Logf("the kernel said:\n%s", out)
			}
		}
		_ = exec.CommandContext(context.Background(), "systemctl", "stop", name).Run()
		_ = os.RemoveAll(dropin)
		_ = os.Remove(file)
		_ = exec.CommandContext(context.Background(), "systemctl", "daemon-reload").Run()
		_ = exec.CommandContext(context.Background(), "systemctl", "reset-failed", name).Run()
	})
	must(t, "systemctl", "daemon-reload")
	return u
}

// start starts the service and waits until it is ready, as a boot would.
func (u *unit) start() {
	u.t.Helper()
	if err := attempt(u.t, "systemctl", "start", u.name); err != nil {
		u.t.Fatalf("%v\n%s\n%s", err, u.status(), u.said())
	}
}

// stop stops the service, and fails unless it ended as it should, with exit 0.
func (u *unit) stop() {
	u.t.Helper()
	must(u.t, "systemctl", "stop", u.name)
	if got := u.show("Result", "ExecMainStatus"); got != "success 0" {
		u.t.Errorf("%s stopped with Result and ExecMainStatus %q, want %q", u.name, got, "success 0")
	}
}

// show gives the values of properties of the service, joined by a space.
func (u *unit) show(properties ...string) string {
	u.t.Helper()
	var values []string
	for _, p := range properties {
		values = append(values, must(u.t, "systemctl", "show", "--value", "-p", p, u.name))
	}
	return strings.Join(values, " ")
}

func (u *unit) status() string {
	out, _ := exec.CommandContext(context.Background(), "systemctl", "status", "--no-pager", "-l", u.name).CombinedOutput()
	return string(out)
}

// said gives what the service wrote.
func (u *unit) said() string {
	data, _ := os.ReadFile(u.log)
	return string(data)
}

// checkRunsAsShipped checks that the service runs as the unit says, in the host's mount namespace: what it mounts is
// everyone's (specs.md §15.13, §21.2). Its settings give it no device policy and a namespace for the host's name alone.
func (u *unit) checkRunsAsShipped() {
	u.t.Helper()
	pid := u.show("MainPID")
	if pid == "" || pid == "0" {
		u.t.Fatalf("%s has no main process\n%s", u.name, u.status())
	}
	for _, ns := range []string{"mnt", "net", "uts"} {
		host, err := os.Readlink("/proc/1/ns/" + ns)
		mustDo(u.t, err)
		own, err := os.Readlink("/proc/" + pid + "/ns/" + ns)
		mustDo(u.t, err)
		switch {
		case ns == "uts":
			u.t.Logf("%s: its own namespace for the host's name (ProtectHostname): %v", u.name, own != host)
		case own != host:
			u.t.Errorf("%s runs in a %s namespace of its own, %s, not the host's %s", u.name, ns, own, host)
		}
	}
	if got := u.show("DevicePolicy", "DeviceAllow"); got != "auto " {
		u.t.Errorf("%s: DevicePolicy and DeviceAllow %q, want none", u.name, got)
	}
	status, err := os.ReadFile("/proc/" + pid + "/status")
	mustDo(u.t, err)
	for _, line := range strings.Split(string(status), "\n") {
		key, value, _ := strings.Cut(line, ":")
		value = strings.TrimSpace(value)
		switch key {
		case "Uid", "Gid":
			if fields := strings.Fields(value); len(fields) == 0 || fields[0] != strconv.Itoa(unitUID) {
				u.t.Errorf("%s: %s %s, want %d", u.name, key, value, unitUID)
			}
		case "Groups":
			// initgroups(3) lists the account's own group there; any other is a supplementary group of the unit's.
			if value != "" && value != strconv.Itoa(unitUID) {
				u.t.Errorf("%s: groups %q, want the account's own alone", u.name, value)
			}
		case "NoNewPrivs", "Seccomp":
			if want := map[string]string{"NoNewPrivs": "1", "Seccomp": "2"}[key]; value != want {
				u.t.Errorf("%s: %s %s, want %s", u.name, key, value, want)
			}
		}
	}
}

// checkNothingRefused fails when the service's log says a process it ran was ended by its filter of calls.
func (u *unit) checkNothingRefused() {
	u.t.Helper()
	for _, line := range strings.Split(u.said(), "\n") {
		if strings.Contains(line, "bad system call") {
			u.t.Errorf("%s: a call out of its filter: %s", u.name, line)
		}
	}
	if out, err := exec.CommandContext(u.t.Context(), "dmesg").CombinedOutput(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, "type=1326") {
				u.t.Errorf("the kernel says: %s", line)
			}
		}
	}
}

// scsiDisk loads scsi_debug until the test ends, and gives the name of the whole disk it made.
func scsiDisk(t *testing.T) string {
	t.Helper()
	if err := attempt(t, "modprobe", "scsi_debug", "dev_size_mb=96"); err != nil {
		t.Fatalf("no scsi_debug in the guest: %v", err)
	}
	t.Cleanup(func() {
		time.Sleep(time.Second)
		_ = exec.CommandContext(context.Background(), "modprobe", "-r", "scsi_debug").Run()
	})
	var links []string
	waitFor(t, "a scsi_debug disk", func() bool {
		links, _ = filepath.Glob("/sys/bus/pseudo/drivers/scsi_debug/adapter*/host*/target*/*:*/block/*")
		return len(links) > 0
	})
	return filepath.Base(links[0])
}

// mappedOnAPartition makes a filesystem on the first partition of a scsi_debug disk, through device-mapper, as a disk
// behind a USB bridge is a whole SCSI disk with a partition: what the daemon asks whether it spins is the whole disk.
func mappedOnAPartition(t *testing.T, fs names.Filesystem, whole string) *mappedVolume {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sfdisk", "-q", "/dev/"+whole)
	cmd.Stdin = strings.NewReader("label: dos\n,,83\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sfdisk: %v: %s", err, out)
	}
	part := "/dev/" + whole + "1"
	waitFor(t, part, func() bool { _, err := os.Stat(part); return err == nil })
	for _, m := range makers {
		if m.fs == fs {
			must(t, m.mkfs[0], append(slices.Clone(m.mkfs[1:]), part)...)
		}
	}
	sectors := must(t, "blockdev", "--getsz", part)
	name := "atoll-" + strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	must(t, "dmsetup", "create", "--noudevsync", "--noudevrules", name, "--table", "0 "+sectors+" linear "+part+" 0")
	must(t, "dmsetup", "mknodes")
	t.Cleanup(func() {
		_ = exec.CommandContext(context.Background(), "dmsetup", "remove", "-f", "--noudevsync", name).Run()
	})
	return &mappedVolume{volume: &volume{fs: fs, device: "/dev/mapper/" + name, uuid: identifierOf(t, part)}, name: name, sectors: sectors, real: part}
}

// rotational says that a whole disk has a spindle, so that the daemon asks it whether it spins once its counters are
// still (specs.md §10).
func rotational(t *testing.T, whole string) {
	t.Helper()
	mustDo(t, os.WriteFile("/sys/block/"+whole+"/queue/rotational", []byte("1"), 0o644))
}

// openAbove opens each folder above a path to every account, as /mnt or /srv is: a test's own are root's alone.
func openAbove(t *testing.T, path string) {
	t.Helper()
	for dir := filepath.Dir(path); dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		mustDo(t, os.Chmod(dir, 0o755))
	}
}

// poolConfig writes a pool of two disks to /etc/atoll/atoll.toml, with its catalog in /var/lib/atoll, as the units
// have them, and makes that catalog; both go at the end of the test.
func poolConfig(t *testing.T, kept *volume, keep string, v *volume, at, more string) (*config.Config, string) {
	t.Helper()
	t.Cleanup(func() {
		_ = os.RemoveAll("/var/lib/atoll")
		_ = os.RemoveAll("/etc/atoll")
		_ = os.RemoveAll("/run/atoll")
	})
	for _, dir := range []string{"/var/lib/atoll", "/etc/atoll"} {
		mustDo(t, os.MkdirAll(dir, 0o755))
	}
	file := "/var/lib/atoll/catalog.db"
	text := fmt.Sprintf("pool_id = \"p\"\ntimezone = \"UTC\"\ncatalog = %q\n", file) + more +
		fmt.Sprintf("[[disk]]\nid = \"k\"\nuuid = %q\nmount = %q\nfs = %q\n", kept.uuid, keep, kept.fs) +
		fmt.Sprintf("[[disk]]\nid = \"v\"\nuuid = %q\nmount = %q\nfs = %q\n", v.uuid, at, v.fs) + benchGuards
	c, err := config.Parse([]byte(text))
	mustDo(t, err)
	mustDo(t, os.WriteFile("/etc/atoll/atoll.toml", []byte(text), 0o640))
	if _, err := catalog.Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	return c, text
}

// aroundNow is a maintenance window that holds for the test, in UTC.
func aroundNow() string {
	now := time.Now().UTC()
	return now.Add(-time.Hour).Format("15:04") + "-" + now.Add(2*time.Hour).Format("15:04")
}

// statusOverTheSocket asks the API for /v1/status on its socket, as root, and gives the answer's status.
func statusOverTheSocket(t *testing.T, socket string) int {
	t.Helper()
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://atoll/v1/status", nil)
	mustDo(t, err)
	resp, err := client.Do(req)
	mustDo(t, err)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// ownerAndMode gives a path's owner, group and mode, setuid, setgid and sticky included, as "uid:gid mode".
func ownerAndMode(t *testing.T, path string) string {
	t.Helper()
	var st syscall.Stat_t
	mustDo(t, syscall.Lstat(path, &st))
	return fmt.Sprintf("%d:%d %04o", st.Uid, st.Gid, st.Mode&0o7777)
}

// TestTheDaemonUnderSystemd: deploy/atoll.service as it is, started by systemd as the account atoll (specs.md §15.13,
// docs/threat-model.md G18): it runs in the host's mount namespace, with no supplementary group, its filter of calls,
// no new privileges and no device policy; it gets ready; it copies a file that was there before it started and one
// written after, through the filesystem mark (§15.1), a folder made 2775 as openmediavault makes shared folders, setgid
// included, and modes and owners as they are; it answers on its socket, of mode 0660; it reads the configuration again
// on systemctl reload, through the unit's own kill; it sets mergerfs's branches when the disk goes and comes back; it
// remounts the disk through /etc/fstab when its device comes back under another name, as after a USB reset, running
// umount and mount, and mount.ntfs-3g for NTFS, itself, as the guest has no system bus here through which systemctl
// would reach systemd (§15.13; TestTheRemountUnderSystemd has one); it confirms every disk by reading its device, and
// asks the whole disk below a mount, a SCSI disk below a partition and device-mapper, or a loop device, whether it
// spins, through SG_IO (§10); it runs smartctl (§23); and it stops with exit 0 on systemctl stop. Nothing it ran was
// ended by its filter.
func TestTheDaemonUnderSystemd(t *testing.T) {
	needSystemd(t)
	needMergerfs(t)
	theAccount(t)
	original, err := os.ReadFile("/etc/fstab")
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile("/etc/fstab", original, 0o644) })
	for _, tc := range []struct {
		name, driver, fstype string
		fs                   names.Filesystem
	}{{"ext4 on a partition of a SCSI disk", "", "ext4", names.Ext4}, {"ntfs-3g", "ntfs-3g", "ntfs-3g", names.NTFS}} {
		t.Run(tc.name, func(t *testing.T) {
			_ = exec.CommandContext(t.Context(), "dmesg", "-C").Run()
			var m *mappedVolume
			whole := ""
			if tc.driver == "" {
				whole = scsiDisk(t)
				m = mappedOnAPartition(t, tc.fs, whole)
				rotational(t, whole)
			} else {
				m = newMapped(t, tc.fs)
			}
			at := mountPoint(t)
			mustDo(t, os.MkdirAll(at, 0o755))
			openAbove(t, at)
			if tc.driver == "ntfs-3g" {
				// mount.ntfs-3g, run by atoll, mounts only on a mount point atoll may write to (deploy/README.md).
				mustDo(t, os.Chown(at, unitUID, unitUID))
			}
			mountVolume(t, m.volume, tc.driver, at)
			renamed := m.name + "-again"
			t.Cleanup(func() {
				_ = exec.CommandContext(context.Background(), "umount", "-l", at).Run()
				time.Sleep(200 * time.Millisecond)
				_ = exec.CommandContext(context.Background(), "dmsetup", "remove", "-f", "--noudevsync", renamed).Run()
			})
			kept := newVolume(t, names.Ext4)
			keep := mountPoint(t)
			mountVolume(t, kept, "", keep)
			// The loop device below k has a spindle too: it gives no answer to CHECK POWER MODE, which the daemon logs.
			rotational(t, filepath.Base(kept.device))
			for _, top := range []string{keep, at} {
				mustDo(t, os.MkdirAll(filepath.Join(top, "media"), 0o755))
			}
			// A shared folder, setgid, with a file before the daemon starts.
			write(t, keep, "Shared/before.mkv", "a film")
			mustDo(t, syscall.Chmod(filepath.Join(keep, "media", "Shared"), 0o2775))
			if got := ownerAndMode(t, filepath.Join(keep, "media", "Shared")); got != "0:0 2775" {
				t.Fatalf("the shared folder is %s", got)
			}
			pool := filepath.Join(t.TempDir(), "pool")
			c, _ := poolConfig(t, kept, keep, m.volume, at,
				"[defaults]\nsettle = \"0s\"\n[maintenance]\nwindow = \""+aroundNow()+"\"\n[view]\nmount = "+strconv.Quote(pool)+"\n")
			minFree, err := c.MinFree()
			mustDo(t, err)
			merged, err := view.NewMergerfs(c, minFree, nil)
			mustDo(t, err)
			mountView(t, merged.Fstab(), pool)
			mustDo(t, os.WriteFile("/etc/fstab", append(slices.Clone(original), []byte("/dev/mapper/"+renamed+" "+at+" "+tc.fstype+" defaults 0 0\n")...), 0o644))

			u := installUnit(t, "atoll.service")
			u.start()
			u.checkRunsAsShipped()
			keepMedia, atMedia := filepath.Join(keep, "media"), filepath.Join(at, "media")
			if got := ownerAndMode(t, "/var/lib/atoll"); got != fmt.Sprintf("%d:%d 0750", unitUID, unitUID) {
				t.Errorf("/var/lib/atoll: %s", got)
			}
			if got := ownerAndMode(t, "/run/atoll/atoll.sock"); got != fmt.Sprintf("%d:%d 0660", unitUID, unitUID) {
				t.Errorf("the socket: %s", got)
			}
			if code := statusOverTheSocket(t, "/run/atoll/atoll.sock"); code != http.StatusOK {
				t.Errorf("GET /v1/status on the socket: %d", code)
			}

			// What was there before the start, then a file written after it: each copied, with its folder's mode.
			waitFor(t, "the file that was there copied", func() bool {
				data, err := os.ReadFile(filepath.Join(atMedia, "Shared", "before.mkv"))
				return err == nil && string(data) == "a film"
			})
			mustDo(t, os.WriteFile(filepath.Join(keepMedia, "Shared", "after.mkv"), []byte("another film"), 0o640))
			waitFor(t, "the file written after the start copied", func() bool {
				data, err := os.ReadFile(filepath.Join(atMedia, "Shared", "after.mkv"))
				return err == nil && string(data) == "another film"
			})
			if tc.driver == "" {
				for _, path := range []string{"Shared", "Shared/before.mkv", "Shared/after.mkv"} {
					if got, want := ownerAndMode(t, filepath.Join(atMedia, path)), ownerAndMode(t, filepath.Join(keepMedia, path)); got != want {
						t.Errorf("%s: the copy %s, the source %s", path, got, want)
					}
				}
			}

			must(t, "systemctl", "reload", u.name)
			if got := u.show("ActiveState"); got != "active" {
				t.Errorf("after systemctl reload: %s\n%s", got, u.said())
			}
			if got := branchesOf(t, pool); !slices.Equal(got, []string{keepMedia, atMedia}) {
				t.Errorf("branches: %v", got)
			}

			// The device goes from under the mount and comes back under another name, as after a USB reset: the daemon
			// mounts it again through /etc/fstab, and its branch comes back.
			must(t, "dmsetup", "rename", m.name, renamed)
			must(t, "dmsetup", "mknodes")
			// A check every 30 s, as the unit has it; a mount that fails, as one while the old FUSE process still holds the
			// device, is tried again at the next.
			remounted := func() bool {
				source, err := exec.CommandContext(t.Context(), "findmnt", "-n", "-o", "SOURCE", at).Output()
				return err == nil && strings.TrimSpace(string(source)) == "/dev/mapper/"+renamed &&
					slices.Equal(branchesOf(t, pool), []string{keepMedia, atMedia})
			}
			for deadline := time.Now().Add(3 * time.Minute); !remounted() && time.Now().Before(deadline); {
				time.Sleep(time.Second)
			}
			if !remounted() {
				t.Fatal("the disk was not mounted again by the daemon within 3 minutes")
			}
			if data, err := os.ReadFile(filepath.Join(atMedia, "Shared", "before.mkv")); err != nil || string(data) != "a film" {
				t.Errorf("the disk's content once mounted again: %q, %v", data, err)
			}

			// Each loop device and SCSI disk with a spindle, once still, is asked whether it spins: the loop device gives no
			// answer; the SCSI disk may answer, or not.
			asked := func(device string) []string {
				var lines []string
				for _, line := range strings.Split(u.said(), "\n") {
					if strings.Contains(line, "CHECK POWER MODE") && strings.Contains(line, `"`+device+`"`) {
						lines = append(lines, line)
					}
				}
				return lines
			}
			loop := filepath.Base(kept.device)
			deadline := time.Now().Add(4 * time.Minute)
			for len(asked(loop)) == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Second)
			}
			if len(asked(loop)) == 0 {
				t.Errorf("%s was never asked whether it spins", loop)
			}
			for _, device := range []string{loop, whole} {
				for _, line := range asked(device) {
					t.Logf("%s", line)
					if strings.Contains(line, "permission denied") || strings.Contains(line, "not permitted") {
						t.Errorf("%s: CHECK POWER MODE refused: %s", device, line)
					}
				}
			}
			for _, line := range strings.Split(u.said(), "\n") {
				if strings.Contains(line, "SMART") || strings.Contains(line, "remount") {
					t.Logf("%s", line)
				}
				if strings.Contains(line, "smartctl is not there") {
					t.Errorf("no smartctl in the guest: %s", line)
				}
			}

			u.stop()
			u.checkNothingRefused()
			// What the daemon mounted is the host's, and stays once it stops: a mount namespace of its own would have kept
			// it to itself. mount.ntfs-3g's process is the service's, and goes with it (docs/threat-model.md).
			source, _ := exec.CommandContext(t.Context(), "findmnt", "-n", "-o", "SOURCE,FSTYPE", at).Output()
			t.Logf("%s once the daemon stopped: %q", at, strings.TrimSpace(string(source)))
			if tc.driver == "" && strings.Join(strings.Fields(string(source)), " ") != "/dev/mapper/"+renamed+" ext4" {
				t.Errorf("once the daemon stopped, %s holds %q", at, source)
			}
		})
	}
}

// polkitRule is where the .deb installs deploy/50-atoll.rules (.goreleaser.yaml).
const polkitRule = "/usr/share/polkit-1/rules.d/50-atoll.rules"

// machineServices starts, until the test ends, what a machine runs and the guest leaves out with sysinit.target: the
// system bus, through which systemctl reaches systemd as any account but root; polkit, which systemd asks whether
// such an account may start a unit; and udev, whose devices systemd waits for before it mounts one. With rule, polkit
// has Atoll's rule, where and as the .deb installs it. Each unit that would pull sysinit.target in gets a drop-in that
// keeps it out, as installUnit's does.
func machineServices(t *testing.T, rule bool) {
	t.Helper()
	for _, name := range []string{"dbus.socket", "dbus.service", "polkit.service"} {
		dropin := filepath.Join("/run/systemd/system", name+".d")
		mustDo(t, os.MkdirAll(dropin, 0o755))
		mustDo(t, os.WriteFile(filepath.Join(dropin, "test.conf"), []byte("[Unit]\nDefaultDependencies=no\n"), 0o644))
		t.Cleanup(func() { _ = os.RemoveAll(dropin) })
	}
	_ = os.Remove(polkitRule)
	if rule {
		data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "50-atoll.rules"))
		mustDo(t, err)
		mustDo(t, os.MkdirAll(filepath.Dir(polkitRule), 0o755))
		mustDo(t, os.WriteFile(polkitRule, data, 0o644))
		t.Cleanup(func() { _ = os.Remove(polkitRule) })
	}
	must(t, "systemctl", "daemon-reload")
	services := []string{"dbus.socket", "dbus.service", "polkit.service", "systemd-udevd.service"}
	t.Cleanup(func() {
		_ = exec.CommandContext(context.Background(), "systemctl", append([]string{"stop", "systemd-udevd-control.socket",
			"systemd-udevd-kernel.socket"}, services...)...).Run()
		// libdevmapper takes udev for running while its control socket is there, and would wait for it forever.
		_ = os.Remove("/run/udev/control")
		_ = exec.CommandContext(context.Background(), "systemctl", "daemon-reload").Run()
	})
	if err := attempt(t, "systemctl", append([]string{"start"}, services...)...); err != nil {
		for _, s := range services {
			out, _ := exec.CommandContext(t.Context(), "systemctl", "status", "--no-pager", "-l", s).CombinedOutput()
			t.Logf("%s", out)
		}
		t.Fatal(err)
	}
	// systemd joins the bus once it is up: atoll reads a unit through it, which takes no authorization.
	waitFor(t, "systemd on the system bus", func() bool {
		return attempt(t, "setpriv", "--reuid="+strconv.Itoa(unitUID), "--regid="+strconv.Itoa(unitUID), "--clear-groups", "--",
			"systemctl", "show", "-p", "Version") == nil
	})
}

// fusePIDs gives the processes whose command line names a path, as the FUSE process of a mount there does.
func fusePIDs(t *testing.T, at string) []string {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	mustDo(t, err)
	var pids []string
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err == nil && slices.Contains(strings.Split(string(cmdline), "\x00"), at) {
			pids = append(pids, e.Name())
		}
	}
	return pids
}

// TestTheRemountUnderSystemd: the remount after a USB reset as a machine runs it (specs.md §15.13), deploy/atoll.service
// started by systemd as it is, with the system bus, polkit and udev. With Atoll's polkit rule installed as the .deb
// installs it, the daemon has systemd start the disk's mount unit once its device comes back under another name: the
// disk is mounted again, an ntfs-3g disk's FUSE process belongs to the mount unit, and the disk stays mounted, and
// readable, after systemctl stop atoll, a start, and systemctl restart atoll; atoll may not stop the unit. On ext4 too.
// Without the rule, systemd refuses the start, the daemon mounts the disk itself and says why, as before 2026-11-19:
// ntfs-3g's FUSE process is then the service's, and a stop of the daemon unmounts the disk. Nothing the daemon ran
// was ended by its filter of calls.
func TestTheRemountUnderSystemd(t *testing.T) {
	needSystemd(t)
	theAccount(t)
	original, err := os.ReadFile("/etc/fstab")
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile("/etc/fstab", original, 0o644) })
	for _, tc := range []struct {
		name, driver, fstype string
		fs                   names.Filesystem
		rule                 bool
	}{
		{"ntfs-3g through its mount unit", "ntfs-3g", "ntfs-3g", names.NTFS, true},
		{"ext4 through its mount unit", "", "ext4", names.Ext4, true},
		{"ntfs-3g without the polkit rule", "ntfs-3g", "ntfs-3g", names.NTFS, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = exec.CommandContext(t.Context(), "dmesg", "-C").Run()
			machineServices(t, tc.rule)
			m := newMapped(t, tc.fs)
			at := mountPoint(t)
			mustDo(t, os.MkdirAll(at, 0o755))
			openAbove(t, at)
			if tc.driver == "ntfs-3g" && !tc.rule {
				// mount.ntfs-3g, run by atoll, mounts only on a mount point atoll may write to (deploy/README.md).
				mustDo(t, os.Chown(at, unitUID, unitUID))
			}
			mountVolume(t, m.volume, tc.driver, at)
			renamed := m.name + "-again"
			t.Cleanup(func() {
				_ = exec.CommandContext(context.Background(), "umount", "-l", at).Run()
				time.Sleep(200 * time.Millisecond)
				_ = exec.CommandContext(context.Background(), "dmsetup", "remove", "-f", "--noudevsync", renamed).Run()
			})
			kept := newVolume(t, names.Ext4)
			keep := mountPoint(t)
			mountVolume(t, kept, "", keep)
			mustDo(t, os.MkdirAll(filepath.Join(keep, "media"), 0o755))
			write(t, at, "film.mkv", "a film")
			film := filepath.Join(at, "media", "film.mkv")
			readable := func() bool { data, err := os.ReadFile(film); return err == nil && string(data) == "a film" }
			source := func() string {
				out, _ := exec.CommandContext(t.Context(), "findmnt", "-n", "-o", "SOURCE", at).Output()
				return strings.TrimSpace(string(out))
			}
			// No mergerfs at view.mount: the daemon sets no branch.
			poolConfig(t, kept, keep, m.volume, at, "[defaults]\nsettle = \"0s\"\n[view]\nmount = "+
				strconv.Quote(filepath.Join(t.TempDir(), "pool"))+"\n")
			mustDo(t, os.WriteFile("/etc/fstab", append(slices.Clone(original), []byte("/dev/mapper/"+renamed+" "+at+" "+tc.fstype+" defaults 0 0\n")...), 0o644))
			// installUnit has systemd read its units again, and its fstab generator make the unit of that entry.
			u := installUnit(t, "atoll.service")
			unit := must(t, "systemd-escape", "--path", "--suffix=mount", at)
			t.Cleanup(func() {
				if t.Failed() {
					out, _ := exec.CommandContext(context.Background(), "systemctl", "status", "--no-pager", "-l", unit).CombinedOutput()
					t.Logf("%s", out)
					out, _ = exec.CommandContext(context.Background(), "systemctl", "list-units", "--all", "--no-pager", "*.device").CombinedOutput()
					t.Logf("%s", out)
				}
			})
			if got := must(t, "systemctl", "show", "-p", "SourcePath", "--value", unit); got != "/etc/fstab" {
				t.Fatalf("%s comes from %q, not /etc/fstab", unit, got)
			}
			u.start()
			u.checkRunsAsShipped()

			// The device goes from under the mount and comes back under another name, as after a USB reset; udev hears
			// of it, as on a machine, and systemd then holds a device unit of that name.
			must(t, "dmsetup", "rename", "--noudevsync", m.name, renamed)
			must(t, "udevadm", "settle")
			must(t, "dmsetup", "mknodes")
			if _, err := os.Lstat("/dev/mapper/" + m.name); err == nil {
				mustDo(t, os.Remove("/dev/mapper/"+m.name))
			}
			// A check every 30 s, as the unit has it.
			for deadline := time.Now().Add(3 * time.Minute); source() != "/dev/mapper/"+renamed && time.Now().Before(deadline); {
				time.Sleep(time.Second)
			}
			if got := source(); got != "/dev/mapper/"+renamed {
				t.Fatalf("the disk was not mounted again by the daemon within 3 minutes: %q", got)
			}
			waitFor(t, "the disk readable once mounted again", readable)
			// The mount shows before the daemon has seen the unit's job end and said so: with KVM, read at once, the log
			// had no line yet, and the stop that followed cut the daemon's wait short (GitHub's run 44579171805).
			waitFor(t, "the daemon saying how the remount ended", func() bool {
				said := u.said()
				return strings.Contains(said, "remounted after a reset") || strings.Contains(said, "the remount failed")
			})
			said := u.said()
			for _, line := range strings.Split(said, "\n") {
				if strings.Contains(line, "remount") {
					t.Logf("%s", line)
				}
			}
			throughUnit := strings.Contains(said, strings.ReplaceAll(unit, `\`, `\\`))
			fuse := fusePIDs(t, at)
			if tc.driver == "ntfs-3g" && len(fuse) == 0 {
				t.Errorf("no FUSE process serves %s", at)
			}
			if !tc.rule {
				if n := strings.Count(said, "systemd refused to start the mount unit"); n != 1 {
					t.Errorf("why the daemon mounted the disk itself, said %d times", n)
				}
				u.stop()
				u.checkNothingRefused()
				// The FUSE process was the service's: the stop ended it, and the disk with it (deploy/README.md).
				if tc.driver == "ntfs-3g" {
					waitFor(t, "the disk gone with the service", func() bool { return !readable() })
					t.Logf("%s once the daemon stopped: %q", at, source())
				}
				return
			}
			if !throughUnit || strings.Contains(said, "the remount mounts the disk itself") {
				t.Errorf("not remounted through %s", unit)
			}
			if got := must(t, "systemctl", "show", "-p", "ActiveState", "--value", unit); got != "active" {
				t.Errorf("%s is %s", unit, got)
			}
			for _, pid := range fuse {
				cgroup, err := os.ReadFile(filepath.Join("/proc", pid, "cgroup"))
				mustDo(t, err)
				if !strings.HasSuffix(strings.TrimSpace(string(cgroup)), "/"+unit) {
					t.Errorf("the FUSE process %s is in %s, not in %s's", pid, strings.TrimSpace(string(cgroup)), unit)
				}
			}

			// The service stops, starts, and restarts: the disk stays.
			stays := func(after string) {
				t.Helper()
				if got := source(); got != "/dev/mapper/"+renamed || !readable() {
					t.Errorf("after %s, %s holds %q, readable %v", after, at, got, readable())
				}
			}
			u.stop()
			u.checkNothingRefused()
			time.Sleep(2 * time.Second)
			stays("systemctl stop atoll")
			u.start()
			stays("systemctl start atoll")
			must(t, "systemctl", "restart", u.name)
			time.Sleep(2 * time.Second)
			stays("systemctl restart atoll")
			if got := u.show("ActiveState"); got != "active" {
				t.Errorf("after systemctl restart: %s\n%s", got, u.said())
			}
			// The rule lets atoll start a mount unit, and nothing else.
			if err := attempt(t, "setpriv", "--reuid="+strconv.Itoa(unitUID), "--regid="+strconv.Itoa(unitUID), "--clear-groups", "--",
				"systemctl", "--no-ask-password", "stop", "--", unit); err == nil {
				t.Errorf("atoll stopped %s", unit)
			} else {
				t.Logf("atoll's stop refused: %v", err)
			}
			stays("atoll's stop of the unit")
			u.stop()
			u.checkNothingRefused()
		})
	}
}

// TestAnIdleDiskUnpluggedUnderSystemd: a disk systemd mounted from /etc/fstab, as at boot, and that nothing holds,
// loses its device as an unplugged USB disk does. systemd itself unmounts it, as the stop of its device unit goes to
// the mount unit, and the daemon may find it only not-mounted, which alone is no reason a USB reset gives: as the
// device it was mounted from left, it is one (specs.md §15.13, the maintainer's decision of 2026-11-19). The daemon
// waits while the device is away, and has systemd mount the disk again once it is back. On ext4 and ntfs-3g. udev's
// rules run for the device, unlike newMapped's: without them systemd has no device unit to stop (measured).
func TestAnIdleDiskUnpluggedUnderSystemd(t *testing.T) {
	needSystemd(t)
	theAccount(t)
	original, err := os.ReadFile("/etc/fstab")
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile("/etc/fstab", original, 0o644) })
	for _, tc := range []struct {
		name, fstype string
		fs           names.Filesystem
	}{
		{"ext4", "ext4", names.Ext4},
		{"ntfs-3g", "ntfs-3g", names.NTFS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			machineServices(t, true)
			v := newVolume(t, tc.fs)
			sectors := must(t, "blockdev", "--getsz", v.device)
			name := "atoll-idle-" + tc.name
			gone := name + "-gone"
			must(t, "dmsetup", "create", name, "--table", "0 "+sectors+" linear "+v.device+" 0")
			must(t, "udevadm", "settle")
			t.Cleanup(func() {
				for _, n := range []string{name, gone} {
					_ = exec.CommandContext(context.Background(), "dmsetup", "remove", "-f", "--noudevsync", n).Run()
				}
			})
			at := mountPoint(t)
			mustDo(t, os.MkdirAll(at, 0o755))
			openAbove(t, at)
			mustDo(t, os.WriteFile("/etc/fstab", append(slices.Clone(original), []byte("/dev/mapper/"+name+" "+at+" "+tc.fstype+" defaults,nofail 0 0\n")...), 0o644))
			must(t, "systemctl", "daemon-reload")
			unit := must(t, "systemd-escape", "--path", "--suffix=mount", at)
			must(t, "systemctl", "start", unit)
			t.Cleanup(func() {
				_ = exec.CommandContext(context.Background(), "umount", "-l", at).Run()
				time.Sleep(200 * time.Millisecond)
			})
			write(t, at, "film.mkv", "a film")
			film := filepath.Join(at, "media", "film.mkv")
			readable := func() bool { data, err := os.ReadFile(film); return err == nil && string(data) == "a film" }
			source := func() string {
				out, _ := exec.CommandContext(t.Context(), "findmnt", "-n", "-o", "SOURCE", at).Output()
				return strings.TrimSpace(string(out))
			}
			kept := newVolume(t, names.Ext4)
			keep := mountPoint(t)
			mountVolume(t, kept, "", keep)
			mustDo(t, os.MkdirAll(filepath.Join(keep, "media"), 0o755))
			poolConfig(t, kept, keep, v, at, "[defaults]\nsettle = \"0s\"\n[view]\nmount = "+
				strconv.Quote(filepath.Join(t.TempDir(), "pool"))+"\n")
			u := installUnit(t, "atoll.service")
			u.start()
			u.checkRunsAsShipped()
			// Once its first pass is over, nothing of the daemon's reads the disk when its device goes: the check finds
			// it, every 30 s.
			waitFor(t, "the daemon's first pass", func() bool { return strings.Contains(u.said(), "a pass ended") })

			must(t, "dmsetup", "rename", name, gone)
			must(t, "udevadm", "settle")
			waitFor(t, "systemd unmounting the idle disk", func() bool { return source() == "" })
			waitFor(t, "the daemon waiting for the device to come back", func() bool {
				return strings.Contains(u.said(), "the remount waits: its device is not there")
			})
			must(t, "dmsetup", "rename", gone, name)
			must(t, "udevadm", "settle")
			for deadline := time.Now().Add(3 * time.Minute); source() != "/dev/mapper/"+name && time.Now().Before(deadline); {
				time.Sleep(time.Second)
			}
			if got := source(); got != "/dev/mapper/"+name {
				t.Fatalf("the disk was not mounted again by the daemon within 3 minutes: %q", got)
			}
			waitFor(t, "the disk readable once mounted again", readable)
			waitFor(t, "the daemon saying how the remount ended", func() bool {
				said := u.said()
				return strings.Contains(said, "remounted after a reset") || strings.Contains(said, "the remount failed")
			})
			said := u.said()
			var wentMissing string
			for _, line := range strings.Split(said, "\n") {
				if strings.Contains(line, "missing") || strings.Contains(line, "remount") || strings.Contains(line, "device left") {
					t.Logf("%s", line)
				}
				if wentMissing == "" && strings.Contains(line, "a disk went missing") {
					wentMissing = line
				}
			}
			// The check finds it not-mounted, unless a pass's confirmation came first, which finds its device gone: either
			// way, a reset. Found not-mounted, the daemon says its device left.
			if strings.Contains(wentMissing, `"reason":"not-mounted"`) && !strings.Contains(said, "no longer mounted and its device left") {
				t.Error("found not-mounted, the disk was not said to have lost its device")
			}
			if !strings.Contains(said, "remounted after a reset") || !strings.Contains(said, strings.ReplaceAll(unit, `\`, `\\`)) {
				t.Errorf("not remounted through %s", unit)
			}
			if got := must(t, "systemctl", "show", "-p", "ActiveState", "--value", unit); got != "active" {
				t.Errorf("%s is %s", unit, got)
			}
			u.stop()
			u.checkNothingRefused()
		})
	}
}

// poolPolkitRule is where root installs the rule atoll polkit-rule prints (deploy/README.md): polkitd reads it before
// polkitRule, the .deb's.
const poolPolkitRule = "/etc/polkit-1/rules.d/49-atoll-pool.rules"

// TestThePoolPolkitRuleUnderSystemd: the rule atoll polkit-rule prints from the configuration (specs.md §15.13, §25;
// docs/threat-model.md, G22), installed where deploy/README.md says, beside the .deb's rule, as polkitd 126 and systemd
// 257 read them. The pool's disk is mounted at <dir>/dev-disk-by-uuid-1234, whose unit's name holds \x2d, and a tmpfs
// of /etc/fstab at <dir>/dev/disk/by/uuid/1234, whose unit is what a rule that left systemd's escapes to JavaScript
// would name instead, "\x2d" read as "-". With the .deb's rule alone, the account atoll may start both units, through
// pkcheck, and starts the tmpfs through systemctl. With the pool's rule, polkit says no to the tmpfs's unit, a refusal
// of the rule's own rather than the action's default, auth_admin, which would ask for a password; atoll's start of it
// is refused, and the tmpfs stays unmounted; and the daemon still mounts the pool's disk again through its unit after
// a USB reset.
func TestThePoolPolkitRuleUnderSystemd(t *testing.T) {
	needSystemd(t)
	theAccount(t)
	original, err := os.ReadFile("/etc/fstab")
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile("/etc/fstab", original, 0o644) })
	machineServices(t, true)
	m := newMapped(t, names.Ext4)
	dir := t.TempDir()
	at := filepath.Join(dir, "dev-disk-by-uuid-1234")
	elsewhere := filepath.Join(dir, "dev", "disk", "by", "uuid", "1234")
	mustDo(t, os.MkdirAll(at, 0o755))
	mustDo(t, os.MkdirAll(elsewhere, 0o755))
	openAbove(t, at)
	mountVolume(t, m.volume, "", at)
	renamed := m.name + "-again"
	t.Cleanup(func() {
		_ = exec.CommandContext(context.Background(), "umount", "-l", elsewhere).Run()
		_ = exec.CommandContext(context.Background(), "umount", "-l", at).Run()
		time.Sleep(200 * time.Millisecond)
		_ = exec.CommandContext(context.Background(), "dmsetup", "remove", "-f", "--noudevsync", renamed).Run()
	})
	kept := newVolume(t, names.Ext4)
	keep := mountPoint(t)
	mountVolume(t, kept, "", keep)
	mustDo(t, os.MkdirAll(filepath.Join(keep, "media"), 0o755))
	write(t, at, "film.mkv", "a film")
	film := filepath.Join(at, "media", "film.mkv")
	readable := func() bool { data, err := os.ReadFile(film); return err == nil && string(data) == "a film" }
	source := func() string {
		out, _ := exec.CommandContext(t.Context(), "findmnt", "-n", "-o", "SOURCE", at).Output()
		return strings.TrimSpace(string(out))
	}
	poolConfig(t, kept, keep, m.volume, at, "[defaults]\nsettle = \"0s\"\n[view]\nmount = "+
		strconv.Quote(filepath.Join(t.TempDir(), "pool"))+"\n")
	mustDo(t, os.WriteFile("/etc/fstab", append(slices.Clone(original), []byte("/dev/mapper/"+renamed+" "+at+" ext4 defaults 0 0\n"+
		"tmpfs "+elsewhere+" tmpfs noauto 0 0\n")...), 0o644))
	must(t, "systemctl", "daemon-reload")
	unit := must(t, "systemd-escape", "--path", "--suffix=mount", at)
	other := must(t, "systemd-escape", "--path", "--suffix=mount", elsewhere)
	if strings.ReplaceAll(unit, `\x2d`, "-") != other {
		t.Fatalf("%s and %s: not the names an unescaped rule would mistake", unit, other)
	}
	active := func(name string) string {
		return must(t, "systemctl", "show", "-p", "ActiveState", "--value", "--", name)
	}

	// A process of the account's, which polkit is asked about through pkcheck, as systemd asks it: the unit and the verb.
	asAtoll := exec.CommandContext(t.Context(), "setpriv", "--reuid="+strconv.Itoa(unitUID), "--regid="+strconv.Itoa(unitUID),
		"--clear-groups", "--", "sleep", "600")
	mustDo(t, asAtoll.Start())
	t.Cleanup(func() { _ = asAtoll.Process.Kill(); _ = asAtoll.Wait() })
	pid := strconv.Itoa(asAtoll.Process.Pid)
	// pkcheck names a process by its pid, its start time and its uid, which polkit checks: once setpriv has set it.
	waitFor(t, "the process running as atoll", func() bool {
		status, err := os.ReadFile("/proc/" + pid + "/status")
		return err == nil && strings.Contains(string(status), "\nUid:\t"+strconv.Itoa(unitUID)+"\t")
	})
	stat, err := os.ReadFile("/proc/" + pid + "/stat")
	mustDo(t, err)
	// The start time is the 22nd field, the 20th after the command's name, which ends with the last ')'.
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	process := pid + "," + fields[19] + "," + strconv.Itoa(unitUID)
	pkcheck := func(name string) int {
		t.Helper()
		out, err := exec.CommandContext(t.Context(), "pkcheck", "--action-id", "org.freedesktop.systemd1.manage-units",
			"--process", process, "--detail", "unit", name, "--detail", "verb", "start").CombinedOutput()
		var exit *exec.ExitError
		switch {
		case err == nil:
			return 0
		case errors.As(err, &exit):
			t.Logf("pkcheck %s: exit %d: %s", name, exit.ExitCode(), strings.TrimSpace(string(out)))
			return exit.ExitCode()
		}
		t.Fatalf("pkcheck %s: %v: %s", name, err, out)
		return -1
	}
	systemctlAsAtoll := func(args ...string) error {
		return attempt(t, "setpriv", append([]string{"--reuid=" + strconv.Itoa(unitUID), "--regid=" + strconv.Itoa(unitUID),
			"--clear-groups", "--", "systemctl", "--no-ask-password"}, args...)...)
	}

	// The .deb's rule alone: any mount unit.
	for _, name := range []string{unit, other} {
		if got := pkcheck(name); got != 0 {
			t.Errorf("with the .deb's rule alone, pkcheck says %d for %s, want 0", got, name)
		}
	}
	if err := systemctlAsAtoll("start", "--", other); err != nil {
		t.Fatalf("with the .deb's rule alone, atoll's start of %s: %v", other, err)
	}
	if got := active(other); got != "active" {
		t.Fatalf("%s is %s once atoll started it", other, got)
	}
	must(t, "systemctl", "stop", "--", other)

	// The pool's rule, printed from the configuration and installed as deploy/README.md says.
	rule, err := exec.CommandContext(t.Context(), atollBinary, "--config", "/etc/atoll/atoll.toml", "polkit-rule").Output()
	if err != nil {
		t.Fatalf("atoll polkit-rule: %v", err)
	}
	if !strings.Contains(string(rule), `"`+strings.ReplaceAll(unit, `\`, `\\`)+`"`) {
		t.Fatalf("the rule does not name %s:\n%s", unit, rule)
	}
	t.Logf("%s:\n%s", poolPolkitRule, rule)
	mustDo(t, os.MkdirAll(filepath.Dir(poolPolkitRule), 0o755))
	mustDo(t, os.WriteFile(poolPolkitRule, rule, 0o644))
	t.Cleanup(func() { _ = os.Remove(poolPolkitRule) })
	must(t, "systemctl", "restart", "polkit.service")
	// 1 is polkit's no, which the rule gives; systemd's policy for the action, auth_admin, would ask for a password, 2.
	for _, c := range []struct {
		unit string
		want int
	}{{unit, 0}, {other, 1}, {"home.mount", 1}} {
		if got := pkcheck(c.unit); got != c.want {
			t.Errorf("with the pool's rule, pkcheck says %d for %s, want %d", got, c.unit, c.want)
		}
	}
	if err := systemctlAsAtoll("start", "--", other); err == nil {
		t.Errorf("with the pool's rule, atoll started %s", other)
	} else {
		t.Logf("atoll's start refused: %v", err)
	}
	if got := active(other); got != "inactive" {
		t.Errorf("%s is %s once atoll's start was refused", other, got)
	}

	// The remount after a USB reset still goes through the pool disk's unit.
	u := installUnit(t, "atoll.service")
	if got := must(t, "systemctl", "show", "-p", "SourcePath", "--value", "--", unit); got != "/etc/fstab" {
		t.Fatalf("%s comes from %q, not /etc/fstab", unit, got)
	}
	u.start()
	u.checkRunsAsShipped()
	must(t, "dmsetup", "rename", "--noudevsync", m.name, renamed)
	must(t, "udevadm", "settle")
	must(t, "dmsetup", "mknodes")
	if _, err := os.Lstat("/dev/mapper/" + m.name); err == nil {
		mustDo(t, os.Remove("/dev/mapper/"+m.name))
	}
	for deadline := time.Now().Add(3 * time.Minute); source() != "/dev/mapper/"+renamed && time.Now().Before(deadline); {
		time.Sleep(time.Second)
	}
	if got := source(); got != "/dev/mapper/"+renamed {
		t.Fatalf("the disk was not mounted again by the daemon within 3 minutes: %q", got)
	}
	waitFor(t, "the disk readable once mounted again", readable)
	waitFor(t, "the daemon saying how the remount ended", func() bool {
		said := u.said()
		return strings.Contains(said, "remounted after a reset") || strings.Contains(said, "the remount failed")
	})
	said := u.said()
	for _, line := range strings.Split(said, "\n") {
		if strings.Contains(line, "remount") {
			t.Logf("%s", line)
		}
	}
	if !strings.Contains(said, "remounted after a reset") || !strings.Contains(said, strings.ReplaceAll(unit, `\`, `\\`)) ||
		strings.Contains(said, "the remount mounts the disk itself") {
		t.Errorf("not remounted through %s", unit)
	}
	if got := active(unit); got != "active" {
		t.Errorf("%s is %s", unit, got)
	}
	u.stop()
	u.checkNothingRefused()
}

// Atoll's names of mount units are systemd's: each name of conformance/vectors/mount_unit.json is what this machine's
// systemd-escape --path --suffix=mount writes (specs.md §15.13).
func TestMountUnitNamesAreSystemds(t *testing.T) {
	if _, err := exec.LookPath("systemd-escape"); err != nil {
		if os.Getenv("ATOLL_QEMU") != "" {
			t.Fatal("no systemd-escape in the qemu guest: test/qemu/run.sh installs systemd")
		}
		t.Skip("no systemd-escape")
	}
	type vector struct {
		Path  string `json:"path"`
		Unit  string `json:"unit"`
		Error bool   `json:"error"`
		Note  string `json:"note"`
	}
	for _, v := range vectors.Load[vector](t, "mount_unit.json") {
		if v.Error {
			continue
		}
		if got := must(t, "systemd-escape", "--path", "--suffix=mount", "--", v.Path); got != v.Unit {
			t.Errorf("%q: systemd-escape writes %q, the vector %q", v.Path, got, v.Unit)
		}
	}
}

// TestTheViewUnderSystemd: deploy/atoll-mount.service as it is, started by systemd as the account atoll (specs.md
// §21.2, docs/threat-model.md G18), with view.engine = "atoll" and view.readonly_mount: it runs in the host's mount
// namespace, with no supplementary group, its filter of calls and no device policy; both views are mounted for every
// process of the host once it is ready; the view read only gives each file to root, and to another account what that
// account may read; the view that writes makes a file, a folder made 2775 as openmediavault makes shared folders, setgid
// included, a file of another account's in it, changes a mode, an owner and a time, renames, links and deletes, each on
// a disk, with the mode asked whatever the unit's mask; and a stop unmounts both, with exit 0. Nothing it ran was ended
// by its filter.
func TestTheViewUnderSystemd(t *testing.T) {
	needSystemd(t)
	theAccount(t)
	_ = exec.CommandContext(t.Context(), "dmesg", "-C").Run()
	v := newVolume(t, names.Ext4)
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	kept := newVolume(t, names.Ext4)
	keep := mountPoint(t)
	mountVolume(t, kept, "", keep)
	for _, top := range []string{keep, at} {
		mustDo(t, os.MkdirAll(filepath.Join(top, "media"), 0o755))
	}
	write(t, keep, "Films/A.mkv", "from k")
	write(t, at, "Films/secret.mkv", "root's alone")
	mustDo(t, os.Chmod(filepath.Join(at, "media", "Films", "secret.mkv"), 0o600))
	// Where another account may reach them, as a media server reaches /srv/pool-ro.
	points := make([]string, 2)
	for i := range points {
		dir, err := os.MkdirTemp("", "atoll-view-")
		mustDo(t, err)
		mustDo(t, os.Chmod(dir, 0o755))
		t.Cleanup(func() { _ = syscall.Unmount(dir, syscall.MNT_DETACH); _ = os.Remove(dir) })
		points[i] = dir
	}
	point, readonlyPoint := points[0], points[1]
	c, text := poolConfig(t, kept, keep, v, at, "[defaults]\nsettle = \"0s\"\n[view]\nengine = \"atoll\"\nmount = "+
		strconv.Quote(point)+"\nreadonly_mount = "+strconv.Quote(readonlyPoint)+"\n")
	// The catalog knows the pool before the view starts, as the daemon's passes leave it.
	db, err := catalog.OpenPool(t.Context(), "/var/lib/atoll/catalog.db", "p")
	mustDo(t, err)
	m, err := disks.NewMachine()
	mustDo(t, err)
	b := &passBench{t: t, db: db, config: c, configText: text, catalog: "/var/lib/atoll/catalog.db", keep: keep, at: at, machine: m}
	b.scan(false)
	catchUp(b)
	b.scan(false)
	mustDo(t, db.Close())
	// The catalog's folder is atoll's, as atoll.service's StateDirectory makes it; /dev/fuse is open to every account,
	// as udev makes it, where the guest, which has no udev, gives it to root alone.
	must(t, "chown", "-R", strconv.Itoa(unitUID)+":"+strconv.Itoa(unitUID), "/var/lib/atoll")
	mustDo(t, os.Chmod("/var/lib/atoll", 0o750))
	fuse, err := os.Stat("/dev/fuse")
	mustDo(t, err)
	mustDo(t, os.Chmod("/dev/fuse", 0o666))
	t.Cleanup(func() { _ = os.Chmod("/dev/fuse", fuse.Mode().Perm()) })

	u := installUnit(t, "atoll-mount.service")
	u.start()
	u.checkRunsAsShipped()
	for _, p := range points {
		if got := mountsAt(t, p); !slices.Equal(got, []string{fusefs.Type}) {
			t.Errorf("mounted at %s: %v", p, got)
		}
	}
	other := func(args ...string) error {
		return attempt(t, "setpriv", append([]string{"--reuid=" + strconv.Itoa(otherUID), "--regid=" + strconv.Itoa(otherUID),
			"--clear-groups", "--"}, args...)...)
	}
	for _, p := range points {
		for name, want := range map[string]string{"A.mkv": "from k", "secret.mkv": "root's alone"} {
			if data, err := os.ReadFile(filepath.Join(p, "Films", name)); err != nil || string(data) != want {
				t.Errorf("%s, as root: %q, %v", filepath.Join(p, "Films", name), data, err)
			}
		}
		if err := other("cat", filepath.Join(p, "Films", "A.mkv")); err != nil {
			t.Errorf("A.mkv at %s, as another account: %v", p, err)
		}
		if err := other("cat", filepath.Join(p, "Films", "secret.mkv")); err == nil || !strings.Contains(err.Error(), "Permission denied") {
			t.Errorf("secret.mkv at %s, as another account: %v, want it refused", p, err)
		}
	}

	in := func(path string) string { return filepath.Join(point, path) }
	onADisk := func(path string) string {
		for _, top := range []string{keep, at} {
			if _, err := os.Lstat(filepath.Join(top, "media", path)); err == nil {
				return filepath.Join(top, "media", path)
			}
		}
		return ""
	}
	// Each copy of a folder on the disks, with its owner and mode.
	folderOnDisks := func(path string) []string {
		var seen []string
		for _, top := range []string{keep, at} {
			if _, err := os.Lstat(filepath.Join(top, "media", path)); err == nil {
				seen = append(seen, filepath.Base(top)+" "+ownerAndMode(t, filepath.Join(top, "media", path)))
			}
		}
		return seen
	}
	mustDo(t, os.WriteFile(in("Films/New.mkv"), []byte("a new film"), 0o644))
	mustDo(t, os.Mkdir(in("Shared"), 0o755))
	mustDo(t, syscall.Chmod(in("Shared"), 0o2775))
	t.Logf("Shared once made 2775 through the view: %v", folderOnDisks("Shared"))
	mustDo(t, os.Chown(in("Shared"), otherUID, otherUID))
	t.Logf("Shared once given to %d through the view: %v", otherUID, folderOnDisks("Shared"))
	if info, err := os.Stat(in("Shared")); err == nil {
		t.Logf("Shared as the view presents it: %v", info.Mode())
	}
	if err := other("sh", "-c", "printf mine > "+in("Shared/theirs.mkv")); err != nil {
		t.Errorf("a file of another account's, made through the view: %v", err)
	}
	mustDo(t, os.Chmod(in("Films/A.mkv"), 0o640))
	mustDo(t, os.Chown(in("Films/A.mkv"), otherUID, otherUID))
	when := time.Unix(1_700_000_000, 0)
	mustDo(t, os.Chtimes(in("Films/A.mkv"), when, when))
	mustDo(t, os.Rename(in("Films/New.mkv"), in("Films/New (2024).mkv")))
	mustDo(t, os.Link(in("Films/A.mkv"), in("Films/A-link.mkv")))
	mustDo(t, os.Remove(in("Films/A-link.mkv")))
	for path, want := range map[string]string{
		"Films/New (2024).mkv": "0:0 0644", "Shared/theirs.mkv": fmt.Sprintf("%d:%d", otherUID, otherUID), "Films/A.mkv": fmt.Sprintf("%d:%d 0640", otherUID, otherUID),
	} {
		disk := onADisk(path)
		if disk == "" {
			t.Errorf("%s is on no disk", path)
			continue
		}
		if got := ownerAndMode(t, disk); !strings.HasPrefix(got, want) {
			t.Errorf("%s on its disk: %s, want %s", path, got, want)
		}
	}
	if info, err := os.Stat(onADisk("Films/A.mkv")); err != nil || !info.ModTime().Equal(when) {
		t.Errorf("A.mkv's time on its disk: %v", err)
	}
	if onADisk("Films/A-link.mkv") != "" {
		t.Error("the link deleted through the view is still on a disk")
	}
	for _, line := range folderOnDisks("Shared") {
		if !strings.HasSuffix(line, fmt.Sprintf("%d:%d 2775", otherUID, otherUID)) {
			t.Errorf("Shared on a disk: %s, want %d:%d 2775", line, otherUID, otherUID)
		}
	}

	u.stop()
	u.checkNothingRefused()
	for _, p := range points {
		if got := mountsAt(t, p); len(got) != 0 {
			t.Errorf("mounted at %s once stopped: %v", p, got)
		}
	}
}
