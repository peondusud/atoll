//go:build loopdev

package loopdev

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
)

// stoppingAt is a machine that runs what a test gives at a point of an operation, once: a pause, or a panic that stands
// for kill -9.
type stoppingAt struct {
	disks.Machine
	at map[string]func()
}

func (s *stoppingAt) Checkpoint(point string) {
	if fire := s.at[point]; fire != nil {
		delete(s.at, point)
		fire()
	}
}

func ctimeAt(t *testing.T, full string) int64 {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(full, &st); err != nil {
		t.Fatal(err)
	}
	return st.Ctim.Nano()
}

// keepingDates writes into a file as a program that keeps its size and its dates does, until its change time has moved:
// nothing but the change time tells the write.
func keepingDates(t *testing.T, top, logical, text string, when time.Time) {
	t.Helper()
	full := filepath.Join(top, "media", logical)
	before := ctimeAt(t, full)
	for range 2000 {
		if err := os.WriteFile(full, []byte(text), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(full, when, when); err != nil {
			t.Fatal(err)
		}
		if ctimeAt(t, full) != before {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s: the change time did not move", full)
}

// TestAWriteKeepingTheDatesOnEachDriver: a copy made on each driver, Atoll killed once its job recorded the change time
// the copy had right after the rename (placed), or right before that record (moved). With nothing written since, the
// next pass finds the copy as its row says: the change time recorded is the one a later look reads, on every driver.
// Written into by a program that keeps its size and its dates, so that only the change time moved: where the change
// time was recorded, the reprise reads a write since and the pass a modification, given to the other copy; where it was
// not, the reprise records the copy unverified, which a full scan then reads as a conflict, both versions kept
// (specs.md §14.2; the limit of flaw 1, closed as the maintainer decided on 2026-11-20).
func TestAWriteKeepingTheDatesOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		for _, point := range []string{"placed", "moved"} {
			for _, written := range []bool{false, true} {
				name := tc.name + "/" + point + "/untouched"
				if written {
					name = tc.name + "/" + point + "/written"
				}
				t.Run(name, func(t *testing.T) {
					options := driverOptions(t, tc.driver)
					v := newVolume(t, tc.fs)
					at := mountPoint(t)
					mountVolume(t, v, tc.driver, at, options...)
					b := newPassBench(t, v, at)
					when := time.Unix(1500000000, 0)
					writeBytes(t, b.keep, "a", []byte("content"), when)
					b.scan(false)
					real := b.machine
					stop := errors.New("stopped")
					// The rename past a clock tick, so that the change time it gives is past the part's.
					b.machine = &stoppingAt{Machine: real, at: map[string]func(){
						"verified": func() { time.Sleep(20 * time.Millisecond) },
						point:      func() { panic(stop) }, //nolint:forbidigo // what stands for kill -9
					}}
					func() {
						defer func() {
							if err, ok := recover().(error); !ok || !errors.Is(err, stop) {
								t.Fatalf("it did not stop at %s: %v", point, err)
							}
						}()
						_, _ = jobs.Replicate(t.Context(), b.db, b.config, b.machine, jobs.ReplicateOptions{Path: "a", Now: true}, func(*jobs.ReplicateItem) error { return nil })
					}()
					recorded := b.text("SELECT count(*) FROM jobs WHERE json_extract(payload, '$.placed') IS NOT NULL")
					if want := map[string]string{"placed": "1", "moved": "0"}[point]; recorded != want {
						t.Fatalf("%s change times recorded at %s", recorded, point)
					}
					if written {
						time.Sleep(20 * time.Millisecond)
						keepingDates(t, at, "a", "CONTENT", when)
					}
					b.machine = &later{Machine: real, by: 11 * time.Minute}
					out := b.pass()
					t.Logf("after the reprise: %+v, %+v; v's copy %s", out.Decisions, out.Operations,
						b.text("SELECT created_by || ' ' || state || ' ' || (verified_ns IS NOT NULL) FROM copies WHERE disk_id = 'v'"))
					if got := b.text("SELECT count(*) FROM ops WHERE state = 'planned'"); got != "0" {
						t.Errorf("%s operations left planned", got)
					}
					switch {
					case !written:
						if len(out.Decisions) != 0 || len(out.Operations) != 0 {
							t.Errorf("a copy nothing wrote into: %+v, %+v", out.Decisions, out.Operations)
						}
						if point == "placed" {
							if got := b.text("SELECT count(*) FROM copies WHERE disk_id = 'v' AND state = 'ok' AND verified_ns IS NOT NULL"); got != "1" {
								t.Error("the copy is not verified")
							}
						}
					case point == "placed":
						if !slices.Equal(out.Decisions, []jobs.Decision{{Path: "a", Kind: "modified"}}) {
							t.Fatalf("%+v, %+v", out.Decisions, out.Operations)
						}
						dropCaches(t)
						wantCopy(t, b.keep, "a", []byte("CONTENT"), when)
						if got := b.text("SELECT group_concat(disk_id || ' ' || created_by, ',') FROM (SELECT * FROM copies ORDER BY disk_id)"); got != "k atoll,v user" {
							t.Errorf("the copies are %s", got)
						}
					default:
						if len(out.Decisions) != 0 {
							t.Errorf("%+v", out.Decisions)
						}
						if got := b.text("SELECT count(*) FROM copies WHERE disk_id = 'v' AND state = 'ok' AND created_by = 'atoll' AND verified_ns IS NULL"); got != "1" {
							t.Fatal("the copy is not recorded unverified")
						}
						b.scan(true)
						if got := b.text("SELECT state FROM files"); got != "conflict" {
							t.Errorf("the file is %s", got)
						}
						dropCaches(t)
						wantCopy(t, b.keep, "a", []byte("content"), when)
						wantCopy(t, at, "a", []byte("CONTENT"), when)
					}
				})
			}
		}
	}
}

// TestTheChangeTimeARenameLeavesOnEachDriver measures what TestAWriteKeepingTheDatesOnEachDriver waits a clock tick
// for: how often the rename of a new copy, right after its part was verified, leaves it the change time the part was
// verified with, as a look at its name reads it, so that a reprise with no record of the change time after the rename
// finds nothing moved and records the copy verified (specs.md §14.2; flaw 1's limit). Atoll is killed right after the
// rename and its look (moved), then the reprise finishes the copy, ten times on each driver. It says each driver's
// count and fails on none: a measure.
func TestTheChangeTimeARenameLeavesOnEachDriver(t *testing.T) {
	needRoot(t)
	const tries = 10
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newPassBench(t, v, at)
			real := b.machine
			same := 0
			for i := range tries {
				name := fmt.Sprintf("a%d", i)
				writeBytes(t, b.keep, name, []byte("content"), time.Unix(1500000000, 0))
				b.scan(false)
				stop := errors.New("stopped")
				b.machine = &stoppingAt{Machine: real, at: map[string]func(){
					"moved": func() { panic(stop) }, //nolint:forbidigo // what stands for kill -9
				}}
				func() {
					defer func() {
						if err, ok := recover().(error); !ok || !errors.Is(err, stop) {
							t.Fatalf("it did not stop at moved: %v", err)
						}
					}()
					_, _ = jobs.Replicate(t.Context(), b.db, b.config, b.machine, jobs.ReplicateOptions{Path: name, Now: true}, func(*jobs.ReplicateItem) error { return nil })
				}()
				part := b.text("SELECT json_extract(payload, '$.part_look.ctime_ns') FROM jobs WHERE kind = 'replicate' ORDER BY id DESC LIMIT 1")
				if part == strconv.FormatInt(ctimeAt(t, filepath.Join(at, "media", name)), 10) {
					same++
				}
				b.machine = &later{Machine: real, by: 11 * time.Minute}
				b.pass()
				b.machine = real
				if got := b.text("SELECT count(*) FROM ops WHERE state = 'planned'"); got != "0" {
					t.Fatalf("%s operations left planned", got)
				}
			}
			t.Logf("%s: the rename left the change time the part was verified with %d times of %d", tc.name, same, tries)
		})
	}
}
