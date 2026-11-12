package jobs

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/trash"
)

// chown gives a copy an owner, a new one each time, as many times as it takes for its change time to move, and says
// the user id it ends with; the group id is the same.
func (b *bench) chown(id, path string) int64 {
	b.t.Helper()
	full := filepath.Join(b.dirs[id], "media", path)
	before := b.ctimeOf(id, path)
	for uid := 4242; ; uid++ {
		if err := os.Chown(full, uid, uid); err != nil {
			b.t.Fatal(err)
		}
		if b.ctimeOf(id, path) != before {
			return int64(uid)
		}
		if uid == 6242 {
			b.t.Fatal("the change time did not move")
		}
		time.Sleep(time.Millisecond)
	}
}

func (b *bench) ownerOf(id, path string) int64 {
	b.t.Helper()
	st, err := os.Stat(filepath.Join(b.dirs[id], "media", path))
	if err != nil {
		b.t.Fatal(err)
	}
	return int64(st.Sys().(*syscall.Stat_t).Uid)
}

func ownerTo(disk, outcome string) Operation {
	return Operation{Disk: disk, Kind: KindChown, Path: "a", Outcome: outcome}
}

// owned gives the first copy a new owner, past settle.
func owned(t *testing.T) (*bench, int64) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("giving a file away takes root")
	}
	b := pool(t)
	uid := b.chown("t1", "a")
	b.now = b.ctimeOf("t1", "a") + settleNS
	return b, uid
}

// A chown on one copy reaches the other through one operation chown, and moves no time: the rows of both say the
// owner their disks hold (specs.md §15.4).
func TestAChownReachesTheOtherCopy(t *testing.T) {
	b, uid := owned(t)
	mtime := b.mtimeOf("t2", "a")
	out := b.pass()
	if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "metadata"}}) || !slices.Equal(out.Operations, []Operation{ownerTo("t2", OutcomeApplied)}) {
		t.Fatalf("%+v", out)
	}
	if b.ownerOf("t2", "a") != uid || b.mtimeOf("t2", "a") != mtime {
		t.Errorf("the copy holds %d and %d", b.ownerOf("t2", "a"), b.mtimeOf("t2", "a"))
	}
	if got := b.count("SELECT count(*) FROM copies WHERE uid = ? AND gid = ?", uid, uid); got != 2 {
		t.Errorf("%d rows say the owner", got)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND ctime_ns = ?", b.ctimeOf("t2", "a")); got != 1 {
		t.Error("the row does not say the change time the operation left")
	}
	if out := b.pass(); len(out.Decisions)+len(out.Operations) != 0 {
		t.Errorf("then %+v", out)
	}
}

// A disk whose driver keeps its own owner: the operation is undone, the outcome kept, the changed copy's row follows
// its look, and the next pass decides nothing again, as the difference is now one that was already there (specs.md
// §15.4).
func TestAnOwnerTheDiskKeeps(t *testing.T) {
	b, uid := owned(t)
	b.fail[b.dirs["t2"]] = func(call, _ string) error {
		if call == "setowner" {
			return syscall.EPERM
		}
		return nil
	}
	out := b.pass()
	if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "metadata"}}) || !slices.Equal(out.Operations, []Operation{ownerTo("t2", OutcomeKept)}) ||
		out.Partial {
		t.Fatalf("%+v", out)
	}
	if got := b.text("SELECT state FROM ops"); got != "undone" {
		t.Errorf("the operation is %s", got)
	}
	if got := b.count("SELECT count(*) FROM events WHERE kind = 'op-undone'"); got != 0 {
		t.Errorf("%d events", got)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't1' AND uid = ?", uid); got != 1 {
		t.Error("the changed copy's row does not follow its look")
	}
	if b.ownerOf("t2", "a") == uid {
		t.Error("the copy took the owner")
	}
	delete(b.fail, b.dirs["t2"])
	if out := b.pass(); len(out.Decisions)+len(out.Operations) != 0 {
		t.Errorf("then %+v", out)
	}
}

// A disk lost while the owner is set keeps its operation planned, for the reprise; the next pass undoes it, decides
// again and applies the owner.
func TestADiskLostDuringAnOwner(t *testing.T) {
	b, uid := owned(t)
	b.fail[b.dirs["t2"]] = func(call, _ string) error {
		if call == "setowner" {
			return syscall.EIO
		}
		return nil
	}
	out := b.pass()
	if !slices.Equal(out.Operations, []Operation{ownerTo("t2", trash.OutcomeIOError)}) || !out.Partial {
		t.Fatalf("%+v", out)
	}
	if got := b.text("SELECT state FROM ops"); got != "planned" {
		t.Errorf("the operation is %s", got)
	}
	delete(b.fail, b.dirs["t2"])
	out = b.pass()
	if !slices.Equal(out.Operations, []Operation{ownerTo("t2", OutcomeApplied)}) || out.Partial {
		t.Fatalf("then %+v", out)
	}
	if got := b.text("SELECT group_concat(state, ',') FROM (SELECT state FROM ops ORDER BY id)"); got != "undone,done" {
		t.Errorf("the operations are %s", got)
	}
	if b.ownerOf("t2", "a") != uid {
		t.Errorf("the copy holds %d", b.ownerOf("t2", "a"))
	}
}

// An operation chown left planned holds the file, as only a reprise can tell what its copy holds; the reprise
// finishes it on the owner its event holds (specs.md §15.4, §17.2).
func TestAChownLeftPlannedHoldsAMetadataChange(t *testing.T) {
	b, uid := owned(t)
	b.crashAt("owner-set")
	b.ro[b.dirs["t2"]] = true
	b.now = b.ctimeOf("t2", "a") + settleNS
	if out := b.pass(); len(out.Decisions)+len(out.Operations) != 0 || out.Waiting != 0 {
		t.Fatalf("%+v", out)
	}
	delete(b.ro, b.dirs["t2"])
	out := b.pass()
	if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "metadata"}}) || len(out.Operations) != 0 {
		t.Fatalf("then %+v", out)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE uid = ? AND gid = ?", uid, uid); got != 2 {
		t.Errorf("%d rows say the owner", got)
	}
	if got := b.text("SELECT state FROM ops"); got != "done" {
		t.Errorf("the operation is %s", got)
	}
}

// A copy whose row has no owner, as the migration leaves it: the next look records the owner it holds, unchanged
// since its row, and a chown then reaches it (specs.md §9).
func TestAnOwnerTheLookRecords(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("giving a file away takes root")
	}
	b := pool(t)
	if _, err := b.db.ExecContext(t.Context(), "UPDATE copies SET uid = NULL, gid = NULL"); err != nil {
		t.Fatal(err)
	}
	if out := b.pass(); len(out.Decisions)+len(out.Operations) != 0 {
		t.Fatalf("%+v", out)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE uid IS NOT NULL AND gid IS NOT NULL"); got != 2 {
		t.Errorf("%d rows have an owner", got)
	}
	uid := b.chown("t1", "a")
	b.now = b.ctimeOf("t1", "a") + settleNS
	if out := b.pass(); !slices.Equal(out.Operations, []Operation{ownerTo("t2", OutcomeApplied)}) {
		t.Fatalf("then %+v", out)
	}
	if b.ownerOf("t2", "a") != uid {
		t.Errorf("the copy holds %d", b.ownerOf("t2", "a"))
	}
}
