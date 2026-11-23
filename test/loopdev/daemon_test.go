//go:build loopdev

package loopdev

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/daemon"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/view"
)

// passSeen is a pass the daemon told of.
type passSeen struct {
	because []string
	outcome *jobs.Outcome
}

// runDaemon runs the daemon on a bench until the test ends, and gives its passes.
func runDaemon(t *testing.T, b *passBench, fstab string) (<-chan passSeen, func()) {
	t.Helper()
	return runDaemonAfter(t, b, fstab, nil)
}

// runDaemonAfter is runDaemon that also calls after at the end of each pass, unless it is nil: in the daemon's own
// goroutine, before its next step, when what the pass held is let go.
func runDaemonAfter(t *testing.T, b *passBench, fstab string, after func()) (<-chan passSeen, func()) {
	t.Helper()
	return runDaemonEvery(t, b, fstab, 300*time.Millisecond, after)
}

// runDaemonEvery is runDaemonAfter with the time between two mount checks.
func runDaemonEvery(t *testing.T, b *passBench, fstab string, every time.Duration, after func()) (<-chan passSeen, func()) {
	t.Helper()
	// The bench's connection goes: the daemon opens the catalog for each pass, and its lock must not be shared
	// with a connection of this process.
	mustDo(t, b.db.Close())
	passes := make(chan passSeen, 64)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- daemon.Run(ctx, daemon.Options{
			Config: b.config, Machine: b.machine, Apply: true, Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
			CheckEvery: every, Fstab: fstab,
			OnPass: func(because []string, o *jobs.Outcome) {
				if after != nil {
					after()
				}
				passes <- passSeen{because, o}
			},
		})
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("the daemon stopped: %v", err)
				}
			case <-time.After(30 * time.Second):
				_ = pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
				t.Error("the daemon did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return passes, stop
}

// waitPass waits for a pass that has a reason, and gives it.
func waitPass(t *testing.T, passes <-chan passSeen, why string) passSeen {
	t.Helper()
	deadline := time.After(60 * time.Second)
	for {
		select {
		case p := <-passes:
			if slices.Contains(p.because, why) {
				return p
			}
		case <-deadline:
			t.Fatalf("no pass because %s", why)
		}
	}
}

// waitFor polls a condition for a while.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("never %s", what)
}

// viewOf mounts mergerfs over the bench's disks, from the configuration Atoll prints, and gives its mount point.
func viewOf(t *testing.T, b *passBench) string {
	t.Helper()
	line, pool := fstabLine(b)
	mountView(t, line, pool)
	return pool
}

func branchesOf(t *testing.T, pool string) []string {
	t.Helper()
	text, err := view.ReadBranches(pool)
	mustDo(t, err)
	var paths []string
	for _, b := range strings.Split(text, ":") {
		paths = append(paths, b[:strings.LastIndex(b, "=")])
	}
	return paths
}

// unmountWhenFree unmounts a disk the daemon may be reading: after a pass it reads new files' hashes inside the
// maintenance window, and a file it holds open makes umount say the target is busy, as it would to a user.
func unmountWhenFree(t *testing.T, at string) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		err := attempt(t, "umount", at)
		if err == nil {
			return
		}
		if !strings.Contains(err.Error(), "busy") || time.Now().After(deadline) {
			t.Fatal(err)
		}
	}
}

// loopbackUp brings up the loopback interface, which the qemu guest leaves down, for a test server on 127.0.0.1.
func loopbackUp(t *testing.T) {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	mustDo(t, err)
	defer func() { _ = unix.Close(fd) }()
	ifr, err := unix.NewIfreq("lo")
	mustDo(t, err)
	mustDo(t, unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr))
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	mustDo(t, unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr))
}

// ntfyTitles is a ntfy server that keeps the title of each alert it gets.
type ntfyTitles struct {
	mu     sync.Mutex
	titles []string
}

func (n *ntfyTitles) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.titles = append(n.titles, r.Header.Get("Title"))
}

