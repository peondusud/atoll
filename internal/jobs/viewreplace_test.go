package jobs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// viewFolderPool makes two disks, each with the folder Shows/D and a file in it, scanned, and the time of the debounce
// gone by.
func viewFolderPool(t *testing.T) *bench {
	t.Helper()
	b := newBench(t, "t1", "t2")
	for _, id := range []string{"t1", "t2"} {
		b.write(id, "Shows/D/e1.mkv", "the episode")
	}
	b.scan(true)
	b.now += 60_000_000_000
	return b
}

// passes runs n passes of the daemon, the debounce going by between them.
func (b *bench) passes(n int) {
	b.t.Helper()
	for range n {
		b.pass()
		b.now += 31_000_000_000
	}
}

func (b *bench) mkdir(id, path string) {
	b.t.Helper()
	if err := os.MkdirAll(filepath.Join(b.dirs[id], "media", path), 0o755); err != nil {
		b.t.Fatal(err)
	}
}

// replacedPool makes two disks, each with Films/a.mkv and Films/b.mkv, scanned, under deletes.direct as given, and
// replaces b by a through the view.
func replacedPool(t *testing.T, direct string) *bench {
	t.Helper()
	b := newBenchWith(t, fmt.Sprintf("[deletes]\ndirect = %q\n", direct), "t1", "t2")
	for _, id := range []string{"t1", "t2"} {
		b.write(id, "Films/a.mkv", "the film at a")
		b.write(id, "Films/b.mkv", "the film at b, older")
	}
	b.scan(true)
	b.now += 60_000_000_000
	if err := b.viewOf().Rename(t.Context(), "Films/a.mkv", "Films/b.mkv", false, viewer); err != nil {
		t.Fatal(err)
	}
	return b
}

// replacedEverywhere says what a replacement through the view leaves once the passes took it in: the file renamed at
// the new name on both disks, nothing at the old, no conflict, the copy replaced on each disk in its trash, and the
// operation taken in.
func (b *bench) replacedEverywhere() {
	b.t.Helper()
	for _, id := range []string{"t1", "t2"} {
		if got := b.readMedia(id, "Films/b.mkv"); got != "the film at a" {
			b.t.Errorf("%s holds %q at Films/b.mkv", id, got)
		}
		if b.exists(id, "media/Films/a.mkv") {
			b.t.Errorf("%s still holds Films/a.mkv", id)
		}
	}
	if got := b.count("SELECT count(*) FROM files WHERE state = 'conflict'"); got != 0 {
		b.t.Errorf("%d files in conflict", got)
	}
	if got := b.count("SELECT count(*) FROM files"); got != 1 {
		b.t.Errorf("%d files", got)
	}
	if got := b.count("SELECT count(DISTINCT disk_id) FROM trash"); got != 2 {
		b.t.Errorf("the copy replaced is in the trash of %d disks", got)
	}
	if open := b.openViewOps(); len(open) != 0 {
		b.t.Errorf("the replacement is not taken in: %+v", open)
	}
	if got := b.count("SELECT count(*) FROM ops WHERE state = 'planned'"); got != 0 {
		b.t.Errorf("%d operations left planned", got)
	}
}

// A rename through Atoll's own view onto a name it presents replaces the file there: the copy presented goes to the
// trash, `overwritten`, and the rename is made on its disk (specs.md §21.2). That copy is the view's, as a deletion's
// is: the file replaced was deleted by the user through the view, whatever deletes.direct says, so its other copy goes
// to its trash, and the rename reaches the other disk; then the operation is taken in (conformance/models/README.md,
// flaw 18, the maintainer's remedy, 2026-11-24). Before, under deletes.direct = "restore", the copy replaced read as one
// a user removed directly, Kept, and the rename never reached the other disk, the operation never taken in.
func TestAReplacementThroughTheViewReachesTheOtherDisk(t *testing.T) {
	for _, direct := range []string{"propagate", "restore"} {
		t.Run(direct, func(t *testing.T) {
			b := replacedPool(t, direct)
			b.passes(6)
			b.replacedEverywhere()
		})
	}
}

// The same with the move of the other disk's copy to its trash failing once, with EIO or out of room: the job stays
// for the next pass, which ends it as a pass with no fault does.
func TestAReplacementThroughTheViewWithAFaultOnTheOtherDisk(t *testing.T) {
	for _, direct := range []string{"propagate", "restore"} {
		for _, err := range []error{syscall.EIO, syscall.ENOSPC} {
			t.Run(direct+"/"+err.Error(), func(t *testing.T) {
				b := replacedPool(t, direct)
				failed := false
				b.fail[b.dirs["t2"]] = func(call, _ string) error {
					if call == "rename" && !failed {
						failed = true
						return err
					}
					return nil
				}
				b.passes(3)
				b.fail[b.dirs["t2"]] = nil
				if !failed {
					t.Fatal("nothing was moved on t2")
				}
				b.passes(6)
				b.replacedEverywhere()
			})
		}
	}
}

// The same with the daemon killed at each point of what it does on the other disk, the copy replaced to its trash and
// the rename after it: the passes after end it as passes with no kill do.
func TestAReplacementThroughTheViewWithAKillOnTheOtherDisk(t *testing.T) {
	for _, direct := range []string{"propagate", "restore"} {
		for _, point := range []string{"decided", "journaled", "trash-folders", "moved", "target-synced", "source-synced"} {
			t.Run(direct+"/"+point, func(t *testing.T) {
				b := replacedPool(t, direct)
				stop := errors.New("stopped")
				killed := false
				b.at[point] = func() {
					killed = true
					panic(stop) //nolint:forbidigo // what stands for kill -9
				}
				for range 6 {
					func() {
						defer func() {
							if r := recover(); r != nil {
								if err, ok := r.(error); !ok || !errors.Is(err, stop) {
									panic(r) //nolint:forbidigo // not the kill
								}
							}
						}()
						_, _ = Run(t.Context(), b.db, b.config, b, true)
					}()
					b.now += 31_000_000_000
				}
				delete(b.at, point)
				if !killed {
					t.Fatalf("no pass reached %s", point)
				}
				b.passes(6)
				b.replacedEverywhere()
			})
		}
	}
}
