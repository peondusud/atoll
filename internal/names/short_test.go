package names

import (
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

func TestShortFormVectors(t *testing.T) {
	type vector struct {
		Name  string `json:"name"`
		Short bool   `json:"short"`
		Note  string `json:"note"`
	}
	for _, v := range vectors.Load[vector](t, "short_form.json") {
		if got := ShortForm(v.Name); got != v.Short {
			t.Errorf("ShortForm(%q) = %v, want %v (%s)", v.Name, got, v.Short, v.Note)
		}
	}
}
