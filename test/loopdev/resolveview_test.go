//go:build loopdev

package loopdev

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/trash"
)

// resolve runs atoll conflicts resolve with --yes on the bench, and gives each copy's outcome by disk.
func (b *passBench) resolve(path, keep string) (*trash.ResolveSummary, map[string]string) {
	b.t.Helper()
	s, err := trash.Open(b.t.Context(), b.db, b.config, b.machine, true)
	mustDo(b.t, err)
	defer s.Close()
	outcomes := map[string]string{}
	sum, err := s.Resolve(b.t.Context(), path, keep, func(i *trash.ResolveItem) error {
		outcomes[i.Disk] = i.Outcome
		return nil
	})
	mustDo(b.t, err)
	return sum, outcomes
}

// TestAResolveLeavesWhatAProgramWritesThroughTheView: flaw 26 of conformance/models/README.md on real disks, Atoll's
// own view served by FUSE, on a pool of ext4 and of ext4 and ntfs-3g. A file in conflict; a program opens it through
// the view to write; atoll conflicts resolve --keep the other disk moves no copy the view holds open
// (resolve-view-free, the maintainer's remedy, 2026-11-25): that copy stays, changed, and so does the conflict. What the
// program writes then is at the file's path, never in the trash. Once it closed it, the resolve sends that copy, as
// the user asks, and the view presents the copy kept (specs.md §15.7, §21.2).
func TestAResolveLeavesWhatAProgramWritesThroughTheView(t *testing.T) {
	needRoot(t)
	for _, tc := range []struct {
		name   string
		fs     names.Filesystem
		driver string
	}{{"ext4", names.Ext4, ""}, {"ntfs-3g", names.NTFS, "ntfs-3g"}} {
		t.Run(tc.name, func(t *testing.T) {
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, driverOptions(t, tc.driver)...)
			b := newPassBenchWith(t, v, at, "[defaults]\nsettle = \"0s\"\n")
			write(t, b.keep, "a.mkv", "the film")
			write(t, b.at, "a.mkv", "another cut")
			b.scan(true)
			if got := b.text("SELECT state FROM files"); got != "conflict" {
				t.Fatalf("the file is %s", got)
			}
			_, point := fstabLine(b)
			mustDo(t, os.MkdirAll(point, 0o755))
			_, err := serveView(t, b, point, true)
			mustDo(t, err)
			seen, err := os.ReadFile(filepath.Join(point, "a.mkv"))
			mustDo(t, err)
			held, top, keep := "k", b.keep, "v"
			if string(seen) != "the film" {
				held, top, keep = "v", b.at, "k"
			}
			f, err := os.OpenFile(filepath.Join(point, "a.mkv"), os.O_WRONLY, 0)
			mustDo(t, err)
			sum, outcomes := b.resolve("a.mkv", keep)
			if sum.Resolved || outcomes[held] != trash.OutcomeChanged {
				t.Errorf("resolved %v, outcomes %v", sum.Resolved, outcomes)
			}
			const late = "a write the program makes after the resolve"
			_, err = f.WriteAt([]byte(late), 0)
			mustDo(t, err)
			mustDo(t, f.Close())
			got, err := os.ReadFile(filepath.Join(top, "media", "a.mkv"))
			if err != nil || !strings.HasPrefix(string(got), late) {
				t.Errorf("the copy the view held: %q, %v", got, err)
			}
			// FUSE tells the view of the close by RELEASE, after close(2) returned: the view lets go of its hold then
			// (view_ops back to done), which a resolve right after the close could precede (the CI, 2026-11-28).
			waitFor(t, "the view to let go of the file", func() bool {
				return b.text("SELECT count(*) FROM view_ops WHERE state = 'open'") == "0"
			})
			sum, outcomes = b.resolve("a.mkv", keep)
			if !sum.Resolved || outcomes[held] != trash.OutcomeTrashed {
				t.Errorf("resolved %v, outcomes %v", sum.Resolved, outcomes)
			}
			b.pass()
			if open := b.text("SELECT count(*) FROM view_ops WHERE taken_ns IS NULL"); open != "0" {
				t.Errorf("%s operations of the view not taken in", open)
			}
			shown, err := os.ReadFile(filepath.Join(point, "a.mkv"))
			if err != nil || strings.HasPrefix(string(shown), late) || string(shown) == string(seen) {
				t.Errorf("the view presents %q, %v; not the copy kept", shown, err)
			}
		})
	}
}
