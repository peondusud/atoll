package jobs

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// A pass sweep runs a pass that propagates what a user changed, once with nothing failing, counting each call it makes
// through the handle on one disk's top, then once for each of those calls, on a new pool each time, with that call
// failing: with EIO, what a disk that cannot be relied on answers, then with EACCES, a refusal that says nothing about
// the disk nor about the name (CLAUDE.md, "Testing rules": fault injection at each step). With crash set, a pass stops
// at that point first, as kill -9 would, and the calls that fail are the reprise's. Whatever fails, and wherever:
//   - what a disk answers is an outcome of the pass, never an end of the command;
//   - once the disk answers again, passes end where the pass with nothing failing ends: the same files, at the same
//     names, with the same bytes, on each disk and in each trash, no operation left planned, and the same rows. Nothing
//     is lost, nothing is left behind, and nothing is done twice (invariants 1, 4, 10, 12; specs.md §17.2).
type passCase struct {
	name string
	// pool makes the pool, scanned, with what the user changed since, whose first pass has run: the next one acts.
	pool func(t *testing.T) *bench
	// disk is the disk whose calls fail.
	disk string
	// crash is a point a pass stops at before, "" for none.
	crash string
	// needs names calls the pass makes on the disk with nothing failing: what says it does what the case is about.
	needs []string
	// from is the call from which on the sweep fails calls, the first of that kind; "" for the first call.
	from string
	// act is what runs on the catalog given: a pass when nil, as the daemon's.
	act func(b *bench, db *sql.DB) error
}

// endsAsWanted says whether a pool ends as another, from their snapshots: the same lines, but for the trash's (marked
// ~), of which it may hold more, never fewer. A fault can leave more there, nothing lost: an old copy a refresh sent
// there, as a copy on a disk lost when its metadata changed becomes stale and is made again (specs.md §15.4), or the
// SSD cache's copy a transfer sent there, which stays for the retention when a fault comes between its move and its
// purge, as its reprise ends it as any move to the trash (§19), a limit.
func endsAsWanted(got, want string) bool {
	split := func(s string) (main, trash []string) {
		for _, line := range strings.Split(s, "\n") {
			if strings.HasPrefix(line, "~") {
				trash = append(trash, line)
			} else {
				main = append(main, line)
			}
		}
		return main, trash
	}
	gotMain, gotTrash := split(got)
	wantMain, wantTrash := split(want)
	if !slices.Equal(gotMain, wantMain) {
		return false
	}
	for _, line := range wantTrash {
		at := slices.Index(gotTrash, line)
		if at < 0 {
			return false
		}
		gotTrash = slices.Delete(gotTrash, at, at+1)
	}
	return true
}

// run runs a case's act on a catalog.
func (c *passCase) run(b *bench, db *sql.DB) error {
	if c.act != nil {
		return c.act(b, db)
	}
	_, err := Run(b.t.Context(), db, b.config, b, true)
	return err
}

// replicateNow is atoll replicate --now of a path, as a case's act.
func replicateNow(path string) func(b *bench, db *sql.DB) error {
	return func(b *bench, db *sql.DB) error {
		_, err := Replicate(b.t.Context(), db, b.config, b, ReplicateOptions{Path: path, Now: true}, func(*ReplicateItem) error { return nil })
		return err
	}
}

// copiesToMake gives a pool of two disks, t1 holding a file and a folder of two that t2 lacks, scanned: atoll replicate
// copies them, the folders made like t1's (specs.md §14.2, §14.3).
func copiesToMake(t *testing.T) *bench {
	t.Helper()
	b := newBench(t, "t1", "t2")
	b.write("t1", "a", "content")
	b.write("t1", "Films/Deep/b", "film b")
	b.write("t1", "Films/Deep/c", "film c")
	b.scan(false)
	return b
}

