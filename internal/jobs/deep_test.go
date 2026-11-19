package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/peondusud/atoll/internal/names"
)

// renameAt renames a name below a folder to another, through handles on both folders, whose paths fit PATH_MAX where
// the name's may not.
func renameAt(t *testing.T, top, from, to string) {
	t.Helper()
	open := func(rel string) int {
		fd, err := unix.Open(filepath.Join(top, filepath.Dir(rel)), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		return fd
	}
	a, b := open(from), open(to)
	defer func() { _ = unix.Close(a); _ = unix.Close(b) }()
	if err := unix.Renameat(a, filepath.Base(from), b, filepath.Base(to)); err != nil {
		t.Fatal(err)
	}
}

// deepName gives a relative path of exactly n bytes, n > 0, made of folders of at most 200 bytes.
func deepName(n int) string {
	var parts []string
	for n > 0 {
		k := min(n, 200)
		if n-k == 1 {
			k--
		}
		parts = append(parts, strings.Repeat("d", k))
		n -= k
		if n > 0 {
			n--
		}
	}
	return strings.Join(parts, "/")
}

// TestADeletionBesideANamePastTheLimit: a copy deleted straight on a disk whose walk leaves a name past the limit of its
// branch may have gone there, so its file is kept, nothing goes to the trash, and the other copy stays; deleted on every
// disk at once, as through the view, the file goes, with nothing left behind (specs.md §15.8, §15.12).
func TestADeletionBesideANamePastTheLimit(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for _, path := range []string{"a/x", "b/y", "c/z"} {
		b.write("t1", path, path)
		b.write("t2", path, path)
	}
	b.scan(true)
	media := filepath.Join(b.dirs["t1"], "media")
	limit := names.MaxBranchPathBytes - len(b.dirs["t1"]) - len("/media/")
	deep := deepName(limit - 1)
	if err := os.MkdirAll(filepath.Join(media, deep), 0o755); err != nil {
		t.Fatal(err)
	}
	renameAt(t, media, "b/y", deep+"/y")
	// a/x straight on t1 alone; c/z on both disks, as the view deletes.
	b.remove("t1", "a/x")
	b.remove("t1", "c/z")
	b.remove("t2", "c/z")
	for range 2 {
		out := b.settle()
		for i := range out.Decisions {
			if d := &out.Decisions[i]; d.Kind == "deletion" {
				t.Errorf("a deletion: %+v", d)
			}
		}
	}
	for _, path := range []string{"a/x", "b/y"} {
		if !b.exists("t2", "media/"+path) || b.count("SELECT count(*) FROM files WHERE lpath = ?", []byte(path)) != 1 {
			t.Errorf("%s: its file or its other copy went", path)
		}
	}
	if got := b.count("SELECT count(*) FROM files WHERE lpath = ?", []byte("c/z")); got != 0 {
		t.Errorf("the file deleted on every disk is still there: %d rows", got)
	}
	if got := b.count("SELECT count(*) FROM jobs WHERE kind = 'trash'") + b.count("SELECT count(*) FROM trash"); got != 0 {
		t.Errorf("%d trash jobs and entries", got)
	}
}

// TestACopyPastTheLimitIsNoDeletion: a folder renamed on one disk so that a copy below it goes past the limit of that
// disk's mergerfs branch, or a copy moved past it, is not read as deleted by the passes after it: its other copies stay
// where they are, nothing goes to the trash, and its data is in the library still once moved back within the limit
// (invariant 4, specs.md §15.8). Where the walk cannot say where the copy went, its file is kept, and made whole again.
func TestACopyPastTheLimitIsNoDeletion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from, to func(limit int) string
		back     string
	}{
		{"a folder above it renamed", func(int) string { return "a" }, func(limit int) string { return deepName(limit-3) + "/a" }, "c/x"},
		{"the copy itself moved", func(int) string { return "a/x" }, func(limit int) string { return deepName(limit-1) + "/x" }, "c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t, "t1", "t2")
			b.write("t1", "a/x", "x")
			b.write("t2", "a/x", "x")
			b.scan(true)
			limit := names.MaxBranchPathBytes - len(b.dirs["t1"]) - len("/media/")
			from, to := tc.from(limit), tc.to(limit)
			// The folder above the target fits, so the kernel takes its path whole.
			if err := os.MkdirAll(filepath.Join(b.dirs["t1"], "media", filepath.Dir(to)), 0o755); err != nil {
				t.Fatal(err)
			}
			media := filepath.Join(b.dirs["t1"], "media")
			renameAt(t, media, from, to)
			for range 3 {
				out := b.settle()
				for i := range out.Operations {
					if op := &out.Operations[i]; op.Kind == KindTrash || op.Kind == KindRename || op.Kind == KindFolder {
						t.Errorf("an operation: %+v", op)
					}
				}
				for i := range out.Decisions {
					if d := &out.Decisions[i]; d.Kind != "kept" {
						t.Errorf("a decision: %+v", d)
					}
				}
			}
			if !b.exists("t2", "media/a/x") || b.count("SELECT count(*) FROM files WHERE lpath = ?", []byte("a/x")) != 1 {
				t.Fatal("the file went")
			}
			if got := b.count("SELECT count(*) FROM trash"); got != 0 {
				t.Errorf("%d entries in the trash", got)
			}
			// Back within the limit, at a name the walk reads: its data is in the library.
			renameAt(t, media, to, "c")
			b.settle()
			if got := b.count("SELECT count(*) FROM copies c JOIN files f ON f.id = c.file_id WHERE c.disk_id = 't1' AND COALESCE(c.old_lpath, f.lpath) = ?", []byte(tc.back)); got != 1 {
				t.Errorf("the data moved back is not in the library at %s", tc.back)
			}
			if !b.exists("t2", "media/a/x") && !b.exists("t2", "media/"+tc.back) {
				t.Error("the other copy went")
			}
		})
	}
}
