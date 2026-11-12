//go:build loopdev

package loopdev

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/watch"
)

// TestWatcherOnEachDriver: on each driver, by each way it can be watched, what a user's process, the mergerfs
// process of the view and Atoll itself do below the root comes as events, each with its path, what it did and
// who did it; a folder moved in is watched below as soon as it arrives, and a folder renamed carries its path;
// nothing outside the root comes (specs.md §15.1, docs/research/watcher.md).
func TestWatcherOnEachDriver(t *testing.T) {
	needRoot(t)
	needMergerfs(t)
	for _, inotify := range []bool{false, true} {
		for _, tc := range trashDrivers {
			name := tc.name
			if inotify {
				name += "/inotify"
			}
			t.Run(name, func(t *testing.T) {
				options := driverOptions(t, tc.driver)
				v := newVolume(t, tc.fs)
				at := mountPoint(t)
				mountVolume(t, v, tc.driver, at, options...)
				for _, dir := range []string{"media", "Atoll/tmp", "outside/Show/S01"} {
					mustDo(t, os.MkdirAll(filepath.Join(at, dir), 0o755))
				}
				pool := filepath.Join(t.TempDir(), "pool")
				mustDo(t, os.MkdirAll(pool, 0o755))
				must(t, "mergerfs", "-o", "category.search=ff,category.action=epall,category.create=mfs,minfreespace=1M", filepath.Join(at, "media")+"=RW", pool)
				t.Cleanup(func() { _ = exec.CommandContext(context.Background(), "umount", pool).Run() })

				w := watch.NewWatcher(watch.WatcherOptions{ViewMount: pool, Now: func() int64 { return time.Now().UnixNano() }, Inotify: inotify})
				defer w.Close()
				how, err := w.Watch(t.Context(), diskOf(v, at))
				if err != nil {
					t.Fatal(err)
				}
				wantHow := watch.HowFilesystem
				switch {
				case inotify:
					wantHow = watch.HowInotify
				case tc.driver == "ntfs-3g" || tc.driver == "lowntfs-3g":
					wantHow = watch.HowFolders
				}
				if how != wantHow {
					t.Errorf("watched by %s, want %s", how, wantHow)
				}
				user, view, self := watch.OriginUser, watch.OriginView, watch.OriginAtoll
				if inotify {
					user, view, self = watch.OriginUnknown, watch.OriginUnknown, watch.OriginUnknown
				}
				r := &recorder{t: t, events: w.Events(), at: at}
				// Made at once with its folder: a watch per folder may see it only by listing the folder, with no process,
				// and then as created, its write over before the folder's mark.
				r.step("mkdir -p media/Films && printf a > media/Films/a.mkv", "", "", "")
				r.wait("Films/a.mkv", watch.KindCreated, user, watch.OriginUnknown)
				r.expect("Films", watch.KindCreated, true, user)
				r.step("mv outside/Show media/Show", "Show", watch.KindMovedTo, user)
				r.step("printf e > media/Show/S01/e01.mkv", "Show/S01/e01.mkv", watch.KindWritten, user)
				r.step("mv media/Show 'media/Show (2020)'", "Show (2020)", watch.KindMovedTo, user)
				r.expect("Show", watch.KindMovedFrom, true, user)
				r.step("printf e > 'media/Show (2020)/S01/e02.mkv'", "Show (2020)/S01/e02.mkv", watch.KindWritten, user)
				r.step("chmod 600 'media/Show (2020)/S01/e02.mkv'", "Show (2020)/S01/e02.mkv", watch.KindAttrib, user)
				r.step("rm media/Films/a.mkv", "Films/a.mkv", watch.KindDeleted, user)
				r.step("printf v > "+pool+"/v.mkv", "v.mkv", watch.KindWritten, view)
				r.step("mv "+pool+"/v.mkv "+pool+"/w.mkv", "w.mkv", watch.KindMovedTo, view)
				r.expect("v.mkv", watch.KindMovedFrom, false, view)
				r.step("rm "+pool+"/w.mkv", "w.mkv", watch.KindDeleted, view)
				mustDo(t, os.WriteFile(filepath.Join(at, "media", "self.mkv"), []byte("s"), 0o644))
				r.wait("self.mkv", watch.KindWritten, self)
				// Elsewhere on the disk, then below the root again, so that the first has had its time to come.
				r.step("printf x > Atoll/tmp/x && printf y > outside/y", "", "", "")
				r.step("printf z > media/z", "z", watch.KindWritten, user)
				for _, e := range r.all {
					if e.Kind == watch.KindOverflow || strings.Contains(e.Path, "tmp") || e.Path == "x" || e.Path == "y" || e.Path == "outside" {
						t.Errorf("an event from outside the root: %+v", e)
					}
				}
			})
		}
	}
}

// recorder reads a watcher's events and keeps them all.
type recorder struct {
	t      *testing.T
	events <-chan watch.Event
	at     string
	all    []watch.Event
}

// step runs a shell command at the top of the disk, as a user's process, and waits for an event with that path
// and kind; an empty kind waits for none.
func (r *recorder) step(script, path, kind, origin string) {
	r.t.Helper()
	cmd := exec.CommandContext(r.t.Context(), "sh", "-c", script)
	cmd.Dir = r.at
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("%s: %v: %s", script, err, out)
	}
	if kind != "" {
		r.wait(path, kind, origin)
	}
}

// wait reads events until one has that path and kind, and checks who made it: one of origins.
func (r *recorder) wait(path, kind string, origins ...string) {
	r.t.Helper()
	for _, e := range r.all {
		if e.Path == path && e.Kind == kind {
			if !slices.Contains(origins, e.Origin) {
				r.t.Errorf("%s %s by %s, want %v", kind, path, e.Origin, origins)
			}
			return
		}
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case e := <-r.events:
			r.all = append(r.all, e)
			if e.Path == path && e.Kind == kind {
				if !slices.Contains(origins, e.Origin) {
					r.t.Errorf("%s %s by %s, want %v", kind, path, e.Origin, origins)
				}
				return
			}
		case <-deadline:
			r.t.Fatalf("no %s at %q; came: %s", kind, path, eventsText(r.all))
		}
	}
}

// expect fails when no event came with that path and kind, by one of the origins, the first unless another
// came.
func (r *recorder) expect(path, kind string, folder bool, origins ...string) {
	r.t.Helper()
	for _, e := range r.all {
		if e.Path == path && e.Kind == kind && (!folder || e.Folder) && slices.Contains(origins, e.Origin) {
			return
		}
	}
	r.t.Errorf("no %s at %q (folder %v) by %v; came: %s", kind, path, folder, origins, eventsText(r.all))
}

func eventsText(list []watch.Event) string {
	var parts []string
	for _, e := range list {
		parts = append(parts, fmt.Sprintf("%s %q %s", e.Kind, e.Path, e.Origin))
	}
	return strings.Join(parts, "; ")
}
