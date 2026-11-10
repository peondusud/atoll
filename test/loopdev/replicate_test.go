//go:build loopdev

package loopdev

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/jobs"
)

func (b *passBench) replicate(path string) []jobs.ReplicateItem {
	b.t.Helper()
	var items []jobs.ReplicateItem
	if _, err := jobs.Replicate(b.t.Context(), b.db, b.config, b.machine, jobs.ReplicateOptions{Path: path, Now: true}, func(i *jobs.ReplicateItem) error {
		items = append(items, *i)
		return nil
	}); err != nil {
		b.t.Fatal(err)
	}
	return items
}

// threeChunks is a file of two whole chunks and a third short one, so that every chunk boundary is crossed.
func threeChunks() []byte {
	out := make([]byte, 2*4<<20+1000)
	for i := range out {
		out[i] = byte(i % 251)
	}
	return out
}

func writeBytes(t *testing.T, top, logical string, data []byte, when time.Time) {
	t.Helper()
	full := filepath.Join(top, "media", logical)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, data, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(full, when, when); err != nil {
		t.Fatal(err)
	}
}

func wantCopy(t *testing.T, top, logical string, data []byte, when time.Time) {
	t.Helper()
	full := filepath.Join(top, "media", logical)
	got, err := os.ReadFile(full)
	if err != nil || !bytes.Equal(got, data) {
		t.Errorf("%s: %d bytes, %v", full, len(got), err)
		return
	}
	st, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(when) {
		t.Errorf("%s: dated %v, want %v", full, st.ModTime(), when)
	}
}

func emptyTmp(t *testing.T, top string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(top, "Atoll", "tmp"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("%s/Atoll/tmp holds %d names", top, len(entries))
	}
}

// TestReplicateOnEachDriver: a file of three chunks and one below a private folder, each copied from ext4
// to the driver's disk and from it, read back through a cold cache: the same bytes, the same date, the
// folder as private as its model on POSIX filesystems, the copies Atoll's and verified, nothing left in
// tmp/ (specs.md §14).
func TestReplicateOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newPassBench(t, v, at)
			data := threeChunks()
			when := time.Unix(1500000000, 0)
			writeBytes(t, b.keep, "big", data, when)
			writeBytes(t, b.keep, "p/small", []byte("small"), when)
			if err := os.Chmod(filepath.Join(b.keep, "media", "p"), 0o750); err != nil {
				t.Fatal(err)
			}
			writeBytes(t, at, "theirs", []byte("from the driver's disk"), when)
			b.scan(false)
			items := b.replicate("")
			if len(items) != 3 {
				t.Fatalf("%+v", items)
			}
			for _, item := range items {
				if item.Outcome != jobs.OutcomeReplicated {
					t.Errorf("%+v", item)
				}
			}
			dropCaches(t)
			wantCopy(t, at, "big", data, when)
			wantCopy(t, at, "p/small", []byte("small"), when)
			wantCopy(t, b.keep, "theirs", []byte("from the driver's disk"), when)
			if tc.driver == "" {
				if st, err := os.Stat(filepath.Join(at, "media", "p")); err != nil || st.Mode().Perm() != 0o750 {
					t.Errorf("the folder made: %v, %v", st.Mode(), err)
				}
			}
			if got := b.text("SELECT count(*) FROM copies WHERE created_by = 'atoll' AND state = 'ok' AND verified_ns IS NOT NULL"); got != "3" {
				t.Errorf("%s copies of Atoll's verified", got)
			}
			if got := b.text("SELECT count(*) FROM jobs"); got != "0" {
				t.Errorf("%s jobs left", got)
			}
			emptyTmp(t, at)
			emptyTmp(t, b.keep)
			// A scan of what was made finds each copy where its row says, as it is.
			b.scan(false)
			if got := b.text("SELECT count(*) FROM copies WHERE gone_ns IS NOT NULL OR state <> 'ok'"); got != "0" {
				t.Errorf("%s copies otherwise after a scan", got)
			}
		})
	}
}