func (n *ntfyTitles) got() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.titles)
}

// TestDaemonRunsForGood: the daemon passes when it starts, when a user's file appears, which it then copies, when
// a disk goes and when it comes back; the runtime branches of mergerfs follow the disk; the disk gone is an alert,
// once, forgotten when it is back; and it stops when asked (specs.md §15.13, §18, §21.1, §23). On a driver watched
// by a filesystem mark, and on one watched folder by folder.
func TestDaemonRunsForGood(t *testing.T) {
	needRoot(t)
	needMergerfs(t)
	for _, tc := range []struct {
		name   string
		fs     names.Filesystem
		driver string
	}{{"xfs", names.XFS, ""}, {"ntfs-3g", names.NTFS, "ntfs-3g"}} {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			// The maintenance window around now, for the new files' hashes; a file of solo/ has no copy to make,
			// so its hash can only come from that reading.
			window := time.Now().Add(-time.Hour).Format("15:04") + "-" + time.Now().Add(time.Hour).Format("15:04")
			loopbackUp(t)
			ntfy := &ntfyTitles{}
			srv := httptest.NewServer(ntfy)
			defer srv.Close()
			b := newPassBenchWith(t, v, at, "[defaults]\nsettle = \"0s\"\n[maintenance]\nwindow = \""+window+"\"\n[alerts]\nntfy = \""+srv.URL+"/atoll\"\n"+
				"[[rule]]\nlabel = \"solo\"\npath = \"solo/**\"\nreplicas = 0\n")
			pool := viewOf(t, b)
			keep, other := filepath.Join(b.keep, "media"), filepath.Join(at, "media")
			passes, stop := runDaemon(t, b, filepath.Join(t.TempDir(), "fstab"))

			waitPass(t, passes, daemon.BecauseStart)
			if got := branchesOf(t, pool); !slices.Equal(got, []string{keep, other}) {
				t.Errorf("branches after the start: %v", got)
			}

			// A user's file on one disk: a pass for the event, and its copy on the other.
			cmd := exec.CommandContext(t.Context(), "sh", "-c", "printf 'a film' > '"+filepath.Join(keep, "new.mkv")+"'")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			write(t, b.keep, "solo/x.mkv", "alone")
			waitPass(t, passes, daemon.BecauseEvents)
			waitFor(t, "a copy on the other disk", func() bool {
				data, err := os.ReadFile(filepath.Join(other, "new.mkv"))
				return err == nil && string(data) == "a film"
			})

			// The disk goes: a pass, and its branch out of mergerfs; back: a pass, and its branch back at its place.
			if got := ntfy.got(); len(got) != 0 {
				t.Errorf("alerts with every disk there: %q", got)
			}
			unmountWhenFree(t, at)
			waitPass(t, passes, daemon.BecauseGone)
			if got := branchesOf(t, pool); !slices.Equal(got, []string{keep}) {
				t.Errorf("branches without the disk: %v", got)
			}
			if got := ntfy.got(); !slices.Equal(got, []string{"Atoll: disk v is missing"}) {
				t.Errorf("alerts with the disk gone: %q", got)
			}
			mountVolume(t, v, tc.driver, at, options...)
			waitPass(t, passes, daemon.BecauseBack)
			if got := branchesOf(t, pool); !slices.Equal(got, []string{keep, other}) {
				t.Errorf("branches with the disk back: %v", got)
			}
			stop()

			db, err := catalog.OpenPool(t.Context(), b.config.Effective()["catalog"].(string), "p")
			mustDo(t, err)
			defer func() { _ = db.Close() }()
			var states string
			mustDo(t, db.QueryRowContext(t.Context(), "SELECT group_concat(id || ' ' || state, ', ') FROM (SELECT id, state FROM disks ORDER BY position)").Scan(&states))
			if states != "k online, v online" {
				t.Errorf("disks: %s", states)
			}
			if rows, err := catalog.ReadAlerts(t.Context(), db); err != nil || len(rows) != 0 {
				t.Errorf("alerts left with the disk back: %+v, %v", rows, err)
			}
			if got := ntfy.got(); len(got) != 1 {
				t.Errorf("alerts in all: %q", got)
			}
			var hashed string
			mustDo(t, db.QueryRowContext(t.Context(), "SELECT (f.blake3 IS NOT NULL) || ' ' || (c.verified_ns IS NOT NULL) FROM files f JOIN copies c ON c.file_id = f.id WHERE CAST(f.lpath AS TEXT) = 'solo/x.mkv'").Scan(&hashed))
			if hashed != "1 1" {
				t.Errorf("solo/x.mkv: hash and verified %s, want both", hashed)
			}
		})
	}
}

