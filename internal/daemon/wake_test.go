package daemon

import (
	"slices"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/value"
	"github.com/peondusud/atoll/internal/vectors"
)

func TestWakeVectors(t *testing.T) {
	type vector struct {
		NowNS         int64    `json:"now_ns"`
		CheckDueNS    int64    `json:"check_due_ns"`
		First         bool     `json:"first"`
		Gone          bool     `json:"gone"`
		Back          bool     `json:"back"`
		PassDueNS     *int64   `json:"pass_due_ns"`
		GroupDueNS    *int64   `json:"group_due_ns"`
		RipeOrigins   []string `json:"ripe_origins"`
		LastPassEndNS *int64   `json:"last_pass_end_ns"`
		RescanEveryNS int64    `json:"rescan_every_ns"`
		Window        string   `json:"window"`
		Timezone      string   `json:"timezone"`
		Check         bool     `json:"check"`
		Because       []string `json:"because"`
		WakeNS        int64    `json:"wake_ns"`
		Error         bool     `json:"error"`
		Note          string   `json:"note"`
	}
	for i, v := range vectors.Load[vector](t, "daemon_wake.json") {
		loc, err := time.LoadLocation(v.Timezone)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		w, err := value.ParseWindow(v.Window)
		var out WakeOutput
		if err == nil {
			out, err = Wake(&WakeInput{
				NowNS: v.NowNS, CheckDueNS: v.CheckDueNS, First: v.First, Gone: v.Gone, Back: v.Back, PassDueNS: v.PassDueNS,
				GroupDueNS: v.GroupDueNS, RipeOrigins: v.RipeOrigins, LastPassEndNS: v.LastPassEndNS, RescanEveryNS: v.RescanEveryNS,
				Window: w, Location: loc,
			})
		}
		switch {
		case v.Error:
			if err == nil {
				t.Errorf("case %d (%s): want an error", i, v.Note)
			}
		case err != nil:
			t.Errorf("case %d (%s): %v", i, v.Note, err)
		case out.Check != v.Check || !slices.Equal(out.Because, v.Because) && (len(out.Because) > 0 || len(v.Because) > 0) || out.WakeNS != v.WakeNS:
			t.Errorf("case %d (%s): check %v, because %v, wake %s; want %v, %v, %s", i, v.Note, out.Check, out.Because,
				time.Unix(0, out.WakeNS).UTC().Format(time.RFC3339), v.Check, v.Because, time.Unix(0, v.WakeNS).UTC().Format(time.RFC3339))
		}
	}
}
