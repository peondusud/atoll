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
	// The bench's connection goes: the daemon opens the catalog for each pass, and its lock must not be shared
	// with a connection of this process.
	mustDo(t, b.db.Close())
	passes := make(chan passSeen, 64)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- daemon.Run(ctx, daemon.Options{
			Config: b.config, Machine: b.machine, Apply: true, Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
			CheckEvery: 300 * time.Millisecond, Fstab: fstab,
			OnPass: func(because []string, o *jobs.Outcome) { passes <- passSeen{because, o} },
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
// (specs.md §10, §15.13).
func TestDaemonRemountsAfterAReset(t *testing.T) {
	needRoot(t)
	needMergerfs(t)
	m := newMapped(t, names.Ext4)
	at := mountPoint(t)
	mountVolume(t, m.volume, "", at)
	renamed := m.name + "-again"
	t.Cleanup(func() {
		_ = exec.CommandContext(context.Background(), "umount", "-l", at).Run()
		_ = exec.CommandContext(context.Background(), "dmsetup", "remove", "-f", "--noudevsync", renamed).Run()
	})
	b := newPassBench(t, m.volume, at)
	pool := viewOf(t, b)
	fstab := filepath.Join(t.TempDir(), "fstab")
	mustDo(t, os.WriteFile(fstab, []byte("/dev/mapper/"+renamed+" "+at+" ext4 defaults 0 0\n"), 0o644))
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
}
