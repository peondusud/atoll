package watch

import (
	"fmt"
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/solver"
)

// The copies of a plan reach Queue a batch at a time, never a file split over two, each once, in plan order,
// leaving out a file in conflict and every job that is not a copy (specs.md §14.1, §26).
func TestCopiesComeInBatches(t *testing.T) {
	const files = 2500
	q := &copyQueue{}
	for i := range files {
		q.files = append(q.files, solver.File{Path: fmt.Sprintf("f%04d", i), Size: int64(i), Placement: &solver.Placement{Priority: i % 5}})
		state := "ok"
		if i == 7 {
			state = string(solver.StateConflict)
		}
		q.remembered = append(q.remembered, settled{id: int64(i + 1), state: state})
	}
	var batches [][]PlannedJob
	q.queue = func(jobs []PlannedJob) error {
		batches = append(batches, slices.Clone(jobs))
		return nil
	}
	var want []string
	for i := range files {
		path := q.files[i].Path
		jobs := []solver.Job{{Kind: solver.JobReplicate, Path: path, Src: "a", Dst: "b"}}
		if i%3 == 0 {
			// A file the plan gives two copies.
			jobs = append(jobs, solver.Job{Kind: solver.JobReplicate, Path: path, Src: "a", Dst: "c"})
		}
		if i == 9 {
			if err := q.add(i, []solver.Job{{Kind: solver.JobRefresh, Path: path, Src: "a", Dst: "b"}}); err != nil {
				t.Fatal(err)
			}
		}
		if err := q.add(i, jobs); err != nil {
			t.Fatal(err)
		}
		if i != 7 {
			for _, j := range jobs {
				want = append(want, j.Path+" "+j.Dst)
			}
		}
	}
	if err := q.flush(); err != nil {
		t.Fatal(err)
	}
	var got []string
	for n, batch := range batches {
		// Full, but for the last, and never more than one file past full: a file here has two copies at most.
		if (n < len(batches)-1 && len(batch) < copyBatch) || len(batch) > copyBatch+1 {
			t.Errorf("batch %d holds %d", n, len(batch))
		}
		// A file's copies end its batch together.
		if last := batch[len(batch)-1]; n < len(batches)-1 && batches[n+1][0].FileID == last.FileID {
			t.Errorf("file %d split between batches %d and %d", last.FileID, n, n+1)
		}
		for _, j := range batch {
			if f := &q.files[j.FileID-1]; j.Kind != solver.JobReplicate || j.Size != f.Size || j.Priority != int64(f.Placement.Priority) {
				t.Errorf("%+v", j)
			}
			got = append(got, j.Path+" "+j.Dst)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("%d copies queued, want %d", len(got), len(want))
	}
}
