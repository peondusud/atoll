package jobs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/view/writer"
)

// cacheBench gives a pool with Atoll's own view, a cache disk c, and two data disks t1 and t2, in UTC, with more after
// the disks. Copies wait for no settle. Its clock is the real one, as a residency counts from the change time of a copy,
// which the disk gives.
func cacheBench(t *testing.T, more string) *bench {
	t.Helper()
	more = "[defaults]\nsettle = \"0s\"\n" + more
	b := newBenchWith(t, more, "c", "t1", "t2")
	b.now = time.Now().UnixNano()
	text := strings.Replace(b.base, "pool_id = \"p\"\n", "pool_id = \"p\"\ntimezone = \"UTC\"\n", 1)
	text = strings.Replace(text, "[view]\n", "[view]\nengine = \"atoll\"\n", 1)
	disk := fmt.Sprintf("mount = %q\nfs = \"ext4\"\n", b.dirs["c"])
	if !strings.Contains(text, disk) {
		t.Fatal("no disk c in the configuration")
	}
	b.base = strings.Replace(text, disk, disk+"role = \"cache\"\n", 1)
	c, err := config.Parse([]byte(b.base + more))
	if err != nil {
		t.Fatal(err)
	}
	b.config = c
	return b
}

// The window holds the bench's time, now in UTC, or not: it opened this hour and closes two hours later, or it opens
// in three hours.
var (
	hour         = time.Now().UTC().Hour()
	windowOpen   = fmt.Sprintf("[replication]\nwindow = \"%02d:00-%02d:00\"\n", hour, (hour+2)%24)
	windowClosed = fmt.Sprintf("[replication]\nwindow = \"%02d:00-%02d:00\"\n", (hour+3)%24, (hour+5)%24)
)

func (b *bench) holders(path string) string {
	b.t.Helper()
	return b.text("SELECT COALESCE(group_concat(disk_id || ':' || created_by || ':' || state, ' '), '') FROM (SELECT c.disk_id, c.created_by, c.state FROM copies c JOIN files f ON f.id = c.file_id WHERE f.lpath = ? ORDER BY c.disk_id)", []byte(path))
}

// A file whose residency is over leaves the cache in the window: the pass makes its copies on the data disks, then
// sends its copy on the cache to the trash and purges it at once. The copy on a data disk that takes the place of the
// user's is a user copy from then on (specs.md §19).
func TestAFileLeavesTheCacheInTheWindow(t *testing.T) {
	b := cacheBench(t, "[cache]\nenabled = true\nmin_age = \"0s\"\n"+windowOpen)
	b.write("c", "show/e1.mkv", "episode")
	b.scan(false)
	out := b.pass()
	if got := outcomeOf(out, KindTransfer, "show/e1.mkv"); got != OutcomeTransferred {
		t.Fatalf("transfer %q, operations %+v", got, out.Operations)
	}
	if b.exists("c", "media/show/e1.mkv") || !b.exists("t1", "media/show/e1.mkv") || !b.exists("t2", "media/show/e1.mkv") {
		t.Fatal("the file is not where it should be")
	}
	if got := b.holders("show/e1.mkv"); got != "t1:user:ok t2:atoll:ok" {
		t.Errorf("copies %q", got)
	}
	if n := b.count("SELECT count(*) FROM trash"); n != 0 {
		t.Errorf("%d entries in the trash", n)
	}
	if got := b.text("SELECT group_concat(kind || ':' || state, ' ') FROM (SELECT kind, state FROM ops WHERE disk_id = 'c' ORDER BY id)"); got != "trash:done unlink:done" {
		t.Errorf("journal of c: %s", got)
	}
	entries, err := os.ReadDir(filepath.Join(b.dirs["c"], "Atoll", "trash"))
	if err == nil && len(entries) != 0 {
		t.Errorf("the trash of c holds %d entries", len(entries))
	}
	if again := b.pass(); len(again.Operations) != 0 {
		t.Errorf("a pass after: %+v", again.Operations)
	}
	if got := b.text("SELECT state FROM files"); got != "ok" {
		t.Errorf("state %s", got)
	}
}

