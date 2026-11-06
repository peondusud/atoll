package solver

import (
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

type stateVector struct {
	Applies *[]State `json:"applies"`
	State   *State   `json:"state"`
	Error   bool     `json:"error"`
	Note    string   `json:"note"`
}

func TestShownStateVectors(t *testing.T) {
	for i, v := range vectors.Load[stateVector](t, "file_state.json") {
		if v.Applies == nil {
			t.Errorf("vector %d: want applies", i)
			continue
		}
		if v.Error == (v.State != nil) {
			t.Errorf("vector %d: want exactly one of state and error", i)
			continue
		}
		got, err := ShownState(*v.Applies)
		if v.Error {
			if err == nil {
				t.Errorf("vector %d: ShownState(%q) = %q, want an error", i, *v.Applies, got)
			}
			continue
		}
		if err != nil || got != *v.State {
			t.Errorf("vector %d: ShownState(%q) = %q, %v, want %q", i, *v.Applies, got, err, *v.State)
		}
	}
}

// Every subset of the states, in both directions: the first in the order wins,
// whatever the order of the input.
func TestShownStateEverySubset(t *testing.T) {
	order := []State{
		StateCorrupt, StateConflict, StateUnavailable, StateNameIncompatible, StateNameCollision,
		StateDegraded, StatePending, StateUnprotected, StateOK,
	}
	for mask := range 1 << len(order) {
		var subset []State
		want := StateOK
		for bit := len(order) - 1; bit >= 0; bit-- {
			if mask&(1<<bit) != 0 {
				subset = append(subset, order[bit])
				want = order[bit]
			}
		}
		for range 2 {
			got, err := ShownState(subset)
			if err != nil || got != want {
				t.Fatalf("ShownState(%q) = %q, %v, want %q", subset, got, err, want)
			}
			for i, j := 0, len(subset)-1; i < j; i, j = i+1, j-1 {
				subset[i], subset[j] = subset[j], subset[i]
			}
		}
	}
}
