//go:build flaws

package writer

import (
	"testing"
)

// The flaws the TLA+ model found and that wait for the maintainer's remedy (conformance/models/README.md), each a test
// that fails as the flaw says until then, kept out of go test ./... by the build tag flaws; make model-flaws runs them.

// Flaw 12: the room of a disk past min_free is shared by the daemon's copies, which a plan made fit there with each
// pending copy holding its room (specs.md §13.4) and which then check nothing but ENOSPC as they write, and the parts
// the view writes in parallel (§21.2), whose targets and room come from the disk's free space as its filesystem says it
// (watch.ParallelTargets, writer.openPart), the pending copies not counted. A copy the daemon queued to t2 holds all
// its room past min_free; a new file written through the view still gets a part on t2, which takes that room, and the
// copy then takes the disk past min_free when it runs (Copies.tla, FlawParallelRoom.cfg).
func TestFlawTheViewsPartsTakeTheRoomAPendingCopyHolds(t *testing.T) {
	b := parallelBench(t, "", 400)
	// t2: 1000 bytes, 400 free, min_free 5 %: 350 past it, all of which a pending copy of a 340-byte file holds.
	for _, q := range []string{
		`INSERT INTO files(id, lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns) VALUES (7, CAST('Films/queued.mkv' AS BLOB), 340, 9, 1, 'immediate', 'pending', 0, 0)`,
		`INSERT INTO copies(file_id, disk_id, size, mtime_ns, created_by, first_seen_ns, state, mode, uid, gid, nlink) VALUES (7, 't1', 340, 9, 'user', 0, 'ok', 420, 0, 0, 1)`,
		`INSERT INTO copies(file_id, disk_id, created_by, first_seen_ns, state) VALUES (7, 't2', 'atoll', 0, 'pending')`,
		`INSERT INTO jobs(kind, file_id, src_disk, dst_disk, state) VALUES ('replicate', 7, 't1', 't2', 'queued')`,
	} {
		if _, err := b.w.db.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	f := b.writeNew("Films/new.mkv", "a new film, written once")
	defer func() { _ = f.Close() }()
	if got := b.partRows(); len(got) != 0 {
		t.Errorf("flaw 12: the view writes a part to t2 (%v) in the room past min_free a pending copy holds there, "+
			"which the copy then takes past min_free", got)
	}
}
