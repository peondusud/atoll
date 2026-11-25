package jobs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/peondusud/atoll/internal/solver"
)

// The mass-modification guard of a file is that of the disk of its changed copy first by position: an edited copy or
// one replaced by a regular file, never one there, nor one replaced by what is no regular file (specs.md §15.6).
func TestTheGuardOfAModificationIsItsFirstChangedCopy(t *testing.T) {
	modCopy := func(disk string, position int, v solver.CopyVerdict, other bool) solver.ModCopy {
		return solver.ModCopy{Disk: disk, Position: position, Verdict: v, Other: other}
	}
	for _, tc := range []struct {
		name   string
		copies []solver.ModCopy
		want   string
	}{
		{"the edited copy", []solver.ModCopy{modCopy("t1", 0, solver.VerdictThere, false), modCopy("t2", 1, solver.VerdictEdited, false)}, "t2"},
		{"the first of two by position", []solver.ModCopy{modCopy("t3", 3, solver.VerdictEdited, false), modCopy("t1", 1, solver.VerdictThere, false),
			modCopy("t2", 2, solver.VerdictReplaced, false)}, "t2"},
		{"a copy there before it", []solver.ModCopy{modCopy("t3", 3, solver.VerdictEdited, false), modCopy("t1", 1, solver.VerdictThere, false)}, "t3"},
		{"replaced by what is no regular file", []solver.ModCopy{modCopy("t1", 0, solver.VerdictReplaced, true), modCopy("t2", 2, solver.VerdictEdited, false)}, "t2"},
		{"none changed", []solver.ModCopy{modCopy("t1", 0, solver.VerdictThere, false)}, ""},
	} {
		got := ""
		if c := firstChanged(&solver.ModInput{Copies: tc.copies}); c != nil {
			got = c.Disk
		}
		if got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A copy whose mode changes during its read gives nothing to decide on either: its change time moved, its size and its
// modification time as they were, and the next pass reads it again (specs.md §15.4).
func TestACopyWhoseModeChangesDuringItsRead(t *testing.T) {
	b := edited(t)
	armed := true
	full := filepath.Join(b.dirs["t1"], "media", "a")
	b.fail[b.dirs["t1"]] = func(call, name string) error {
		if armed && call == "read" && name == "a" {
			armed = false
			before := b.ctimeOf("t1", "a")
			for mode := os.FileMode(0o600); b.ctimeOf("t1", "a") == before; mode ^= 0o040 {
				pastTick()
				if err := os.Chmod(full, mode); err != nil {
					t.Error(err)
					return nil
				}
			}
		}
		return nil
	}
	out := b.pass()
	if armed || len(out.Decisions) != 0 || len(out.Operations) != 0 || out.Partial {
		t.Fatalf("%+v", out)
	}
}
