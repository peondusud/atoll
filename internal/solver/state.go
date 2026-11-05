package solver

import "fmt"

// State is the state of a file (specs.md §4).
type State string

// The states of a file, in the order ShownState applies them.
const (
	StateUnavailable      State = "unavailable"
	StateCorrupt          State = "corrupt"
	StateConflict         State = "conflict"
	StateNameIncompatible State = "name-incompatible"
	StateNameCollision    State = "name-collision"
	StateDegraded         State = "degraded"
	StatePending          State = "pending"
	StateUnprotected      State = "unprotected"
	StateOK               State = "ok"
)

// ShownState returns the one state a file shows when the given states apply to
// it: the first in the order of specs.md §4, and StateOK when none applies. It
// serves display and alerts only: placement and job order read the copies. An
// error means that a state is unknown.
func ShownState(applying []State) (State, error) {
	shown := StateOK
	for _, s := range applying {
		rank, err := stateRank(s)
		if err != nil {
			return "", err
		}
		shownRank, err := stateRank(shown)
		if err != nil {
			return "", err
		}
		if rank < shownRank {
			shown = s
		}
	}
	return shown, nil
}

func stateRank(s State) (int, error) {
	switch s {
	case StateUnavailable:
		return 0, nil
	case StateCorrupt:
		return 1, nil
	case StateConflict:
		return 2, nil
	case StateNameIncompatible:
		return 3, nil
	case StateNameCollision:
		return 4, nil
	case StateDegraded:
		return 5, nil
	case StatePending:
		return 6, nil
	case StateUnprotected:
		return 7, nil
	case StateOK:
		return 8, nil
	default:
		return 0, fmt.Errorf("unknown file state %q", s)
	}
}
