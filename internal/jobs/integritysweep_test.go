package jobs

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// An integrity sweep runs a scrub or a repair of a copy that rotted, once with nothing failing, counting each call it
// makes on one disk, then once for each of those calls, on a new pool, with that call failing with EIO, then with
// EACCES. Whatever fails, and wherever (CLAUDE.md, "Testing rules"; specs.md §16, §16.1):
//   - what a disk answers is an outcome of the run, never an end of the command;
//   - the healthy copy is never written to, and the bytes of the rotten one are never lost: they stay in the copy until
//     they reached the trash (invariant 1);
//   - once the disk answers again, the same run, atoll repair, then passes and a full scan, end where nothing failing
//     ends: each copy holds its file's bytes, with the same rows. The trash may hold more: a copy that could not be read
//     is corrupt, as a read that fails on a disk that confirms is the chunk's fault (invariant 4).
type integrityCase struct {
	name string
	more string
	// disk is the disk whose calls fail.
	disk string
	// before runs on the pool once the copy on t2 rotted, before the run the sweep is about.
	before func(b *bench)
	// run runs it, on a catalog.
	run func(b *bench, db *sql.DB) error
}

const (
	healthyFilm = "a film, of one short chunk"
	rottenFilm  = "a FILM, of one short chunk"
)

// rotten gives the pool of an integrity case: a film on two disks, read whole, due for a scrub, rotten on t2.
func rotten(t *testing.T, c *integrityCase) *bench {
	t.Helper()
	b := newBenchWith(t, c.more, "t1", "t2")
	b.write("t1", "a.mkv", healthyFilm)
	b.write("t2", "a.mkv", healthyFilm)
	b.scan(true)
	b.now += int64(month)
	b.rot("t2", "a.mkv", rottenFilm)
	if c.before != nil {
		c.before(b)
	}
	return b
}

// runIntegrity runs a case with the call at a position failing with inject (0 fails none), then ends it, and gives the
// calls, the error of the run that failed, and what the pool ends with.
func runIntegrity(t *testing.T, c *integrityCase, at int, inject error) (calls []string, runErr error, end string) {
	t.Helper()
	b := rotten(t, c)
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
	return calls, runErr, endIntegrity(t, c, b)
}

// endIntegrity checks what a run that failed left, then runs it again with nothing failing, then atoll repair, and
// passes until all is done, and says what the pool ends with.
func endIntegrity(t *testing.T, c *integrityCase, b *bench) string {
	t.Helper()
	if got := b.read("t1", "a.mkv"); got != healthyFilm {
		t.Errorf("the healthy copy holds %q", got)
	}
	if !b.holds("t2", rottenFilm) && b.read("t2", "a.mkv") != healthyFilm {
		t.Errorf("the rotten copy's bytes are lost, and the copy holds %q", b.read("t2", "a.mkv"))
	}
	if err := c.run(b, b.db); err != nil {
		t.Fatalf("the run again, with nothing failing: %v", err)
	}
	// A copy a scrub could neither repair nor send to the trash, its disk failing then, stays corrupt where it is, and
	// no later scrub reads it again: atoll repair begins again (specs.md §16.1).
	if _, err := Repair(t.Context(), b.db, b.config, b, RepairOptions{Apply: true}); err != nil {
		t.Fatalf("a repair, with nothing failing: %v", err)
	}
	return b.settled(true)
}

// runIntegrityCatalog runs a case with the catalog's statement at a position failing (0 fails none), then ends it.
func runIntegrityCatalog(t *testing.T, c *integrityCase, at int) (seen []string, runErr error, end string) {
	t.Helper()
	b := rotten(t, c)
	db, f := b.faultyCatalog(at)
	f.arm(true)
	runErr = c.run(b, db)
	f.arm(false)
	_ = db.Close()
	f.mu.Lock()
	seen = append(seen, f.seen...)
	f.mu.Unlock()
	return seen, runErr, endIntegrity(t, c, b)
}

// sweepIntegrityCatalog is sweepIntegrity with each statement of the catalog failing in turn, one in stride: the run
// ends with the catalog's error, or goes on where it does without the statement, and the pool ends the same.
func sweepIntegrityCatalog(t *testing.T, c integrityCase, stride int) {
	t.Helper()
	t.Run(c.name, func(t *testing.T) {
		t.Parallel()
		seen, err, want := runIntegrityCatalog(t, &c, 0)
		if err != nil {
			t.Fatalf("with nothing failing: %v", err)
		}
		if len(seen) == 0 {
			t.Fatal("no statement went through the driver")
		}
		if raceBuild {
			stride *= sweepStride
		}
		for i := 0; i < len(seen); i += stride {
			where := fmt.Sprintf("statement %d of %d (%.80s) failing", i+1, len(seen), seen[i])
			_, err, got := runIntegrityCatalog(t, &c, i+1)
			if err != nil && !errors.Is(err, errCatalogFault) {
				t.Errorf("%s: the run ended with another error: %v", where, err)
			}
			if got != want {
				t.Errorf("%s: it ends otherwise:\n%s\nwhere nothing failing ends:\n%s", where, got, want)
			}
		}
	})
}

