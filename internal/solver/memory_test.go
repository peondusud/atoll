package solver

import (
	"testing"
	"unsafe"
)

// A plan of the whole pool holds, for each file, its File, its copies and a planFile, all at once: the first pass of a
// pool of 400,000 copies plans them within the 200 MB of specs.md §26. A word one of them gains costs 1.5 MiB on
// 200,000 files, twice that for a copy: the fields of the SSD cache, held for every file, put that pass past the 200 MB
// (DONE.md, 2026-11-18). What few files need is held apart, for them alone (planFile.rare, planFile.marks).
func TestAPlanHoldsLittleForEachFile(t *testing.T) {
	for _, c := range []struct {
		name       string
		size, most uintptr
	}{
		{"planFile", unsafe.Sizeof(planFile{}), 160},
		{"File", unsafe.Sizeof(File{}), 80},
		{"Copy", unsafe.Sizeof(Copy{}), 80},
	} {
		if c.size > c.most {
			t.Errorf("%s: %d bytes, for %d at most", c.name, c.size, c.most)
		}
	}
}
