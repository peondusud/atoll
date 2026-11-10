package jobs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
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
// gone, and a pass decides nothing of it. Queued again, nothing new comes (specs.md §14.1, §14.3).
func TestReplicateQueuesFirst(t *testing.T) {
	b := replicatePool(t)
	items, _ := b.replicate("a", ReplicateOptions{})
	only(t, items, OutcomeQueued)
	if items, _ := b.replicate("a", ReplicateOptions{}); len(items) != 0 {
		t.Errorf("queued again: %+v", items)
	}
	b.scan(false)
	out := b.settle()
	if len(out.Decisions)+len(out.Operations) != 0 {
		t.Errorf("a pass acted: %+v", out)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE gone_ns IS NOT NULL"); got != 0 {
		t.Errorf("%d clocks", got)
	}
	if got := b.count("SELECT count(*) FROM copies WHERE state = 'pending'") + b.count("SELECT count(*) FROM jobs WHERE state = 'queued'"); got != 2 {
		t.Errorf("the queue went: %d", got)
	}
	if b.exists("t2", "media/a") {
		t.Error("copied without --now")
	}
	items, _ = b.replicate("a", ReplicateOptions{Now: true})
	only(t, items, OutcomeReplicated)
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
		{"a source changed before", func(b *bench) { b.write("t1", "a", "other") }, "changed", "undone", 0, true, "changed"},
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
			if got := b.text("SELECT state FROM ops WHERE kind = 'create' ORDER BY id LIMIT 1"); got != tc.state {
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
		})
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
	if b.exists("t2", "media/b") {
		t.Error("a file beside the path was copied")
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
