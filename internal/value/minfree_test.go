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
		if v.MinFree == nil || v.Capacity == nil {
			t.Errorf("vector %d (%s): want min_free and capacity", i, v.Note)
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