// TestDaemonRemountsAfterAReset: a disk whose device goes away under its mount, as a USB reset does, and comes
// back under another name, is remounted through its fstab entry, then walked, and its branch comes back
// (specs.md §10, §15.13). On ext4, and on ntfs-3g, whose mount is a FUSE process of its own that mount starts again
// through mount.ntfs-3g. The daemon runs as root here: as the dedicated user of deploy/atoll.service, it is not run.
func TestDaemonRemountsAfterAReset(t *testing.T) {
	needRoot(t)
	needMergerfs(t)
	for _, tc := range []struct {
		name, driver, fstype string
		fs                   names.Filesystem
	}{{"ext4", "", "ext4", names.Ext4}, {"ntfs-3g", "ntfs-3g", "ntfs-3g", names.NTFS}} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMapped(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, m.volume, tc.driver, at)
			renamed := m.name + "-again"
			t.Cleanup(func() {
				_ = exec.CommandContext(context.Background(), "umount", "-l", at).Run()
				time.Sleep(200 * time.Millisecond)
				_ = exec.CommandContext(context.Background(), "dmsetup", "remove", "-f", "--noudevsync", renamed).Run()
			})
			b := newPassBench(t, m.volume, at)
			pool := viewOf(t, b)
			fstab := filepath.Join(t.TempDir(), "fstab")
			mustDo(t, os.WriteFile(fstab, []byte("/dev/mapper/"+renamed+" "+at+" "+tc.fstype+" defaults 0 0\n"), 0o644))
			passes, _ := runDaemon(t, b, fstab)
			waitPass(t, passes, daemon.BecauseStart)

			// The device goes from under the mount and comes back under another name.
			must(t, "dmsetup", "rename", m.name, renamed)
			must(t, "dmsetup", "mknodes")
			waitPass(t, passes, daemon.BecauseGone)
			back := waitPass(t, passes, daemon.BecauseBack)
			walked := false
			for _, line := range back.outcome.Disks {
				walked = walked || line.ID == "v" && line.Walked
			}
			if !walked {
				t.Errorf("not walked once back: %+v", back.outcome.Disks)
			}
			if source := must(t, "findmnt", "-n", "-o", "SOURCE", at); source != "/dev/mapper/"+renamed {
				t.Errorf("mounted from %s", source)
			}
			if got := branchesOf(t, pool); !slices.Equal(got, []string{filepath.Join(b.keep, "media"), filepath.Join(at, "media")}) {
				t.Errorf("branches once back: %v", got)
			}
		})
	}
}

