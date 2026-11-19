package jobs

import (
	"slices"
	"syscall"
	"testing"
)

// How a copy's operation is left planned with its job running, on a disk that is then away: Atoll killed once the
// copy is at its name, or the disk lost then, an I/O error ending the session on it (specs.md §14.1, §17.1).
var leftPlanned = []struct {
	name  string
	leave func(b *bench)
}{
	{"killed at source-synced", func(b *bench) { b.replicateStopping("a", "source-synced") }},
	{"lost after the rename", func(b *bench) {
		at("moved", failOn("t2", "sync", syscall.EIO))(b)
		b.replicate("a", ReplicateOptions{Now: true})
		delete(b.fail, b.dirs["t2"])
	}},
}

// awayWithAJobRunning leaves the copy of a on t2 at its name, its operation planned and its job running, and t2 away.
func (b *bench) awayWithAJobRunning(leave func(b *bench)) {
	b.t.Helper()
	leave(b)
	if got := b.count("SELECT count(*) FROM ops WHERE kind = 'create' AND state = 'planned'") + b.count("SELECT count(*) FROM jobs WHERE state = 'running'"); got != 2 {
		b.t.Fatalf("%d of the operation planned and its job running", got)
	}
	b.gone[b.dirs["t2"]] = true
}

// running says that the job of a's copy on t2 runs still, with its pending copy.
func (b *bench) running() {
	b.t.Helper()
	if got := b.count("SELECT count(*) FROM jobs WHERE state = 'running'") + b.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND state = 'pending'"); got != 2 {
		b.t.Fatalf("%d of the job running and its pending copy left", got)
	}
}

// A file whose copy's job runs, its operation planned on a disk away, waits for its decisions: the user deleted its
// other copy, and once the disk is back its reprise finishes the copy, which the deletion then reaches, to the trash.
// The decision Gone dropped the file with the job; back, the reprise finished the copy with no job and no file, and the
// next walk took Atoll's copy for a user's new file: the file the user deleted came back (conformance/models/README.md,
// flaw 2).
func TestAFileWithAJobRunningWaitsForItsDeletion(t *testing.T) {
	for _, tc := range leftPlanned {
		t.Run(tc.name, func(t *testing.T) {
			b := replicatePool(t)
			b.awayWithAJobRunning(tc.leave)
			b.remove("t1", "a")
			for range 3 {
				b.settle()
			}
			b.running()
			if got := b.count("SELECT count(*) FROM files"); got != 1 {
				t.Fatal("the file went with its job running")
			}
			delete(b.gone, b.dirs["t2"])
			b.settle()
			if b.exists("t2", "media/a") {
				t.Error("the copy the user deleted the file of is still in the library")
			}
			if got := b.count("SELECT count(*) FROM trash WHERE disk_id = 't2' AND reason = 'deleted'"); got != 1 {
				t.Errorf("%d copies in the trash of t2", got)
			}
			if got := b.count("SELECT count(*) FROM files") + b.count("SELECT count(*) FROM jobs") + b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
				t.Errorf("%d files, jobs or operations left", got)
			}
		})
	}
}

// The plan keeps a pending copy whose job runs, on a disk away, and counts it: a modification of the file's other copy
// waits for it, and once the disk is back its reprise finishes the copy of the version before, which the modification
// then makes stale, sends to the trash and makes again. The plan canceled the pending copy and its job, the
// modification found nothing to wait for, and the reprise recorded the copy of the version before as ok, counted at
// the file's level (conformance/models/README.md, flaw 2).
func TestAPendingCopyWithItsJobRunningIsKept(t *testing.T) {
	for _, tc := range leftPlanned {
		t.Run(tc.name, func(t *testing.T) {
			b := replicatePool(t)
			b.awayWithAJobRunning(tc.leave)
			b.pass()
			b.running()
			b.changeAgain("t1", "a", "CONTENT!!")
			b.now = b.ctimeOf("t1", "a") + settleNS
			if out := b.pass(); len(out.Decisions) != 0 {
				t.Fatalf("decided with a job running: %+v", out)
			}
			b.running()
			delete(b.gone, b.dirs["t2"])
			b.pass()
			if got := b.read("t2", "a"); got != "CONTENT!!" {
				t.Errorf("t2 holds %q", got)
			}
			if got := b.count("SELECT count(*) FROM trash WHERE disk_id = 't2' AND reason = 'overwritten'"); got != 1 {
				t.Errorf("%d versions before in the trash of t2", got)
			}
			if got := b.text("SELECT group_concat(disk_id || ' ' || created_by || ' ' || state, ',') FROM (SELECT * FROM copies ORDER BY disk_id)"); got != "t1 user ok,t2 atoll ok" {
				t.Errorf("the copies are %s", got)
			}
		})
	}
}

// A file whose copy's job runs waits for a rename too, as for any decision: nothing is decided of it until the reprise
// of its operation is over (specs.md §14.1, §15.12). Back, the reprise finishes the copy, and the rename is decided.
func TestAFileWithAJobRunningWaitsForItsRename(t *testing.T) {
	for _, tc := range leftPlanned {
		t.Run(tc.name, func(t *testing.T) {
			b := replicatePool(t)
			b.awayWithAJobRunning(tc.leave)
			b.rename("t1", "a", "b")
			for range 2 {
				b.settle()
			}
			b.running()
			if got := b.text("SELECT CAST(lpath AS TEXT) FROM files"); got != "a" {
				t.Fatalf("renamed to %s with a job running", got)
			}
			delete(b.gone, b.dirs["t2"])
			out := b.pass()
			if !slices.Contains(out.Decisions, Decision{Path: "a", Kind: "renamed", To: "b"}) {
				t.Errorf("%+v", out.Decisions)
			}
			if got := b.count("SELECT count(*) FROM jobs WHERE state = 'running'") + b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
				t.Errorf("%d jobs running or operations planned", got)
			}
			if got := b.count("SELECT count(*) FROM files WHERE CAST(lpath AS TEXT) = 'b'"); got != 1 {
				t.Error("no file b")
			}
		})
	}
}