// runPass runs a case with the call at a position failing with inject (0 fails none), then passes with nothing failing
// until all is done, and gives the calls, what the pool ends with, and the error of the pass that failed.
func runPass(t *testing.T, c *passCase, at int, inject error) (calls []string, end string, runErr error) {
	t.Helper()
	b := c.pool(t)
	if c.crash != "" {
		b.crashAt(c.crash)
	}
	armed := true
	b.fail[b.dirs[c.disk]] = func(call, name string) error {
		if !armed {
			return nil
		}
		calls = append(calls, call+" "+name)
		if len(calls) == at {
			return inject
		}
		return nil
	}
	runErr = c.run(b, b.db)
	armed = false
	return calls, b.settled(false), runErr
}

// sweepStride is how many calls of a pass a sweep with the race detector takes for one it fails.
const sweepStride = 8

// sweepPass runs a pass sweep, beside the others of its test: each run has a pool of its own.
func sweepPass(t *testing.T, c passCase) {
	t.Helper()
	t.Run(c.name, func(t *testing.T) {
		t.Parallel()
		calls, want, err := runPass(t, &c, 0, nil)
		if err != nil {
			t.Fatalf("with nothing failing: %v", err)
		}
		for _, need := range c.needs {
			if !slices.ContainsFunc(calls, func(call string) bool { return strings.HasPrefix(call, need+" ") }) {
				t.Fatalf("with nothing failing, no %s: %q", need, calls)
			}
		}
		first := 0
		if c.from != "" {
			if first = slices.IndexFunc(calls, func(call string) bool { return strings.HasPrefix(call, c.from+" ") }); first < 0 {
				t.Fatalf("with nothing failing, no %s: %q", c.from, calls)
			}
		}
		for i, call := range calls[first:] {
			i += first
			for k, inject := range []error{syscall.EIO, syscall.EACCES} {
				if raceBuild && (i+2*k)%sweepStride != 0 {
					continue
				}
				where := fmt.Sprintf("call %d of %d (%s) failing with %v", i+1, len(calls), call, inject)
				_, got, err := runPass(t, &c, i+1, inject)
				if err != nil {
					t.Errorf("%s: the pass ended with %v", where, err)
				}
				if !endsAsWanted(got, want) {
					t.Errorf("%s: it ends otherwise:\n%s\nwhere nothing failing ends:\n%s", where, got, want)
				}
			}
		}
	})
}

// snapshot says what the pool holds: on each disk, each name below its root and what it holds, each entry of its trash
// by the logical path below its operation's folder, marked ~, unless the trash is left out, and what is in its tmp/;
// then the operations left planned, and the rows of the files and their copies. What differs between two runs that end
// the same, the times, the inodes, the ids and dates of operations, is left out.
func (b *bench) snapshot(trashLeftOut bool) string {
	b.t.Helper()
	var lines []string
	for _, id := range slices.Sorted(maps.Keys(b.dirs)) {
		lines = append(lines, b.tree(id, "media", 0)...)
		if !trashLeftOut {
			for _, line := range b.tree(id, "Atoll/trash", 2) {
				lines = append(lines, "~"+line)
			}
		}
		lines = append(lines, b.tree(id, "Atoll/tmp", 0)...)
	}
	lines = append(lines, fmt.Sprintf("planned operations: %d", b.count("SELECT count(*) FROM ops WHERE state = 'planned'")))
	rows, err := b.db.QueryContext(b.t.Context(), `SELECT CAST(f.lpath AS TEXT), f.state, c.disk_id, c.state, coalesce(c.size, -1)
		FROM files f JOIN copies c ON c.file_id = f.id ORDER BY f.lpath, c.disk_id`)
	if err != nil {
		b.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var path, fileState, disk, copyState string
		var size int64
		if err := rows.Scan(&path, &fileState, &disk, &copyState, &size); err != nil {
			b.t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf("row %s (%s): %s %s %d", path, fileState, disk, copyState, size))
	}
	if err := rows.Err(); err != nil {
		b.t.Fatal(err)
	}
	lines = append(lines, fmt.Sprintf("jobs: %d", b.count("SELECT count(*) FROM jobs")))
	return strings.Join(lines, "\n")
}

