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
	// HashPending says that files may be waiting for their hash, which is read inside the window.
	HashPending bool
	// ScrubPending says that copies may be due for the scrub, which reads them inside ScrubWindow, integrity.window,
	// ScrubBudgetNS a stretch of it (specs.md §16). ScrubSpentNS is what the stretch that began at
	// ScrubSpentOpenedNS spent: a stretch begun at another time has a budget of its own.
	ScrubPending       bool
	ScrubWindow        value.Window
	ScrubBudgetNS      int64
	ScrubSpentNS       int64
	ScrubSpentOpenedNS int64
}

// WakeOutput says what to do now, and when to look again. Hash says to read new files' hashes, once any check
// and pass due are done, and Scrub to scrub, when there is no hash to read. ScrubOpenedNS is when the stretch of
// the scrub's window that holds now began, 0 outside it or with no scrub pending.
type WakeOutput struct {
	Check         bool
	Because       []string
	Hash          bool
	Scrub         bool
	ScrubOpenedNS int64
	WakeNS        int64
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
	if in.HashPending {
		nowLocal := time.Unix(0, now).In(in.Location)
		out.Hash = in.Window.Contains(nowLocal)
		earliest(in.Window.Next(nowLocal).UnixNano())
	}
	if in.ScrubPending {
		if in.ScrubBudgetNS <= 0 {
			return WakeOutput{}, fmt.Errorf("wake: the scrub's budget must be positive, not %d", in.ScrubBudgetNS)
		}
		nowLocal := time.Unix(0, now).In(in.Location)
		opened := in.ScrubWindow.Opened(nowLocal)
		switch {
		case opened.IsZero():
			earliest(in.ScrubWindow.Next(nowLocal).UnixNano())
		case in.ScrubSpentOpenedNS != opened.UnixNano() || in.ScrubSpentNS < in.ScrubBudgetNS:
			out.ScrubOpenedNS = opened.UnixNano()
			out.Scrub = !out.Hash
		default:
			// The budget of this stretch is spent: the next stretch begins a day after this one did.
			out.ScrubOpenedNS = opened.UnixNano()
			earliest(in.ScrubWindow.Next(opened.AddDate(0, 0, 1)).UnixNano())
		}
	}
	if len(out.Because) > 0 || out.Hash || out.Scrub {
		wake = now
	}
	out.WakeNS = max(wake, now)
	return out, nil
}
