package watch

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
)

// A file that cannot say what it is, before its read or after it, leaves its disk unknown: the disk cannot be relied
// on, and its hashes stop there; what was hashed before stays (invariant 4, specs.md §15.10).
func TestAFileThatCannotSayWhatItIsDuringItsRead(t *testing.T) {
	for _, at := range []int{1, 2} {
		b := newBench(t, "t1")
		b.write("t1", "a", "x")
		b.write("t1", "b", "y")
		b.write("t1", "c", "z")
		calls := 0
		b.wrap = func(_ string, d fsx.Dir) fsx.Dir {
			return fsx.Fault(d, func(call, name string) error {
				if call == "fileinfo" && name == "b" {
					calls++
					if calls == at {
						return syscall.EIO
					}
				}
				return nil
			})
		}
		r := b.scan(Options{Full: true}).Disks[0]
		if r.Verdict.State != disks.Missing || r.Verdict.Reason != disks.IOError {
			t.Errorf("info %d of b fails: %+v", at, r)
		}
		if got := b.count("SELECT count(*) FROM files WHERE blake3 IS NOT NULL AND lpath = ?", []byte("b")); got != 0 {
			t.Errorf("info %d of b fails: b has a hash", at)
		}
		if got := b.count("SELECT count(*) FROM files WHERE blake3 IS NOT NULL AND lpath = ?", []byte("a")); got != 1 {
			t.Errorf("info %d of b fails: what was hashed before is lost", at)
		}
	}
}

// linksFault is a bench whose disk's folder of links, <workdir>/links, fails as fail says: the link guard's own folder,
// which a walk lists to take Atoll's links out of the count of a file's names (specs.md §17).
type linksFault struct {
	*bench
	fail func(call, name string) error
}

func (m *linksFault) OpenRoot(mount, root string) (fsx.Dir, error) {
	dir, err := m.bench.OpenRoot(mount, root)
	if err != nil || root != "Atoll/links" {
		return dir, err
	}
	return fsx.Fault(dir, m.fail), nil
}

// The link guard's links are no names of a file a user gave it: a walk takes them out of each file's count of names.
// Its folder not answering leaves the disk unknown, as any folder of the disk would; a link gone in the meantime is no
// link (invariant 4, specs.md §17).
func TestTheFolderOfLinksWhenItDoesNotAnswer(t *testing.T) {
	setup := func(t *testing.T) *bench {
		b := newBench(t, "t1")
		b.write("t1", "a", "x")
		if err := os.Link(filepath.Join(b.media("t1"), "a"), filepath.Join(b.media("t1"), "a2")); err != nil {
			t.Fatal(err)
		}
		links := filepath.Join(b.dirs["t1"], "Atoll", "links")
		if err := os.MkdirAll(links, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(b.media("t1"), "a"), filepath.Join(links, "1")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(links, "2"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		return b
	}
	look := func(t *testing.T, b *bench, fail func(call, name string) error) *Report {
		t.Helper()
		verdicts, err := CheckDisks(t.Context(), b.db, b.config, b, b.now)
		if err != nil {
			t.Fatal(err)
		}
		r, err := LookAtDisk(t.Context(), b.db, b.config, &linksFault{bench: b, fail: fail}, &b.config.Disks[0], verdicts[0], b.now, &Look{Write: true})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, tc := range []struct{ call, name string }{{"names", ""}, {"stat", "1"}} {
		b := setup(t)
		r := look(t, b, func(call, name string) error {
			if call == tc.call && name == tc.name {
				return syscall.EIO
			}
			return nil
		})
		if r.Walked || r.Verdict.State != disks.Missing || b.count("SELECT count(*) FROM files") != 0 {
			t.Errorf("%s fails: %+v", tc.call, r)
		}
	}
	b := setup(t)
	r := look(t, b, func(call, name string) error {
		if call == "stat" && name == "2" {
			return syscall.ENOENT
		}
		return nil
	})
	if !r.Walked || r.New != 2 {
		t.Fatalf("a link gone in the meantime: %+v", r)
	}
	// a and a2 are one file of two names, the guard's link aside.
	if got := b.count("SELECT count(*) FROM copies WHERE nlink = 2"); got != 2 {
		t.Errorf("%d copies of two names", got)
	}
}

// A confirmation that cannot be made is an error, and nothing is written.
func TestAConfirmationThatCannotBeMade(t *testing.T) {
	b := newBench(t, "t1")
	b.write("t1", "a", "x")
	verdicts, err := disks.CheckAll(t.Context(), b, b.config.Disks)
	if err != nil {
		t.Fatal(err)
	}
	refused := errors.New("no confirmation")
	_, err = LookAtDisk(t.Context(), b.db, b.config, b, &b.config.Disks[0], verdicts[0], b.now, &Look{Write: true, ConfirmEach: true,
		Confirm: func(context.Context, *config.Disk) (bool, error) { return false, refused }})
	if !errors.Is(err, refused) {
		t.Errorf("%v", err)
	}
	if got := b.count("SELECT count(*) FROM files"); got != 0 {
		t.Error("a look that could not be confirmed wrote what it found")
	}
}

// An info of a kind the watcher does not read is skipped; one too short to hold a handle, or a record cut inside an
// info's header, is malformed: the disk is looked at whole (specs.md §15.1).
func TestFanotifyInfosItDoesNotRead(t *testing.T) {
	other := []byte{99, 0, 8, 0, 1, 2, 3, 4}
	records, err := parseFanotify(fanEvent(fanCreate, 1, other, fanHandle(infoDFIDName, "root", "a")))
	if err != nil || len(records) != 1 || len(records[0].infos) != 1 || records[0].infos[0].name != "a" {
		t.Errorf("an info of another kind: %+v, %v", records, err)
	}
	short := []byte{infoDFIDName, 0, 12, 0, 1, 2, 3, 4, 5, 6, 7, 8}
	if _, err := parseFanotify(fanEvent(fanCreate, 1, short)); !errors.Is(err, errMalformed) {
		t.Errorf("a handle info too short: %v", err)
	}
	cut := fanEvent(fanCreate, 1, []byte{1, 2})
	if _, err := parseFanotify(cut); !errors.Is(err, errMalformed) {
		t.Errorf("an info header cut short: %v", err)
	}
	long := inotifyEvent(1, syscall.IN_CREATE, 0, "a")
	binary.NativeEndian.PutUint32(long[12:], 64)
	if _, err := parseInotify(long); !errors.Is(err, errMalformed) {
		t.Errorf("an inotify name past the read: %v", err)
	}
}
