package value

import (
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

func TestMinFreeVectors(t *testing.T) {
	type vector struct {
		MinFree  *[]string `json:"min_free"`
		Capacity *int64    `json:"capacity"`
		Reserved *int64    `json:"reserved"`
		Usable   *int64    `json:"usable"`
		Error    bool      `json:"error"`
		Note     string    `json:"note"`
	}
	for i, v := range vectors.Load[vector](t, "min_free.json") {
		if v.MinFree == nil {
			t.Errorf("vector %d (%s): want min_free", i, v.Note)
			continue
		}
		if v.Capacity == nil {
			unknownCapacity(t, i, v.Note, *v.MinFree, v.Reserved, v.Usable, v.Error)
			continue
		}
		if v.Error == (v.Reserved != nil) || (v.Reserved == nil) != (v.Usable == nil) {
			t.Errorf("vector %d (%s): want reserved and usable, or error", i, v.Note)
			continue
		}
		m, err := ParseMinFree(*v.MinFree)
		var reserved, usable int64
		if err == nil {
			reserved, err = m.Reserved(*v.Capacity)
		}
		if err == nil {
			usable, err = m.Usable(*v.Capacity)
		}
		switch {
		case v.Error:
			if err == nil {
				t.Errorf("vector %d (%s): got %d and %d, want an error", i, v.Note, reserved, usable)
			}
		case err != nil:
			t.Errorf("vector %d (%s): %v", i, v.Note, err)
		case reserved != *v.Reserved || usable != *v.Usable:
			t.Errorf("vector %d (%s): got %d and %d, want %d and %d", i, v.Note, reserved, usable, *v.Reserved, *v.Usable)
		}
	}
}

// unknownCapacity checks a vector whose capacity is null: only the smallest size holds, and nothing with
// percentages only (specs.md §21.1).
func unknownCapacity(t *testing.T, i int, note string, values []string, reserved, usable *int64, wantError bool) {
	t.Helper()
	if usable != nil || (wantError && reserved != nil) {
		t.Errorf("vector %d (%s): with no capacity, want reserved, null, or an error", i, note)
		return
	}
	m, err := ParseMinFree(values)
	switch {
	case wantError:
		if err == nil {
			t.Errorf("vector %d (%s): want an error", i, note)
		}
		return
	case err != nil:
		t.Errorf("vector %d (%s): %v", i, note, err)
		return
	}
	got, ok := m.SmallestSize()
	switch {
	case reserved == nil && ok:
		t.Errorf("vector %d (%s): got %d, want nothing", i, note, got)
	case reserved != nil && (!ok || got != *reserved):
		t.Errorf("vector %d (%s): got %d (%v), want %d", i, note, got, ok, *reserved)
	}
}
