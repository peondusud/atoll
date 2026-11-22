package daemon

import (
	"testing"

	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
)

// A disk a walk's pass found readonly by what a confirmation read of its marker stays so in the alerts of the passes
// that spare it, or look at some of its names alone, while its mount check finds it online, as only a confirmation
// reads the marker: its alert disk-readonly holds rather than end and begin again. A mount check that finds it
// otherwise says so, and a walk that finds it online ends it (specs.md §10, §23).
func TestAMarkerHoldsThroughThePassesThatSpareTheDisk(t *testing.T) {
	l := &loop{}
	noMarker := disks.Verdict{State: disks.Readonly, Reason: disks.NoMarker, Message: "its marker is gone"}
	online := disks.Verdict{State: disks.Online}
	missing := disks.Verdict{State: disks.Missing, Reason: disks.DeviceGone, Message: "gone"}
	verdictOf := func(lines ...jobs.DiskLine) disks.Verdict {
		t.Helper()
		out := l.withLastLines(&jobs.Outcome{Disks: lines})
		return out.Disks[0].Verdict
	}
	if got := verdictOf(jobs.DiskLine{ID: "t1", Verdict: noMarker, Walked: true}); got != noMarker {
		t.Errorf("the walk: %+v", got)
	}
	if got := verdictOf(jobs.DiskLine{ID: "t1", Verdict: online, Spared: true}); got != noMarker {
		t.Errorf("spared: %+v", got)
	}
	if got := verdictOf(jobs.DiskLine{ID: "t1", Verdict: online, Walked: true, Partial: true}); got != noMarker {
		t.Errorf("some names looked at: %+v", got)
	}
	if got := verdictOf(jobs.DiskLine{ID: "t1", Verdict: missing, Spared: true}); got != missing {
		t.Errorf("its check finds it missing: %+v", got)
	}
	if got := verdictOf(jobs.DiskLine{ID: "t1", Verdict: online, Walked: true}); got != online {
		t.Errorf("a walk that finds it online: %+v", got)
	}
	if got := verdictOf(jobs.DiskLine{ID: "t1", Verdict: online, Spared: true}); got != online {
		t.Errorf("spared after that walk: %+v", got)
	}
}
