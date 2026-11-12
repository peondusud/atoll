package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/config"
)

// The watcher by inotify, which needs no privilege: what changes below the root comes, with no process, a
// folder made is watched below, and a disk stopped, then all, stop sending (specs.md §15.1). The race detector
// runs it in CI; the fanotify ways need root, and test/loopdev runs them in qemu.
func TestWatcherByInotify(t *testing.T) {
	top := t.TempDir()
	for _, dir := range []string{"media", "Atoll"} {
		if err := os.Mkdir(filepath.Join(top, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w := NewWatcher(WatcherOptions{Now: func() int64 { return time.Now().UnixNano() }, Inotify: true})
	defer w.Close()
	d := &config.Disk{ID: "t1", Mount: top, Root: "media"}
	how, err := w.Watch(t.Context(), d)
	if err != nil || how != HowInotify {
		t.Fatalf("%s, %v", how, err)
	}
	next := func(path, kind string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case e := <-w.Events():
				if e.Disk != "t1" || e.Origin != OriginUnknown || e.Path == "x" {
					t.Errorf("%+v", e)
				}
				if e.Path == path && e.Kind == kind {
					return
				}
			case <-deadline:
				t.Fatalf("no %s at %q", kind, path)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(top, "Atoll", "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(top, "media", "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	next("d", KindCreated)
	if err := os.WriteFile(filepath.Join(top, "media", "d", "a"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	next("d/a", KindWritten)
	if err := os.Rename(filepath.Join(top, "media", "d"), filepath.Join(top, "media", "e")); err != nil {
		t.Fatal(err)
	}
	next("e", KindMovedTo)
	if err := os.Remove(filepath.Join(top, "media", "e", "a")); err != nil {
		t.Fatal(err)
	}
	next("e/a", KindDeleted)

	// Stopped, the disk sends nothing more, and watching it again works.
	w.Stop("t1")
	if err := os.WriteFile(filepath.Join(top, "media", "f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-w.Events():
		t.Errorf("after Stop: %+v", e)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := w.Watch(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(top, "media", "g"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	next("g", KindWritten)
	w.Close()
	if _, err := w.Watch(t.Context(), d); err == nil {
		t.Error("a closed watcher watches")
	}
}

// A root that is not there cannot be watched.
func TestWatcherWithoutRoot(t *testing.T) {
	w := NewWatcher(WatcherOptions{Now: func() int64 { return 0 }, Inotify: true})
	defer w.Close()
	if _, err := w.Watch(t.Context(), &config.Disk{ID: "t1", Mount: t.TempDir(), Root: "media"}); err == nil {
		t.Error("watched a root that is not there")
	}
}
