package solver

import (
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

type capacityVector struct {
	Sizes    *[]int64 `json:"sizes"`
	K        *int     `json:"k"`
	Capacity *int64   `json:"capacity"`
	Error    bool     `json:"error"`
	Note     string   `json:"note"`
}

func TestCapacityVectors(t *testing.T) {
	for i, v := range vectors.Load[capacityVector](t, "capacity.json") {
		if v.Sizes == nil || v.K == nil {
			t.Errorf("vector %d: want sizes and k", i)
			continue
		}
		if v.Error == (v.Capacity != nil) {
			t.Errorf("vector %d: want exactly one of capacity and error", i)
			continue
		}
		got, err := Capacity(*v.Sizes, *v.K)
		switch {
		case v.Error && err == nil:
			t.Errorf("Capacity(%d, %d) = %d, want an error", *v.Sizes, *v.K, got)
		case !v.Error && err != nil:
			t.Errorf("Capacity(%d, %d): %v", *v.Sizes, *v.K, err)
		case !v.Error && got != *v.Capacity:
			t.Errorf("Capacity(%d, %d) = %d, want %d", *v.Sizes, *v.K, got, *v.Capacity)
		}
	}
}

// largestFit is the meaning of U(k), written without the formula: the largest x
// such that k copies of x bytes fit. Each disk holds at most one copy of any
// byte, so at most x of these bytes, and together they must take k·x bytes.
func largestFit(sizes []int64, k int) int64 {
	var total, best int64
	for _, s := range sizes {
		total += s
	}
	for x := int64(0); x <= total; x++ {
		var room int64
		for _, s := range sizes {
			room += min(s, x)
		}
		if room >= int64(k)*x {
			best = x
		}
	}
	return best
}

// The formula, the rounding and the rule for more copies than disks all follow
// from the meaning of U(k), on small pools in random order.
func TestCapacityIsLargestFit(t *testing.T) {
	state := uint64(0x9E3779B97F4A7C15)
	next := func(n uint64) uint64 {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		return state % n
	}
	for range 20000 {
		sizes := make([]int64, next(8))
		for i := range sizes {
			sizes[i] = int64(next(64))
		}
		k := 1 + int(next(6))
		before := slices.Clone(sizes)
		got, err := Capacity(sizes, k)
		if err != nil {
			t.Fatalf("Capacity(%d, %d): %v", sizes, k, err)
		}
		if !slices.Equal(sizes, before) {
			t.Fatalf("Capacity changed its input: %d became %d", before, sizes)
		}
		if want := largestFit(sizes, k); got != want {
			t.Fatalf("Capacity(%d, %d) = %d, largest fit = %d", sizes, k, got, want)
		}
	}
}
