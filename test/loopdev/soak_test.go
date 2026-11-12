//go:build loopdev

package loopdev

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/daemon"
	"github.com/peondusud/atoll/internal/names"
)

// How long the soak runs, and how many files each disk starts with: test/qemu/run.sh passes SOAK and SOAK_FILES.
var (
	soakFor   = flag.Duration("atoll.soak", 0, "run TestDaemonSoak for this long; 0 skips it")
	soakFiles = flag.Int("atoll.soak-files", 2000, "the files each disk of TestDaemonSoak starts with")
)

// soakGuards keeps the guards out of the writer's way, a minute's window and counts it never reaches: a soak in which
// they trip measures a daemon that holds what the writer does and decides nothing more, as the first one did.
const soakGuards = "[deletes]\ndebounce = \"1s\"\n" +
	"[deletes.mass_guard]\nfiles = 1000000000\npercent = \"100%\"\nwindow = \"1m\"\n" +
	"[modifies.mass_guard]\nfiles = 1000000000\npercent = \"100%\"\nwindow = \"1m\"\n"

// memorySample is the memory of the process at one moment, after a collection, and the size of the catalog.
type memorySample struct {
	at                time.Duration
	heap, rss, hwm    uint64
	goroutines, files int
	catalog           int64
}

// sampleMemory collects, then reads the heap in use, what the kernel says of the process, and the catalog's size
// with its write-ahead log.
func sampleMemory(t *testing.T, since time.Time, catalog string) memorySample {
	t.Helper()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	s := memorySample{at: time.Since(since).Round(time.Second), heap: ms.HeapInuse, goroutines: runtime.NumGoroutine()}
	f, err := os.Open("/proc/self/status")
	mustDo(t, err)
	defer func() { _ = f.Close() }()
	lines := bufio.NewScanner(f)
	for lines.Scan() {
		key, val, _ := strings.Cut(lines.Text(), ":")
		kb, _ := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(val), " kB"), 10, 64)
		switch key {
		case "VmRSS":
			s.rss = kb << 10
		case "VmHWM":
			s.hwm = kb << 10
		}
	}
	if entries, err := os.ReadDir("/proc/self/fd"); err == nil {
		s.files = len(entries)
	}
	for _, name := range []string{catalog, catalog + "-wal"} {
		if info, err := os.Stat(name); err == nil {
			s.catalog += info.Size()
		}
	}
	return s
}

