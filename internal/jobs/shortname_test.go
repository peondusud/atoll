package jobs

import (
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/trash"
)

// shortNames is a folder of an NTFS disk as its drivers answer for the short names of its entries, measured
// (docs/research/ntfs-driver.md and TestShortNamesOnEachNTFSDriver): no listing shows a short name; a look at one leads
// to the entry it names, and nothing is made under it, whatever its case on ntfs3, and only in capitals on ntfs-3g
// (exact), which makes a name in small letters beside it.
type shortNames struct {
	fsx.Dir
	at      string
	exact   bool
	entries map[string]map[string]string // by folder from the top, then by short name in capitals: the entry's own name
}

func (s *shortNames) Unwrap() fsx.Dir { return s.Dir }

// entry gives the entry a short name names, in any case, and whether a look at that name finds it.
func (s *shortNames) entry(name string) (long string, named, looked bool) {
	long, named = s.entries[s.at][strings.ToUpper(name)]
	return long, named, named && (!s.exact || name == strings.ToUpper(name))
}

func (s *shortNames) Stat(name string) (fsx.Stat, error) {
	if long, _, looked := s.entry(name); looked {
		name = long
	}
	return s.Dir.Stat(name) //nolint:wrapcheck // a test double
}

func (s *shortNames) OpenDir(name string) (fsx.Dir, error) {
	if long, _, looked := s.entry(name); looked {
		name = long
	}
	d, err := s.Dir.OpenDir(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // a test double
	}
	return &shortNames{Dir: d, at: path.Join(s.at, name), exact: s.exact, entries: s.entries}, nil
}

func (s *shortNames) Mkdir(name string, mode uint32) error {
	if _, _, looked := s.entry(name); looked {
		return syscall.EEXIST
	}
	return s.Dir.Mkdir(name, mode) //nolint:wrapcheck // a test double
}

func named(to fsx.Dir, name string) bool {
	t, ok := to.(*shortNames)
	if !ok {
		return false
	}
	_, _, looked := t.entry(name)
	return looked
}

func (s *shortNames) Rename(name string, to fsx.Dir, newName string) error {
	if named(to, newName) {
		return syscall.EEXIST
	}
	return s.Dir.Rename(name, to, newName) //nolint:wrapcheck // a test double
}

func (s *shortNames) Link(name string, to fsx.Dir, newName string) error {
	if named(to, newName) {
		return syscall.EEXIST
	}
	return s.Dir.Link(name, to, newName) //nolint:wrapcheck // a test double
}

// shortNamePool gives a pool whose second disk is NTFS, where Long Folder and Film Name.mkv have the short names
// LONGFO~1 and FILMNA~1.MKV, and whose first disk holds a folder and a file named so, other files, and b.mkv and a
// folder dir on both.
func shortNamePool(t *testing.T, exact bool) *bench {
	t.Helper()
	b := newBench(t, "t1", "t2")
	b.config.Disks[1].FS = names.NTFS
	b.tops[b.dirs["t2"]] = func(top fsx.Dir) fsx.Dir {
		return &shortNames{Dir: top, exact: exact, entries: map[string]map[string]string{"media": {"LONGFO~1": "Long Folder", "FILMNA~1.MKV": "Film Name.mkv"}}}
	}
	b.write("t1", "LONGFO~1/x.mkv", "x")
	b.write("t1", "filmna~1.mkv", "y")
	b.write("t1", "b.mkv", "b")
	b.write("t2", "b.mkv", "b")
	b.write("t1", "dir/f.mkv", "f")
	b.write("t2", "dir/f.mkv", "f")
	b.write("t2", "Long Folder/z.mkv", "z")
	b.write("t2", "Film Name.mkv", "f")
	b.scan(true)
	return b
}

// On NTFS, the short name of another entry is no name of its own: no driver creates it, and a look at it leads to that
// entry. A copy or a rename whose path goes through one is refused, before anything is copied where a look tells, and
// nothing ever lands in the folder it names (specs.md §15.8).
func TestAShortNameIsNoWayIn(t *testing.T) {
	for _, tc := range []struct {
		driver string
		exact  bool
	}{{"ntfs3", false}, {"ntfs-3g", true}} {
		t.Run(tc.driver, func(t *testing.T) {
			b := shortNamePool(t, tc.exact)
			b.rename("t1", "b.mkv", "LONGFO~1/b.mkv")
			if err := os.Rename(filepath.Join(b.dirs["t1"], "media", "dir"), filepath.Join(b.dirs["t1"], "media", "LONGFO~1", "dir")); err != nil {
				t.Fatal(err)
			}
			b.now += settleNS
			b.pass()
			b.now += settleNS
			out := b.pass()
			for _, p := range []string{"LONGFO~1/x.mkv", "filmna~1.mkv"} {
				if got := outcomeOf(out, KindReplicate, p); got != trash.OutcomeNameRefused {
					t.Errorf("%s: %q, want name-refused (%+v)", p, got, out.Operations)
				}
			}
			// A folder renamed there cannot go as one operation either: its files are refused one by one.
			for _, p := range []string{"b.mkv", "dir/f.mkv"} {
				if got := outcomeOf(out, KindRename, p); got != trash.OutcomeNameRefused {
					t.Errorf("the rename of %s: %q, want name-refused (%+v)", p, got, out.Operations)
				}
			}
			if b.exists("t2", "media/Long Folder/x.mkv") {
				t.Error("the copy landed in the folder the short name names")
			}
			if got := b.text("SELECT count(*) FROM ops WHERE kind = 'create' AND disk_id = 't2'"); got != "0" {
				t.Errorf("%s copies journaled on t2", got)
			}
			if got := listed(t, filepath.Join(b.dirs["t2"], "media")); !slices.Equal(got, []string{"Film Name.mkv", "Long Folder", "b.mkv", "dir"}) {
				t.Errorf("t2 holds %v", got)
			}
			if got := listed(t, filepath.Join(b.dirs["t2"], "media", "Long Folder")); !slices.Equal(got, []string{"z.mkv"}) {
				t.Errorf("Long Folder holds %v", got)
			}
			// The other way, the files of t2 are copied to t1 as ever.
			if !b.exists("t1", "media/Long Folder/z.mkv") || !b.exists("t1", "media/Film Name.mkv") {
				t.Errorf("%+v", out.Operations)
			}
		})
	}
}

// listed gives the names a folder holds, in order.
func listed(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}
