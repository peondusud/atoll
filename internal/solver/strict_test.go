package solver

import (
	"fmt"
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

type strictVector struct {
	Domains  *[][]int64 `json:"domains"`
	K        *int       `json:"k"`
	Capacity *int64     `json:"capacity"`
	Error    bool       `json:"error"`
	Note     string     `json:"note"`
}

func TestStrictCapacityVectors(t *testing.T) {
	for i, v := range vectors.Load[strictVector](t, "capacity_strict.json") {
		if v.Domains == nil || v.K == nil {
			t.Errorf("vector %d (%s): want domains and k", i, v.Note)
			continue
		}
		if v.Error == (v.Capacity != nil) {
			t.Errorf("vector %d (%s): want exactly one of capacity and error", i, v.Note)
			continue
		}
		got, err := StrictCapacity(*v.Domains, *v.K)
		switch {
		case v.Error:
			if err == nil {
				t.Errorf("vector %d (%s): got %d, want an error", i, v.Note, got)
			}
		case err != nil || got != *v.Capacity:
			t.Errorf("vector %d (%s): got %d, %v, want %d", i, v.Note, got, err, *v.Capacity)
		}
	}
}

// bestPacking is the meaning of the strict capacity, written without any formula:
// the most files of one byte that really fit, each with k copies on k different
// disks, placed by trying every choice. With fewer copies than domains the copies
// are in different domains; otherwise every domain holds one.
func bestPacking(domains [][]int64, k int) int64 {
	var sizes []int64
	var domainOf []int
	for d, disks := range domains {
		for _, s := range disks {
			sizes = append(sizes, s)
			domainOf = append(domainOf, d)
		}
	}
	var choices [][]int
	var choose func(from int, picked []int)
	choose = func(from int, picked []int) {
		if len(picked) == k {
			seen := map[int]bool{}
			for _, disk := range picked {
				seen[domainOf[disk]] = true
			}
			if len(seen) == min(k, len(domains)) {
				choices = append(choices, slices.Clone(picked))
			}
			return
		}
		for disk := from; disk < len(sizes); disk++ {
			choose(disk+1, append(picked, disk))
		}
	}
	choose(0, nil)

	memo := map[string]int64{}
	var fill func(room []int64) int64
	fill = func(room []int64) int64 {
		key := fmt.Sprint(room)
		if best, ok := memo[key]; ok {
			return best
		}
		var best int64
		for _, c := range choices {
			fits := true
			for _, disk := range c {
				fits = fits && room[disk] > 0
			}
			if !fits {
				continue
			}
			next := slices.Clone(room)
			for _, disk := range c {
				next[disk]--
			}
			best = max(best, 1+fill(next))
		}
		memo[key] = best
		return best
	}
	return fill(sizes)
}

// The formula holds against files really placed, on small pools in every shape of
// domains, for fewer copies than domains, as many, and more.
func TestStrictCapacityIsBestPacking(t *testing.T) {
	state := uint64(0x9E3779B97F4A7C15)
	next := func(n int) int {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		return int(state % uint64(n))
	}
	for range 4000 {
		domains := make([][]int64, 1+next(3))
		for d := range domains {
			domains[d] = make([]int64, 1+next(2))
			for i := range domains[d] {
				domains[d][i] = int64(next(4))
			}
		}
		k := 1 + next(4)
		got, err := StrictCapacity(domains, k)
		if err != nil {
			t.Fatalf("StrictCapacity(%d, %d): %v", domains, k, err)
		}
		if want := bestPacking(domains, k); got != want {
			t.Fatalf("StrictCapacity(%d, %d) = %d, the best packing holds %d", domains, k, got, want)
		}
	}
}
