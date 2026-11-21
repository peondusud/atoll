//go:build loopdev

package loopdev

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"modernc.org/libc"

	"github.com/peondusud/atoll/internal/daemon"
	"github.com/peondusud/atoll/internal/names"
)

// How long the soak runs, and how many files each disk starts with: test/qemu/run.sh passes SOAK and SOAK_FILES.
var (
	soakFor   = flag.Duration("atoll.soak", 0, "run TestDaemonSoak for this long; 0 skips it")
	soakFiles = flag.Int("atoll.soak-files", 2000, "the files each disk of TestDaemonSoak starts with")
)

// sizedVolume makes an ext4 volume for files of a few bytes: 64 MiB holds the soak's default 2,000; past that, blocks of
// 1 KiB and an inode for each file, its folders and the trash's turnover, as mkfs would give a small volume too few.
func sizedVolume(t *testing.T, files int) *volume {
	t.Helper()
	if files <= 2000 {
		return newVolume(t, names.Ext4)
	}
	inodes := files*6/5 + 50000
	// 2 KiB a file, a block and its share of the folders, and 256 bytes an inode, past what a small volume needs.
	mb := int64(64 + (files*2+inodes/4)/1024)
	return newVolumeSized(t, names.Ext4, mb, "-b", "1024", "-N", strconv.Itoa(inodes))
}

// daemonMemoryLimit is the soft memory limit cmd/atoll sets unless GOMEMLIMIT says otherwise (specs.md §26). The soak
// and TestScale run the daemon in the test's own process, which must have it too to measure what the daemon holds:
// without it, the collector lets the heap grow far past it between collections.
const daemonMemoryLimit = 155 << 20

// asTheBinary gives the test's process the memory limit of the daemon's, until the test ends, and no transparent huge
// pages, as cmd/atoll has none, for good: the flag cannot be undone without the process meeting huge pages again.
func asTheBinary(t *testing.T) {
	t.Helper()
	if err := unix.Prctl(unix.PR_SET_THP_DISABLE, 1, 0, 0, 0); err != nil {
		t.Fatalf("transparent huge pages: %v", err)
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		before := debug.SetMemoryLimit(daemonMemoryLimit)
		t.Cleanup(func() { debug.SetMemoryLimit(before) })
	}
}

// firstPassWithin is how long the first pass of a run with files a disk may take: it walks every file of both disks,
// which in qemu without KVM is slow.
func firstPassWithin(files int) time.Duration {
	return 15*time.Minute + time.Duration(files)*10*time.Millisecond
}

// soakGuards keeps the guards out of the writer's way, a minute's window and counts it never reaches: a soak in which
// they trip measures a daemon that holds what the writer does and decides nothing more, as the first one did.
const soakGuards = "[deletes]\ndebounce = \"1s\"\n" +
	"[deletes.mass_guard]\nfiles = 1000000000\npercent = \"100%\"\nwindow = \"1m\"\n" +
	"[modifies.mass_guard]\nfiles = 1000000000\npercent = \"100%\"\nwindow = \"1m\"\n" +
	"[modifies.day_guard]\nfiles = 1000000000\npercent = \"100%\"\nwindow = \"1m\"\n"

// memorySample is the memory of the process at one moment, after a collection, and the size of the catalog. live,
// held and peak are what a goMemory saw since it was last asked.
type memorySample struct {
	at                time.Duration
	heap, rss, hwm    uint64
	live, held        uint64
	peak              residentPeak
	goroutines, files int
	catalog           int64
}