// TestTheDaemonsRemountUnderTheViewOnEachFUSEDriver: the daemon's remount after a reset (umount -l, then mount from the
// fstab entry; specs.md §10), while atoll mount runs in a process of its own, holding its handles on the disk, with a
// deletion it left planned there (§21.2), on ntfs-3g and lowntfs-3g. Two resets: the device goes from under the mount
// and comes back under another name, its numbers the same, as in TestDaemonRemountsAfterAReset; and the FUSE process
// killed, the device staying, as a crash of it leaves the mount (ENOTCONN). A mount of a device whose superblock is
// alive gets that superblock, and the view's handles keep the old mount, unmounted lazily, alive until its next check
// (docs/research/ntfs-driver.md, 2026-11-23). So, renamed: the new process finds its FUSE device bound to no connection
// and exits, and the old one, alive, serves the new mount. Killed: the same leaves at the path a mount no process
// serves, until the view's check, finding the disk lost, lets go of its handles; the daemon's next check, the disk still
// missing, unmounts that and mounts again, a superblock of its own, a new process serving it. Either way the disk is
// back, and the view's check, finding it on a mount of its own and confirmed, resolves the deletion: nothing moved, so it
// is undone, the copy at its name.
func TestTheDaemonsRemountUnderTheViewOnEachFUSEDriver(t *testing.T) {
	needRoot(t)
	for _, driver := range []string{"ntfs-3g", "lowntfs-3g"} {
		for _, reset := range []string{"renamed", "killed"} {
			t.Run(driver+"/"+reset, func(t *testing.T) {
				// Where mount(8) looks for the helper of an fstab entry's type.
				if _, err := os.Stat("/sbin/mount." + driver); err != nil {
					t.Skipf("no mount.%s: %v", driver, err)
				}
				m := newMapped(t, names.NTFS)
				at := mountPoint(t)
				mountVolume(t, m.volume, driver, at)
				source := m.device
				if reset == "renamed" {
					source = "/dev/mapper/" + m.name + "-again"
					t.Cleanup(func() {
						_ = exec.CommandContext(context.Background(), "umount", "-l", at).Run()
						time.Sleep(200 * time.Millisecond)
						_ = exec.CommandContext(context.Background(), "dmsetup", "remove", "-f", "--noudevsync", m.name+"-again").Run()
					})
				}
				b := viewPool(t, m.volume, at)
				fstab := filepath.Join(t.TempDir(), "fstab")
				mustDo(t, os.WriteFile(fstab, []byte(source+" "+at+" "+driver+" defaults 0 0\n"), 0o644))
				point := filepath.Join(t.TempDir(), "view")
				mustDo(t, os.Mkdir(point, 0o755))
				view := startViewWith(t, b, point, nil, childWritesEnv+"=1", childEveryEnv+"=3s", childFailMoveEnv+"="+filepath.Join(at, "a.mkv"))
				view.mounted(t)
				if err := os.Remove(filepath.Join(point, "Films", "a.mkv")); err == nil {
					t.Fatalf("the deletion went through a disk that failed:\n%s", view.said())
				}
				if got := b.viewOps(); got != "unlink planned" {
					t.Fatalf("the view's journal once the disk failed: %s\n%s", got, view.said())
				}

				passes, stop := runDaemon(t, b, fstab)
				waitPass(t, passes, daemon.BecauseStart)
				before := fusePids(driver, m.device)
				switch reset {
				case "renamed":
					must(t, "dmsetup", "rename", m.name, m.name+"-again")
					must(t, "dmsetup", "mknodes")
				case "killed":
					for _, pid := range before {
						mustDo(t, syscall.Kill(pid, syscall.SIGKILL))
					}
				}
				waitPass(t, passes, daemon.BecauseGone)
				waitPass(t, passes, daemon.BecauseBack)
				stop()
				// The old process names the device as it was mounted, a new one as the fstab entry names it.
				after := fusePids(driver, m.device)
				if source != m.device {
					after = append(after, fusePids(driver, source)...)
				}
				t.Logf("back: %s; %s's processes %v before the reset, %v once back", findmnt(at), driver, before, after)
				if len(after) == 0 {
					t.Errorf("no %s process serves the disk once back", driver)
				}
				if reset == "killed" && slices.ContainsFunc(after, func(pid int) bool { return slices.Contains(before, pid) }) {
					t.Errorf("a process killed serves the disk: %v", after)
				}
				if data, err := os.ReadFile(filepath.Join(at, "media", "Films", "a.mkv")); err != nil || string(data) != "a film" {
					t.Errorf("the copy once back: %q, %v", data, err)
				}

				// The daemon gone, the bench reads the catalog again, and the view resolves the deletion.
				db, err := catalog.OpenPool(t.Context(), b.catalog, "p")
				mustDo(t, err)
				t.Cleanup(func() { _ = db.Close() })
				b.db = db
				waitFor(t, "the deletion resolved by the view's check", func() bool { return !strings.Contains(b.viewOps(), "planned") })
				if got := b.viewOps(); got != "unlink undone" {
					t.Errorf("the view's journal once the disk is back: %s\n%s", got, view.said())
				}
				if got := readOr(filepath.Join(at, "media", "Films", "a.mkv")); got != "a film" {
					t.Errorf("v holds %q at the copy's name", got)
				}
				if got := trashFiles(t, at); len(got) != 0 {
					t.Errorf("the trash of v holds %v", got)
				}
				if got := b.text("SELECT count(*) FROM ops WHERE state = 'planned'"); got != "0" {
					t.Errorf("%s operations left planned", got)
				}
			})
		}
	}
}

