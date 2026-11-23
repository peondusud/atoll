package names

import (
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

func TestHoldsWildcardVectors(t *testing.T) {
	type vector struct {
		Path    *string `json:"path"`
		PathB64 *string `json:"path_b64"`
		Holds   bool    `json:"holds"`
		Note    string  `json:"note"`
	}
	for _, v := range vectors.Load[vector](t, "wildcard_name.json") {
		path, err := vectors.Path(v.Path, v.PathB64)
		if err != nil {
			t.Fatal(err)
		}
		if got := HoldsWildcard(path); got != v.Holds {
			t.Errorf("HoldsWildcard(%q) = %v, want %v (%s)", path, got, v.Holds, v.Note)
		}
	}
}
