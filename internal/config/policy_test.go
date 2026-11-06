package config

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/peondusud/atoll/internal/rules"
	"github.com/peondusud/atoll/internal/vectors"
)

type policyVector struct {
	TOML     *string           `json:"toml"`
	TOMLPath *string           `json:"toml_path"`
	Path     *string           `json:"path"`
	PathB64  *string           `json:"path_b64"`
	Size     *int64            `json:"size"`
	Rule     *string           `json:"rule"`
	NoRule   bool              `json:"no_rule"`
	Policy   json.RawMessage   `json:"policy"`
	Sources  map[string]string `json:"sources"`
	Note     string            `json:"note"`
}

// policyJSON writes a policy under the names of conformance/vectors/README.md.
func policyJSON(p rules.Policy) map[string]any {
	if p.Action == rules.Ignore {
		return map[string]any{"action": string(p.Action)}
	}
	list := func(l []string) []string {
		if l == nil {
			return []string{}
		}
		return l
	}
	return map[string]any{
		"action":           string(p.Action),
		"replicas":         p.Replicas,
		"mode":             p.Mode,
		"settle_ns":        p.Settle.Nanoseconds(),
		"priority":         p.Priority,
		"tags":             list(p.Tags),
		"tags_mode":        p.TagsMode,
		"domains":          list(p.Domains),
		"spread":           p.Spread,
		"spread_mode":      p.SpreadMode,
		"colocate":         p.Colocate,
		"overflow":         p.Overflow,
		"cache":            p.Cache,
		"cache_min_age_ns": p.CacheMinAge.Nanoseconds(),
	}
}

// These live here rather than in internal/rules because a case starts from the text of
// a configuration, and config imports rules.
func TestPolicyVectors(t *testing.T) {
	for i, v := range vectors.Load[policyVector](t, "policy.json") {
		if (v.TOML == nil) == (v.TOMLPath == nil) || (v.Rule == nil) == !v.NoRule || v.Policy == nil {
			t.Errorf("vector %d (%s): want toml or toml_path, rule or no_rule, and policy", i, v.Note)
			continue
		}
		var text []byte
		if v.TOML != nil {
			text = []byte(*v.TOML)
		} else {
			text = vectors.RepoFile(t, *v.TOMLPath)
		}
		c, err := Parse(text)
		if err != nil {
			t.Errorf("vector %d (%s): %v", i, v.Note, err)
			continue
		}
		path, err := vectors.Path(v.Path, v.PathB64)
		if err != nil {
			t.Errorf("vector %d (%s): %v", i, v.Note, err)
			continue
		}
		p := c.Policy(path, v.Size)
		switch {
		case v.NoRule && p.Rule != nil:
			t.Errorf("vector %d (%s): rule %q, want none", i, v.Note, p.Rule.Label)
		case !v.NoRule && p.Rule == nil:
			t.Errorf("vector %d (%s): no rule, want %q", i, v.Note, *v.Rule)
		case !v.NoRule && p.Rule.Label != *v.Rule:
			t.Errorf("vector %d (%s): rule %q, want %q", i, v.Note, p.Rule.Label, *v.Rule)
		}
		raw, err := json.Marshal(policyJSON(p))
		if err != nil {
			t.Fatal(err)
		}
		if a, b := canonical(t, raw), canonical(t, v.Policy); a != b {
			t.Errorf("vector %d (%s): policy\n%s, want\n%s", i, v.Note, a, b)
		}
		if p.Action == rules.Ignore && !reflect.DeepEqual(p, rules.Policy{Rule: p.Rule, Action: rules.Ignore}) {
			t.Errorf("vector %d (%s): an ignored file has other values: %+v", i, v.Note, p)
		}
		sources := p.Sources(c.Pool)
		for name, want := range v.Sources {
			if got, ok := sources[name]; !ok || string(got) != want {
				t.Errorf("vector %d (%s): %s comes from %q, want %q", i, v.Note, name, got, want)
			}
		}
	}
}

// A policy names every property, under the name the vectors use, and an ignored file
// names only its action.
func TestSourcesNames(t *testing.T) {
	c, err := Parse([]byte("pool_id = \"p\"\n[view]\nmount = \"/srv/pool\"\n[[rule]]\nlabel = \"a\"\npath = \"a\"\naction = \"ignore\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	manage := c.Policy("b", nil)
	got := manage.Sources(c.Pool)
	want := policyJSON(manage)
	if len(got) != len(want) {
		t.Errorf("%d sources for %d values", len(got), len(want))
	}
	if ignored := c.Policy("a", nil).Sources(c.Pool); !reflect.DeepEqual(ignored, map[string]rules.Source{"action": rules.FromRule}) {
		t.Errorf("an ignored file: %v", ignored)
	}
}

// The cost of classifying a file, for the 200,000 files of specs.md §26.
func BenchmarkPolicy(b *testing.B) {
	c, err := Parse(vectors.RepoFile(b, "deploy/atoll.toml"))
	if err != nil {
		b.Fatal(err)
	}
	size := int64(1 << 30)
	for b.Loop() {
		c.Policy("Téléchargements/Some.Show.S01E02.1080p.WEB-DL.mkv", &size)
	}
}
