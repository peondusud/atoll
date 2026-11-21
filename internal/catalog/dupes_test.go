package catalog

import (
	"bytes"
	"reflect"
	"testing"
)

// dupesPool is files of three sizes: two groups of the same content, a file alone with its content, files of no
// hash, and empty files of one hash, which hold nothing to save (specs.md §16).
func dupesPool(t *testing.T) *testDB {
	t.Helper()
	b := openForOps(t)
	h1, h2 := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	for _, f := range []struct {
		path string
		size int64
		hash []byte
	}{
		{"M/b", 100, h1}, {"M/a", 100, h1}, {"N/c", 100, h1}, // one content, three paths
		{"M/d", 100, h2},                 // the same size, another content: alone
		{"P/e", 50, h2}, {"P/f", 50, h2}, // a smaller group
		{"Q/g", 0, h1}, {"Q/h", 0, h1}, // empty: never a group
		{"R/i", 70, nil}, {"R/j", 70, nil}, // no hash yet
		{"RS/k", 70, nil}, {"R/z/l", 0, nil}, // not below R, and empty
	} {
		b.exec("INSERT INTO files(lpath, size, mtime_ns, blake3, replicas_target, mode, state, first_seen_ns, updated_ns) VALUES (?, ?, 1, ?, 1, 'deferred', 'ok', 1, 1)",
			[]byte(f.path), f.size, f.hash)
	}
	return b
}

// Dupes gives the groups of files of one size and one hash, by size, then hash, then path, each group's paths in byte
// order; a file alone, an unhashed one and an empty one are in none, and minSize leaves the smaller groups out.
func TestDupes(t *testing.T) {
	b := dupesPool(t)
	groups, err := Dupes(t.Context(), b.sql(), 1)
	if err != nil {
		t.Fatal(err)
	}
	want := []DupeGroup{
		{Size: 50, Hash: bytes.Repeat([]byte{2}, 32), Paths: []string{"P/e", "P/f"}},
		{Size: 100, Hash: bytes.Repeat([]byte{1}, 32), Paths: []string{"M/a", "M/b", "N/c"}},
	}
	if !reflect.DeepEqual(groups, want) {
		t.Errorf("%+v", groups)
	}
	if groups, err = Dupes(t.Context(), b.sql(), 51); err != nil || len(groups) != 1 || groups[0].Size != 100 {
		t.Errorf("of 51 bytes or more: %+v, %v", groups, err)
	}
	// Empty files are never a group, even asked for files of any size.
	if groups, err = Dupes(t.Context(), b.sql(), 0); err != nil || len(groups) != 2 {
		t.Errorf("of any size: %+v, %v", groups, err)
	}
	if groups, err = Dupes(t.Context(), b.sql(), 101); err != nil || groups != nil {
		t.Errorf("above every size: %+v, %v", groups, err)
	}
}

// Unhashed counts the files not yet hashed that hold something, at a path and below it by whole components: R holds
// R/i and R/j, not RS/k; the empty R/z/l counts for nothing; no path counts the whole pool.
func TestUnhashed(t *testing.T) {
	b := dupesPool(t)
	for under, want := range map[string]int64{"": 3, "R": 2, "R/i": 1, "RS": 1, "R/z": 0, "M": 0, "S": 0} {
		if got, err := Unhashed(t.Context(), b.sql(), under); err != nil || got != want {
			t.Errorf("under %q: %d, %v; want %d", under, got, err, want)
		}
	}
}

// RuleSubjects gives each file once with its size, and each path an ignored entry holds once without one, however
// many disks hold it (specs.md §11.3).
func TestRuleSubjects(t *testing.T) {
	b := openForOps(t)
	for i, id := range []string{"u1", "u2"} {
		b.exec(`INSERT INTO disks(id, fs_uuid, fs_type, mount_path, root, workdir, role, position, tags, state) VALUES (?, ?, 'ext4', ?, 'media', 'Atoll', 'data', ?, '[]', 'online')`,
			id, id, "/"+id, i)
		b.exec("INSERT INTO entries(lpath, disk_id, kind, created_by, size) VALUES (?, ?, 'ignored', 'user', 3)", []byte("x.tmp"), id)
	}
	b.exec("INSERT INTO entries(lpath, disk_id, kind, created_by) VALUES (?, 'u1', 'dir', 'user')", []byte("D"))
	b.exec("INSERT INTO files(lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns) VALUES (?, 7, 1, 1, 'deferred', 'ok', 1, 1)", []byte("D/a"))
	got := map[string]int64{}
	err := RuleSubjects(t.Context(), b.sql(), func(path string, size *int64) {
		if _, twice := got[path]; twice {
			t.Errorf("%q twice", path)
		}
		got[path] = -1
		if size != nil {
			got[path] = *size
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	// A folder is no subject of the rules.
	if want := map[string]int64{"D/a": 7, "x.tmp": -1}; !reflect.DeepEqual(got, want) {
		t.Errorf("%v", got)
	}
}
