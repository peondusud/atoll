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
	// AfterPass says that the last step the daemon took was a pass, a check aside.
	AfterPass bool
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
	// ChangedPending says that the last pass left files whose changed copies must be read to be decided, which they
	// are at any time (specs.md §15.4).
	ChangedPending bool
	// PurgePending says that the purge of the trash stopped at its slice with entries left (specs.md §17).
	PurgePending bool
	// AlertRetryDueNS is when the alerts that could not be sent are next tried again, alone (specs.md §23): nil when
	// none waits.
	AlertRetryDueNS *int64
	// ScrubPending says that copies may be due for the scrub, which reads them inside ScrubWindow, integrity.window,
	// ScrubBudgetNS a stretch of it (specs.md §16). ScrubSpentNS is what the stretch that began at
	// ScrubSpentOpenedNS spent: a stretch begun at another time has a budget of its own.
	ScrubPending       bool
	ScrubWindow        value.Window
	ScrubBudgetNS      int64
	ScrubSpentNS       int64
	ScrubSpentOpenedNS int64
	// BackupPending says that the stretch of Window that holds now, or the next one, has no catalog backup yet
	// (specs.md §9).
	BackupPending bool
	// SmartDueNS is when the disks' SMART is next to be read (specs.md §23): nil when it is never.
	SmartDueNS *int64
	// NotifyDueNS is when a media server may next be told of a folder (specs.md §21.5): nil when none is waiting.
	NotifyDueNS *int64
	// BalanceDueNS is when the daemon next balances (specs.md §13.3): nil when it never does.
	BalanceDueNS *int64
}

// WakeOutput says what to do now, and when to look again. Read says to read the changed copies the last pass left, once
// any check and pass due are done; Purge to go on with the purge of the trash, after that; Alerts to send again, with
// no pass, the alerts that could not be sent, after that; Notify to tell the media servers of the folders that
// changed, after that; Smart to
// read the disks' SMART, after that; Balance to balance, after that; Hash to read new files' hashes, after that; Backup
// to back the catalog up, when there is no hash to read; and Scrub to scrub, when there is neither. ScrubOpenedNS is
// when the stretch of the scrub's window that holds now began, 0 outside it or with no scrub pending.
type WakeOutput struct {
	Check         bool
	Because       []string
	Read          bool
	Purge         bool
	Alerts        bool
	Notify        bool
	Smart         bool
	Balance       bool
	Hash          bool
	Backup        bool
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
	out.Read, out.Purge = in.ChangedPending, in.PurgePending
	if in.AlertRetryDueNS != nil {
		if *in.AlertRetryDueNS <= now {
			out.Alerts = true
		} else {
			earliest(*in.AlertRetryDueNS)
		}
	}
	if in.NotifyDueNS != nil {
		if *in.NotifyDueNS <= now {
			out.Notify = true
		} else {
			earliest(*in.NotifyDueNS)
		}
	}
	if in.SmartDueNS != nil {
		if *in.SmartDueNS <= now {
			out.Smart = true
		} else {
			earliest(*in.SmartDueNS)
		}
	}
	if in.BalanceDueNS != nil {
		if *in.BalanceDueNS <= now {
			out.Balance = true
		} else {
			earliest(*in.BalanceDueNS)
		}
	}
	if in.HashPending {
		nowLocal := time.Unix(0, now).In(in.Location)
		out.Hash = in.Window.Contains(nowLocal)
		earliest(in.Window.Next(nowLocal).UnixNano())
	}
	if in.BackupPending {
		nowLocal := time.Unix(0, now).In(in.Location)
		if in.Window.Contains(nowLocal) {
			out.Backup = !out.Hash
		} else {
			earliest(in.Window.Next(nowLocal).UnixNano())
		}
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
			out.Scrub = !out.Hash && !out.Backup
		default:
			// The budget of this stretch is spent: the next stretch begins a day after this one did.
			out.ScrubOpenedNS = opened.UnixNano()
			earliest(in.ScrubWindow.Next(opened.AddDate(0, 0, 1)).UnixNano())
		}
	}
	other := out.Read || out.Purge || out.Alerts || out.Notify || out.Smart || out.Balance || out.Hash || out.Backup || out.Scrub
	// Right after a pass, another step due runs before a pass for events or next_due alone, which keeps them for the
	// turn after it: passes that each take longer than the changes take to come, as on a large pool, would otherwise
	// follow one another for as long as the changes do, and nothing else would ever run. A pass for any other reason
	// does not wait.
	if in.AfterPass && other && !slices.ContainsFunc(out.Because, func(why string) bool { return why != BecauseEvents && why != BecauseDue }) {
		out.Because = nil
	}
	if len(out.Because) > 0 || other {
		wake = now
	}
	out.WakeNS = max(wake, now)
	return out, nil
}
