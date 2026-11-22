package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/watch"
)

// The daemon looks at the names its watcher heard, and walks no disk whole for them (specs.md §15.13): a file written on
// one disk is recorded by a look at its name, the other disk spared; a file deleted there is gone by the event that said
// so, and its copy on the other disk goes to the trash once due, by a look at its name again.
func TestTheDaemonLooksAtTheNamesItHeard(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "catalog.db")
	text := "pool_id = \"p\"\ncatalog = " + strconv.Quote(file) + "\n[view]\nmount = " + strconv.Quote(filepath.Join(dir, "pool")) +
		"\n[defaults]\nsettle = \"0s\"\n[deletes]\ndebounce = \"1s\"\n[deletes.mass_guard]\npercent = \"100%\"\n"
	for i, id := range []string{"t1", "t2"} {
		mount := filepath.Join(dir, id)
		if err := os.MkdirAll(filepath.Join(mount, "media", "Films"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"a", "d"} {
			if err := os.WriteFile(filepath.Join(mount, "media", "Films", name), []byte("film "+name), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		text += "[[disk]]\nid = " + strconv.Quote(id) + "\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3" + strconv.Itoa(i) + "\"\nmount = " + strconv.Quote(mount) + "\nfs = \"ext4\"\n"
	}
	c, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	real, err := disks.NewMachine()
	if err != nil {
		t.Fatal(err)
	}
	m := &folderDisks{Machine: real, list: c.Disks}
	db, err := catalog.OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := watch.Scan(t.Context(), db, c, m, watch.Options{Full: true}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var lines [][]jobs.DiskLine
	passes := make(chan []string, 64)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Config: c, Machine: m, Apply: true, Log: slog.New(slog.DiscardHandler), CheckEvery: 50 * time.Millisecond, Inotify: true,
			OnPass: func(because []string, o *jobs.Outcome) {
				mu.Lock()
				lines = append(lines, slices.Clone(o.Disks))
				mu.Unlock()
				passes <- because
			}})
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("stopped: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("did not stop")
		}
	}()
	select {
	case <-passes:
	case <-time.After(20 * time.Second):
		t.Fatal("no first pass")
	}
	// The pass that starts walks every disk; every pass after it looks at names alone.
	mu.Lock()
	started := len(lines)
	mu.Unlock()
	if err := os.WriteFile(filepath.Join(dir, "t1", "media", "Films", "b"), []byte("another film"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "b recorded", func() bool { return hasFile(t, file, "Films/b") })
	if err := os.Remove(filepath.Join(dir, "t1", "media", "Films", "a")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a's copy on t2 in the trash", func() bool {
		_, err := os.Stat(filepath.Join(dir, "t2", "media", "Films", "a"))
		return os.IsNotExist(err)
	})
	// A rename: found by the look at both names, decided once due by a look at the new name its pass left to see again.
	if err := os.Rename(filepath.Join(dir, "t1", "media", "Films", "d"), filepath.Join(dir, "t1", "media", "Films", "e")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "d renamed e on t2", func() bool {
		_, err := os.Stat(filepath.Join(dir, "t2", "media", "Films", "e"))
		return err == nil
	})
	mu.Lock()
	defer mu.Unlock()
	for _, pass := range lines[started:] {
		for _, line := range pass {
			if line.Walked && !line.Partial {
				t.Errorf("%s walked whole after the start: %+v", line.ID, pass)
			}
		}
	}
}

func hasFile(t *testing.T, file, path string) bool {
	t.Helper()
	db, err := catalog.OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM files WHERE lpath = ?", []byte(path)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 20 s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
