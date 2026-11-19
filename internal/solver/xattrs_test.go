package solver

import (
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

type xattrVector struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type xattrsVector struct {
	Name   *string       `json:"name"`
	Kept   bool          `json:"kept"`
	Want   []xattrVector `json:"want"`
	Have   []xattrVector `json:"have"`
	Set    []string      `json:"set"`
	Remove []string      `json:"remove"`
	Note   string        `json:"note"`
}

func bytesOf(list []xattrVector) map[string][]byte {
	out := make(map[string][]byte, len(list))
	for _, a := range list {
		out[a.Name] = []byte(a.Value)
	}
	return out
}

func TestXattrsVectors(t *testing.T) {
	for i, v := range vectors.Load[xattrsVector](t, "xattrs.json") {
		if v.Name != nil {
			if got := KeptXattr(*v.Name); got != v.Kept {
				t.Errorf("vector %d (%s): kept %v, want %v", i, v.Note, got, v.Kept)
			}
			continue
		}
		got := PlanXattrs(bytesOf(v.Want), bytesOf(v.Have))
		if !slices.Equal(got.Set, v.Set) || !slices.Equal(got.Remove, v.Remove) {
			t.Errorf("vector %d (%s): %+v, want set %q and remove %q", i, v.Note, got, v.Set, v.Remove)
		}
	}
}