// TestDaemonSoak: the daemon that runs for good, for -atoll.soak, on two ext4 disks, while a writer adds, edits,
// renames and deletes files every second or so. It logs the memory every tenth of the run; at the end, the heap in use
// after a collection, its open files and its goroutines must not have grown past the second tenth's by more than a
// tenth and 8 MiB, 16 files and 16 goroutines: what a daemon that runs for days would keep growing (specs.md §26).
// No pass may be held by a guard, and the last tenth must run at least half the passes of the second: a pass that
// costs more as the catalog grows would run fewer.
func TestDaemonSoak(t *testing.T) {
	needRoot(t)
	if *soakFor <= 0 {
		t.Skip("-atoll.soak is not set")
	}
	v := newVolume(t, names.Ext4)
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	window := time.Now().Add(-time.Hour).Format("15:04") + "-" + time.Now().Add(2*time.Hour+*soakFor).Format("15:04")
	b := newPassBenchGuarded(t, v, at, soakGuards, "[defaults]\nsettle = \"0s\"\n[maintenance]\nwindow = \""+window+"\"\n")
	// The same files on both disks: the first pass finds them in step, and has nothing to copy.
	for i := range *soakFiles {
		for _, top := range []string{b.keep, at} {
			write(t, top, fmt.Sprintf("set/%03d/%d.mkv", i%100, i), fmt.Sprintf("film %d", i))
		}
	}
	passes, stop := runDaemon(t, b, filepath.Join(t.TempDir(), "fstab"))
	// The first pass walks every file of both disks, which in qemu without KVM is slow.
	for started := false; !started; {
		select {
		case p := <-passes:
			started = slices.Contains(p.because, daemon.BecauseStart)
		case <-time.After(15 * time.Minute):
			t.Fatal("no first pass")
		}
	}
	var mu sync.Mutex
	count, held := 0, 0
	go func() {
		for p := range passes {
			mu.Lock()
			count++
			if p.outcome.Held {
				held++
			}
			mu.Unlock()
		}
	}()

	start := time.Now()
	// The writer is a process of its own: the watcher takes what the daemon's own process does for Atoll's own
	// operations, and would see nothing of it (specs.md §15.1). Each second it edits a file, renames another or puts
	// it back, adds a file and deletes the one it added five seconds before.
	writer := exec.CommandContext(t.Context(), "sh", "-c", soakWriter, "sh", filepath.Join(b.keep, "media"), strconv.Itoa(*soakFiles))
	mustDo(t, writer.Start())
	stopWriter := func() {
		_ = writer.Process.Kill()
		_ = writer.Wait()
	}
	defer stopWriter()
	var samples []memorySample
	var tenths []int
	every := *soakFor / 10
	for range 10 {
		time.Sleep(every)
		s := sampleMemory(t, start, b.catalog)
		mu.Lock()
		passed := count
		mu.Unlock()
		before := 0
		for _, n := range tenths {
			before += n
		}
		tenths = append(tenths, passed-before)
		t.Logf("after %v: %d passes, %d this tenth, heap in use %.1f MiB, resident %.1f MiB, at most %.1f MiB, %d goroutines, %d open files, catalog %.1f MiB",
			s.at, passed, passed-before, float64(s.heap)/(1<<20), float64(s.rss)/(1<<20), float64(s.hwm)/(1<<20), s.goroutines, s.files, float64(s.catalog)/(1<<20))
		samples = append(samples, s)
	}
	stopWriter()
	stop()
	first, last := samples[1], samples[len(samples)-1]
	heaps := make([]uint64, 0, len(samples))
	for _, s := range samples[1:] {
		heaps = append(heaps, s.heap)
	}
	t.Logf("heap in use from the second tenth on: %v bytes; the most resident %.1f MiB", heaps, float64(slices.MaxFunc(samples, func(a, b memorySample) int {
		return int(a.hwm>>20) - int(b.hwm>>20)
	}).hwm)/(1<<20))
	if grown := int64(last.heap) - int64(first.heap); grown > int64(first.heap)/10 && grown > 8<<20 {
		t.Errorf("the heap grew by %.1f MiB, from %.1f MiB", float64(grown)/(1<<20), float64(first.heap)/(1<<20))
	}
	if last.files > first.files+16 || last.goroutines > first.goroutines+16 {
		t.Errorf("open files %d → %d, goroutines %d → %d", first.files, last.files, first.goroutines, last.goroutines)
	}
	mu.Lock()
	defer mu.Unlock()
	// A soak in which the daemon did nothing, or held everything, measures nothing.
	if minutes := int(*soakFor / time.Minute); count < minutes {
		t.Errorf("%d passes in %v: the writer's changes did not reach the daemon", count, *soakFor)
	}
	if held > 0 {
		t.Errorf("a guard held %d passes of %d: the soak measured a daemon that decides nothing", held, count)
	}
	if tenths[9]*2 < tenths[1] {
		t.Errorf("the passes slowed down: %d in the second tenth, %d in the last (%v)", tenths[1], tenths[9], tenths)
	}
}

// soakWriter is the writer of TestDaemonSoak: $1 the media folder of a disk, $2 the files it holds.
const soakWriter = `media=$1 n=$2 i=0
while :; do
	k=$((i % n)); printf 'film %d, edit %d' $k $i >"$media/set/$(printf %03d $((k % 100)))/$k.mkv"
	j=$(((i / 2 * 7 + 50) % n)); f="$media/set/$(printf %03d $((j % 100)))/$j.mkv"
	if [ $((i % 2)) -eq 0 ]; then mv "$f" "$f.moved"; else mv "$f.moved" "$f"; fi
	mkdir -p "$media/new"; printf new >"$media/new/$i.mkv"; rm -f "$media/new/$((i - 5)).mkv"
	i=$((i + 1)); sleep 1
done`
