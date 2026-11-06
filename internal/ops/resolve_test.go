package ops

import (
	"errors"
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

type recoveryVector struct {
	Kind         *string `json:"kind"`
	Source       *Seen   `json:"source"`
	Target       *Seen   `json:"target"`
	Changed      bool    `json:"changed"`
	Action       *string `json:"action"`
	RemoveSource *bool   `json:"remove_source"`
	RemoveTarget *bool   `json:"remove_target"`
	MoveBack     *bool   `json:"move_back"`
	Warn         *bool   `json:"warn"`
	Error        bool    `json:"error"`
	Note         string  `json:"note"`
}

func resolveVector(v *recoveryVector) (Decision, error) {
	moves := v.Source != nil && v.Target != nil
	switch *v.Kind {
	case "trash":
		if moves {
			return ResolveTrash(*v.Target, *v.Source, v.Changed)
		}
	case "restore":
		if moves {
			return ResolveRestore(*v.Target, *v.Source)
		}
	case "unlink":
		if v.Source != nil && v.Target == nil {
			return ResolveUnlink(*v.Source)
		}
	case "mkdir":
		if v.Source == nil && v.Target != nil {
			return ResolveMkdir(*v.Target)
		}
	}
	return Decision{}, errors.New("this kind does not go with these names")
}

func TestRecoveryVectors(t *testing.T) {
	for i, v := range vectors.Load[recoveryVector](t, "op_recovery.json") {
		if v.Kind == nil {
			t.Fatalf("vector %d: want kind", i)
		}
		got, err := resolveVector(&v)
		if v.Error {
			if err == nil || v.Action != nil {
				t.Errorf("vector %d (%s): %+v, want an error", i, v.Note, got)
			}
			continue
		}
		if v.Action == nil || v.RemoveSource == nil || v.RemoveTarget == nil || v.MoveBack == nil || v.Warn == nil {
			t.Fatalf("vector %d: want action, remove_source, remove_target, move_back and warn", i)
		}
		if err != nil {
			t.Errorf("vector %d (%s): %v", i, v.Note, err)
			continue
		}
		if got.Finish != (*v.Action == "finish") || got.RemoveSource != *v.RemoveSource || got.RemoveTarget != *v.RemoveTarget ||
			got.MoveBack != *v.MoveBack || got.Warn != *v.Warn {
			t.Errorf("vector %d (%s): %+v", i, v.Note, got)
		}
	}
}
