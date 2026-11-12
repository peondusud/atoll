package jobs

import "testing"

// findSwap takes, among copies that wait for a target another holds, the first by place whose targets lead round a
// cycle back to it, and the one at its target (specs.md §15.12).
func TestFindSwap(t *testing.T) {
	for _, tc := range []struct {
		name            string
		places, targets []string
		i, j            int
	}{
		{"nothing", nil, nil, -1, -1},
		{"one alone", []string{"a"}, []string{"b"}, -1, -1},
		{"two names swapped", []string{"a", "b"}, []string{"b", "a"}, 0, 1},
		{"three names round", []string{"a", "b", "c"}, []string{"b", "c", "a"}, 0, 1},
		{"a chain that ends", []string{"a", "b"}, []string{"b", "c"}, -1, -1},
		{"one that waits on a cycle it is not in", []string{"a", "b", "c"}, []string{"b", "c", "b"}, 1, 2},
		{"two cycles, the first by place", []string{"a", "b", "x", "y"}, []string{"b", "a", "y", "x"}, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, j := findSwap(len(tc.places), func(k int) string { return tc.places[k] }, func(k int) string { return tc.targets[k] })
			if i != tc.i || j != tc.j {
				t.Errorf("%d, %d; want %d, %d", i, j, tc.i, tc.j)
			}
		})
	}
}
