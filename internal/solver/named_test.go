package solver

import (
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

func TestNamedVerdictVectors(t *testing.T) {
	type vector struct {
		Note    string      `json:"note"`
		Verdict CopyVerdict `json:"verdict"`
		Deleted bool        `json:"deleted"`
		RowKind string      `json:"row_kind"`
		Next    CopyVerdict `json:"next"`
		Whole   bool        `json:"whole"`
	}
	for _, v := range vectors.Load[vector](t, "named_verdict.json") {
		next, whole := NamedVerdict(v.Verdict, v.Deleted, v.RowKind)
		if next != v.Next || whole != v.Whole {
			t.Errorf("%s: %s, whole %v; want %s, whole %v", v.Note, next, whole, v.Next, v.Whole)
		}
	}
}
