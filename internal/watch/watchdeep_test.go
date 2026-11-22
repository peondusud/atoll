package watch

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func watchedPaths(s *inotifySource) []string {
	var out []string
	for _, n := range s.tree.nodes {
		if p, ok := n.path(); ok && p != "" {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// TestAWatchGoesNoFurtherThanAWalk: a watch folder by folder marks no folder a walk of the disk does not read, past the
// limit of its mergerfs branch, neither at its start nor when one arrives; with walk.long_paths = "walk", it marks
// them all, holding no more folders open than walk.max_open says, and opens those deeper again from the root
// (specs.md §15.1, §15.8).
func TestAWatchGoesNoFurtherThanAWalk(t *testing.T) {
	levels := []string{"aaaa", "bbbb", "cccc", "dddd", "eeee", "ffff", "gggg", "hhhh", "iiii", "jjjj"}
	var all []string
	for i := range levels {
		all = append(all, strings.Join(levels[:i+1], "/"))
	}
	for _, tc := range []struct {
		name   string
		limits Limits
		want   []string
	}{
		{"skip", Limits{MaxPath: 10}, all[:2]},
		{"walk", Limits{MaxPath: NoBound, MaxOpen: 3}, all},
		{"walk, deeper than it holds", Limits{MaxPath: NoBound, MaxOpen: 4}, all},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "media")
			if err := os.MkdirAll(filepath.Join(root, all[len(all)-1]), 0o755); err != nil {
				t.Fatal(err)
			}
			s, err := openInotify(t.Context(), root, tc.limits)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.close() }()
			if got := watchedPaths(s); !slices.Equal(got, tc.want) {
				t.Errorf("watched %q, want %q", got, tc.want)
			}
			// One that arrives below the deepest watched.
			var deepest *folderNode[int32]
			for _, n := range s.tree.nodes {
				if p, _ := n.path(); p == tc.want[len(tc.want)-1] {
					deepest = n
				}
			}
			made := filepath.Join(root, tc.want[len(tc.want)-1], "new", "below")
			if err := os.MkdirAll(made, 0o755); err != nil {
				t.Fatal(err)
			}
			found := s.arrive(deepest, "new")
			got := watchedPaths(s)
			if tc.limits.MaxPath != NoBound {
				if len(found) != 0 || !slices.Equal(got, tc.want) {
					t.Errorf("past the limit: found %+v, watched %q", found, got)
				}
				return
			}
			if want := append(slices.Clone(tc.want), tc.want[len(tc.want)-1]+"/new", tc.want[len(tc.want)-1]+"/new/below"); !slices.Equal(got, want) {
				t.Errorf("watched %q, want %q", got, want)
			}
		})
	}
}