// A file whose residency is not over stays on the cache, with no copy on a data disk, pending rather than degraded.
func TestAResidentFileStaysOnTheCache(t *testing.T) {
	b := cacheBench(t, "[cache]\nenabled = true\n"+windowOpen)
	b.write("c", "a", "x")
	b.scan(false)
	out := b.pass()
	if len(out.Operations) != 0 || !b.exists("c", "media/a") || b.exists("t1", "media/a") || b.exists("t2", "media/a") {
		t.Fatalf("operations %+v", out.Operations)
	}
	if got := b.text("SELECT state FROM files"); got != "pending" {
		t.Errorf("state %s", got)
	}
	if out.Waiting != 1 || !out.NextDue.Valid || out.NextDue.Int64 < b.now+int64(6*24*time.Hour) {
		t.Errorf("waiting %d, next due %v", out.Waiting, out.NextDue)
	}
}

// A file due outside the window waits for it.
func TestADueFileWaitsForTheWindow(t *testing.T) {
	b := cacheBench(t, "[cache]\nenabled = true\nmin_age = \"0s\"\n"+windowClosed)
	b.write("c", "a", "x")
	b.scan(false)
	out := b.pass()
	if len(out.Operations) != 0 || !b.exists("c", "media/a") {
		t.Fatalf("operations %+v", out.Operations)
	}
	// The daemon wakes when the window opens, three hours on, with no event to wake it.
	opens := time.Now().UTC().Truncate(time.Hour).Add(3 * time.Hour)
	if out.Waiting != 1 || !out.NextDue.Valid || out.NextDue.Int64 != opens.UnixNano() {
		t.Errorf("waiting %d, next due %v, want %s", out.Waiting, out.NextDue, opens)
	}
}

// Above cache.max_fill, the oldest file leaves at once, window closed and residency not over.
func TestAboveMaxFillTheOldestLeaves(t *testing.T) {
	b := cacheBench(t, "[cache]\nenabled = true\nmax_fill = \"40%\"\n"+windowClosed)
	b.write("c", "a", "x")
	b.scan(false)
	out := b.pass()
	if got := outcomeOf(out, KindTransfer, "a"); got != OutcomeTransferred || b.exists("c", "media/a") {
		t.Fatalf("transfer %q, operations %+v", got, out.Operations)
	}
}

// With mergerfs, the cache does nothing: its copies count as any other, and nothing leaves it.
func TestTheCacheDoesNothingWithMergerfs(t *testing.T) {
	b := cacheBench(t, "[cache]\nenabled = true\nmin_age = \"0s\"\n"+windowOpen)
	c, err := config.Parse([]byte(strings.Replace(b.base, "engine = \"atoll\"\n", "", 1) + "[defaults]\nsettle = \"0s\"\n[cache]\nenabled = true\nmin_age = \"0s\"\n" + windowOpen))
	if err != nil {
		t.Fatal(err)
	}
	b.config = c
	b.write("c", "a", "x")
	b.scan(false)
	out := b.pass()
	if outcomeOf(out, KindTransfer, "a") != "" || !b.exists("c", "media/a") {
		t.Fatalf("operations %+v", out.Operations)
	}
}

// The data disks already hold a file the cache holds too: only the transfer runs, and a copy written on the cache
// between the plan and the move stays, as the move is checked against its row.
func TestACopyWrittenDuringItsTransferStays(t *testing.T) {
	b := cacheBench(t, "[cache]\nenabled = true\nmin_age = \"0s\"\n"+windowOpen)
	for _, id := range []string{"c", "t1", "t2"} {
		b.write(id, "a", "x")
	}
	b.scan(true)
	b.at["journaled"] = func() { b.write("c", "a", "a better release") }
	out := b.pass()
	if got := outcomeOf(out, KindTransfer, "a"); got != "changed" {
		t.Fatalf("transfer %q, operations %+v", got, out.Operations)
	}
	got, err := os.ReadFile(filepath.Join(b.dirs["c"], "media", "a"))
	if err != nil || string(got) != "a better release" {
		t.Fatalf("on the cache: %q, %v", got, err)
	}
}

