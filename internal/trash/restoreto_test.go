package trash

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/integrity"
	"github.com/peondusud/atoll/internal/watch"
)

// restoredTo restores what is at or below a path to a disk, and gives the outcomes.
func (b *bench) restoredTo(under, to string, apply bool) ([]string, *Summary) {
	b.t.Helper()
	s, err := Open(b.t.Context(), b.db, b.config, b, apply)
	if err != nil {
		b.t.Fatal(err)
	}
	defer s.Close()
	var outcomes []string
	summary, err := s.RestoreTo(b.t.Context(), under, to, func(i *EntryItem) error {
		outcomes = append(outcomes, i.Outcome)
		return nil
	})
	if err != nil {
		b.t.Fatal(err)
	}
	if err := s.Finish(b.t.Context()); err != nil {
		b.t.Fatal(err)
	}
	return outcomes, summary
}

// pruned trashes t3's copy of a, and takes t1's away behind Atoll's back, so that a restore to t1 has the path free.
func pruned(t *testing.T) *bench {
	t.Helper()
	b := newBench(t)
	b.prune()
	if err := os.Remove(filepath.Join(b.dirs["t1"], "media", "a")); err != nil {
		t.Fatal(err)
	}
	return b
}

func (b *bench) read(id, path string) string {
	b.t.Helper()
	data, err := os.ReadFile(filepath.Join(b.dirs[id], "media", filepath.FromSlash(path)))
	if err != nil {
		return ""
	}
	return string(data)
}

func (b *bench) partsOn(id string) int {
	b.t.Helper()
	entries, err := os.ReadDir(filepath.Join(b.dirs[id], "Atoll", "tmp"))
	if err != nil {
		return 0
	}
	return len(entries)
}

// An entry is copied to another disk, the user's copy there, and stays in its trash (specs.md §17.4).
func TestARestoreToCopies(t *testing.T) {
	b := pruned(t)
	outcomes, summary := b.restoredTo("a", "t1", true)
	if len(outcomes) != 1 || outcomes[0] != OutcomeRestored || summary.DiskLost {
		t.Fatalf("%v, %+v", outcomes, summary)
	}
	if b.read("t1", "a") != "x" || b.partsOn("t1") != 0 {
		t.Errorf("t1 holds %q, with %d parts", b.read("t1", "a"), b.partsOn("t1"))
	}
	if _, err := os.Stat(b.inTrash(1, "a")); err != nil {
		t.Errorf("the entry left its trash: %v", err)
	}
	if got := b.text("SELECT created_by FROM copies WHERE disk_id = 't1'"); got != "user" {
		t.Errorf("the copy is %s's", got)
	}
	if got := b.text("SELECT state FROM ops WHERE kind = 'create'"); got != "done" {
		t.Errorf("the operation is %s", got)
	}
}

// The disk whose trash an entry is copied from is the one lost when it fails, late as it may: the folder the copy goes
// in is made like the one in that trash, looked at once the copy is read and verified (specs.md §17.4, invariant 4).
// The disk the copy went to was lost in its place.
func TestARestoreToFromATrashThatFailsLate(t *testing.T) {
	b := newBench(t)
	for _, id := range []string{"t1", "t2", "t3"} {
		b.write(id, "Films/b", "y")
	}
	if _, err := watch.Scan(t.Context(), b.db, b.config, b, watch.Options{Full: true}); err != nil {
		t.Fatal(err)
	}
	b.prune()
	if err := os.RemoveAll(filepath.Join(b.dirs["t1"], "media", "Films")); err != nil {
		t.Fatal(err)
	}
	late := false
	b.at["verified"] = func() { late = true }
	b.fail[b.dirs["t3"]] = func(call, name string) error {
		if late && call == "stat" && name == "Films" {
			return syscall.EIO
		}
		return nil
	}
	outcomes, _ := b.restoredTo("Films/b", "t1", true)
	if len(outcomes) != 1 || outcomes[0] != OutcomeIOError {
		t.Fatalf("%v", outcomes)
	}
	if got := b.text("SELECT state FROM disks WHERE id = 't1'"); got == "missing" {
		t.Error("the disk the copy goes to is lost for an error of the trash it comes from")
	}
	if got := b.text("SELECT state FROM disks WHERE id = 't3'"); got != "missing" {
		t.Errorf("the disk whose trash failed is %s", got)
	}
}

// A dry run says what it would do, and writes nothing.
func TestARestoreToDryRun(t *testing.T) {
	b := pruned(t)
	if outcomes, _ := b.restoredTo("a", "t1", false); len(outcomes) != 1 || outcomes[0] != OutcomeToRestore {
		t.Fatalf("%v", outcomes)
	}
	if b.read("t1", "a") != "" || b.count("SELECT count(*) FROM ops WHERE kind = 'create'") != 0 {
		t.Error("a dry run wrote")
	}
}

// A disk that cannot be written to takes nothing.
func TestARestoreToAReadOnlyDisk(t *testing.T) {
	b := pruned(t)
	b.ro[b.dirs["t1"]] = true
	if outcomes, _ := b.restoredTo("a", "t1", true); len(outcomes) != 1 || outcomes[0] != outcomeUnavailable {
		t.Fatalf("%v", outcomes)
	}
}

