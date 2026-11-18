package watch

import (
	"testing"
	"unsafe"
)

// What a plan of the whole pool remembers of each file is held for every file at once, beside the plan's own (solver's
// TestAPlanHoldsLittleForEachFile): a word more costs 1.5 MiB on 200,000 files, within the 200 MB of specs.md §26.
func TestASettleRemembersLittleOfEachFile(t *testing.T) {
	if size := unsafe.Sizeof(settled{}); size > 48 {
		t.Errorf("settled: %d bytes, for 48 at most", size)
	}
}
