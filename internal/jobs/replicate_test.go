package jobs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/trash"
	"github.com/peondusud/atoll/internal/watch"
)

func (b *bench) replicate(path string, opts ReplicateOptions) ([]ReplicateItem, *ReplicateSummary) {
	b.t.Helper()
	opts.Path = path
	var items []ReplicateItem
	sum, err := Replicate(b.t.Context(), b.db, b.config, b, opts, func(i *ReplicateItem) error {
		items = append(items, *i)
		return nil
	})
	if err != nil {
		b.t.Fatal(err)
	}
	return items, sum
}

// replicateStopping runs a replicate that stops at a point, as kill -9 would.
func (b *bench) replicateStopping(path, point string) {
	b.t.Helper()
	stop := errors.New("stopped")
	b.at[point] = func() { panic(stop) } //nolint:forbidigo // what stands for kill -9
	defer func() {
		if err, ok := recover().(error); !ok || !errors.Is(err, stop) {
			b.t.Fatalf("it did not stop at %s: %v", point, err)
		}
	}()
	_, _ = Replicate(b.t.Context(), b.db, b.config, b, ReplicateOptions{Path: path, Now: true}, func(*ReplicateItem) error { return nil })
}

func (b *bench) read(id, path string) string {
	b.t.Helper()
	data, err := os.ReadFile(filepath.Join(b.dirs[id], "media", path))
	if err != nil {
		b.t.Fatal(err)
	}
	return string(data)
}