// unitCapabilities are those deploy/atoll.service gives the dedicated user, and no others.
const unitCapabilities = "-all,+sys_admin,+dac_read_search,+dac_override,+chown,+fowner,+fsetid,+sys_rawio"

// asTheUnit runs a command as the daemon's dedicated user runs it under deploy/atoll.service: another account than
// root, with no supplementary group, the unit's capabilities alone, ambient, and no new privileges, so that a setuid
// program gains nothing. It gives what the command wrote, and its error.
func asTheUnit(t *testing.T, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "setpriv", append([]string{"--reuid=990", "--regid=990", "--clear-groups", "--no-new-privs",
		"--inh-caps=" + unitCapabilities, "--ambient-caps=" + unitCapabilities, "--bounding-set=" + unitCapabilities, "--", name}, args...)...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err //nolint:wrapcheck // what the command said is the test's to report
}

// TestRemountAsTheDedicatedUser: the remount after a USB reset, umount -l then mount through the fstab entry
// (specs.md §15.13), works as the daemon runs it, as its dedicated user with the unit's capabilities alone, on ext4 and
// on ntfs-3g, whose mount.ntfs-3g starts a FUSE process (deploy/README.md).
func TestRemountAsTheDedicatedUser(t *testing.T) {
	needRoot(t)
	if _, err := exec.LookPath("setpriv"); err != nil {
		t.Skip("no setpriv")
	}
	original, err := os.ReadFile("/etc/fstab")
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile("/etc/fstab", original, 0o644) })
	// The account, as the package's postinst would make it: ntfs-3g asks for a user's name.
	passwd, err := os.ReadFile("/etc/passwd")
	mustDo(t, err)
	t.Cleanup(func() { _ = os.WriteFile("/etc/passwd", passwd, 0o644) })
	mustDo(t, os.WriteFile("/etc/passwd", append(slices.Clone(passwd), []byte("atoll:x:990:990::/nonexistent:/usr/sbin/nologin\n")...), 0o644))
	for _, tc := range []struct {
		name, driver, fstype, seen string
		fs                         names.Filesystem
	}{{"ext4", "", "ext4", "ext4", names.Ext4}, {"ntfs-3g", "ntfs-3g", "ntfs-3g", "fuseblk", names.NTFS}} {
		t.Run(tc.name, func(t *testing.T) {
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			// The folders above the mount point open to every account, as /mnt or /srv is: a test's own are not.
			for dir := filepath.Dir(at); dir != "/" && dir != "."; dir = filepath.Dir(dir) {
				mustDo(t, os.Chmod(dir, 0o755))
			}
			mountVolume(t, v, tc.driver, at)
			write(t, at, "a", "a")
			mustDo(t, os.WriteFile("/etc/fstab", append(slices.Clone(original), []byte(v.device+" "+at+" "+tc.fstype+" defaults 0 0\n")...), 0o644))
			if out, err := asTheUnit(t, "umount", "-l", at); err != nil {
				t.Fatalf("umount -l as the dedicated user: %v: %s", err, out)
			}
			if out, _ := exec.CommandContext(t.Context(), "findmnt", "-n", at).CombinedOutput(); len(out) != 0 {
				t.Fatalf("still mounted: %s", out)
			}
			out, err := asTheUnit(t, "mount", at)
			if err != nil && tc.driver == "ntfs-3g" {
				t.Logf("mount as the dedicated user, its mount point root's: %v: %s", err, out)
				mustDo(t, os.Chown(at, 990, 990))
				out, err = asTheUnit(t, "mount", at)
			}
			if err != nil {
				t.Fatalf("mount as the dedicated user: %v: %s", err, out)
			}
			if got := must(t, "findmnt", "-n", "-o", "SOURCE,FSTYPE", at); strings.Join(strings.Fields(got), " ") != v.device+" "+tc.seen {
				t.Errorf("mounted as %q", got)
			}
			if data, err := os.ReadFile(filepath.Join(at, "media", "a")); err != nil || string(data) != "a" {
				t.Errorf("the disk's content once mounted again: %q, %v", data, err)
			}
			// What a media server reads each disk as: another account, with no capability at all.
			t.Logf("mounted again with %s", must(t, "findmnt", "-n", "-o", "OPTIONS", at))
			other := exec.CommandContext(t.Context(), "setpriv", "--reuid=991", "--regid=991", "--clear-groups", "--no-new-privs",
				"--inh-caps=-all", "--bounding-set=-all", "--", "cat", filepath.Join(at, "media", "a"))
			if out, err := other.CombinedOutput(); err != nil || string(out) != "a" {
				t.Errorf("another account reads %q, %v", out, err)
			}
		})
	}
}

