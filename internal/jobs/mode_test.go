package jobs

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

// chmod gives a copy a mode, as many times as it takes for its change time to move, which one clock tick may not
// do, and says the mode it ends with.
func (b *bench) chmod(id, path string, modes ...os.FileMode) os.FileMode {
	b.t.Helper()
	full := filepath.Join(b.dirs[id], "media", path)
	before := b.ctimeOf(id, path)
	for i := 0; ; i++ {
		mode := modes[i%len(modes)]
		if err := os.Chmod(full, mode); err != nil {
			b.t.Fatal(err)
		}
		if b.ctimeOf(id, path) != before {
			return mode
		}
		if i == 2000 {
			b.t.Fatal("the change time did not move")
		}
		time.Sleep(time.Millisecond)
	}
}

func (b *bench) modeOf(id, path string) int64 {
	b.t.Helper()
	st, err := os.Stat(filepath.Join(b.dirs[id], "media", path))
	if err != nil {
		b.t.Fatal(err)
	}
	return int64(st.Sys().(*syscall.Stat_t).Mode & 0o7777)
}

func modeTo(disk, outcome string) Operation {
	return Operation{Disk: disk, Kind: KindChmod, Path: "a", Outcome: outcome}
}

// A chmod on one copy reaches the other through one operation chmod, and moves no time: the rows of both say the
// mode their disks hold (specs.md §15.4).
func TestAChmodReachesTheOtherCopy(t *testing.T) {
	b := pool(t)
	mtime := b.mtimeOf("t2", "a")
	mode := int64(b.chmod("t1", "a", 0o600, 0o640))
	b.now = b.ctimeOf("t1", "a") + settleNS
	out := b.pass()
	if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "metadata"}}) || !slices.Equal(out.Operations, []Operation{modeTo("t2", OutcomeApplied)}) {
		t.Fatalf("%+v", out)
	}
	if b.modeOf("t2", "a") != mode || b.mtimeOf("t2", "a") != mtime {
		t.Errorf("the copy holds %o and %d", b.modeOf("t2", "a"), b.mtimeOf("t2", "a"))
	}
	if got := b.count("SELECT count(*) FROM copies WHERE mode = ?", mode); got != 2 {
		t.Errorf("%d rows say the mode", got)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND ctime_ns = ?", b.ctimeOf("t2", "a")); got != 1 {
		t.Error("the row does not say the change time the operation left")
	}
	if out := b.pass(); len(out.Decisions)+len(out.Operations) != 0 {
		t.Errorf("then %+v", out)
	}
}

// A copy whose row has no mode, as a migration leaves it, and that holds the mode a chmod gave the other copy: a
// dry run, which records nothing, reads its mode as unknown and lists an operation; a pass first records the mode
// its look found, unchanged since its row, and applies nothing (specs.md §9, §15.12).
func TestAModeTheLookRecordsBeforeTheDecision(t *testing.T) {
	b := pool(t)
	mode := int64(b.chmod("t1", "a", 0o600, 0o640))
	// The other copy held that mode already, before its row was recorded with none.
	if err := os.Chmod(filepath.Join(b.dirs["t2"], "media", "a"), os.FileMode(mode)); err != nil {
		t.Fatal(err)
	}
	b.exec("UPDATE copies SET mode = NULL, ctime_ns = ? WHERE disk_id = 't2'", b.ctimeOf("t2", "a"))
	b.now = b.ctimeOf("t1", "a") + settleNS
	dry, err := Run(t.Context(), b.db, b.config, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(dry.Operations, []Operation{modeTo("t2", OutcomeToApply)}) {
		t.Errorf("the dry run says %+v", dry.Operations)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND mode IS NULL"); got != 1 {
		t.Error("the dry run recorded a mode")
	}
	out := b.pass()
	if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "metadata"}}) || len(out.Operations) != 0 {
		t.Errorf("%+v", out)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE mode = ?", mode); got != 2 {
		t.Errorf("%d rows say the mode", got)
	}
}

