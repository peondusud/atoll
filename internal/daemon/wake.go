// Package daemon runs Atoll for good: passes when something calls for one, the mount check every 30 s, the
// watcher, the runtime branches of mergerfs, and the remount after a USB reset (specs.md §15.13).
package daemon

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/peondusud/atoll/internal/value"
	"github.com/peondusud/atoll/internal/watch"
)

// Why a pass runs (specs.md §15.13), in the order a pass gives them.
const (
	BecauseStart  = "start"
	BecauseReload = "reload"
	BecauseGone   = "gone"
	BecauseBack   = "back"
	BecauseEvents = "events"
	BecauseDue    = "due"
	BecauseRescan = "rescan"
)

var origins = []string{watch.OriginView, watch.OriginAtoll, watch.OriginUser, watch.OriginUnknown}

// WakeInput is what the daemon knows at a time, for Wake.
type WakeInput struct {
	NowNS      int64
	CheckDueNS int64
	// First says that no pass has run since it started, Reloaded that the configuration was read again since the
	// last pass, and Gone and Back that a disk went missing, or came back.
	First, Reloaded, Gone, Back bool
	// PassDueNS is the last pass's next_due, GroupDueNS when a waiting group ripens: nil when none.
	PassDueNS, GroupDueNS *int64
	// RipeOrigins holds who made the groups ripe now.
	RipeOrigins []string
	// LastPassEndNS is when the last pass ended: nil before the first.
	LastPassEndNS *int64
	RescanEveryNS int64
	Window        value.Window
	Location      *time.Location
}

// WakeOutput says what to do now, and when to look again.
type WakeOutput struct {
	Check   bool
	Because []string
	WakeNS  int64
}

// Wake says whether the mount check and a pass are due at a time, and when to wake when nothing is (specs.md
// §15.13). It does no I/O.
func Wake(in *WakeInput) (WakeOutput, error) {
	if in.RescanEveryNS <= 0 {
		return WakeOutput{}, fmt.Errorf("wake: rescan_every must be positive, not %d", in.RescanEveryNS)
	}
	if in.Location == nil {
		return WakeOutput{}, errors.New("wake: no time zone")
	}
	events := false
	for _, o := range in.RipeOrigins {
		if !slices.Contains(origins, o) {
			return WakeOutput{}, fmt.Errorf("wake: unknown origin %q", o)
		}
		events = events || o != watch.OriginAtoll
	}
	now := in.NowNS
	out := WakeOutput{Check: in.CheckDueNS <= now}
	wake := in.CheckDueNS
	earliest := func(at int64) {
		wake = min(wake, at)
	}
	for _, r := range []struct {
		holds bool
		why   string
	}{{in.First, BecauseStart}, {in.Reloaded, BecauseReload}, {in.Gone, BecauseGone}, {in.Back, BecauseBack}, {events, BecauseEvents}} {
		if r.holds {
			out.Because = append(out.Because, r.why)
		}
	}
	if in.PassDueNS != nil {
		if *in.PassDueNS <= now {
			out.Because = append(out.Because, BecauseDue)
		}
		earliest(*in.PassDueNS)
	}
	if in.GroupDueNS != nil {
		earliest(*in.GroupDueNS)
	}
	if in.LastPassEndNS != nil {
		at := *in.LastPassEndNS + in.RescanEveryNS
		if at <= now && in.Window.Contains(time.Unix(0, now).In(in.Location)) {
			out.Because = append(out.Because, BecauseRescan)
		}
		earliest(in.Window.Next(time.Unix(0, max(at, now)).In(in.Location)).UnixNano())
	}
	if len(out.Because) > 0 {
		wake = now
	}
	out.WakeNS = max(wake, now)
	return out, nil
}
