package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/view"
	"github.com/peondusud/atoll/internal/view/writer"
)

// cachePool is a pool of three disks, t1 the SSD cache, with Atoll's own view, whose new files land on t1 and whose
// transfer runs at the bench's time: its catalog made by init, a pass run.
func cachePool(t *testing.T) *poolBench {
	t.Helper()
	// The window holds the hour of the system's clock, which it is read by, in UTC.
	hour := time.Now().UTC().Hour()
	b := newPoolBenchViewing(t, 3, "engine = \"atoll\"\n", fmt.Sprintf("[defaults]\nsettle = \"0s\"\n[cache]\nenabled = true\n"+
		"min_age = \"0s\"\n[replication]\nwindow = \"%02d:00-%02d:00\"\n[modifies.mass_guard]\npercent = \"100%%\"\n[modifies.day_guard]\npercent = \"100%%\"\n", hour, (hour+2)%24))
	// The bench's clock is the real one, as a copy is due from the change time its disk gives it.
	b.machine.now = time.Now().UnixNano()
	text, err := os.ReadFile(b.configPath)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(text), "pool_id = \"p\"\n", "pool_id = \"p\"\ntimezone = \"UTC\"\n", 1)
	s = strings.Replace(s, "fs = \"ext4\"\n", "fs = \"ext4\"\nrole = \"cache\"\n", 1)
	if err := os.WriteFile(b.configPath, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	b.ok(exitOK, "init")
	// A pass records each disk's room, which the view reads to place a new file.
	b.ok(exitOK, "daemon", "--until-idle")
	return b
}

// mounted is Atoll's own view as atoll mount serves it: over the catalog it opens once, as it starts, under the view's
// side of the lock, held until the test ends, and the disks.
func (b *poolBench) mounted() *writer.Writer {
	b.t.Helper()
	c, err := load(b.configPath)
	if err != nil {
		b.t.Fatal(err)
	}
	db, release, err := openView(context.Background(), c)
	if err != nil {
		b.t.Fatal(err)
	}
	b.t.Cleanup(release)
	b.t.Cleanup(func() { _ = db.Close() })
	roots, handles := view.NewDiskRoots(), writer.NewHandles()
	b.t.Cleanup(roots.Close)
	b.t.Cleanup(handles.Close)
	for i := range c.Disks {
		d := &c.Disks[i]
		root, err := b.machine.OpenRoot(d.Mount, d.Root)
		if err != nil {
			b.t.Fatal(err)
		}
		roots.Set(d.ID, root)
		h, err := ops.Open(b.machine, d, 0)
		if err != nil {
			b.t.Fatal(err)
		}
		handles.Set(d.ID, h)
	}
	settings, err := c.Cache()
	if err != nil {
		b.t.Fatal(err)
	}
	tree := view.NewTree(db, roots, view.Owner{}, b.machine.Now()).WithCache(settings.InView)
	w, err := writer.New(db, c, b.machine, tree, handles, slog.New(slog.DiscardHandler))
	if err != nil {
		b.t.Fatal(err)
	}
	return w
}

// atoll catalog rebuild while Atoll's own view holds a file open to write: the file stays on the cache while the view
// holds it, and what the program writes late reaches the library (specs.md §9, §19, §21.2). Flaw 35 of the TLA+ models
// (conformance/models/README.md, Rebuild.tla): the new catalog, made from the disks, knew nothing of what the view held
// open, and the view went on journaling in the old one, which it opened at its start, so that the SSD cache's transfer
// moved the file and purged it under the program, whose later write was lost. A rebuild and the view exclude each other
// now (rebuild-view, the maintainer, 2026-11-27): the rebuild is refused while the view runs.
func TestARebuildKeepsWhatTheViewHoldsOpen(t *testing.T) {
	b := cachePool(t)
	w := b.mounted()
	who := writer.Caller{Owner: writer.Owner{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}, Creds: &view.Credentials{Caps: ^view.Caps(0)}}
	f, _, err := w.Create(t.Context(), "new.mkv", 0o644, who)
	if err != nil {
		t.Fatal(err)
	}
	if f.Disk != "t1" {
		t.Fatalf("the new file landed on %s", f.Disk)
	}
	if _, err := f.WriteAt([]byte("episode"), 0); err != nil {
		t.Fatal(err)
	}
	// The rebuild, while the view runs; refused or not.
	_, _, _ = b.atoll("catalog", "rebuild", "--yes")
	for range 3 {
		b.ok(exitOK, "daemon", "--until-idle")
		b.machine.advance(time.Minute)
	}
	if b.read(1, "new.mkv") == "absent" {
		t.Errorf("the file left the cache while the view held it open: t2 %q, t3 %q", b.read(2, "new.mkv"), b.read(3, "new.mkv"))
	}
	const late = "a late write"
	if _, err := f.WriteAt([]byte(late), 0); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	for range 4 {
		b.ok(exitOK, "daemon", "--until-idle")
		b.machine.advance(time.Minute)
	}
	found := false
	for disk := 1; disk <= 3; disk++ {
		found = found || strings.HasPrefix(b.read(disk, "new.mkv"), late)
	}
	if !found {
		t.Errorf("the late write is in no copy: t1 %q, t2 %q, t3 %q; the trash of t1 holds %v", b.read(1, "new.mkv"),
			b.read(2, "new.mkv"), b.read(3, "new.mkv"), trashOf(b.mounts[0]))
	}
}

