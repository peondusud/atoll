package view

import (
	"io"
	"path/filepath"
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/vectors"
)

type viewReadDisk struct {
	ID       string           `json:"id"`
	Position int              `json:"position"`
	State    solver.DiskState `json:"state"`
	Role     string           `json:"role"`
	Tags     []string         `json:"tags"`
}

type viewReadCopy struct {
	Disk     string           `json:"disk"`
	State    solver.CopyState `json:"state"`
	GoneKind string           `json:"gone_kind"`
	OldPath  string           `json:"old_path"`
}

type viewReadVector struct {
	Disks     []viewReadDisk `json:"disks"`
	Copies    []viewReadCopy `json:"copies"`
	Presented string         `json:"presented"`
	Policy    string         `json:"policy"`
	Classes   []string       `json:"classes"`
	Read      []string       `json:"read"`
	Note      string         `json:"note"`
}

// TestReadOrderVectors: which copy the view reads, best first (specs.md §7, §21.2).
func TestReadOrderVectors(t *testing.T) {
	for i, v := range vectors.Load[viewReadVector](t, "view_read.json") {
		var list []Disk
		for _, d := range v.Disks {
			list = append(list, Disk{ID: d.ID, Position: d.Position, State: d.State, Cache: d.Role == "cache", Tags: d.Tags})
		}
		var copies []Copy
		for _, c := range v.Copies {
			copies = append(copies, Copy{Disk: c.Disk, State: c.State, GoneKind: c.GoneKind, OldPath: c.OldPath})
		}
		policy := ReadPolicy{PreferSpinning: v.Policy == "prefer-spinning", Classes: v.Classes}
		if v.Policy != "oldest" && v.Policy != "prefer-spinning" {
			t.Fatalf("vector %d: policy %q", i, v.Policy)
		}
		if got := ReadOrder(list, copies, v.Presented, policy); !slices.Equal(got, v.Read) {
			t.Errorf("vector %d (%s): %q, want %q", i, v.Note, got, v.Read)
		}
	}
}

// readBench is a catalog of two disks, each holding Films/a.mkv as "from <disk>", t1 the original, and a tree over it.
func readBench(t *testing.T, states, tags [2]string) (*Tree, *DiskRoots) {
	t.Helper()
	top := t.TempDir()
	file := filepath.Join(top, "catalog.db")
	if _, err := catalog.Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	db, err := catalog.OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	roots := NewDiskRoots()
	t.Cleanup(roots.Close)
	var list []catalog.Disk
	for i, id := range []string{"t1", "t2"} {
		dir := filepath.Join(top, id)
		mustWrite(t, filepath.Join(dir, "Films", "a.mkv"), "from "+id)
		root, err := fsx.OpenDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		roots.Set(id, root)
		d := catalog.Disk{ID: id, UUID: "u-" + id, FS: "ext4", Mount: "/mnt/" + id, Root: "media", Workdir: "Atoll", Role: "data", Position: i, State: "online"}
		if tags[i] != "" {
			d.Tags = []string{tags[i]}
		}
		list = append(list, d)
	}
	if err := catalog.SyncDisks(t.Context(), db, list); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO files(id, lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns)
			VALUES (1, x'46696c6d732f612e6d6b76', 7, 9, 1, 'deferred', 'ok', 0, 0)`,
		`INSERT INTO copies(file_id, disk_id, size, mtime_ns, created_by, first_seen_ns, btime_ns, state, mode, uid, gid, nlink)
			VALUES (1, 't1', 7, 9, 'user', 0, 100, 'ok', 420, 1, 2, 1), (1, 't2', 7, 9, 'atoll', 0, 200, 'ok', 420, 1, 2, 1)`,
		`UPDATE disks SET state = '` + states[0] + `' WHERE id = 't1'`,
		`UPDATE disks SET state = '` + states[1] + `' WHERE id = 't2'`,
	} {
		if _, err := db.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	return NewTree(db, roots, Owner{UID: 7, GID: 8}, 99), roots
}

func readOf(t *testing.T, tree *Tree) string {
	t.Helper()
	n, err := tree.Lookup(t.Context(), "Films/a.mkv")
	if err != nil || n == nil || n.Disk != "t1" {
		t.Fatalf("the file presents %+v, %v; want t1's original", n, err)
	}
	f, err := tree.Open(t.Context(), "Films/a.mkv", n)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The view reads the original by default, even when its disk is in standby. With prefer-spinning it reads a copy on a
// disk that spins, then by class; a copy it cannot open now gives way to the next (specs.md §7, §21.2). The view
// presents the original's attributes either way.
func TestTreeReadsAsThePolicySays(t *testing.T) {
	tree, roots := readBench(t, [2]string{"standby", "online"}, [2]string{})
	if got := readOf(t, tree); got != "from t1" {
		t.Errorf("oldest: read %q", got)
	}
	tree.WithRead(ReadPolicy{PreferSpinning: true})
	if got := readOf(t, tree); got != "from t2" {
		t.Errorf("prefer-spinning, t1 asleep: read %q", got)
	}
	roots.Drop("t2")
	if got := readOf(t, tree); got != "from t1" {
		t.Errorf("prefer-spinning, t2 not readable now: read %q", got)
	}

	tree, _ = readBench(t, [2]string{"online", "online"}, [2]string{"usb", "sata"})
	tree.WithRead(ReadPolicy{PreferSpinning: true})
	if got := readOf(t, tree); got != "from t1" {
		t.Errorf("both spinning, no classes: read %q", got)
	}
	tree.WithRead(ReadPolicy{PreferSpinning: true, Classes: []string{"sata", "usb"}})
	if got := readOf(t, tree); got != "from t2" {
		t.Errorf("both spinning, SATA before USB: read %q", got)
	}
	tree.WithRead(ReadPolicy{Classes: []string{"sata", "usb"}})
	if got := readOf(t, tree); got != "from t1" {
		t.Errorf("oldest with classes: read %q", got)
	}
}