// TestAnNTFSDiskMountedAgainByAnotherAccount: an ntfs-3g disk mounted again by the daemon's dedicated user, as the
// remount after a USB reset does (specs.md §15.13), shows every file as that account's: with no user mapping, the
// owner is the mount's, not the file's. A pass after it must not take that for a chown of every file and apply it to
// the copies of the other disks (§15.4), nor change anything else of them.
func TestAnNTFSDiskMountedAgainByAnotherAccount(t *testing.T) {
	needRoot(t)
	if _, err := exec.LookPath("setpriv"); err != nil {
		t.Skip("no setpriv")
	}
	passwd, err := os.ReadFile("/etc/passwd")
	mustDo(t, err)
	t.Cleanup(func() { _ = os.WriteFile("/etc/passwd", passwd, 0o644) })
	mustDo(t, os.WriteFile("/etc/passwd", append(slices.Clone(passwd), []byte("atoll:x:990:990::/nonexistent:/usr/sbin/nologin\n")...), 0o644))
	original, err := os.ReadFile("/etc/fstab")
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile("/etc/fstab", original, 0o644) })

	v := newVolume(t, names.NTFS)
	at := mountPoint(t)
	mountVolume(t, v, "ntfs-3g", at)
	// A change is decided as soon as it is seen.
	b := newPassBenchWith(t, v, at, "[defaults]\nsettle = \"0s\"\n")
	for _, top := range []string{b.keep, at} {
		write(t, top, "d/a.mkv", "film")
	}
	b.settle()
	kept := filepath.Join(b.keep, "media", "d", "a.mkv")
	before, err := os.Stat(kept)
	mustDo(t, err)

	// Mounted again by the dedicated user, its mount point given to it, as a remount needs.
	mustDo(t, os.WriteFile("/etc/fstab", append(slices.Clone(original), []byte(v.device+" "+at+" ntfs-3g defaults 0 0\n")...), 0o644))
	unmountWhenFree(t, at)
	mustDo(t, os.Chown(at, 990, 990))
	if out, err := asTheUnit(t, "mount", at); err != nil {
		t.Fatalf("mount as the dedicated user: %v: %s", err, out)
	}
	ntfsCopy := filepath.Join(at, "media", "d", "a.mkv")
	if info, err := os.Stat(ntfsCopy); err != nil {
		t.Fatal(err)
	} else if uid := info.Sys().(*syscall.Stat_t).Uid; uid != 990 {
		t.Fatalf("the copy reads as uid %d once mounted again, which this test is about", uid)
	}
	out := b.settle()
	after, err := os.Stat(kept)
	mustDo(t, err)
	was, now := before.Sys().(*syscall.Stat_t), after.Sys().(*syscall.Stat_t)
	if now.Uid != was.Uid || now.Gid != was.Gid || after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("the other disk's copy went from %d:%d %v %v to %d:%d %v %v", was.Uid, was.Gid, before.Mode(), before.ModTime(), now.Uid, now.Gid, after.Mode(), after.ModTime())
	}
	for _, op := range out.Operations {
		t.Errorf("an operation once mounted again: %+v", op)
	}

	// Then the copy's date changes on that disk, as Radarr's "change file date" does: the date reaches the other copy,
	// and the owner the mount gave every file does not.
	when := time.Unix(1600000000, 0)
	mustDo(t, os.Chtimes(ntfsCopy, when, when))
	b.settle()
	after, err = os.Stat(kept)
	mustDo(t, err)
	now = after.Sys().(*syscall.Stat_t)
	if !after.ModTime().Equal(when) {
		t.Errorf("the date did not reach the other copy: %v", after.ModTime())
	}
	if now.Uid != was.Uid || now.Gid != was.Gid {
		t.Errorf("the other disk's copy took the owner %d:%d, which only the mount gave, for %d:%d", now.Uid, now.Gid, was.Uid, was.Gid)
	}
}