// A file of the trash that cannot be read ends the copy as io-error: the part goes, the operation is undone, and the
// entry stays.
func TestARestoreToWhoseSourceFails(t *testing.T) {
	b := pruned(t)
	b.failOn("t3", "read", syscall.EIO)
	outcomes, summary := b.restoredTo("a", "t1", true)
	if len(outcomes) != 1 || outcomes[0] != OutcomeIOError {
		t.Fatalf("%v, %+v", outcomes, summary)
	}
	if b.partsOn("t1") != 0 || b.read("t1", "a") != "" {
		t.Error("a part or a copy stayed")
	}
	if got := b.text("SELECT state FROM ops WHERE kind = 'create'"); got != "undone" {
		t.Errorf("the operation is %s", got)
	}
}

// A file of the trash that is no longer the entry's gives changed, with an op-undone event.
func TestARestoreToOfAnEntryReplaced(t *testing.T) {
	b := pruned(t)
	b.replace(b.inTrash(1, "a"), "y")
	if outcomes, _ := b.restoredTo("a", "t1", true); len(outcomes) != 1 || outcomes[0] != OutcomeChanged {
		t.Fatalf("%v", outcomes)
	}
	if got := b.count("SELECT count(*) FROM events WHERE kind = 'op-undone'"); got != 1 {
		t.Errorf("%d events", got)
	}
	if b.read("t1", "a") != "" {
		t.Error("another file was copied")
	}
}

// A part whose sync fails keeps the operation planned, and the disk is lost; the reprise removes the part, and a new
// run makes the copy.
func TestARestoreToWhosePartFails(t *testing.T) {
	b := pruned(t)
	b.failOn("t1", "filesync", syscall.EIO)
	outcomes, summary := b.restoredTo("a", "t1", true)
	if len(outcomes) != 1 || outcomes[0] != OutcomeIOError || !summary.DiskLost {
		t.Fatalf("%v, %+v", outcomes, summary)
	}
	if got := b.text("SELECT state FROM ops WHERE kind = 'create'"); got != "planned" {
		t.Errorf("the operation is %s", got)
	}
	delete(b.fail, b.dirs["t1"])
	outcomes, _ = b.restoredTo("a", "t1", true)
	if len(outcomes) != 1 || outcomes[0] != OutcomeRestored {
		t.Fatalf("then %v", outcomes)
	}
	if got := b.text("SELECT group_concat(state, ',') FROM (SELECT state FROM ops WHERE kind = 'create' ORDER BY id)"); got != "undone,done" {
		t.Errorf("the operations are %s", got)
	}
	if b.partsOn("t1") != 0 || b.read("t1", "a") != "x" {
		t.Errorf("t1 holds %q, with %d parts", b.read("t1", "a"), b.partsOn("t1"))
	}
}

// A copy from the trash is filled chunk by chunk, each chunk's hash given, and read back chunk by chunk against them:
// a part of two chunks, the last shorter, reads back as written; one read back against fewer hashes than it has
// chunks, against another's, or as a part of another size, does not (specs.md §14.2, step 5).
func TestAPartOfTwoChunksIsFilledAndReadBack(t *testing.T) {
	b := newBench(t)
	content := append(bytes.Repeat([]byte("a"), integrity.ChunkSize), "xyz"...)
	b.write("t3", "big", string(content))
	size := int64(len(content))
	handle, err := b.opened().Handle(t.Context(), "t3")
	if err != nil || handle == nil {
		t.Fatalf("%v, %v", handle, err)
	}
	dir, err := fsx.OpenDir(filepath.Join(b.dirs["t3"], "media"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	source, err := dir.OpenFile("big")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Close() }()
	part, err := handle.MakePart(&catalog.Op{ID: 77, Disk: "t3", Kind: catalog.OpCreate, Path: "big", Path2: "77.part"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = part.Close() }()
	hashes, err := fillPart(t.Context(), part, source, size)
	first, last := integrity.ChunkSum(content[:integrity.ChunkSize]), integrity.ChunkSum(content[integrity.ChunkSize:])
	if err != nil || !bytes.Equal(hashes, append(first[:], last[:]...)) {
		t.Fatalf("the hashes of the chunks: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(b.dirs["t3"], "Atoll", "tmp", "77.part")); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("the part does not hold the file: %v", err)
	}
	whole, ok, err := verifyPart(t.Context(), part, hashes, size)
	want, _, _ := integrity.HashReader(t.Context(), bytes.NewReader(content))
	if err != nil || !ok || !bytes.Equal(whole, want[:]) {
		t.Errorf("as written: %v, %v", ok, err)
	}
	for _, tc := range []struct {
		name   string
		hashes []byte
		size   int64
	}{
		{"fewer hashes", first[:], size},
		{"another's hashes", append(last[:], first[:]...), size},
		{"another size", first[:], integrity.ChunkSize},
	} {
		if _, ok, err := verifyPart(t.Context(), part, tc.hashes, tc.size); err != nil || ok {
			t.Errorf("%s: %v, %v", tc.name, ok, err)
		}
	}
}

// A copy a restore made on another disk is a change of this run, which what reads the copies' rows after it takes in.
func TestARestoreToMarksTheSessionChanged(t *testing.T) {
	b := pruned(t)
	s := b.opened()
	if _, err := s.RestoreTo(t.Context(), "a", "t1", func(*EntryItem) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if b.read("t1", "a") != "x" || !s.Changed() {
		t.Errorf("t1 holds %q; changed %v", b.read("t1", "a"), s.Changed())
	}
}