// A changed copy whose row has no mode gives none: what moved cannot be told from what was there, and its row takes
// the mode its look found (specs.md §9, §15.4).
func TestAChangedCopyWithNoModeGivesNone(t *testing.T) {
	b := pool(t)
	b.exec("UPDATE copies SET mode = NULL WHERE disk_id = 't1'")
	mode := int64(b.chmod("t1", "a", 0o600, 0o640))
	b.now = b.ctimeOf("t1", "a") + settleNS
	out := b.pass()
	if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "metadata"}}) || len(out.Operations) != 0 {
		t.Fatalf("%+v", out)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't1' AND mode = ?", mode); got != 1 {
		t.Error("the changed copy's row does not take its mode")
	}
	if b.modeOf("t2", "a") == mode {
		t.Error("the other copy was given the mode")
	}
}

// A mode and a time from one copy: the other copy gets the mode, then the time, an operation each, and the time is
// set on the copy as the operation chmod left it (specs.md §15.4).
func TestAModeThenATime(t *testing.T) {
	b := pool(t)
	mode := int64(b.chmod("t1", "a", 0o600, 0o640))
	b.touch("t1", "a", touchedNS)
	b.now = b.ctimeOf("t1", "a") + settleNS
	out := b.pass()
	if want := []Operation{modeTo("t2", OutcomeApplied), timeTo("t2", OutcomeApplied)}; !slices.Equal(out.Operations, want) {
		t.Fatalf("%+v", out.Operations)
	}
	if b.modeOf("t2", "a") != mode || b.mtimeOf("t2", "a") != touchedNS {
		t.Error("the copy does not hold both")
	}
	if got := b.text("SELECT group_concat(kind || ' ' || state, ',') FROM (SELECT kind, state FROM ops ORDER BY id)"); got != "chmod done,utimens done" {
		t.Errorf("the operations are %s", got)
	}
}

// An operation chmod left planned holds the file, as only a reprise can tell what its copy holds, as for a time
// (specs.md §15.4, §17.2).
func TestAChmodLeftPlannedHoldsAMetadataChange(t *testing.T) {
	b := pool(t)
	mode := int64(b.chmod("t1", "a", 0o600, 0o640))
	b.now = b.ctimeOf("t1", "a") + settleNS
	b.crashAt("mode-set")
	b.ro[b.dirs["t2"]] = true
	// Past settle for the change time the operation left too: only the operation holds the file.
	b.now = b.ctimeOf("t2", "a") + settleNS
	if out := b.pass(); len(out.Decisions)+len(out.Operations) != 0 || out.Waiting != 0 {
		t.Fatalf("%+v", out)
	}
	delete(b.ro, b.dirs["t2"])
	out := b.pass()
	if !slices.Equal(out.Decisions, []Decision{{Path: "a", Kind: "metadata"}}) || len(out.Operations) != 0 {
		t.Fatalf("then %+v", out)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE mode = ?", mode); got != 2 {
		t.Errorf("%d rows say the mode", got)
	}
}

// The operations run copy by copy by position, which is the configuration's order, not the order of the disks' ids.
func TestTheOperationsRunByPosition(t *testing.T) {
	b := newBench(t, "t2", "t1", "t3")
	for _, id := range []string{"t1", "t2", "t3"} {
		b.write(id, "a", "x")
	}
	b.scan(true)
	b.chmod("t3", "a", 0o600, 0o640)
	b.now = b.ctimeOf("t3", "a") + settleNS
	out := b.pass()
	if want := []Operation{modeTo("t2", OutcomeApplied), modeTo("t1", OutcomeApplied)}; !slices.Equal(out.Operations, want) {
		t.Errorf("%+v", out.Operations)
	}
}

// Setuid, setgid and sticky are part of a mode: setgid given to one copy reaches the other (specs.md §9).
func TestSetgidIsPartOfTheMode(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("setgid on a file of another group takes root")
	}
	b := pool(t)
	// Go spells setgid os.ModeSetgid; stat(2), and the catalog, 02000.
	mode := int64(b.chmod("t1", "a", os.ModeSetgid|0o640, os.ModeSetgid|0o600).Perm()) | 0o2000
	b.now = b.ctimeOf("t1", "a") + settleNS
	if out := b.pass(); !slices.Equal(out.Operations, []Operation{modeTo("t2", OutcomeApplied)}) {
		t.Fatalf("%+v", out)
	}
	if b.modeOf("t2", "a") != mode {
		t.Errorf("the copy holds %o", b.modeOf("t2", "a"))
	}
	if got := b.count("SELECT count(*) FROM copies WHERE mode = ?", mode); got != 2 {
		t.Errorf("%d rows say the mode", got)
	}
}
