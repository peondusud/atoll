package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/watch"
)

// TestMeasureAViewOperation times a pass of the daemon that takes in one operation of Atoll's own view on a large pool
// (specs.md §15.13, §21.2): ATOLL_MEASURE=50000 files a disk, on two disks, each file on both. The pass is given the
// name the watcher heard, as the daemon gives it; it takes the operation in, and its time is the log's.
func TestMeasureAViewOperation(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("ATOLL_MEASURE"))
	if n == 0 {
		t.Skip("ATOLL_MEASURE gives the files a disk")
	}
	b := newBench(t, "t1", "t2")
	for _, id := range []string{"t1", "t2"} {
		for i := range n {
			full := filepath.Join(b.dirs[id], "media", fmt.Sprintf("D%03d", i%500), fmt.Sprintf("f%06d", i))
			if i < 500 {
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(full, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	b.scan(false)
	b.now += 3_600_000_000_000
	// A pass with nothing to do, given a name, as a reference.
	timed := func(what string, scope Scope) *Outcome {
		start := time.Now()
		out, err := RunLaterOn(t.Context(), b.db, b.config, b, true, nil, nil, scope, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %v", what, time.Since(start).Round(time.Millisecond))
		return out
	}
	timed("a pass given one name, nothing to take in", Scope{"t1": {Names: []watch.Named{{Path: "D000/f000000"}}}})
	for _, tc := range []struct {
		what string
		op   catalog.ViewOp
		on   func()
	}{
		{"a file the view made", catalog.ViewOp{Kind: catalog.ViewCreate, Disk: "t1", Path: "D001/new"}, func() { b.write("t1", "D001/new", "new") }},
		{"a file the view renamed", catalog.ViewOp{Kind: catalog.ViewRename, Disk: "t1", Path: "D002/f000002", Path2: "D002/renamed"}, func() { b.rename("t1", "D002/f000002", "D002/renamed") }},
	} {
		tc.on()
		v := tc.op
		v.AtNS = b.now
		if err := catalog.BeginViewOp(t.Context(), b.db, &v); err != nil {
			t.Fatal(err)
		}
		if err := catalog.FinishViewOp(t.Context(), b.db, &v, catalog.OpDone, false, b.now); err != nil {
			t.Fatal(err)
		}
		names := []watch.Named{{Path: v.Path}}
		if v.Path2 != "" {
			names = append(names, watch.Named{Path: v.Path2})
		}
		out := timed(tc.what+", the pass for its event", Scope{"t1": {Names: names}})
		for range 3 {
			if b.count("SELECT count(*) FROM view_ops WHERE taken_ns IS NULL") == 0 {
				break
			}
			b.now += 60_000_000_000
			scope := Scope{}
			for id, again := range out.Again {
				scope.Add(id, again)
			}
			if len(scope) == 0 {
				scope["t2"] = Looking{Names: []watch.Named{{Path: "D000/f000000"}}}
			}
			out = timed(tc.what+", a pass while it is not taken in", scope)
		}
		if got := b.count("SELECT count(*) FROM view_ops WHERE taken_ns IS NULL"); got != 0 {
			t.Errorf("%s: not taken in", tc.what)
		}
	}
}
