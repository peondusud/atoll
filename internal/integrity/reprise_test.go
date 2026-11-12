package integrity

import (
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

type repriseChunk struct {
	Bad   *bool   `json:"bad"`
	Saved *bool   `json:"saved"`
	Holds *string `json:"holds"`
}

type repriseVector struct {
	Note      string         `json:"note"`
	Chunks    []repriseChunk `json:"chunks"`
	CtimeSame *bool          `json:"ctime_same"`
	Action    *string        `json:"action"`
	Error     bool           `json:"error"`
}

func TestRepairRepriseVectors(t *testing.T) {
	for i, v := range vectors.Load[repriseVector](t, "repair_reprise.json") {
		if v.Chunks == nil || v.CtimeSame == nil || (v.Action == nil) == !v.Error {
			t.Fatalf("vector %d: want chunks, ctime_same, and exactly one of action and error", i)
		}
		chunks := make([]RepairChunk, len(v.Chunks))
		for j, c := range v.Chunks {
			if c.Bad == nil || c.Saved == nil || c.Holds == nil {
				t.Fatalf("vector %d: chunk %d wants bad, saved and holds", i, j)
			}
			chunks[j] = RepairChunk{Bad: *c.Bad, Saved: *c.Saved, Holds: Holds(*c.Holds)}
		}
		got, err := RepairReprise(chunks, *v.CtimeSame)
		switch {
		case v.Error && err == nil:
			t.Errorf("vector %d (%s): %q, want an error", i, v.Note, got)
		case !v.Error && (err != nil || string(got) != *v.Action):
			t.Errorf("vector %d (%s): %q, %v, want %q", i, v.Note, got, err, *v.Action)
		}
	}
}

// Whatever the chunks hold, the reprise never says finish while a chunk holds anything but its hash, and never
// writes a time over a copy whose content its repair does not explain (specs.md §16.1).
func TestRepairRepriseNeverFinishesABadCopy(t *testing.T) {
	words := []Holds{HoldsGood, HoldsSaved, HoldsOther}
	var walk func(chunks []RepairChunk)
	walk = func(chunks []RepairChunk) {
		if len(chunks) == 4 {
			for _, same := range []bool{false, true} {
				got, err := RepairReprise(chunks, same)
				if err != nil {
					return
				}
				for _, c := range chunks {
					if got == RepairFinish && c.Holds != HoldsGood {
						t.Errorf("%+v, %v: finish", chunks, same)
					}
					if (got == RepairKeep || got == RepairFinish) && !c.Bad && c.Holds != HoldsGood {
						t.Errorf("%+v, %v: %s over a chunk the repair never touched", chunks, same, got)
					}
				}
			}
			return
		}
		for _, bad := range []bool{false, true} {
			for _, saved := range []bool{false, true} {
				for _, w := range words {
					walk(append(chunks[:len(chunks):len(chunks)], RepairChunk{Bad: bad, Saved: saved, Holds: w}))
				}
			}
		}
	}
	walk(nil)
}
