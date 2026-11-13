package daemon

import (
	"slices"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/watch"
)

// TestRipeGroupsWaitForAPass: a group ripe in a turn of the loop that does something else first, as a check, is out
// of the grouping by then; what it asks for must still bring a pass at the next turn, its closed file included, and
// the pass takes it (specs.md §14.1, §15.13). The loop used to keep it in that turn alone, and a file closed while a
// check was due waited for its settle.
func TestRipeGroupsWaitForAPass(t *testing.T) {
	g, err := watch.NewGrouping(10)
	if err != nil {
		t.Fatal(err)
	}
	l := &loop{grouping: g, closed: jobs.Closed{}, changed: map[string]bool{}}
	for _, kind := range []string{watch.KindCreated, watch.KindWritten} {
		if err := g.Add(watch.Event{Disk: "t1", Path: "direct/a.mkv", Kind: kind, Origin: watch.OriginUser, AtNS: 1}); err != nil {
			t.Fatal(err)
		}
	}
	l.takeRipe(100)
	// The next turn, once the check is done: nothing is ripe any more, and what was must still be there.
	if _, waiting := l.takeRipe(200); waiting || g.Len() != 0 {
		t.Fatalf("groups left: %d", g.Len())
	}
	if !slices.Equal(l.origins, []string{watch.OriginUser}) || !l.closed["direct/a.mkv"] {
		t.Errorf("origins %v, closed %v", l.origins, l.closed)
	}
	out, err := Wake(&WakeInput{NowNS: 200, CheckDueNS: 1000, RipeOrigins: l.origins, RescanEveryNS: 1, Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(out.Because, BecauseEvents) {
		t.Errorf("no pass because of the events: %+v", out)
	}
}