// tree lists the files and symbolic links below a folder of a disk, and what each holds, its permission bits and its
// user. extended attributes, by their path below it less its first skip names: an entry of the trash by its logical
// path. Folders are left out: one the trash keeps empty harms nothing.
func (b *bench) tree(id, folder string, skip int) []string {
	b.t.Helper()
	top := filepath.Join(b.dirs[id], filepath.FromSlash(folder))
	var out []string
	err := filepath.WalkDir(top, func(path string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(top, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		name := strings.Join(parts[min(skip, len(parts)-1):], "/")
		what := ""
		if d.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			what = "-> " + target
		} else {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			what = fmt.Sprintf("%s %o%s", data, info.Mode().Perm(), userXattrs(path))
		}
		out = append(out, fmt.Sprintf("%s %s: %s = %q", id, folder, name, what))
		return nil
	})
	if err != nil {
		b.t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// userXattrs gives the user. extended attributes of a file, sorted, as " name=value" each; none where a filesystem keeps
// none.
func userXattrs(path string) string {
	size, err := syscall.Listxattr(path, nil)
	if err != nil || size == 0 {
		return ""
	}
	list := make([]byte, size)
	if size, err = syscall.Listxattr(path, list); err != nil {
		return ""
	}
	var out []string
	for _, name := range strings.Split(string(list[:size]), "\x00") {
		if !strings.HasPrefix(name, "user.") {
			continue
		}
		value := make([]byte, 1024)
		n, err := syscall.Getxattr(path, name, value)
		if err != nil {
			n = 0
		}
		out = append(out, " "+name+"="+string(value[:n]))
	}
	slices.Sort(out)
	return strings.Join(out, "")
}

// attributed gives a pool of two disks with Films/a.mkv on both, where a user set an extended attribute through
// Atoll's own view, on the copy on t1 it presents: the next pass gives it the copy on t2 (specs.md §21.2).
func attributed(t *testing.T) *bench {
	t.Helper()
	b, w, _, other := xattrPool(t, "")
	if other != "t2" {
		t.Fatalf("the view presents the copy on %s", other)
	}
	setThroughView(b, w, "Films/a.mkv", "user.DOSATTRIB", "0x20")
	return b
}

// propagated gives a pool of two disks, each with the same files, where a user, on t1, renamed a file, deleted
// another, renamed a folder, and changed the mode and the time of one: the next pass acts on t2 (specs.md §15.4,
// §15.12).
func propagated(t *testing.T) *bench {
	t.Helper()
	b := newBench(t, "t1", "t2")
	for _, id := range []string{"t1", "t2"} {
		b.write(id, "Films/a", "film a")
		b.write(id, "Films/b", "film b")
		b.write(id, "Series/S01/e01", "episode 1")
		b.write(id, "Series/S01/e02", "episode 2")
		b.write(id, "x", "x")
	}
	b.scan(true)
	b.rename("t1", "Films/a", "Films/Kept/a")
	b.remove("t1", "Films/b")
	b.rename("t1", "Series", "Shows")
	b.pass()
	// The mode and the time after that first pass, which would apply them: a change of metadata waits for settle from
	// its change time, which the disk gives, and the deletions and renames for the debounce.
	b.chmod("t1", "x", 0o600, 0o640)
	b.touch("t1", "x", touchedNS)
	b.now = max(b.now+30_000_000_000, b.ctimeOf("t1", "x")+settleNS)
	return b
}

// refreshed gives a pool of two disks where a user, on t1, edited a file both hold and made a new one in new folders,
// settle passed: the next pass sends the old copy on t2 to the trash, copies the new content there, and copies the new
// file, its folders made like t1's (specs.md §14.2, §15.4).
func refreshed(t *testing.T) *bench {
	t.Helper()
	b := pool(t)
	b.write("t1", "Deep/New/n", "new file")
	b.changeAgain("t1", "a", "y")
	b.scan(false)
	b.now = b.ctimeOf("t1", "a") + settleNS
	return b
}

// cached gives a pool with an SSD cache c, whose file's residency is over, in the window: the next pass makes its
// copies on the data disks, then sends its copy on the cache to the trash and purges it at once (specs.md §19).
func cached(t *testing.T) *bench {
	t.Helper()
	b := cacheBench(t, "[cache]\nenabled = true\nmin_age = \"0s\"\n"+windowOpen)
	b.write("c", "show/e1.mkv", "episode")
	b.scan(false)
	return b
}

// swapped gives a pool of two disks where a user, on t1, swapped the names of two files: on t2 each waits for the name
// the other holds, which only an exchange of the two names gets them to (specs.md §15.12).
func swapped(t *testing.T) *bench {
	t.Helper()
	b := newBench(t, "t1", "t2")
	for _, id := range []string{"t1", "t2"} {
		b.write(id, "Films/a", "film a")
		b.write(id, "b", "film b")
	}
	b.scan(true)
	b.rename("t1", "Films/a", "swap")
	b.rename("t1", "b", "Films/a")
	b.rename("t1", "swap", "b")
	b.pass()
	b.now += 30_000_000_000
	return b
}

// What a pass propagates to a disk ends as it would with nothing failing, whatever call fails on that disk, and
// whatever call fails on it when it resumes after a crash at each point (specs.md §15.12, §17.2): renames, a folder's
// included, deletions, metadata, a refresh, copies, and an exchange of two names.
func TestAPassEndsTheSameWhereverItFails(t *testing.T) {
	moves := []string{"rename", "mkdir", "setmode", "settimes"}
	sweepPass(t, passCase{name: "a pass", pool: propagated, disk: "t2", needs: moves})
	for _, point := range []string{"journaled", "mode-set", "times-set", "trash-folders", "moved", "target-synced"} {
		sweepPass(t, passCase{name: "the reprise after a crash at " + point, pool: propagated, disk: "t2", crash: point, needs: []string{"rename"}})
	}
	copies := []string{"create", "write", "rename", "mkdir"}
	sweepPass(t, passCase{name: "a refresh and a copy, on the target", pool: refreshed, disk: "t2", needs: copies})
	sweepPass(t, passCase{name: "a refresh and a copy, on the source", pool: refreshed, disk: "t1", needs: []string{"read", "openfile"}})
	for _, point := range []string{"mkdir-made", "copied", "placed", "source-synced"} {
		sweepPass(t, passCase{name: "a copy's reprise after a crash at " + point, pool: refreshed, disk: "t2", crash: point, needs: []string{"rename"}})
	}
	sweepPass(t, passCase{name: "atoll replicate, on the target", pool: copiesToMake, disk: "t2", act: replicateNow(""), needs: copies})
	sweepPass(t, passCase{name: "atoll replicate, on the source", pool: copiesToMake, disk: "t1", act: replicateNow(""), needs: []string{"read"}})
	sweepPass(t, passCase{name: "a transfer, on the cache", pool: cached, disk: "c", needs: []string{"rename", "unlink"}})
	sweepPass(t, passCase{name: "a transfer, on a data disk", pool: cached, disk: "t1", needs: []string{"create", "rename"}})
	// From the look at the copy to give: a disk lost before it keeps its own set, disk-unavailable (specs.md §21.2).
	sweepPass(t, passCase{name: "an attribute set through the view", pool: attributed, disk: "t2", needs: []string{"setxattr"}, from: "xattrs"})
	sweepPass(t, passCase{name: "two names exchanged", pool: swapped, disk: "t2", needs: []string{"exchange"}})
	sweepPass(t, passCase{name: "two names exchanged, after a crash at moved", pool: swapped, disk: "t2", crash: "moved", needs: []string{"stat"}})
}
