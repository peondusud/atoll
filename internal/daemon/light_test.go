package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/names"
)

// ReadPastCache confirms a folder disk as its mount check does: its identifier is the one the configuration gives.
func (m *folderDisks) ReadPastCache(fs names.Filesystem, entry *disks.MountEntry) disks.DeviceSeen {
	return m.Device(fs, entry)
}

// lightBench gives a configuration of two disks that are folders, the first holding a file, and a machine whose mount
// checks and confirmations pass on them, and that walks and writes them for real.
func lightBench(t *testing.T) (*config.Config, *folderDisks) {
	t.Helper()
	top := t.TempDir()
	file := filepath.Join(top, "catalog.db")
	text := "pool_id = \"p\"\ncatalog = " + strconv.Quote(file) + "\n[view]\nmount = " + strconv.Quote(filepath.Join(top, "pool")) + "\n" +
		"[deletes]\ndebounce = \"1s\"\n"
	for i, id := range []string{"t1", "t2"} {
		mount := filepath.Join(top, id)
		if err := os.MkdirAll(filepath.Join(mount, "media"), 0o755); err != nil {
			t.Fatal(err)
		}
		text += "[[disk]]\nid = \"" + id + "\"\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3" + strconv.Itoa(i) + "\"\nmount = " + strconv.Quote(mount) + "\nfs = \"ext4\"\n"
	}
	if err := os.WriteFile(filepath.Join(top, "t1", "media", "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
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
	return c, &folderDisks{Machine: real, list: c.Disks}
}

type lightPass struct {
	because []string
	lines   []jobs.DiskLine
}

// runLight runs the daemon on a bench until the test ends, and gives its passes.
func runLight(t *testing.T, c *config.Config, m disks.Machine) <-chan lightPass {
	t.Helper()
	passes := make(chan lightPass, 64)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Config: c, Machine: m, Apply: true, Log: slog.New(slog.DiscardHandler), CheckEvery: 100 * time.Millisecond, Inotify: true,
			OnPass: func(because []string, o *jobs.Outcome) {
				passes <- lightPass{because, slices.Clone(o.Disks)}
			}})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the daemon stopped: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("the daemon did not stop")
		}
	})
	return passes
}

func waitLight(t *testing.T, passes <-chan lightPass, why string) lightPass {
	t.Helper()
	deadline := time.After(30 * time.Second)
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

func lineIn(lines []jobs.DiskLine, id string) jobs.DiskLine {
	at := slices.IndexFunc(lines, func(l jobs.DiskLine) bool { return l.ID == id })
	if at < 0 {
		return jobs.DiskLine{}
	}
	return lines[at]
}

// The first pass walks every disk. Then a change on one disk, which its watch hears, calls for a pass that walks that
// disk alone: the other, watched all along and heard of nothing, is spared, so that it may sleep on (specs.md §15.13).
func TestAPassForEventsWalksOnlyTheDisksThatChanged(t *testing.T) {
	c, m := lightBench(t)
	passes := runLight(t, c, m)
	first := waitLight(t, passes, BecauseStart)
	if !lineIn(first.lines, "t1").Walked || !lineIn(first.lines, "t2").Walked {
		t.Fatalf("the first pass: %+v", first.lines)
	}
	if err := os.WriteFile(filepath.Join(c.Disks[1].Mount, "media", "b"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := waitLight(t, passes, BecauseEvents)
	one, two := lineIn(p.lines, "t1"), lineIn(p.lines, "t2")
	if !two.Walked || one.Walked || !one.Spared || one.Verdict.State != disks.Online {
		t.Fatalf("the pass for events (%v): %+v", p.because, p.lines)
	}
}