// holds says whether a file below a disk's root or in its trash holds a text.
func (b *bench) holds(id, text string) bool {
	b.t.Helper()
	found := false
	for _, folder := range []string{"media", filepath.Join("Atoll", "trash")} {
		err := filepath.WalkDir(filepath.Join(b.dirs[id], folder), func(path string, d fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil || d.IsDir() {
				return err
			}
			data, err := os.ReadFile(path)
			found = found || err == nil && string(data) == text
			return nil
		})
		if err != nil {
			b.t.Fatal(err)
		}
	}
	return found
}

func sweepIntegrity(t *testing.T, c integrityCase) {
	t.Helper()
	t.Run(c.name, func(t *testing.T) {
		t.Parallel()
		calls, err, want := runIntegrity(t, &c, 0, nil)
		if err != nil {
			t.Fatalf("with nothing failing: %v", err)
		}
		if len(calls) == 0 {
			t.Fatal("no call went through the disk's top")
		}
		for i, call := range calls {
			for k, inject := range []error{syscall.EIO, syscall.EACCES} {
				if raceBuild && (i+2*k)%sweepStride != 0 {
					continue
				}
				where := fmt.Sprintf("call %d of %d (%s) failing with %v", i+1, len(calls), call, inject)
				_, err, got := runIntegrity(t, &c, i+1, inject)
				if err != nil {
					t.Errorf("%s: the run ended with %v", where, err)
				}
				if got != want {
					t.Errorf("%s: it ends otherwise:\n%s\nwhere nothing failing ends:\n%s", where, got, want)
				}
			}
		}
	})
}

// A scrub that finds a copy rotten, and a repair of one a scrub found, loses nothing whatever call fails on either
// disk, and ends as with nothing failing once the disk answers again (specs.md §16, §16.1).
func TestScrubsAndRepairsEndTheSameWhereverTheyFail(t *testing.T) {
	for _, disk := range []string{"t2", "t1"} {
		sweepIntegrity(t, integrityCase{name: "a scrub that repairs, failing on " + disk, disk: disk, run: scrubQuarantine})
		sweepIntegrity(t, integrityCase{name: "a repair by chunk, failing on " + disk, disk: disk, before: foundCorrupt2, run: repairBy(RepairChunk)})
		sweepIntegrity(t, integrityCase{name: "a repair of the whole copy, failing on " + disk, disk: disk, before: foundCorrupt2, run: repairBy(RepairFile)})
	}
	sweepIntegrity(t, integrityCase{name: "a scrub that quarantines", more: "[integrity]\nauto_repair = false\n", disk: "t2", run: scrubQuarantine})
}

// The same, whatever statement of the catalog fails (specs.md §9, §16.1).
func TestScrubsAndRepairsEndTheSameWhereverTheirCatalogFails(t *testing.T) {
	sweepIntegrityCatalog(t, integrityCase{name: "a scrub that repairs", run: scrubQuarantine}, 1)
	sweepIntegrityCatalog(t, integrityCase{name: "a scrub that quarantines", more: "[integrity]\nauto_repair = false\n", run: scrubQuarantine}, 1)
	sweepIntegrityCatalog(t, integrityCase{name: "a repair by chunk", before: foundCorrupt2, run: repairBy(RepairChunk)}, 1)
	sweepIntegrityCatalog(t, integrityCase{name: "a repair of the whole copy", before: foundCorrupt2, run: repairBy(RepairFile)}, 1)
}

func scrubQuarantine(b *bench, db *sql.DB) error {
	_, err := Scrub(b.t.Context(), db, b.config, b, ScrubOptions{Apply: true, Quarantine: true})
	return err
}

// foundCorrupt2 has a scrub find the copy on t2 corrupt, and leave it so.
func foundCorrupt2(b *bench) {
	b.t.Helper()
	b.scrub(ScrubOptions{Apply: true})
	if got := b.text("SELECT state FROM copies WHERE disk_id = 't2'"); got != "corrupt" {
		b.t.Fatalf("the copy found %s", got)
	}
}

func repairBy(mode string) func(b *bench, db *sql.DB) error {
	return func(b *bench, db *sql.DB) error {
		_, err := Repair(b.t.Context(), db, b.config, b, RepairOptions{Apply: true, Mode: mode})
		return err
	}
}