// parts lists what a disk's tmp/ holds.
func (b *bench) parts(id string) []string {
	b.t.Helper()
	entries, err := os.ReadDir(filepath.Join(b.dirs[id], "Atoll", "tmp"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		b.t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func replicatePool(t *testing.T) *bench {
	t.Helper()
	b := newBench(t, "t1", "t2")
	b.write("t1", "a", "content")
	b.scan(false)
	return b
}

func only(t *testing.T, items []ReplicateItem, outcome string) {
	t.Helper()
	if len(items) != 1 || items[0].Path != "a" || items[0].Disk != "t2" || items[0].Source != "t1" || items[0].Outcome != outcome {
		t.Fatalf("%+v, want %s", items, outcome)
	}
}

// A source that fails once the copy is read and verified is the disk that is lost, not the target: the folder a copy
// goes in is made like the source's, which is looked at then (specs.md §14.2, invariant 4). The healthy target was
// lost in its place, every other job of the run with it, and the failing source stayed in service.
func TestASourceThatFailsLateIsTheOneLost(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "Films/a", "content")
	b.scan(false)
	late := false
	b.at["verified"] = func() { late = true }
	b.fail[b.dirs["t1"]] = func(call, name string) error {
		if late && call == "stat" && name == "Films" {
			return syscall.EIO
		}
		return nil
	}
	items, _ := b.replicate("Films/a", ReplicateOptions{Now: true})
	if len(items) != 1 || items[0].Outcome != trash.OutcomeIOError {
		t.Fatalf("%+v", items)
	}
	if got := b.text("SELECT state FROM disks WHERE id = 't2'"); got == "missing" {
		t.Error("the target is lost for an error of the source")
	}
	if got := b.text("SELECT state FROM disks WHERE id = 't1'"); got != "missing" {
		t.Errorf("the source that failed is %s", got)
	}
	if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
		t.Errorf("%d operations left planned on a target that did not fail", got)
	}
}

// The folder of the parts is closed to others, one an earlier version left open included, and a part takes its owner
// before its mode (specs.md §14.2). A part carries its source's mode and owner while it is read again, which takes as
// long as the file is large: in a folder anyone may enter, a file that only its folders kept private was open to every
// account for that time. And a mode set first lost its setuid and setgid bits to the change of owner that followed:
// the copy of such a file was made without them.
func TestAPartIsPrivateAndTakesItsOwnerFirst(t *testing.T) {
	for _, left := range []bool{false, true} {
		b := newBench(t, "t1", "t2")
		b.write("t1", "a", "content")
		if err := os.Chmod(filepath.Join(b.dirs["t1"], "media", "a"), 0o755|os.ModeSetuid); err != nil {
			t.Fatal(err)
		}
		tmp := filepath.Join(b.dirs["t2"], "Atoll", "tmp")
		if left {
			if err := os.MkdirAll(tmp, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(tmp, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		b.scan(false)
		items, _ := b.replicate("a", ReplicateOptions{Now: true})
		only(t, items, OutcomeReplicated)
		if st, err := os.Stat(tmp); err != nil || st.Mode().Perm()&0o077 != 0 {
			t.Errorf("left open before %v: tmp/ is %v, %v", left, st.Mode(), err)
		}
		if st, err := os.Stat(filepath.Join(b.dirs["t2"], "media", "a")); err != nil || st.Mode() != 0o755|os.ModeSetuid {
			t.Errorf("left open before %v: the copy's mode is %v, %v", left, st.Mode(), err)
		}
	}
}

// A copy made: the same bytes and date at the same path, a copy of Atoll's, verified, with its source,
// and the file's hash; nothing left of the job, of its operation or in tmp/ (specs.md §14.2).
func TestReplicateMakesTheCopy(t *testing.T) {
	b := replicatePool(t)
	items, sum := b.replicate("a", ReplicateOptions{Now: true})
	only(t, items, OutcomeReplicated)
	if sum.Failed != 0 || sum.DiskLost || sum.Bytes != 7 {
		t.Errorf("%+v", sum)
	}
	if got := b.read("t2", "a"); got != "content" {
		t.Errorf("%q", got)
	}
	src, err := os.Stat(filepath.Join(b.dirs["t1"], "media", "a"))
	if err != nil {
		t.Fatal(err)
	}
	dst, err := os.Stat(filepath.Join(b.dirs["t2"], "media", "a"))
	if err != nil {
		t.Fatal(err)
	}
	if !dst.ModTime().Equal(src.ModTime()) || dst.Mode() != src.Mode() {
		t.Errorf("%v %v, want %v %v", dst.ModTime(), dst.Mode(), src.ModTime(), src.Mode())
	}
	if got := b.count("SELECT count(*) FROM copies WHERE verified_ns IS NOT NULL AND state = 'ok'"); got != 2 {
		t.Errorf("%d copies verified", got)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND created_by = 'atoll'"); got != 1 {
		t.Errorf("%d copies of Atoll's", got)
	}
	if got := b.count("SELECT count(*) FROM files WHERE blake3 IS NOT NULL AND state = 'ok'"); got != 1 {
		t.Errorf("%d files hashed and ok", got)
	}
	if got := b.count("SELECT count(*) FROM jobs") + b.count("SELECT count(*) FROM ops WHERE state <> 'done'"); got != 0 {
		t.Errorf("%d left", got)
	}
	if parts := b.parts("t2"); len(parts) != 0 {
		t.Errorf("tmp/ holds %q", parts)
	}
}

// Without --now a copy is only queued, and its pending copy is nothing on its disk: a scan does not find it
// gone, and a pass decides nothing of it, then runs it, as it is due. Queued again, nothing new comes
// (specs.md §14.1, §14.3, §15.12).
func TestReplicateQueuesFirst(t *testing.T) {
	b := replicatePool(t)
	items, _ := b.replicate("a", ReplicateOptions{})
	only(t, items, OutcomeQueued)
	if items, _ := b.replicate("a", ReplicateOptions{}); len(items) != 0 {
		t.Errorf("queued again: %+v", items)
	}
	// A full scan reads nothing of it, and finds nothing missing.
	full, err := watch.Scan(b.t.Context(), b.db, b.config, b, watch.Options{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := range full.Disks {
		if d := &full.Disks[i]; len(d.Unreadable)+len(d.Absent) != 0 {
			t.Errorf("%s: %+v", d.ID, d)
		}
	}
	if b.exists("t2", "media/a") {
		t.Error("copied without --now")
	}
	out := b.pass()
	want := []Operation{{Disk: "t2", Kind: KindReplicate, Path: "a", Source: "t1", Outcome: OutcomeReplicated}}
	if len(out.Decisions) != 0 || !slices.Equal(out.Operations, want) {
		t.Errorf("decisions %+v, operations %+v, want %+v", out.Decisions, out.Operations, want)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE gone_ns IS NOT NULL"); got != 0 {
		t.Errorf("%d clocks", got)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE state = 'pending'") + b.count("SELECT count(*) FROM jobs"); got != 0 {
		t.Errorf("%d left of the queue", got)
	}
	if !b.exists("t2", "media/a") {
		t.Error("the pass did not copy it")
	}
	if items, _ := b.replicate("a", ReplicateOptions{Now: true}); len(items) != 0 {
		t.Errorf("copied again: %+v", items)
	}
}

// A dry run writes nothing, and lists what is queued already besides what it would queue.
func TestReplicateDryRun(t *testing.T) {
	b := replicatePool(t)
	items, sum := b.replicate("a", ReplicateOptions{Now: true, DryRun: true})
	only(t, items, OutcomeToReplicate)
	if sum.Applied || b.count("SELECT count(*) FROM jobs")+b.count("SELECT count(*) FROM ops") != 0 {
		t.Errorf("%+v, it wrote", sum)
	}
	b.replicate("a", ReplicateOptions{})
	items, _ = b.replicate("a", ReplicateOptions{DryRun: true})
	only(t, items, OutcomeToReplicate)
}

// What each failure leaves, and what the next run makes of it once the disk answers again (specs.md §14.1,
// §14.2).
func TestReplicateFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arm     func(b *bench)
		outcome string
		// what is left: the operation's state, parts in tmp/, a queued job, a pending copy
		state string
		parts int
		job   bool
		// what the next run gives, once nothing fails: "" when it has nothing left to run
		then string
	}{
		{"no room", failOn("t2", "write", syscall.ENOSPC), OutcomeNoRoom, "undone", 0, true, OutcomeReplicated},
		// Making the part can find no room too, with no block or no inode left: the same outcome, and no I/O error.
		{"no room for the part", failOn("t2", "create", syscall.ENOSPC), OutcomeNoRoom, "undone", 0, true, OutcomeReplicated},
		{"no room for the part's folder", failOn("t2", "mkdir", syscall.ENOSPC), OutcomeNoRoom, "undone", 0, true, OutcomeReplicated},
		{"a disk lost while writing", failOn("t2", "write", syscall.EIO), "io-error", "planned", 1, true, OutcomeReplicated},
		{"a disk lost at the part's sync", failOn("t2", "filesync", syscall.EIO), "io-error", "planned", 1, true, OutcomeReplicated},
		{"a disk lost at the rename", failOn("t2", "rename", syscall.EIO), "io-error", "planned", 1, true, OutcomeReplicated},
		{"a disk lost after the move", at("moved", failOn("t2", "sync", syscall.EIO)), "io-error", "planned", 0, true, ""},
		{"a name taken since the look", at("verified", func(b *bench) { b.write("t2", "a", "theirs") }), OutcomeConflict, "undone", 0, false, ""},
		{"a name refused", func(b *bench) {
			b.fail[b.dirs["t2"]] = func(call, _ string) error {
				if call == "rename" || call == "link" {
					return syscall.EINVAL
				}
				return nil
			}
		}, "name-refused", "undone", 0, false, OutcomeReplicated},
		// Until a scan records what the source is now, it is not what its row says.
		{"a source changed after the copy", at("copied", func(b *bench) { b.write("t1", "a", "changed!") }), "changed", "undone", 0, true, "changed"},
		// A source looked at before anything is journaled: no operation, no part, and the job waits settle (§14.2).
		{"a source changed before", func(b *bench) { b.write("t1", "a", "other") }, "changed", "", 0, true, "changed"},
		{"a source gone before", func(b *bench) {
			if err := os.Remove(filepath.Join(b.dirs["t1"], "media", "a")); err != nil {
				b.t.Fatal(err)
			}
		}, "changed", "", 0, true, "changed"},
		{"a disk lost when its cache is dropped", failOn("t2", "dropcache", syscall.EIO), "io-error", "planned", 1, true, OutcomeReplicated},
		// The part is synced once made, and again once its times are restored: this is the second.
		{"a disk lost at the part's last sync", func(b *bench) {
			syncs := 0
			b.fail[b.dirs["t2"]] = func(call, _ string) error {
				if call == "filesync" {
					if syncs++; syncs == 2 {
						return syscall.EIO
					}
				}
				return nil
			}
		}, "io-error", "planned", 1, true, OutcomeReplicated},
		{"a source edited in place at the same size", at("copied", func(b *bench) { b.write("t1", "a", "CONTENT") }), "changed", "undone", 0, true, "changed"},
		{"a part longer than its source", at("copied", func(b *bench) {
			f, err := os.OpenFile(filepath.Join(b.dirs["t2"], "Atoll", "tmp", "1.part"), os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				b.t.Fatal(err)
			}
			_, _ = f.Write([]byte("more"))
			_ = f.Close()
		}), "io-error", "undone", 0, true, OutcomeReplicated},
		{"a source that no longer qualifies", func(b *bench) {
			b.replicate("a", ReplicateOptions{})
			b.exec("UPDATE copies SET state = 'stale' WHERE disk_id = 't1'")
		}, "changed", "", 0, false, ""},
		{"a copy that reads back wrong", at("copied", func(b *bench) {
			if err := os.WriteFile(filepath.Join(b.dirs["t2"], "Atoll", "tmp", "1.part"), []byte("CONTENT"), 0o600); err != nil {
				b.t.Fatal(err)
			}
		}), "io-error", "undone", 0, true, OutcomeReplicated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := replicatePool(t)
			tc.arm(b)
			items, sum := b.replicate("a", ReplicateOptions{Now: true})
			only(t, items, tc.outcome)
			if sum.Failed != 1 {
				t.Errorf("%+v", sum)
			}
			if got := b.text("SELECT COALESCE((SELECT state FROM ops WHERE kind = 'create' ORDER BY id LIMIT 1), '')"); got != tc.state {
				t.Errorf("the operation is %q", got)
			}
			if parts := b.parts("t2"); len(parts) != tc.parts {
				t.Errorf("tmp/ holds %q", parts)
			}
			job := b.count("SELECT count(*) FROM jobs WHERE kind = 'replicate'") == 1
			pending := b.count("SELECT count(*) FROM copies WHERE state = 'pending'") == 1
			if job != tc.job || pending != tc.job {
				t.Errorf("job %v, pending copy %v", job, pending)
			}
			// The next run, with nothing failing. What took the name is a copy for a scan to find: then the disk
			// holds one, and nothing is queued for it.
			delete(b.fail, b.dirs["t2"])
			b.now += 3_600_000_000_000
			if tc.outcome == OutcomeConflict {
				b.scan(false)
			}
			items, _ = b.replicate("a", ReplicateOptions{Now: true})
			switch {
			case tc.then == "" && len(items) != 0:
				t.Errorf("then %+v", items)
			case tc.then != "":
				only(t, items, tc.then)
			}
			if tc.outcome == OutcomeConflict {
				if got := b.read("t2", "a"); got != "theirs" {
					t.Errorf("what took the name: %q", got)
				}
			}
			if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
				t.Errorf("%d operations left planned", got)
			}
			// A copy made, by this run or by its reprise, is verified: its hashes were recorded before it moved.
			if b.exists("t2", "media/a") && tc.outcome != OutcomeConflict {
				if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND verified_ns IS NOT NULL AND created_by = 'atoll'"); got != 1 {
					t.Errorf("the copy made is not verified")
				}
			}
		})
	}
}

// A source that changed waits for settle before its job runs again (specs.md §14.1).
func TestAChangedSourceWaitsForSettle(t *testing.T) {
	b := replicatePool(t)
	b.at["copied"] = func() { b.write("t1", "a", "changed!") }
	b.replicate("a", ReplicateOptions{Now: true})
	if got := b.count("SELECT count(*) FROM jobs WHERE not_before_ns = ?", b.now+600_000_000_000); got != 1 {
		t.Errorf("%d jobs waiting for settle", got)
	}
}

// A source whose bytes do not read as its file's hash is not copied: what it holds is not the file's
// version (specs.md §14.2).
func TestASourceThatDoesNotReadAsItsFile(t *testing.T) {
	b := replicatePool(t)
	b.scan(true)
	b.exec("UPDATE files SET blake3 = zeroblob(32)")
	items, _ := b.replicate("a", ReplicateOptions{Now: true})
	only(t, items, "changed")
	if b.exists("t2", "media/a") || len(b.parts("t2")) != 0 {
		t.Error("the copy was kept")
	}
}

// The parents of a copy are made like the same folders on the source's disk, each as an operation of its
// own (specs.md §14.2).
func TestAParentIsMadeLikeTheSources(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "p/a", "content")
	if err := os.Chmod(filepath.Join(b.dirs["t1"], "media", "p"), 0o750); err != nil {
		t.Fatal(err)
	}
	b.scan(false)
	items, _ := b.replicate("p", ReplicateOptions{Now: true})
	if len(items) != 1 || items[0].Outcome != OutcomeReplicated {
		t.Fatalf("%+v", items)
	}
	st, err := os.Stat(filepath.Join(b.dirs["t2"], "media", "p"))
	if err != nil || st.Mode().Perm() != 0o750 {
		t.Errorf("%v, %v", st, err)
	}
	if got := b.count("SELECT count(*) FROM ops WHERE kind = 'mkdir' AND state = 'done' AND CAST(path AS TEXT) = 'p'"); got != 1 {
		t.Errorf("%d folders journaled", got)
	}
}

