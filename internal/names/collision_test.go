package names

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

type collisionVector struct {
	Existing    *[]string `json:"existing"`
	ExistingB64 []string  `json:"existing_b64"`
	Path        *string   `json:"path"`
	PathB64     *string   `json:"path_b64"`
	Component   *int      `json:"component"`
	With        *[]string `json:"with"`
	Error       bool      `json:"error"`
	Note        string    `json:"note"`
}

func TestNTFSCollisionVectors(t *testing.T) {
	for i, v := range vectors.Load[collisionVector](t, "ntfs_collision.json") {
		if v.Existing == nil {
			t.Errorf("vector %d: want existing", i)
			continue
		}
		if v.Error == (v.With != nil) {
			t.Errorf("vector %d: want exactly one of with and error", i)
			continue
		}
		if (v.With != nil && len(*v.With) > 0) != (v.Component != nil) {
			t.Errorf("vector %d: component goes with a non-empty with", i)
			continue
		}
		path, err := vectors.Path(v.Path, v.PathB64)
		if err != nil {
			t.Errorf("vector %d: %v", i, err)
			continue
		}
		target := NewTargetNames()
		var addErr error
		for _, name := range *v.Existing {
			addErr = errors.Join(addErr, target.Add(name))
		}
		for j := range v.ExistingB64 {
			name, err := vectors.Path(nil, &v.ExistingB64[j])
			if err != nil {
				t.Errorf("vector %d: %v", i, err)
				continue
			}
			addErr = errors.Join(addErr, target.Add(name))
		}
		got, err := target.Find(path)
		if v.Error {
			if addErr == nil && err == nil {
				t.Errorf("vector %d: Find(%+q) = %v, want an error", i, path, got)
			}
			continue
		}
		if addErr != nil || err != nil {
			t.Errorf("vector %d: Find(%+q): %v, %v", i, path, addErr, err)
			continue
		}
		var want *Collision
		if len(*v.With) > 0 {
			want = &Collision{Component: *v.Component, With: *v.With}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("vector %d: Find(%+q) = %+v, want %+v", i, path, got, want)
		}
	}
}

// pairwise is the rule of specs.md §15.7, written without the index: every pair
// of prefixes at one depth is compared.
func pairwise(existing []string, path string) *Collision {
	parts := strings.Split(path, "/")
	for depth := range parts {
		prefix := strings.Join(parts[:depth+1], "/")
		key, ok := CollisionKey(prefix)
		if !ok {
			return nil
		}
		seen := map[string]bool{}
		for _, e := range existing {
			eParts := strings.Split(e, "/")
			if depth >= len(eParts) {
				continue
			}
			other := strings.Join(eParts[:depth+1], "/")
			if otherKey, ok := CollisionKey(other); ok && otherKey == key && other != prefix {
				seen[other] = true
			}
		}
		if len(seen) > 0 {
			with := make([]string, 0, len(seen))
			for name := range seen {
				with = append(with, name)
			}
			slices.Sort(with)
			return &Collision{Component: depth, With: with}
		}
	}
	return nil
}

// A plan built by adding only the names Find accepts never holds two names that
// collide, whatever the order. The names mix case, NFC and NFD forms, and bytes
// that are not valid UTF-8.
func TestTargetNamesAgainstPairwise(t *testing.T) {
	components := []string{
		"a", "A", "b", "B", "caf\u00e9", "cafe\u0301", "CAF\u00c9",
		"\u03c2", "\u03c3", "\u00df", "\u1e9e", "x\xff", "X\xff",
	}
	state := uint64(0x9E3779B97F4A7C15)
	next := func(n int) int {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		return int(state % uint64(n))
	}
	randomPath := func() string {
		parts := make([]string, 1+next(3))
		for i := range parts {
			parts[i] = components[next(len(components))]
		}
		return strings.Join(parts, "/")
	}
	for range 3000 {
		target := NewTargetNames()
		var names, plan []string
		for range next(12) {
			p := randomPath()
			got, err := target.Find(p)
			if err != nil {
				t.Fatal(err)
			}
			if want := pairwise(names, p); !reflect.DeepEqual(got, want) {
				t.Fatalf("existing %+q, Find(%+q) = %+v, pairwise = %+v", names, p, got, want)
			}
			if err := target.Add(p); err != nil {
				t.Fatal(err)
			}
			names = append(names, p)
		}
		planned := NewTargetNames()
		for range 12 {
			p := randomPath()
			got, err := planned.Find(p)
			if err != nil {
				t.Fatal(err)
			}
			if got != nil {
				continue
			}
			if err := planned.Add(p); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, p)
		}
		for _, p := range plan {
			if c := pairwise(plan, p); c != nil {
				t.Fatalf("plan %+q: %+q collides with %+v", plan, p, c)
			}
		}
	}
}
