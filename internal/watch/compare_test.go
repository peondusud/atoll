package watch

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/disks"
)

// snapshot gives every row of the tables a look could write, in a stable order.
func (b *bench) snapshot() string {
	b.t.Helper()
	var lines []string
	for _, table := range []string{"files", "copies", "entries", "disks", "ops", "events", "jobs", "trash"} {
		lines = append(lines, b.rowsOf(table)...)
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

func (b *bench) rowsOf(table string) []string {
	b.t.Helper()
	rows, err := b.db.QueryContext(b.t.Context(), "SELECT * FROM "+table)
	if err != nil {
		b.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		b.t.Fatal(err)
	}
	var lines []string
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			b.t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf("%s %v", table, values))
	}
	if err := rows.Err(); err != nil {
		b.t.Fatal(err)
	}
	return lines
}

// atoll catalog verify walks each disk not failed and compares it with the catalog as a scan would: what is new,
// changed and absent is reported, and nothing is written, to the catalog or to a disk (specs.md §9, §25). A disk that
// fails its confirmation past the cache is missing, and nothing is concluded of it.
func TestCompareWritesNothing(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "a", "x")
	b.write("t1", "b", "y")
	b.write("t2", "c", "z")
	b.scan(Options{})
	if err := os.Remove(filepath.Join(b.media("t1"), "a")); err != nil {
		t.Fatal(err)
	}
	b.write("t1", "b", "longer")
	b.write("t1", "new", "n")
	before := b.snapshot()
	out, err := Compare(t.Context(), b.db, b.config, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Disks) != 2 {
		t.Fatalf("%+v", out.Disks)
	}
	r := out.Disks[0]
	if r.ID != "t1" || !r.Walked || r.New != 1 || !slices.Equal(r.Changed, []string{"b"}) || r.Seen != nil {
		t.Errorf("t1: %+v", r)
	}
	if !slices.Contains(r.Absent, "a") && !slices.ContainsFunc(out.Disks[0].Moved, func(m MovedCopy) bool { return m.Path == "a" }) {
		t.Errorf("a is not reported gone: %+v", r)
	}
	if after := b.snapshot(); after != before {
		t.Errorf("the catalog was written:\nbefore %s\nafter %s", before, after)
	}

	b.unread = map[string]bool{b.dirs["t2"]: true}
	out, err = Compare(t.Context(), b.db, b.config, b)
	if err != nil {
		t.Fatal(err)
	}
	if r := out.Disks[1]; r.Walked || r.Verdict.State != disks.Missing {
		t.Errorf("t2, unreadable past the cache: %+v", r)
	}
	if after := b.snapshot(); after != before {
		t.Error("the catalog was written for a disk that failed its confirmation")
	}

	b.reconfigure("", "", map[string]string{"t2": "state = \"failed\"\n"}, "")
	out, err = Compare(t.Context(), b.db, b.config, b)
	if err != nil || len(out.Disks) != 1 || out.Disks[0].ID != "t1" {
		t.Errorf("a failed disk is never read again: %+v, %v", out, err)
	}
}

// A catalog that cannot be read is an error, never an answer of nothing: no plan, no capacity, no transfer, no
// target, no comparison and no status comes of it (specs.md §9).
func TestACatalogThatCannotBeRead(t *testing.T) {
	b := newBench(t, "t1", "t2")
	b.write("t1", "a", "x")
	b.scan(Options{})
	if err := b.db.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, c := t.Context(), b.config
	all := func(string) bool { return true }
	for what, call := range map[string]func() error{
		"scan":     func() error { _, err := Scan(ctx, b.db, c, b, Options{}); return err },
		"settle":   func() error { return Settle(ctx, b.db, c, b.now, nil) },
		"compare":  func() error { _, err := Compare(ctx, b.db, c, b); return err },
		"capacity": func() error { _, err := Capacity(ctx, b.db, c, b.now); return err },
		"targets": func() error {
			_, err := ParallelTargets(ctx, b.db, c, b.now, "b", "t1", func(string) (int64, bool) { return 0, false }, all)
			return err
		},
		"transfers": func() error { _, err := CacheTransfers(ctx, b.db, c, b.now, nil); return err },
		"waits":     func() error { _, _, err := CacheWaits(ctx, b.db, c, b.now); return err },
		"status": func() error {
			_, err := ReadStatus(ctx, b.db, "", nil, func(*catalog.FileRow) error { return nil })
			return err
		},
		"hashes":   func() error { _, err := HashNew(ctx, b.db, c, b, b.now+1); return err },
		"replicas": func() error { return ReadReplicas(ctx, b.db, c, b, b.now, all, all) },
	} {
		if err := call(); err == nil {
			t.Errorf("%s: no error from a catalog that cannot be read", what)
		}
	}
}