// TestAPassCopiesOnEachDriver: a copy queued runs in the next pass of the daemon, at the lowest best-effort I/O
// priority, on every driver, as a copy atoll replicate makes does (specs.md §14, §14.1, §15.12).
func TestAPassCopiesOnEachDriver(t *testing.T) {
	needRoot(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			b := newPassBench(t, v, at)
			data := threeChunks()
			when := time.Unix(1500000000, 0)
			writeBytes(t, b.keep, "big", data, when)
			b.scan(false)
			if _, err := jobs.Replicate(b.t.Context(), b.db, b.config, b.machine, jobs.ReplicateOptions{}, func(*jobs.ReplicateItem) error { return nil }); err != nil {
				t.Fatal(err)
			}
			out := b.pass()
			want := []jobs.Operation{{Disk: "v", Kind: jobs.KindReplicate, Path: "big", Source: "k", Outcome: jobs.OutcomeReplicated}}
			if !slices.Equal(out.Operations, want) || out.Partial {
				t.Errorf("%+v, partial %v", out.Operations, out.Partial)
			}
			dropCaches(t)
			wantCopy(t, at, "big", data, when)
			emptyTmp(t, at)
			if got := b.text("SELECT count(*) FROM jobs"); got != "0" {
				t.Errorf("%s jobs left", got)
			}
		})
	}
}

// TestADeviceThatFailsDuringACopy: whatever a driver answered while the device was dead, once it is back the
// next run resolves the operation and resumes or finishes the copy: the bytes at the file's path, and the
// catalog saying so (specs.md §14.2, §17.2).
func TestADeviceThatFailsDuringACopy(t *testing.T) {
	needRoot(t)
	for _, tc := range failingDrivers {
		for _, point := range []string{"journaled", "copied", "verified", "moved", "target-synced", "source-synced"} {
			t.Run(tc.name+"/"+point, func(t *testing.T) {
				options := driverOptions(t, tc.driver)
				m := newMapped(t, tc.fs)
				at := mountPoint(t)
				mountVolume(t, m.volume, tc.driver, at, options...)
				b := newPassBench(t, m.volume, at)
				data := threeChunks()
				when := time.Unix(1500000000, 0)
				writeBytes(t, b.keep, "big", data, when)
				b.scan(false)
				must(t, "sync")
				real := b.machine
				b.machine = &failingAt{Machine: real, point: point, fire: func() { m.fail(t) }}
				t.Logf("while it was dead: %+v", b.replicate("big"))
				bringBack(t, m, tc.driver, at, options)
				b.machine = real
				after := b.replicate("big")
				dropCaches(t)
				target := filepath.Join(at, "media", "big")
				if tc.driver == "ntfs3" {
					part := filepath.Join(at, "Atoll", "tmp", b.text("SELECT COALESCE(max(CAST(path2 AS TEXT)), '') FROM ops WHERE kind = 'create'"))
					skipWhereNamesDisagree(t, target, part)
					// Both names lead to the copy: nothing is concluded, and the job waits with it (§17.2).
					if namesDisagree(target, part) {
						t.Logf("a name and its record disagree: %s and %s", target, part)
						if got := b.text("SELECT state FROM ops WHERE kind = 'create' ORDER BY id DESC LIMIT 1"); got != "planned" {
							t.Errorf("the operation is %q: %+v", got, after)
						}
						if got := b.text("SELECT count(*) FROM jobs"); got != "1" {
							t.Errorf("%s jobs: %+v", got, after)
						}
						wantCopy(t, at, "big", data, when)
						return
					}
				}
				if got := b.text("SELECT count(*) FROM ops WHERE state = 'planned'"); got != "0" {
					t.Errorf("%s operations left planned: %+v", got, after)
				}
				wantCopy(t, at, "big", data, when)
				if got := b.text("SELECT count(*) FROM copies WHERE disk_id = 'v' AND state = 'ok' AND created_by = 'atoll'"); got != "1" {
					t.Errorf("%s copies recorded: %+v", got, after)
				}
				if got := b.text("SELECT count(*) FROM jobs"); got != "0" {
					t.Errorf("%s jobs left: %+v", got, after)
				}
				emptyTmp(t, at)
			})
		}
	}
}