// TestDaemonHearsADiskMountedAgain: a disk unmounted and mounted again between two mount checks, which find it
// mounted both times, is watched again at the next check and looked at whole. A file put on it while nobody listened
// gets its copy, and one put after is heard (specs.md §15.1, §15.13). The kernel's marks went with the filesystem
// that was unmounted, without a word: the daemon kept the disk for watched, and heard nothing more of it until it
// started again. On a driver watched by a filesystem mark, and on one watched folder by folder.
func TestDaemonHearsADiskMountedAgain(t *testing.T) {
	needRoot(t)
	for _, tc := range []struct {
		name   string
		fs     names.Filesystem
		driver string
	}{{"xfs", names.XFS, ""}, {"ntfs-3g", names.NTFS, "ntfs-3g"}} {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newPassBenchWith(t, v, at, "[defaults]\nsettle = \"0s\"\n")
			keep := filepath.Join(b.keep, "media")
			// Checks far enough apart for the disk to go and come back between two. Should one fall in between, the
			// daemon sees the disk go and come back, which watches it again too.
			passes, _ := runDaemonEvery(t, b, filepath.Join(t.TempDir(), "fstab"), 5*time.Second, nil)
			waitPass(t, passes, daemon.BecauseStart)
			copied := func(name, text string) func() bool {
				return func() bool {
					data, err := os.ReadFile(filepath.Join(keep, name))
					return err == nil && string(data) == text
				}
			}

			unmountWhenFree(t, at)
			mountVolume(t, v, tc.driver, at, options...)
			// Put while nobody listens: only a look at the whole disk finds it.
			write(t, at, "unheard.mkv", "a film")
			waitFor(t, "a copy of the file put while nobody listened", copied("unheard.mkv", "a film"))

			// Once nothing moves any more, a file put on the disk is heard: a pass comes for its event.
			for quiet := false; !quiet; {
				select {
				case <-passes:
				case <-time.After(3 * time.Second):
					quiet = true
				}
			}
			// By another process: what this one writes is Atoll's own to the watcher, and brings no pass.
			cmd := exec.CommandContext(t.Context(), "sh", "-c", "printf 'another' > '"+filepath.Join(at, "media", "heard.mkv")+"'")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			waitPass(t, passes, daemon.BecauseEvents)
			waitFor(t, "a copy of the file put once the disk is watched again", copied("heard.mkv", "another"))
		})
	}
}
