package config

import (
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/rules"
	"github.com/peondusud/atoll/internal/vectors"
)

type rulesCheckVector struct {
	TOML  string `json:"toml"`
	Files []struct {
		Path    *string `json:"path"`
		PathB64 *string `json:"path_b64"`
		Size    *int64  `json:"size"`
	} `json:"files"`
	Unreached []string `json:"unreached"`
	Hidden    []struct {
		Rule string `json:"rule"`
		By   string `json:"by"`
	} `json:"hidden"`
	Note string `json:"note"`
}

// These live here rather than in internal/rules for the reason the policy's vectors do: a case starts from the text
// of a configuration.
func TestRulesCheckVectors(t *testing.T) {
	for i, v := range vectors.Load[rulesCheckVector](t, "rules_check.json") {
		c, err := Parse([]byte(v.TOML))
		if err != nil {
			t.Errorf("vector %d (%s): %v", i, v.Note, err)
			continue
		}
		reach := rules.NewReach(c.Rules)
		for _, f := range v.Files {
			path, err := vectors.Path(f.Path, f.PathB64)
			if err != nil {
				t.Fatal(err)
			}
			reach.Add(path, f.Size)
		}
		var unreached []string
		for _, at := range reach.Unreached() {
			unreached = append(unreached, c.Rules[at].Label)
		}
		if !slices.Equal(unreached, v.Unreached) && (len(unreached) != 0 || len(v.Unreached) != 0) {
			t.Errorf("vector %d (%s): unreached %q, want %q", i, v.Note, unreached, v.Unreached)
		}
		type hiding struct{ rule, by string }
		var got, want []hiding
		for _, h := range rules.Hidden(c.Rules) {
			got = append(got, hiding{c.Rules[h.Rule].Label, c.Rules[h.By].Label})
		}
		for _, h := range v.Hidden {
			want = append(want, hiding{h.Rule, h.By})
		}
		if !slices.Equal(got, want) {
			t.Errorf("vector %d (%s): hidden %v, want %v", i, v.Note, got, want)
		}
	}
}
