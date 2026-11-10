package main

import (
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/jobs"
)

// A line of the daemon's text output names a copy's source, and an operation that moves its new name.
func TestTheTextOfAnOperation(t *testing.T) {
	var out strings.Builder
	d := &textDaemon{w: &out}
	for _, op := range []jobs.Operation{
		{Disk: "t2", Kind: jobs.KindReplicate, Path: "a/b", Source: "t1", Outcome: jobs.OutcomeReplicated},
		{Disk: "t1", Kind: jobs.KindRename, Path: "a", To: "b", Outcome: jobs.OutcomeRenamed},
	} {
		if err := d.Operation(&op); err != nil {
			t.Fatal(err)
		}
	}
	if want := "t2: replicate a/b (from t1): replicated\nt1: rename a -> b: renamed\n"; out.String() != want {
		t.Errorf("%q, want %q", out.String(), want)
	}
}