// trashOf lists what a disk's trash holds, its paths below Atoll/trash/.
func trashOf(mount string) []string {
	var out []string
	top := filepath.Join(mount, "Atoll", "trash")
	_ = filepath.WalkDir(top, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(top, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

// inode gives a file's inode, 0 when it is not there.
func inode(t *testing.T, file string) uint64 {
	t.Helper()
	info, err := os.Stat(file)
	if err != nil {
		return 0
	}
	return info.Sys().(*syscall.Stat_t).Ino
}

// A rebuild while Atoll's own view runs is refused, exit 1, nothing changed: no catalog made beside the old one, the
// old one where it was, its lock let go of; once the view is over, the rebuild goes (specs.md §9, §21.2, §25). It went
// through, and the view went on with the catalog it set aside (flaw 35).
func TestARebuildIsRefusedWhileTheViewRuns(t *testing.T) {
	b := cachePool(t)
	c, err := load(b.configPath)
	if err != nil {
		t.Fatal(err)
	}
	db, release, err := openView(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	file := catalogPath(c)
	before := inode(t, file)
	code, stdout, stderr := b.atoll("catalog", "rebuild", "--yes")
	if code != exitError || !strings.Contains(stderr, "atoll mount") || stdout != "" {
		t.Errorf("a rebuild while the view runs: exit %d, %q, %q", code, stdout, stderr)
	}
	if inode(t, file) != before || inode(t, file+".rebuild") != 0 {
		t.Errorf("the refused rebuild changed the catalog, or made one beside it")
	}
	// What only reads, and what writes, goes on while the view runs: the catalog's own lock is another one.
	b.ok(exitOK, "daemon", "--until-idle")
	_ = db.Close()
	release()
	if out := b.json(exitOK, "catalog", "rebuild", "--yes"); out["done"] != true {
		t.Errorf("once the view is over: %v", out)
	}
	if inode(t, file) == before {
		t.Error("the rebuild once the view is over left the old catalog in place")
	}
}

// Atoll's own view does not start while a rebuild runs, exit 1, nothing journaled; once the rebuild is over, it
// starts, over the catalog the rebuild made (specs.md §21.2, §25). A view started then opened the catalog the rebuild
// set aside (flaw 35, the remedy's first design).
func TestTheViewDoesNotStartWhileARebuildRuns(t *testing.T) {
	b := cachePool(t)
	c, err := load(b.configPath)
	if err != nil {
		t.Fatal(err)
	}
	release, err := lockForRebuild(c)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := b.atoll("mount"); code != exitError || !strings.Contains(stderr, "a catalog rebuild runs") {
		t.Errorf("atoll mount while a rebuild runs: exit %d, %q", code, stderr)
	}
	if _, _, err := openView(t.Context(), c); err == nil || !strings.Contains(err.Error(), "a catalog rebuild runs") {
		t.Errorf("the view's catalog while a rebuild runs: %v", err)
	}
	// A second rebuild is refused too, as one runs.
	if code, _, stderr := b.atoll("catalog", "rebuild", "--yes"); code != exitError || !strings.Contains(stderr, "another rebuild") {
		t.Errorf("a second rebuild: exit %d, %q", code, stderr)
	}
	release()
	db, again, err := openView(t.Context(), c)
	if err != nil {
		t.Fatalf("once the rebuild is over: %v", err)
	}
	_ = db.Close()
	again()
}

// lockChildEnv names, for TestALockHolderKilledLetsTheOtherGo's child, the configuration whose lock it takes, the side
// in lockSideEnv, view or rebuild; it says "held" on standard output, then waits to be killed.
const (
	lockChildEnv = "ATOLL_TEST_LOCK_CONFIG"
	lockSideEnv  = "ATOLL_TEST_LOCK_SIDE"
)

// A view or a rebuild killed as by kill -9 leaves no lock behind: the kernel lets go of it with the process, and the
// other side goes on at once (specs.md §21.2, §25).
func TestALockHolderKilledLetsTheOtherGo(t *testing.T) {
	if path := os.Getenv(lockChildEnv); path != "" {
		c, err := load(path)
		if err != nil {
			os.Exit(exitError)
		}
		take := lockForView
		if os.Getenv(lockSideEnv) == "rebuild" {
			take = lockForRebuild
		}
		if _, err := take(c); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(exitError)
		}
		fmt.Println("held")
		time.Sleep(time.Minute)
		os.Exit(exitOK)
	}
	for _, side := range []string{"view", "rebuild"} {
		t.Run(side, func(t *testing.T) {
			b := cachePool(t)
			c, err := load(b.configPath)
			if err != nil {
				t.Fatal(err)
			}
			child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestALockHolderKilledLetsTheOtherGo$") //nolint:gosec // this test's own binary
			child.Env = append(os.Environ(), lockChildEnv+"="+b.configPath, lockSideEnv+"="+side)
			out, err := child.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			line, err := bufio.NewReader(out).ReadString('\n')
			if err != nil || line != "held\n" {
				t.Fatalf("the child said %q: %v", line, err)
			}
			held := func() bool {
				if side == "view" {
					code, _, _ := b.atoll("catalog", "rebuild", "--yes")
					return code == exitError
				}
				db, release, err := openView(t.Context(), c)
				if err == nil {
					_ = db.Close()
					release()
				}
				return err != nil
			}
			if !held() {
				t.Fatalf("the %s's lock held by another process lets the other side go", side)
			}
			if err := child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = child.Wait()
			if held() {
				t.Errorf("the %s killed left its lock behind", side)
			}
		})
	}
}
