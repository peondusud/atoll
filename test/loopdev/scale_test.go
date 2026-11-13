//go:build loopdev

package loopdev

import (
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/watch"
)

var scaleFiles = flag.Int("atoll.scale", 0, "run TestScale with this many files a disk; 0 skips it")

// TestScale measures, at -atoll.scale files a disk on two disks, what grows with the pool and no other test reaches
// (specs.md §26): the first pass, which finds every copy of a catalog that knows none, as the daemon's first does; a
// scan after it; reading the hash of every copy, and a pass then, with nothing left to do; atoll status, with no strict
// setting and with a strict rule, for which it plans the whole pool to give bytes_placed; and with the link guard, the
// first pass, which makes a link for each copy, a pass after it, and the pass that removes them once the guard is off.
// Each is logged with the memory the process holds then, and the most it held during that step. In qemu without KVM
// every instruction is emulated: the times say how each grows, not what a machine takes.
func TestScale(t *testing.T) {
	needRoot(t)
	if *scaleFiles <= 0 {
		t.Skip("-atoll.scale is not set")
	}
	asTheBinary(t)
	n := *scaleFiles
	v := sizedVolume(t, n)
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	b := newPassBenchKept(t, sizedVolume(t, n), v, at, benchGuards, "")
	start := time.Now()
	for i := range n {
		for _, top := range []string{b.keep, at} {
			write(t, top, fmt.Sprintf("set/%03d/%d.mkv", i%100, i), fmt.Sprintf("film %d", i))
		}
	}
	t.Logf("%d files written on each disk in %v", n, time.Since(start).Round(time.Second))
	measure := func(what string, do func()) {
		t.Helper()
		// Each step logs its own peak.
		resetPeak(t)
		began := time.Now()
		do()
		took := time.Since(began).Round(time.Millisecond)
		s := sampleMemory(t, start, b.catalog)
		t.Logf("%s: %v; heap in use %.1f MiB, resident %.1f MiB, at most %.1f MiB, catalog %.1f MiB", what, took,
			float64(s.heap)/(1<<20), float64(s.rss)/(1<<20), float64(s.hwm)/(1<<20), float64(s.catalog)/(1<<20))
	}
	measure("the first pass, which finds every copy", func() { b.pass() })
	measure("a scan with every copy known", func() { b.scan(false) })
	measure("reading the hash of every copy", func() {
		if left, err := watch.HashNew(t.Context(), b.db, b.config, b.machine, math.MaxInt64); err != nil || left {
			t.Fatalf("%v, %v", left, err)
		}
	})
	measure("a pass with nothing to do", func() {
		if out := b.pass(); len(out.Decisions) != 0 {
			t.Errorf("%d decisions", len(out.Decisions))
		}
	})
	status := func(misplaced map[string]bool) *watch.Status {
		t.Helper()
		s, err := watch.ReadStatus(t.Context(), b.db, "", misplaced, func(*catalog.FileRow) error { return nil })
		mustDo(t, err)
		return s
	}
	measure("atoll status", func() {
		if s := status(nil); s.FilesTotal != n || s.BytesPlaced != s.BytesTotal {
			t.Errorf("%+v", s)
		}
	})
	strict := b.reconfigure("[[rule]]\nlabel = \"usb\"\npath = \"**\"\ntags = [\"usb\"]\ntags_mode = \"strict\"\n")
	measure("atoll status with a strict rule, its plan of the pool included", func() {
		misplaced, err := jobs.MisplacedFiles(t.Context(), b.db, strict, b.machine.Now())
		mustDo(t, err)
		// Every copy is the user's, and no user copy is ever misplaced: the plan is what is measured.
		if s := status(misplaced); len(misplaced) != 0 || s.BytesPlaced != s.BytesTotal {
			t.Errorf("%d misplaced, %+v", len(misplaced), s)
		}
	})
	off := b.config
	b.config = b.reconfigure("[trash]\nlink_guard = true\n")
	measure("the first pass with the link guard, which makes a link for each copy", func() { b.pass() })
	for _, top := range []string{b.keep, at} {
		links, err := os.ReadDir(filepath.Join(top, "Atoll", "links"))
		mustDo(t, err)
		if len(links) != n {
			t.Errorf("%s: %d links for %d copies", top, len(links), n)
		}
	}
	measure("a pass with the link guard", func() { b.pass() })
	b.config = off
	measure("the pass that removes the links, the guard off", func() { b.pass() })
	for _, top := range []string{b.keep, at} {
		if _, err := os.Stat(filepath.Join(top, "Atoll", "links")); !os.IsNotExist(err) {
			t.Errorf("%s: the links folder stays: %v", top, err)
		}
	}
}
