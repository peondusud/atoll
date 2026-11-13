package layout

import (
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

type workNameVector struct {
	Folder string `json:"folder"`
	Name   string `json:"name"`
	OpID   *int64 `json:"op_id"`
	Index  *int64 `json:"index"`
	Chunks bool   `json:"chunks"`
	Note   string `json:"note"`
}

// A name of the work folder names an operation only as Atoll writes it: what reads one back decides what tmp/ loses
// and where a new catalog starts its operations, in both versions alike (specs.md §9, §14.2, §16.1, §17).
func TestWorkNameVectors(t *testing.T) {
	for i, v := range vectors.Load[workNameVector](t, "work_names.json") {
		switch v.Folder {
		case "tmp":
			if v.Chunks || (v.Index != nil && v.OpID == nil) {
				t.Fatalf("vector %d: a name of tmp/ wants op_id, with index for a chunk's", i)
			}
			op, index, chunk, ok := PartOp(v.Name)
			switch {
			case v.OpID == nil:
				if ok {
					t.Errorf("vector %d (%s): %q names the operation %d", i, v.Note, v.Name, op)
				}
			case !ok || op != *v.OpID || chunk != (v.Index != nil) || (chunk && index != *v.Index):
				t.Errorf("vector %d (%s): %q read as %d, %d, %v, %v", i, v.Note, v.Name, op, index, chunk, ok)
			case chunk && ChunkPartName(op, index) != v.Name, !chunk && PartName(op) != v.Name:
				t.Errorf("vector %d (%s): %q is not written back as it is read", i, v.Note, v.Name)
			}
		case "trash":
			if v.Index != nil || (v.Chunks && v.OpID == nil) {
				t.Fatalf("vector %d: a name of the trash wants op_id, with chunks for a folder of chunks", i)
			}
			op, chunks, ok := TrashOp(v.Name)
			switch {
			case v.OpID == nil:
				if ok {
					t.Errorf("vector %d (%s): %q names the operation %d", i, v.Note, v.Name, op)
				}
			case !ok || op != *v.OpID || chunks != v.Chunks:
				t.Errorf("vector %d (%s): %q read as %d, %v, %v", i, v.Note, v.Name, op, chunks, ok)
			}
		default:
			t.Fatalf("vector %d: folder %q", i, v.Folder)
		}
	}
}

// What TrashPath and ChunksPath write, TrashOp reads back.
func TestTrashOpReadsWhatIsWritten(t *testing.T) {
	for _, id := range []int64{1, 4813, 1<<63 - 1} {
		for _, chunks := range []bool{false, true} {
			where := TrashPath
			if chunks {
				where = ChunksPath
			}
			path, err := where(0, id, "a")
			if err != nil {
				t.Fatal(err)
			}
			folder := path[len("1970-01-01/") : len(path)-len("/a")]
			if op, isChunks, ok := TrashOp(folder); !ok || op != id || isChunks != chunks {
				t.Errorf("%q read as %d, %v, %v", folder, op, isChunks, ok)
			}
		}
	}
}
