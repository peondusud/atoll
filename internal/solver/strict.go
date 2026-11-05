package solver

import (
	"errors"
	"slices"
)

// StrictCapacity returns the capacity with spread_mode = "strict" (specs.md
// §12.4, §13.1): domains holds the sizes of each failure domain's disks, and k
// is the copies per file. With fewer copies than domains, each copy goes to a
// different domain, so Capacity applies to the domains' sizes. Otherwise every
// domain holds a copy of every file, so no file can exceed the smallest domain.
// An error means that Capacity would refuse the input, or that a domain has no
// disk.
func StrictCapacity(domains [][]int64, k int) (int64, error) {
	all := slices.Concat(domains...)
	// Checks k, the sizes and the total first, so the sums below cannot overflow.
	capacity, err := Capacity(all, k)
	if err != nil {
		return 0, err
	}
	totals := make([]int64, 0, len(domains))
	for _, disks := range domains {
		if len(disks) == 0 {
			return 0, errors.New("capacity: a domain with no disk")
		}
		var total int64
		for _, s := range disks {
			total += s
		}
		totals = append(totals, total)
	}
	if k < len(totals) {
		return Capacity(totals, k)
	}
	for _, total := range totals {
		capacity = min(capacity, total)
	}
	return capacity, nil
}