// crashAtNth is crashAt at the nth time a pass reaches a point.
func (b *bench) crashAtNth(point string, n int) {
	b.t.Helper()
	stop := errors.New("stopped")
	reached := 0
	var fire func()
	fire = func() {
		if reached++; reached == n {
			panic(stop) //nolint:forbidigo // what stands for kill -9
		}
		b.at[point] = fire
	}
	b.at[point] = fire
	defer func() {
		if err, ok := recover().(error); !ok || !errors.Is(err, stop) {
			b.t.Fatalf("it did not stop at %s %d: %v", point, n, err)
		}
	}()
	_, _ = Run(b.t.Context(), b.db, b.config, b, true)
}

// A pass stopped at each point of a transfer, as by kill -9, and the next: the file is never lost, and it ends on the
// data disks alone. The move to the trash, then the purge.
func TestATransferStoppedAtEachPoint(t *testing.T) {
	for _, at := range []struct {
		point string
		nth   int
	}{{"journaled", 1}, {"moved", 1}, {"journaled", 2}, {"moved", 2}} {
		t.Run(fmt.Sprintf("%s %d", at.point, at.nth), func(t *testing.T) {
			b := cacheBench(t, "[cache]\nenabled = true\nmin_age = \"0s\"\n"+windowOpen)
			for _, id := range []string{"c", "t1", "t2"} {
				b.write(id, "a", "x")
			}
			b.scan(true)
			b.crashAtNth(at.point, at.nth)
			b.pass()
			b.pass()
			if !b.exists("t1", "media/a") || !b.exists("t2", "media/a") || b.exists("c", "media/a") {
				t.Fatal("the file is not where it should be")
			}
			if got := b.holders("a"); got != "t1:user:ok t2:user:ok" {
				t.Errorf("copies %q", got)
			}
			if n := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); n != 0 {
				t.Errorf("%d operations still planned", n)
			}
		})
	}
}

// A file made through Atoll's own view, on the cache, leaves it only once a pass has taken the view's operation in: the
// view presents the file from the disk it made it on, and a transfer before that would empty that disk, so that the
// view presented nothing there, and the operation could never be taken in (specs.md §19, §21.2). The pass that waits
// counts it, and is due again a debounce on.
func TestATransferWaitsForTheViewsOperation(t *testing.T) {
	b := cacheBench(t, "[cache]\nenabled = true\nmin_age = \"0s\"\n"+windowOpen)
	b.scan(false)
	f, _, err := b.viewOf().Create(t.Context(), "a.mkv", 0o644, writer.Owner{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("x"), 0); err != nil {
		t.Fatal(errors.Join(err, f.Close()))
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if !b.exists("c", "media/a.mkv") {
		t.Fatal("the view did not make the file on the cache")
	}
	out := b.pass()
	if got := outcomeOf(out, KindTransfer, "a.mkv"); got != "" || !b.exists("c", "media/a.mkv") {
		t.Fatalf("transferred before the view's operation was taken in: %q", got)
	}
	if out.Waiting == 0 || !out.NextDue.Valid {
		t.Errorf("waiting %d, next due %v", out.Waiting, out.NextDue)
	}
	if open := b.openViewOps(); len(open) != 0 {
		t.Fatalf("the view's operations not taken in: %+v", open)
	}
	out = b.pass()
	if got := outcomeOf(out, KindTransfer, "a.mkv"); got != OutcomeTransferred || b.exists("c", "media/a.mkv") {
		t.Fatalf("transfer %q, operations %+v", got, out.Operations)
	}
}