// residentPeak is what the process held at the most resident a goMemory saw it, ten times a second, and of what, from
// /proc/self/smaps read then: the Go runtime's own memory, which the soft limit bounds; SQLite's, which the C library
// of modernc.org/sqlite maps itself, outside the Go heap; the rest of the anonymous memory; the pages of files mapped,
// the binary's own first; and how much of it all was in transparent huge pages, which the kernel may keep resident
// after the runtime gave them back. The kernel's peak (VmHWM) may be above it, by what lasted less than a tenth of a
// second, or less than a MiB past the last peak seen.
type residentPeak struct {
	rss, runtime, sqlite uint64
	// sqliteKnown says that the test was built with the tag memory.counters, without which modernc.org/memory counts
	// nothing.
	sqliteKnown bool
	maps        residentMaps
}

// residentMaps sums what /proc/<pid>/smaps says of a process's mappings: the resident pages of anonymous memory, those
// of files, and the transparent huge pages among them.
type residentMaps struct {
	anonymous, files, huge uint64
}

// readMaps reads /proc/<pid>/smaps.
func readMaps(text string) residentMaps {
	var m residentMaps
	var anonymous bool
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// A mapping's line starts with its range of addresses; each of its fields' lines, with a name and a colon. An
		// anonymous mapping names no file: its inode is 0, and its name, when it has one, is in brackets.
		if _, _, isRange := strings.Cut(fields[0], "-"); isRange && !strings.HasSuffix(fields[0], ":") {
			anonymous = len(fields) < 6 || strings.HasPrefix(fields[5], "[")
			continue
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch {
		case fields[0] == "Rss:" && anonymous:
			m.anonymous += kb << 10
		case fields[0] == "Rss:":
			m.files += kb << 10
		case fields[0] == "AnonHugePages:":
			m.huge += kb << 10
		}
	}
	return m
}

// String says it in MiB. What is anonymous and neither the runtime's nor SQLite's may be below zero, by what the
// runtime has mapped and not yet touched.
func (p residentPeak) String() string {
	mib := func(n uint64) float64 { return float64(n) / (1 << 20) }
	if p.rss == 0 {
		return "no resident peak seen"
	}
	other := int64(p.maps.anonymous) - int64(p.runtime)
	sqlite := "SQLite's allocator not counted (built without memory.counters)"
	if p.sqliteKnown {
		sqlite = fmt.Sprintf("SQLite's allocator %.1f", mib(p.sqlite))
		other -= int64(p.sqlite)
	}
	return fmt.Sprintf("seen at most %.1f MiB resident: the Go runtime %.1f, %s, other anonymous memory %.1f, files mapped %.1f; of it all, %.1f in transparent huge pages",
		mib(p.rss), mib(p.runtime), sqlite, float64(other)/(1<<20), mib(p.maps.files), mib(p.maps.huge))
}

// readStatus reads what /proc/<pid>/status says of a process's memory, in bytes: its resident size, and its peak.
func readStatus(text string) (rss, hwm uint64) {
	for _, line := range strings.Split(text, "\n") {
		key, val, _ := strings.Cut(line, ":")
		kb, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(val), " kB"), 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "VmRSS":
			rss = kb << 10
		case "VmHWM":
			hwm = kb << 10
		}
	}
	return rss, hwm
}

// goMemory watches what the Go runtime holds: the most the heap held live after a collection, and the most the runtime
// held of the system's memory, which is what the soft limit bounds (specs.md §26). What is resident beyond the second
// is outside the runtime, as SQLite's own memory and the binary are. The first says whether a peak is what a step
// needs, and no limit can lower it, or what the collector had not yet freed.
type goMemory struct {
	live, held atomic.Uint64
	mu         sync.Mutex
	peak       residentPeak
}

