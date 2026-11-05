package solver

import (
	"errors"
	"fmt"
	"math"
	"slices"
)

// Capacity returns U(k), the bytes a pool can hold with k copies of every file,
// each on its own disk (specs.md §13.1). sizes are in bytes, in any order, and
// are left unchanged.
func Capacity(sizes []int64, k int) (int64, error) {
	if k < 1 {
		return 0, fmt.Errorf("capacity: %d copies per file, want at least 1", k)
	}
	var total int64
	for _, s := range sizes {
		if s < 0 {
			return 0, fmt.Errorf("capacity: negative disk size %d", s)
		}
		// Checked before each addition: a wrapped sum can look valid again.
		if s > math.MaxInt64-total {
			return 0, errors.New("capacity: the total size does not fit in a signed 64-bit integer")
		}
		total += s
	}
	if k > len(sizes) {
		return 0, nil
	}
	largest := slices.Clone(sizes)
	slices.Sort(largest)
	slices.Reverse(largest)
	// Integer division rounds these non-negative terms down, and the smallest
	// rounded term is the rounded smallest term. Floats would lose exactness.
	best := total / int64(k)
	rest := total
	for j := 1; j < k; j++ {
		rest -= largest[j-1]
		if u := rest / int64(k-j); u < best {
			best = u
		}
	}
	return best, nil
}
