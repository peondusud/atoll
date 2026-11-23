package writer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/layout"
)

// queuePending queues a copy of a 350-byte file to t2, as a plan does: its pending copy, and its job, whose payload names
// the part it copies into when part is not empty.
func (b *bench) queuePending(part string) {
	b.t.Helper()
	payload := "NULL"
	if part != "" {
		payload = `'{"part":"` + part + `"}'`
	}
	for _, q := range []string{
		`INSERT INTO files(id, lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns) VALUES (7, CAST('Films/queued.mkv' AS BLOB), 350, 9, 1, 'immediate', 'pending', 0, 0)`,
		`INSERT INTO copies(file_id, disk_id, size, mtime_ns, created_by, first_seen_ns, state, mode, uid, gid, nlink) VALUES (7, 't1', 350, 9, 'user', 0, 'ok', 420, 0, 0, 1)`,
		`INSERT INTO copies(file_id, disk_id, created_by, first_seen_ns, state) VALUES (7, 't2', 'atoll', 0, 'pending')`,
		`INSERT INTO jobs(kind, file_id, src_disk, dst_disk, state, payload) VALUES ('replicate', 7, 't1', 't2', 'queued', ` + payload + `)`,
	} {
		if _, err := b.w.db.ExecContext(b.t.Context(), q); err != nil {
			b.t.Fatal(err)
		}
	}
}

// The room of a disk past min_free is shared by the daemon's copies, which a plan made fit there with each pending copy
// holding its room (specs.md §13.4), and the parts the view writes in parallel (§21.2): the view's room takes off what
// the pending copies still have to write there. A copy the daemon queued to t2 holds all its room past min_free, and a
// new file written through the view gets no part on t2, where it once got one in that room (conformance/models/README.md,
// flaw 12, the maintainer's remedy; Copies.tla).
func TestTheViewsPartsLeaveTheRoomAPendingCopyHolds(t *testing.T) {
	b := parallelBench(t, "", 400)
	// t2: 1000 bytes, 400 free, min_free 5 %: 350 past it, all of which a pending copy of a 350-byte file holds.
	b.queuePending("")
	f := b.writeNew("Films/new.mkv", "a new film, written once")
	defer func() { _ = f.Close() }()
	if got := b.partRows(); len(got) != 0 {
		t.Errorf("the view writes a part to t2 (%v) in the room past min_free a pending copy holds there, "+
			"which the copy then takes past min_free", got)
	}
}

// What a pending copy's part already holds on its disk is in the disk's free space already: the copy holds the room of
// what it still has to write, and the view's parts take the rest. A copy of 350 bytes that wrote 300 to its part on t2
// holds 50 of the 350 past min_free there, and a new file gets its part on t2.
func TestTheViewsPartsTakeTheRoomAPendingCopyNoLongerNeeds(t *testing.T) {
	b := parallelBench(t, "", 400)
	part := layout.PartName(99)
	tmp := filepath.Join(b.mount["t2"], "Atoll", "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, part), []byte(strings.Repeat("x", 300)), 0o600); err != nil {
		t.Fatal(err)
	}
	b.queuePending(part)
	f := b.writeNew("Films/new.mkv", "a new film, written once")
	defer func() { _ = f.Close() }()
	if got := b.partRows(); len(got) != 1 || got[0] != "t2:planned:planned" {
		t.Errorf("the parts while it is written: %v", got)
	}
}