// A copy in progress is private, and the folder it lands in keeps its date (specs.md §6.1, item 8, §14.2).
func TestAPartIsPrivateAndTheFolderKeepsItsDate(t *testing.T) {
	b := replicatePool(t)
	media := filepath.Join(b.dirs["t2"], "media")
	when := time.Unix(1400000000, 0)
	if err := os.Chtimes(media, when, when); err != nil {
		t.Fatal(err)
	}
	b.at["copied"] = func() {
		st, err := os.Stat(filepath.Join(b.dirs["t2"], "Atoll", "tmp", "1.part"))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("the part: %v, %v", st, err)
		}
	}
	b.replicate("a", ReplicateOptions{Now: true})
	if st, err := os.Stat(media); err != nil || !st.ModTime().Equal(when) {
		t.Errorf("the folder: %v, %v", st, err)
	}
}

// A job keeps its part from one run to the next, so that the next resumes it without reading the source
// again; a part that an operation done or undone names, and that no job owns, goes when a command that
// writes opens its disk, and nothing else in tmp/ is touched (specs.md §14.2).
func TestPartsBetweenRuns(t *testing.T) {
	t.Run("kept", func(t *testing.T) {
		b := newBench(t, "t1", "t2")
		if err := os.WriteFile(filepath.Join(b.dirs["t1"], "media", "a"), bytes.Repeat([]byte("x"), 2*4<<20+10), 0o644); err != nil {
			t.Fatal(err)
		}
		b.scan(false)
		once := true
		b.fail[b.dirs["t2"]] = func(call, _ string) error {
			if call == "settimes" && once {
				once = false
				return syscall.EACCES
			}
			return nil
		}
		items, _ := b.replicate("a", ReplicateOptions{Now: true})
		only(t, items, "io-error")
		reads := 0
		b.fail[b.dirs["t1"]] = func(call, _ string) error {
			if call == "read" {
				reads++
			}
			return nil
		}
		items, _ = b.replicate("a", ReplicateOptions{Now: true})
		only(t, items, OutcomeReplicated)
		if reads != 0 {
			t.Errorf("the source was read %d times", reads)
		}
	})
	t.Run("orphaned", func(t *testing.T) {
		b := replicatePool(t)
		tmp := filepath.Join(b.dirs["t2"], "Atoll", "tmp")
		if err := os.MkdirAll(tmp, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"7.part", "x.part", "someone's"} {
			if err := os.WriteFile(filepath.Join(tmp, name), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		b.exec("INSERT INTO ops(id, disk_id, kind, path, path2, started_ns, state) VALUES (7, 't2', 'create', CAST('z' AS BLOB), CAST('7.part' AS BLOB), 1, 'undone')")
		items, _ := b.replicate("a", ReplicateOptions{Now: true})
		only(t, items, OutcomeReplicated)
		if got := b.parts("t2"); len(got) != 2 || got[0] != "someone's" || got[1] != "x.part" {
			t.Errorf("tmp/ holds %q", got)
		}
	})
}

// A job whose operation the reprise could not conclude waits: here both names lead to the copy, and the part's
// name is one unlink cannot remove, as ntfs3 leaves after a device fails between the syncs of the two
// folders. The copy at the target is the job's own, never a conflict (specs.md §17.2).
func TestAJobWaitsForItsOperation(t *testing.T) {
	b := replicatePool(t)
	refused := func(call, name string) error {
		if call == "rename" {
			return syscall.EINVAL
		}
		return nil
	}
	b.fail[b.dirs["t2"]] = refused
	b.replicateStopping("a", "linked")
	b.fail[b.dirs["t2"]] = func(call, name string) error {
		if call == "unlink" && name == "1.part" {
			return syscall.ENOENT
		}
		return refused(call, name)
	}
	items, _ := b.replicate("a", ReplicateOptions{Now: true})
	only(t, items, "io-error")
	if got := b.text("SELECT state FROM ops WHERE kind = 'create'"); got != "planned" {
		t.Errorf("the operation is %q", got)
	}
	if got := b.count("SELECT count(*) FROM jobs") + b.count("SELECT count(*) FROM copies WHERE state = 'pending'"); got != 2 {
		t.Errorf("%d of the job and its pending copy left", got)
	}
	if got := b.read("t2", "a"); got != "content" {
		t.Errorf("%q", got)
	}
	// Once the volume is repaired, the reprise finishes it.
	b.fail[b.dirs["t2"]] = refused
	items, _ = b.replicate("a", ReplicateOptions{Now: true})
	if len(items) != 0 || b.text("SELECT state FROM ops WHERE kind = 'create'") != "done" {
		t.Errorf("%+v", items)
	}
}

// A copy is synced at its new name, then its folder, then the folder of its part (specs.md §14.2).
func TestTheSyncsOfACopy(t *testing.T) {
	b := replicatePool(t)
	syncs := b.recordSyncs("t2")
	b.replicate("a", ReplicateOptions{Now: true})
	at := -1
	for i, s := range *syncs {
		if s == "entry a" {
			at = i
		}
	}
	if at < 0 || len(*syncs) < at+3 || (*syncs)[at+1] != "media" || (*syncs)[at+2] != "tmp" {
		t.Errorf("%q", *syncs)
	}
}

func failOn(id, call string, err error) func(b *bench) {
	return func(b *bench) {
		b.fail[b.dirs[id]] = func(c, _ string) error {
			if c == call {
				return err
			}
			return nil
		}
	}
}

func at(point string, then func(b *bench)) func(b *bench) {
	return func(b *bench) {
		b.at[point] = func() { then(b) }
	}
}

// A parent of the target that is a file: the copy ends, and the next plan decides again.
func TestReplicateWithAParentThatIsAFile(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "d/a", "content")
	b.scan(false)
	b.write("t2", "d", "a file")
	items, _ := b.replicate("d", ReplicateOptions{Now: true})
	if len(items) != 1 || items[0].Outcome != "path-taken" {
		t.Fatalf("%+v", items)
	}
	if got := b.count("SELECT count(*) FROM jobs") + b.count("SELECT count(*) FROM copies WHERE state = 'pending'"); got != 0 {
		t.Errorf("%d left", got)
	}
}

// A resume reads the part against the hashes recorded, keeps what matches, and reads the source only from
// the first chunk missing or wrong (specs.md §14).
func TestReplicateResumesFromTheFirstWrongChunk(t *testing.T) {
	b := newBench(t, "t1", "t2")
	content := bytes.Repeat([]byte("0123456789abcdef"), (2*4<<20+100)/16)
	if err := os.WriteFile(filepath.Join(b.dirs["t1"], "media", "a"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	b.scan(false)
	b.replicateStopping("a", "copied")
	part := filepath.Join(b.dirs["t2"], "Atoll", "tmp", "1.part")
	f, err := os.OpenFile(part, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("WRONG"), 4<<20+10); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	reads := 0
	b.fail[b.dirs["t1"]] = func(call, _ string) error {
		if call == "read" {
			reads++
		}
		return nil
	}
	items, _ := b.replicate("a", ReplicateOptions{Now: true})
	only(t, items, OutcomeReplicated)
	if reads != 2 {
		t.Errorf("the source was read %d times, from its first chunk", reads)
	}
	data, err := os.ReadFile(filepath.Join(b.dirs["t2"], "media", "a"))
	if err != nil || !bytes.Equal(data, content) {
		t.Errorf("the copy differs: %d bytes, %v", len(data), err)
	}
	if got := b.text("SELECT CAST(path2 AS TEXT) FROM ops WHERE state = 'done'"); got != "1.part" {
		t.Errorf("resumed as %q", got)
	}
}

// A pending copy goes with its file when the file's last copy is deleted, and follows a rename, as its job
// finds the file's path when it runs (specs.md §14.1, §14.2).
func TestAPendingCopyAndPropagation(t *testing.T) {
	t.Run("a deletion", func(t *testing.T) {
		b := replicatePool(t)
		b.replicate("a", ReplicateOptions{})
		b.remove("t1", "a")
		b.settle()
		if got := b.count("SELECT count(*) FROM files") + b.count("SELECT count(*) FROM jobs") + b.count("SELECT count(*) FROM copies"); got != 0 {
			t.Errorf("%d rows left", got)
		}
	})
	t.Run("a rename", func(t *testing.T) {
		b := replicatePool(t)
		b.replicate("a", ReplicateOptions{})
		b.rename("t1", "a", "b")
		b.settle()
		items, _ := b.replicate("b", ReplicateOptions{Now: true})
		if len(items) != 1 || items[0].Path != "b" || items[0].Outcome != OutcomeReplicated {
			t.Fatalf("%+v", items)
		}
		if got := b.read("t2", "b"); got != "content" || b.exists("t2", "media/a") {
			t.Errorf("%q", got)
		}
	})
}

// A pending copy the plan no longer keeps is canceled, with its job: here its disk became readonly.
func TestAPlanCancelsAPendingCopy(t *testing.T) {
	b := replicatePool(t)
	b.replicate("a", ReplicateOptions{})
	b.ro[b.dirs["t2"]] = true
	b.scan(false)
	if got := b.count("SELECT count(*) FROM jobs") + b.count("SELECT count(*) FROM copies WHERE state = 'pending'"); got != 0 {
		t.Errorf("%d left", got)
	}
}

// A manual file waits for atoll replicate, which releases what is below its path only (specs.md §12.3).
func TestReplicateReleasesManualFiles(t *testing.T) {
	b := newBenchWith(t, "[defaults]\nmode = \"manual\"\n", "t1", "t2")
	b.write("t1", "a", "content")
	b.write("t1", "b", "content")
	b.scan(false)
	if got := b.count("SELECT count(*) FROM files WHERE state = 'unprotected'"); got != 2 {
		t.Errorf("%d files waiting", got)
	}
	items, _ := b.replicate("a", ReplicateOptions{Now: true})
	only(t, items, OutcomeReplicated)
	if b.exists("t2", "media/b") || b.count("SELECT count(*) FROM jobs") != 0 {
		t.Error("a file beside the path was queued")
	}
}

// --force runs what --now runs (specs.md §14.3).
func TestReplicateForceRuns(t *testing.T) {
	b := replicatePool(t)
	items, _ := b.replicate("a", ReplicateOptions{Force: true})
	only(t, items, OutcomeReplicated)
}

// The chunk hashes are recorded 64 MiB at a time as the copy goes, so that a copy stopped past them resumes
// from there (specs.md §14.2).
func TestChunksAreRecordedAsTheCopyGoes(t *testing.T) {
	b := newBench(t, "t1", "t2")
	if err := os.WriteFile(filepath.Join(b.dirs["t1"], "media", "a"), bytes.Repeat([]byte("y"), 17*4<<20+10), 0o644); err != nil {
		t.Fatal(err)
	}
	b.scan(false)
	reads := 0
	b.fail[b.dirs["t1"]] = func(call, _ string) error {
		if call == "read" {
			if reads++; reads == 17 {
				return syscall.EACCES
			}
		}
		return nil
	}
	items, _ := b.replicate("a", ReplicateOptions{Now: true})
	only(t, items, "io-error")
	reads = 0
	b.fail[b.dirs["t1"]] = func(call, _ string) error {
		if call == "read" {
			reads++
		}
		return nil
	}
	items, _ = b.replicate("a", ReplicateOptions{Now: true})
	only(t, items, OutcomeReplicated)
	if reads != 2 {
		t.Errorf("the source was read %d times: from chunk %d", reads, 18-reads)
	}
}

// A walk that meets an operation create the reprise could not conclude leaves the copy at its target to that
// reprise: it is not a file of the user's, and the job and its pending copy stay (specs.md §17.2).
func TestAWalkLeavesAPlannedCopyAlone(t *testing.T) {
	b := replicatePool(t)
	refused := func(call, name string) error {
		if call == "rename" {
			return syscall.EINVAL
		}
		return nil
	}
	b.fail[b.dirs["t2"]] = refused
	b.replicateStopping("a", "linked")
	b.fail[b.dirs["t2"]] = func(call, name string) error {
		if call == "unlink" && name == "1.part" {
			return syscall.ENOENT
		}
		return refused(call, name)
	}
	b.pass()
	if got := b.count("SELECT count(*) FROM jobs") + b.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND state = 'pending'"); got != 2 {
		t.Errorf("%d of the job and its pending copy left", got)
	}
	b.fail[b.dirs["t2"]] = refused
	b.replicate("a", ReplicateOptions{Now: true})
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND state = 'ok' AND verified_ns IS NOT NULL"); got != 1 {
		t.Errorf("%d copies verified", got)
	}
}

// --force does what --now does, and re-reads the copies Atoll made: one that reads as its file's hash is
// verified again, and one that does not is no longer (specs.md §14.3, §15.10).
func TestReplicateForceRereadsReplicas(t *testing.T) {
	b := replicatePool(t)
	b.replicate("a", ReplicateOptions{Now: true})
	b.exec("UPDATE copies SET verified_ns = NULL")
	b.now += 60_000_000_000
	if items, _ := b.replicate("a", ReplicateOptions{Force: true}); len(items) != 0 {
		t.Errorf("%+v", items)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND verified_ns = ?", b.now); got != 1 {
		t.Errorf("%d re-read", got)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't1' AND verified_ns IS NULL"); got != 1 {
		t.Errorf("a user's copy was read: %d", got)
	}
	// The replica's bytes changed, with its size and date kept, and its row told of its new change time: what
	// the disk itself would have done, where only a read can tell.
	full := filepath.Join(b.dirs["t2"], "media", "a")
	st, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(full, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	var now syscall.Stat_t
	if err := syscall.Stat(full, &now); err != nil {
		t.Fatal(err)
	}
	b.exec("UPDATE copies SET ctime_ns = ? WHERE disk_id = 't2'", now.Ctim.Sec*1_000_000_000+now.Ctim.Nsec)
	b.now += 60_000_000_000
	b.replicate("a", ReplicateOptions{Force: true})
	if got := b.count("SELECT count(*) FROM copies WHERE disk_id = 't2' AND verified_ns IS NOT NULL"); got != 0 {
		t.Errorf("a replica that reads otherwise stays verified")
	}
}

// With replication.enabled = false nothing is queued or copied (specs.md §14.3, §21.4).
func TestReplicationDisabled(t *testing.T) {
	b := newBenchWith(t, "[replication]\nenabled = false\n", "t1", "t2")
	b.write("t1", "a", "content")
	b.scan(false)
	items, sum := b.replicate("a", ReplicateOptions{Now: true})
	if len(items) != 0 || !sum.Disabled || b.count("SELECT count(*) FROM jobs") != 0 {
		t.Errorf("%+v, %+v", items, sum)
	}
}