// watch samples ten times a second until ctx is done.
func (g *goMemory) watch(ctx context.Context) {
	read := []metrics.Sample{{Name: "/gc/heap/live:bytes"}, {Name: "/memory/classes/total:bytes"}, {Name: "/memory/classes/heap/released:bytes"}}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		metrics.Read(read)
		var runtimeHeld uint64
		if read[0].Value.Kind() == metrics.KindUint64 && read[1].Value.Kind() == metrics.KindUint64 && read[2].Value.Kind() == metrics.KindUint64 {
			runtimeHeld = read[1].Value.Uint64() - read[2].Value.Uint64()
			g.live.Store(max(g.live.Load(), read[0].Value.Uint64()))
			g.held.Store(max(g.held.Load(), runtimeHeld))
		}
		if text, err := os.ReadFile("/proc/self/status"); err == nil {
			rss, _ := readStatus(string(text))
			g.mu.Lock()
			// The mappings are read only at a peak a MiB past the last, as reading them costs the process too.
			if g.peak.rss == 0 || rss > g.peak.rss+(1<<20) {
				stat := libc.MemStat()
				p := residentPeak{rss: rss, runtime: runtimeHeld, sqlite: uint64(max(stat.Bytes, 0)), sqliteKnown: stat.Bytes > 0 || stat.Mmaps > 0}
				if smaps, err := os.ReadFile("/proc/self/smaps"); err == nil {
					p.maps = readMaps(string(smaps))
				}
				g.peak = p
			}
			g.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// take gives the two peaks, which start again.
func (g *goMemory) take() (live, held uint64) {
	return g.live.Swap(0), g.held.Swap(0)
}

// takePeak gives what the process held at the most resident it was seen, which starts again.
func (g *goMemory) takePeak() residentPeak {
	g.mu.Lock()
	defer g.mu.Unlock()
	p := g.peak
	g.peak = residentPeak{}
	return p
}

// sampleMemory collects, then reads the heap in use, what the kernel says of the process, and the catalog's size
// with its write-ahead log. It may run in any goroutine.
func sampleMemory(t *testing.T, since time.Time, catalog string) memorySample {
	t.Helper()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	s := memorySample{at: time.Since(since).Round(time.Second), heap: ms.HeapInuse, goroutines: runtime.NumGoroutine()}
	text, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Error(err)
		return s
	}
	s.rss, s.hwm = readStatus(string(text))
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

// soakWindow gives the maintenance window of a soak that starts at a time and lasts for one: it holds for the whole
// run, so that the hashes, the backup, the scrub and the rescans take their turns all along, from an hour before the
// start to two hours past the end. A run that fills the day has all of the day but a minute, the most a window holds
// (specs.md §29.5): written from its two ends alone, the window of a 72-hour run was 3 hours a day.
func soakWindow(start time.Time, lasts time.Duration) string {
	from, to := start.Add(-time.Hour), start.Add(2*time.Hour+lasts)
	if to.Sub(from) >= 24*time.Hour {
		to = from.Add(24*time.Hour - time.Minute)
	}
	return from.Format("15:04") + "-" + to.Format("15:04")
}

// What /proc/<pid>/status says of a process's memory is read in bytes, whatever its other lines say.
func TestReadStatus(t *testing.T) {
	rss, hwm := readStatus("Name:\tloopdev.test\nVmHWM:\t  225400 kB\nVmRSS:\t  180000 kB\nRssAnon:\t  160000 kB\nThreads:\t9\n")
	if rss != 180000<<10 || hwm != 225400<<10 {
		t.Errorf("%d %d", rss, hwm)
	}
}

// What /proc/<pid>/smaps says is summed by kind: anonymous memory, its own name in brackets or none, and files.
func TestReadMaps(t *testing.T) {
	text := "c000000000-c004000000 rw-p 00000000 00:00 0 \nSize:  65536 kB\nRss:  40000 kB\nAnonHugePages:  20480 kB\n" +
		"7f1000000000-7f1000100000 r-xp 00000000 08:01 1234  /init/loopdev.test\nRss:  9000 kB\nAnonHugePages:  0 kB\n" +
		"7ffd00000000-7ffd00021000 rw-p 00000000 00:00 0  [stack]\nRss:  100 kB\n"
	if m := readMaps(text); m != (residentMaps{anonymous: 40100 << 10, files: 9000 << 10, huge: 20480 << 10}) {
		t.Errorf("%+v", m)
	}
}

// A soak's window holds its whole run, whatever it lasts.
func TestSoakWindow(t *testing.T) {
	start := time.Date(2026, 11, 13, 3, 26, 45, 0, time.UTC)
	for lasts, want := range map[time.Duration]string{
		4 * time.Minute: "02:26-05:30", 4 * time.Hour: "02:26-09:26", 20 * time.Hour: "02:26-01:26",
		21 * time.Hour: "02:26-02:25", 72 * time.Hour: "02:26-02:25",
	} {
		if got := soakWindow(start, lasts); got != want {
			t.Errorf("%v: %s, want %s", lasts, got, want)
		}
	}
}

// resetPeak makes the most the kernel records the process held start again from what it holds now (VmHWM). It may run
// in any goroutine.
func resetPeak(t *testing.T) {
	t.Helper()
	if err := os.WriteFile("/proc/self/clear_refs", []byte("5"), 0); err != nil {
		t.Error(err)
	}
}

// TestDaemonSoak: the daemon that runs for good, for -atoll.soak, on two ext4 disks, while a writer adds, edits,
// renames and deletes files every second or so. It logs the memory at the end of the first pass, which finds every
// copy of a catalog that knows none, and every tenth of the run, at the end of the pass that follows it, each with the
// most the process held since the sample before. At the end, the heap in use after a collection, its open files and
// its goroutines must not have grown past the second tenth's by more than a tenth and 8 MiB, 16 files and 16
// goroutines: what a daemon that runs for days would keep growing (specs.md §26). No pass may be held by a guard, and
// the last tenth must run at least half the passes of the second: a pass that costs more as the catalog grows would
// run fewer.
func TestDaemonSoak(t *testing.T) {
	needRoot(t)
	if *soakFor <= 0 {
		t.Skip("-atoll.soak is not set")
	}
	asTheBinary(t)
	v := sizedVolume(t, *soakFiles)
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	window := soakWindow(time.Now(), *soakFor)
	// The trash turns over every ten minutes: the writer sends about two copies a second there, each with its own
	// folders, and a retention of 30 days would fill the inodes of these small disks within the hour.
	b := newPassBenchKept(t, sizedVolume(t, *soakFiles), v, at, soakGuards, "[defaults]\nsettle = \"0s\"\n[maintenance]\nwindow = \""+window+"\"\n[trash]\nretention = \"10m\"\n")
	// The same files on both disks: the first pass finds them in step, and has nothing to copy.
	for i := range *soakFiles {
		for _, top := range []string{b.keep, at} {
			write(t, top, fmt.Sprintf("set/%03d/%d.mkv", i%100, i), fmt.Sprintf("film %d", i))
		}
	}
	// Each tenth's memory is read at the end of the pass that follows it, in the daemon's own goroutine: on 200,000
	// files a disk a pass holds over a hundred MiB while it runs, and a sample taken in the middle of one would say
	// how far it had gone rather than what the daemon keeps.
	var start time.Time
	asked := make(chan chan memorySample, 1)
	firstPass := make(chan memorySample, 1)
	asked <- firstPass
	var mem goMemory
	go mem.watch(t.Context())
	passes, stop := runDaemonAfter(t, b, filepath.Join(t.TempDir(), "fstab"), func() {
		select {
		case reply := <-asked:
			s := sampleMemory(t, start, b.catalog)
			s.live, s.held = mem.take()
			s.peak = mem.takePeak()
			reply <- s
			// Each sample's peak is its own: what the pass that just ended left resident goes back to the system, and
			// the peak starts again from what the daemon keeps.
			debug.FreeOSMemory()
			resetPeak(t)
		default:
		}
	})
	// The first pass walks every file of both disks, which in qemu without KVM is slow.
	for started := false; !started; {
		select {
		case p := <-passes:
			started = slices.Contains(p.because, daemon.BecauseStart)
		case <-time.After(firstPassWithin(*soakFiles)):
			t.Fatal("no first pass")
		}
	}
	// The hook answers before the pass is sent.
	s := <-firstPass
	t.Logf("the first pass: heap in use %.1f MiB, resident %.1f MiB, at most %.1f MiB, of which the runtime held at most %.1f MiB and the heap %.1f MiB live, %d goroutines, %d open files, catalog %.1f MiB; %v",
		float64(s.heap)/(1<<20), float64(s.rss)/(1<<20), float64(s.hwm)/(1<<20), float64(s.held)/(1<<20), float64(s.live)/(1<<20), s.goroutines, s.files, float64(s.catalog)/(1<<20), s.peak)
	most := s.hwm
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

	start = time.Now()
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
	for i := range 10 {
		time.Sleep(time.Until(start.Add(time.Duration(i+1) * every)))
		reply := make(chan memorySample, 1)
		asked <- reply
		select {
		case s = <-reply:
		case <-time.After(firstPassWithin(*soakFiles)):
			t.Fatalf("no pass ended within %v after %v", firstPassWithin(*soakFiles), time.Since(start).Round(time.Second))
		}
		mu.Lock()
		passed := count
		mu.Unlock()
		before := 0
		for _, n := range tenths {
			before += n
		}
		tenths = append(tenths, passed-before)
		t.Logf("after %v: %d passes, %d this tenth, heap in use %.1f MiB, resident %.1f MiB, at most %.1f MiB, of which the runtime held at most %.1f MiB and the heap %.1f MiB live, %d goroutines, %d open files, catalog %.1f MiB; %v",
			s.at, passed, passed-before, float64(s.heap)/(1<<20), float64(s.rss)/(1<<20), float64(s.hwm)/(1<<20), float64(s.held)/(1<<20), float64(s.live)/(1<<20), s.goroutines, s.files, float64(s.catalog)/(1<<20), s.peak)
		samples = append(samples, s)
		most = max(most, s.hwm)
	}
	stopWriter()
	stop()
	first, last := samples[1], samples[len(samples)-1]
	heaps := make([]uint64, 0, len(samples))
	for _, s := range samples[1:] {
		heaps = append(heaps, s.heap)
	}
	t.Logf("heap in use from the second tenth on: %v bytes; the most resident %.1f MiB", heaps, float64(most)/(1<<20))
	if grown := int64(last.heap) - int64(first.heap); grown > int64(first.heap)/10 && grown > 8<<20 {
		t.Errorf("the heap grew by %.1f MiB, from %.1f MiB", float64(grown)/(1<<20), float64(first.heap)/(1<<20))
	}
	if last.files > first.files+16 || last.goroutines > first.goroutines+16 {
		t.Errorf("open files %d → %d, goroutines %d → %d", first.files, last.files, first.goroutines, last.goroutines)
	}
	mu.Lock()
	defer mu.Unlock()
	// A soak in which the daemon did nothing, or held everything, measures nothing. The floor dates from passes that
	// walked every file of both disks: one a minute on the default 2,000 files a disk, a hundred times fewer on a
	// hundred times as many. A pass for events now looks at the names the watcher heard (specs.md §15.13), and 12
	// hours on 200,000 files a disk held 911 of them.
	if want := max(10, int(*soakFor/time.Minute)*2000/max(*soakFiles, 2000)); count < want {
		t.Errorf("%d passes in %v: the writer's changes did not reach the daemon", count, *soakFor)
	}
	if held > 0 {
		t.Errorf("a guard held %d passes of %d: the soak measured a daemon that decides nothing", held, count)
	}
	// Fewer passes than that in a tenth say nothing of a trend: on 200,000 files a disk in qemu without KVM, a tenth of
	// an hour held one at most while each pass walked both disks whole, and holds about eight now.
	if tenths[1] >= 4 && tenths[9]*2 < tenths[1] {
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
