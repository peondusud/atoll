package ops

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/config"
)

// What a catalog rebuild finds in a disk's trash: an operation's folder that holds one file, below folders of its
// path; a repair's that holds the chunks of one file. Anything else is no entry, and is told of: a name that is no
// date or no operation's, an empty folder, two files, a link, chunks of two files or what is no chunk (specs.md §9,
// §17).
func TestFoundTrash(t *testing.T) {
	mount := t.TempDir()
	trash := filepath.Join(mount, "Atoll", "trash")
	write := func(path, text string) {
		t.Helper()
		full := filepath.Join(trash, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("2026-01-02/9/a/b.mkv", "four")
	write("2026-01-02/3.chunks/c.mkv/00000000", "abc")
	write("2026-01-02/3.chunks/c.mkv/00000007", "")
	write("2026-01-02/4/x", "1")
	write("2026-01-02/4/y", "2")
	write("2026-01-02/5.chunks/d/00000000", "1")
	write("2026-01-02/5.chunks/e/00000000", "2")
	write("2026-01-02/6.chunks/f/notes", "3")
	write("2026-01-02/07/g", "leading zero")
	write("2026-1-2/8/h", "no date as Atoll writes one")
	write("2026-01-01/2/i", "an earlier date")
	if err := os.MkdirAll(filepath.Join(trash, "2026-01-02", "10"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(trash, "2026-01-02", "11"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", filepath.Join(trash, "2026-01-02", "11", "link")); err != nil {
		t.Fatal(err)
	}
	d, err := Open(&onDisk{}, &config.Disk{ID: "t1", Mount: mount, Workdir: "Atoll"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var left []string
	found, err := d.FoundTrash(func(place, why string) { left = append(left, place+": "+why) })
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range found {
		key := "no inode"
		if e.FileKey != "" {
			key = "inode"
		}
		got = append(got, strings.Join([]string{e.Day.Format("2006-01-02"), e.TrashPath, e.Path, key}, " ")+" "+strconv.FormatInt(e.Size, 10))
		if e.Chunks != strings.Contains(e.TrashPath, ".chunks/") {
			t.Errorf("%+v", e)
		}
	}
	want := []string{
		"2026-01-01 2026-01-01/2/i i inode 15",
		"2026-01-02 2026-01-02/3.chunks/c.mkv c.mkv no inode 3",
		"2026-01-02 2026-01-02/9/a/b.mkv a/b.mkv inode 4",
	}
	if !slices.Equal(got, want) {
		t.Errorf("found %q", got)
	}
	wantLeft := []string{
		"2026-01-02/07: no operation's",
		"2026-01-02/4: holds 2 files",
		"2026-01-02/5.chunks: holds the chunks of several files",
		"2026-01-02/6.chunks: holds what is no chunk",
		"2026-01-02/10: holds 0 files",
		"2026-01-02/11: holds what is no file or folder",
		"2026-1-2: not a date",
	}
	slices.Sort(left)
	slices.Sort(wantLeft)
	if !slices.Equal(left, wantLeft) {
		t.Errorf("left %q", left)
	}
	// A disk with no trash has none.
	empty, err := Open(&onDisk{}, &config.Disk{ID: "t2", Mount: t.TempDir(), Workdir: "Atoll"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	if none, err := empty.FoundTrash(func(string, string) { t.Error("told of something") }); err != nil || none != nil {
		t.Errorf("%v, %v", none, err)
	}
}
