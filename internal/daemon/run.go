package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"slices"
	"time"

	"github.com/peondusud/atoll/internal/alerts"
	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/notify"
	"github.com/peondusud/atoll/internal/trash"
	"github.com/peondusud/atoll/internal/value"
	"github.com/peondusud/atoll/internal/view"
	"github.com/peondusud/atoll/internal/watch"
)

// checkEvery is how often the daemon checks every disk's mount: a missing disk is found within 30 s (specs.md
// §18).
const checkEvery = 30 * time.Second

// retryAfter is how long a pass that failed waits before the next, so that a catalog that cannot be used is not
// asked again and again.
const retryAfter = time.Minute

// alertRetry is when a pass comes again after an alert could not be sent, so that it is tried again soon
// (specs.md §23).
const alertRetry = 5 * time.Minute

// hashSlice is how long the daemon reads new files' hashes under the catalog's lock at a time, so that a command
// never waits long for it, and events and checks come in between.
const hashSlice = 30 * time.Second

// Options is what Run needs.
type Options struct {
	Config  *config.Config
	Machine disks.Machine
	// Apply is false in shadow mode (--dry-run): passes write nothing, and no branch or mount is touched.
	Apply bool
	Log   *slog.Logger
	// CheckEvery, Fstab and Inotify are for tests: the mount check every 30 s, /etc/fstab, fanotify.
	CheckEvery time.Duration
	Fstab      string
	Inotify    bool
	// OnPass is told of each pass, for tests.
	OnPass func(because []string, outcome *jobs.Outcome)
	// Reload says to read the configuration again, through Load: SIGHUP (specs.md §15.13).
	Reload <-chan struct{}
	Load   func() (*config.Config, error)
}

// ErrStartup says that the daemon could not start: its configuration or its catalog cannot be used.
var ErrStartup = errors.New("the daemon cannot start")

// settings is what the loop reads of the configuration.
type settings struct {
	catalog      string
	viewMount    string
	debounce     time.Duration
	rescanEvery  time.Duration
	window       value.Window
	location     *time.Location
	remount      bool
	minFree      value.MinFree
	disksByID    map[string]*config.Disk
	checkEvery   time.Duration
	fstab        string
	resetReasons []disks.Reason
	channels     []alerts.Channel
	servers      []notify.Server
	// scrubWindow is integrity.window, maintenance.window when left out, and scrubBudget integrity.scrub_budget.
	scrubWindow value.Window
	scrubBudget time.Duration
	// balanceSchedule is balance.schedule: never, immediate, or a window, which balanceWindow holds; balanceBudget is
	// balance.budget, and balanceRatio and balanceMore balance.trigger (specs.md §13.3).
	balanceSchedule string
	balanceWindow   value.Window
	balanceBudget   time.Duration
	balanceRatio    value.Percent
	balanceMore     int64
}

func readSettings(c *config.Config, opt *Options) (*settings, error) {
	e := c.Effective()
	s := &settings{disksByID: map[string]*config.Disk{}, checkEvery: opt.CheckEvery, fstab: opt.Fstab,
		resetReasons: []disks.Reason{disks.DeviceGone, disks.DeviceUnread, disks.IOError}}
	s.catalog, _ = e["catalog"].(string)
	s.viewMount, _ = e["view.mount"].(string)
	s.remount, _ = e["health.remount_after_reset"].(bool)
	if s.checkEvery <= 0 {
		s.checkEvery = checkEvery
	}
	if s.fstab == "" {
		s.fstab = "/etc/fstab"
	}
	var err error
	text := func(key string) string { v, _ := e[key].(string); return v }
	if s.debounce, err = value.ParseDuration(text("deletes.debounce")); err != nil {
		return nil, fmt.Errorf("deletes.debounce: %w", err)
	}
	if s.rescanEvery, err = value.ParseDuration(text("maintenance.rescan_every")); err != nil {
		return nil, fmt.Errorf("maintenance.rescan_every: %w", err)
	}
	if s.window, err = value.ParseWindow(text("maintenance.window")); err != nil {
		return nil, fmt.Errorf("maintenance.window: %w", err)
	}
	if s.scrubWindow, err = value.ParseWindow(text("integrity.window")); err != nil {
		return nil, fmt.Errorf("integrity.window: %w", err)
	}
	if s.scrubBudget, err = value.ParseDuration(text("integrity.scrub_budget")); err != nil {
		return nil, fmt.Errorf("integrity.scrub_budget: %w", err)
	}
	s.balanceSchedule = text("balance.schedule")
	if s.balanceSchedule != scheduleNever && s.balanceSchedule != scheduleImmediate {
		if s.balanceWindow, err = value.ParseWindow(s.balanceSchedule); err != nil {
			return nil, fmt.Errorf("balance.schedule: %w", err)
		}
	}
	if s.balanceBudget, err = value.ParseDuration(text("balance.budget")); err != nil {
		return nil, fmt.Errorf("balance.budget: %w", err)
	}
	if s.balanceRatio, err = value.ParsePercent(text("balance.trigger.ratio_below")); err != nil {
		return nil, fmt.Errorf("balance.trigger.ratio_below: %w", err)
	}
	if s.balanceMore, err = value.ParseSize(text("balance.trigger.or_more_than")); err != nil {
		return nil, fmt.Errorf("balance.trigger.or_more_than: %w", err)
	}
	if zone := text("timezone"); zone == "local" || zone == "" {
		s.location = time.Local
	} else if s.location, err = time.LoadLocation(zone); err != nil {
		return nil, fmt.Errorf("timezone: %w", err)
	}
	if s.minFree, err = c.MinFree(); err != nil {
		return nil, err
	}
	for i := range c.Disks {
		s.disksByID[c.Disks[i].ID] = &c.Disks[i]
	}
	s.channels = alerts.Channels(c)
	s.servers = notify.Servers(c)
	return s, nil
}

// diskState is what the loop keeps of a disk between checks.
type diskState struct {
	state disks.State
	// walked says that a pass walked it since it was last found again: its watch and its branch may come back.
	walked  bool
	watched bool
	// reset says that it went missing for a reason a USB reset gives, so each check tries the remount.
	reset bool
}

type loop struct {
	opt      *Options
	set      *settings
	log      *slog.Logger
	watcher  *watch.Watcher
	grouping *watch.Grouping
	disks    map[string]*diskState

	first, reloaded, gone, back bool
	// hashPending says that files may be waiting for their hash: set by each pass, cleared once none is left.
	hashPending bool
	// scrubSpent is what the scrub spent in the stretch of its window that began at scrubOpened, and scrubIdle the
	// stretch in which it found no copy left to read: copies come due again with the next one (specs.md §16).
	scrubSpent  time.Duration
	scrubOpened int64
	scrubIdle   int64
	// backupOpened is the stretch of maintenance.window whose catalog backup was made, or tried (specs.md §9).
	backupOpened int64
	checkDue     int64
	passDue      *int64
	// closed holds what ripe groups said was closed after a write, until a pass takes it (specs.md §14.1).
	closed  jobs.Closed
	lastEnd *int64
	// smartDue is when the disks' SMART is next read: nil when it is not (specs.md §23).
	smartDue *int64
	// queues holds what each media server is to be told, by its name, and changed the folders ripe groups named,
	// until a pass takes them (specs.md §21.5).
	queues  map[string]*notify.Queue
	changed map[string]bool
	// balanceStretch is the stretch of balance.schedule whose trigger was checked last, balanceNext when "immediate"
	// checks it next, balanceWork that it held and copies are left to move, and balanceSpent what moving them spent
	// since that check (specs.md §13.3).
	balanceStretch int64
	balanceNext    int64
	balanceWork    bool
	balanceSpent   time.Duration
}

// Run runs the daemon until ctx is done (specs.md §15.13). It returns ErrStartup, wrapped, when it cannot start,
// and nil once stopped.
func Run(ctx context.Context, opt Options) error {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	set, err := readSettings(opt.Config, &opt)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrStartup, err)
	}
	// The catalog must be there, and this pool's, before anything else.
	db, err := catalog.OpenPool(ctx, set.catalog, opt.Config.PoolID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrStartup, err)
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("%w: %w", ErrStartup, err)
	}
	grouping, err := watch.NewGrouping(set.debounce.Nanoseconds())
	if err != nil {
		return fmt.Errorf("%w: %w", ErrStartup, err)
	}
	l := &loop{
		opt: &opt, set: set, log: opt.Log, grouping: grouping, disks: map[string]*diskState{}, first: true, closed: jobs.Closed{},
		watcher:  watch.NewWatcher(watch.WatcherOptions{ViewMount: set.viewMount, Now: opt.Machine.Now, Inotify: opt.Inotify}),
		smartDue: smartFirst(opt.Machine),
		queues:   newQueues(set.servers, nil), changed: map[string]bool{},
	}
	defer l.watcher.Close()
	l.notify("READY=1")
	l.check(ctx)
	for id, st := range l.disks {
		if st.state != disks.Missing {
			l.watch(ctx, id)
		}
	}
	l.gone, l.back = false, false
	err = l.run(ctx)
	l.notify("STOPPING=1")
	return err
}

func (l *loop) notify(state string) {
	if err := sdNotify(state); err != nil {
		l.log.Warn("cannot tell the service manager", "state", state, "error", err)
	}
}

// run is the loop of §15.13: what Wake says is due, then a sleep until it says to look again.
func (l *loop) run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			// Asked to stop: what an operation left is the reprise's at the next start.
			return nil
		default:
		}
		now := l.opt.Machine.Now()
		ripe, _, groupNext, waiting := l.grouping.Ripe(now)
		for _, path := range ClosedPaths(ripe) {
			l.closed[path] = true
		}
		if len(l.queues) > 0 {
			for _, folder := range ChangedFolders(ripe) {
				l.changed[folder] = true
			}
		}
		var origins []string
		for _, g := range ripe {
			for _, o := range g.Origins {
				if !slices.Contains(origins, o) {
					origins = append(origins, o)
				}
			}
		}
		// A stretch of integrity.window in which the scrub found nothing left waits for the next one.
		nowLocal := time.Unix(0, now).In(l.set.location)
		opened := l.set.scrubWindow.Opened(nowLocal)
		idle := !opened.IsZero() && l.scrubIdle == opened.UnixNano()
		// One catalog backup a stretch of maintenance.window.
		window := l.set.window.Opened(nowLocal)
		backedUp := !window.IsZero() && l.backupOpened == window.UnixNano()
		in := WakeInput{NowNS: now, CheckDueNS: l.checkDue, First: l.first, Reloaded: l.reloaded, Gone: l.gone, Back: l.back, PassDueNS: l.passDue,
			RipeOrigins: origins, LastPassEndNS: l.lastEnd, RescanEveryNS: l.set.rescanEvery.Nanoseconds(), Window: l.set.window, Location: l.set.location,
			HashPending: l.hashPending && l.opt.Apply,
			// A daemon in shadow mode scrubs nothing, as it would read the same copies again and again.
			ScrubPending: l.opt.Apply && l.lastEnd != nil && !idle,
			ScrubWindow:  l.set.scrubWindow, ScrubBudgetNS: l.set.scrubBudget.Nanoseconds(), ScrubSpentNS: l.scrubSpent.Nanoseconds(),
			ScrubSpentOpenedNS: l.scrubOpened, BackupPending: l.opt.Apply && l.lastEnd != nil && !backedUp, SmartDueNS: l.smartDue,
			NotifyDueNS: l.notifyDue(), BalanceDueNS: l.balanceDue(now)}
		if waiting {
			in.GroupDueNS = &groupNext
		}
		out, err := Wake(&in)
		if err != nil {
			return fmt.Errorf("daemon: %w", err)
		}
		if out.Check {
			l.check(ctx)
			continue
		}
		if len(out.Because) > 0 {
			l.pass(ctx, out.Because)
			continue
		}
		if out.Notify {
			l.tell(ctx)
			continue
		}
		if out.Smart {
			l.readSmart(ctx)
			continue
		}
		if out.Balance {
			l.balance(ctx)
			continue
		}
		if out.Hash {
			l.hash(ctx)
			continue
		}
		if out.Backup {
			l.backup(ctx, window.UnixNano())
			continue
		}
		if out.Scrub {
			l.scrub(ctx, out.ScrubOpenedNS)
			continue
		}
		timer := time.NewTimer(time.Duration(out.WakeNS - now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case e := <-l.watcher.Events():
			timer.Stop()
			l.add(e)
		case <-l.opt.Reload:
			timer.Stop()
			l.reload()
		case <-timer.C:
		}
	}
}

// reload reads the configuration again (specs.md §15.13). One that does not load is left aside, and the one read
// before stays; one that does takes effect at once, with a check, then a pass.
func (l *loop) reload() {
	if l.opt.Load == nil {
		return
	}
	c, err := l.opt.Load()
	if err != nil {
		l.log.Warn("the configuration did not load: the one read before stays", "error", err)
		return
	}
	set, err := readSettings(c, l.opt)
	if err != nil {
		l.log.Warn("the configuration did not load: the one read before stays", "error", err)
		return
	}
	l.opt.Config, l.set = c, set
	l.queues = newQueues(set.servers, l.queues)
	for id := range l.disks {
		if set.disksByID[id] == nil {
			l.unwatch(id)
			delete(l.disks, id)
		}
	}
	l.reloaded = true
	l.checkDue = l.opt.Machine.Now()
	if l.smartDue == nil {
		l.smartDue = smartFirst(l.opt.Machine)
	}
	l.log.Info("the configuration was read again", "disks", len(c.Disks))
}

// add puts an event, and every other already waiting, in its group.
func (l *loop) add(e watch.Event) {
	for {
		if err := l.grouping.Add(e); err != nil {
			l.log.Warn("an event the grouping refused", "disk", e.Disk, "error", err)
		}
		select {
		case e = <-l.watcher.Events():
		default:
			return
		}
	}
}

// check runs the mount check of every disk, follows what changed, and tries the remount of a disk a USB reset
// took (specs.md §15.13).
func (l *loop) check(ctx context.Context) {
	l.checkDue = l.opt.Machine.Now() + l.set.checkEvery.Nanoseconds()
	verdicts, err := disks.CheckAll(ctx, l.opt.Machine, l.opt.Config.Disks)
	if err != nil {
		l.log.Warn("the mount check stopped", "error", err)
		return
	}
	for i := range l.opt.Config.Disks {
		d, v := &l.opt.Config.Disks[i], verdicts[i]
		st := l.disks[d.ID]
		if st == nil {
			st = &diskState{state: v.State}
			l.disks[d.ID] = st
			if v.State == disks.Missing {
				l.log.Warn("a disk is missing", "disk", d.ID, "reason", v.Reason, "message", v.Message)
			}
		}
		switch {
		case d.State == config.DiskFailed:
			// Nothing reads or writes it: going and coming back calls for no pass.
		case st.state != disks.Missing && v.State == disks.Missing:
			l.gone = true
			st.walked, st.reset = false, slices.Contains(l.set.resetReasons, v.Reason)
			l.unwatch(d.ID)
			l.log.Warn("a disk went missing", "disk", d.ID, "reason", v.Reason, "message", v.Message)
		case st.state == disks.Missing && v.State != disks.Missing:
			l.back = true
			st.walked, st.reset = false, false
			l.log.Info("a disk is back", "disk", d.ID, "state", v.State)
		case st.state != v.State:
			l.log.Info("a disk changed state", "disk", d.ID, "from", st.state, "to", v.State, "reason", v.Reason)
		}
		st.state = v.State
		if st.state == disks.Missing && st.reset && d.State != config.DiskFailed {
			l.remount(ctx, d)
		}
	}
}

// remount tries umount -l then mount of a disk a USB reset took, through its fstab entry (specs.md §15.13).
func (l *loop) remount(ctx context.Context, d *config.Disk) {
	if !l.set.remount || !l.opt.Apply {
		return
	}
	table, err := fsx.ReadFile(l.set.fstab)
	if err != nil || !fstabHas(table, d.Mount) {
		return
	}
	if out, err := exec.CommandContext(ctx, "umount", "-l", d.Mount).CombinedOutput(); err != nil {
		l.log.Info("umount before the remount", "disk", d.ID, "error", err, "output", string(out))
	}
	args := []string{d.Mount}
	if l.set.fstab != "/etc/fstab" {
		args = []string{"-T", l.set.fstab, d.Mount}
	}
	if out, err := exec.CommandContext(ctx, "mount", args...).CombinedOutput(); err != nil {
		l.log.Warn("the remount failed", "disk", d.ID, "error", err, "output", string(out))
		return
	}
	l.log.Info("remounted after a reset", "disk", d.ID, "mount", d.Mount)
}

func (l *loop) watch(ctx context.Context, id string) {
	st, d := l.disks[id], l.set.disksByID[id]
	if st == nil || d == nil || st.watched || d.State == config.DiskFailed {
		return
	}
	how, err := l.watcher.Watch(ctx, d)
	if err != nil {
		l.log.Warn("cannot watch a disk: only passes will see it", "disk", id, "error", err)
		return
	}
	st.watched = true
	l.log.Info("watching a disk", "disk", id, "how", how)
}

func (l *loop) unwatch(id string) {
	l.watcher.Stop(id)
	if st := l.disks[id]; st != nil {
		st.watched = false
	}
}

// pass runs one pass of §15.12 under the catalog's lock, then puts the runtime branches right.
func (l *loop) pass(ctx context.Context, because []string) {
	start := l.opt.Machine.Now()
	l.first, l.reloaded, l.gone, l.back = false, false, false, false
	l.log.Info("a pass begins", "because", because)
	// The sink counts what the pass did instead of keeping each line: a pass over a whole pool would not fit in the
	// memory of specs.md §26.
	sink := &passSink{folders: map[string]bool{}}
	var due *alerts.Due
	outcome, err := l.locked(ctx, func(db *sql.DB) (*jobs.Outcome, error) {
		outcome, err := jobs.RunClosed(ctx, db, l.opt.Config, l.opt.Machine, l.opt.Apply, sink, l.closed)
		if err != nil {
			return nil, err
		}
		for _, line := range outcome.Disks {
			st := l.disks[line.ID]
			switch {
			case st == nil:
			case line.Walked:
				st.walked = true
			case line.Verdict.State == disks.Missing && st.state != disks.Missing:
				// A device that fails still answers the mount check from the cache: only the pass's confirmation
				// finds it, and the pass has recorded it already.
				st.state, st.walked, st.reset = disks.Missing, false, slices.Contains(l.set.resetReasons, line.Verdict.Reason)
				l.unwatch(line.ID)
				l.log.Warn("a disk went missing", "disk", line.ID, "reason", line.Verdict.Reason, "message", line.Verdict.Message)
			}
		}
		l.branches(ctx, db)
		l.purge(ctx, db)
		due = l.prepareAlerts(ctx, db, outcome, &sink.refused)
		return outcome, nil
	})
	end := l.opt.Machine.Now()
	l.lastEnd = &end
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		retry := end + retryAfter.Nanoseconds()
		l.passDue = &retry
		l.log.Error("the pass failed", "error", err)
		return
	}
	// What the watcher saw closed served this pass, and what changed goes to the media servers.
	l.closed = jobs.Closed{}
	l.queueFolders(sink.folders)
	l.passDue = nil
	l.hashPending = true
	if outcome.NextDue.Valid {
		next := outcome.NextDue.Int64
		l.passDue = &next
	}
	for id, st := range l.disks {
		if st.walked && st.state != disks.Missing {
			l.watch(ctx, id)
		}
	}
	l.log.Info("a pass ended", "because", because, "seconds", float64(end-start)/1e9, "decisions", sink.decisions,
		"operations", sink.operations, "waiting", outcome.Waiting, "partial", outcome.Partial, "held", outcome.Held)
	l.sendAlerts(ctx, due)
	if l.opt.OnPass != nil {
		l.opt.OnPass(because, outcome)
	}
}

// passSink counts what a pass decided and did, and keeps the names a driver refused and the folders its operations
// changed.
type passSink struct {
	decisions, operations int64
	refused               alerts.Refused
	folders               map[string]bool
}

func (s *passSink) Disks([]jobs.DiskLine) error { return nil }

func (s *passSink) Decision(*jobs.Decision) error {
	s.decisions++
	return nil
}

func (s *passSink) Operation(op *jobs.Operation) error {
	s.operations++
	for _, f := range OperationFolders(op) {
		if s.folders != nil {
			s.folders[f] = true
		}
	}
	if op.Outcome == trash.OutcomeNameRefused {
		// A rename was refused its new name, a copy its own.
		name := op.Path
		if op.To != "" {
			name = op.To
		}
		s.refused.Add(name)
	}
	return nil
}

// prepareAlerts works out, under the catalog's lock, the alerts a pass leaves to send (specs.md §23). With no
// channel set, the table alerts stays as it is, so that a channel set later hears of what holds then.
func (l *loop) prepareAlerts(ctx context.Context, db *sql.DB, outcome *jobs.Outcome, refused *alerts.Refused) *alerts.Due {
	if len(l.set.channels) == 0 {
		return nil
	}
	now := l.opt.Machine.Now()
	facts, err := alerts.Gather(ctx, db, l.opt.Config, now, outcome, refused)
	if err == nil {
		var due *alerts.Due
		if due, err = alerts.Prepare(ctx, db, facts, now, l.opt.Apply); err == nil {
			if l.opt.Apply {
				return due
			}
			for _, m := range due.Messages {
				l.log.Info("an alert is due; a dry run sends none", "key", m.Key, "title", m.Title)
			}
			return nil
		}
	}
	if ctx.Err() == nil {
		l.log.Warn("the alerts could not be worked out: the next pass tries again", "error", err)
	}
	return nil
}

// sendAlerts sends what a pass left to send, outside the catalog's lock, then records what went under it. An
// alert that did not go brings a pass sooner, which tries it again.
func (l *loop) sendAlerts(ctx context.Context, due *alerts.Due) {
	if due == nil || len(due.Messages) == 0 {
		return
	}
	sent := alerts.SendAll(ctx, l.set.channels, due.Messages, func(m *alerts.Message, c alerts.Channel, err error) {
		if ctx.Err() == nil {
			l.log.Warn("an alert could not be sent: a pass tries again", "key", m.Key, "channel", c.Name(), "error", err)
		}
	})
	for _, key := range sent {
		l.log.Info("an alert was sent", "key", key)
	}
	if len(sent) > 0 {
		if _, err := l.locked(ctx, func(db *sql.DB) (*jobs.Outcome, error) { return nil, alerts.Record(ctx, db, due, sent) }); err != nil && ctx.Err() == nil {
			l.log.Warn("the alerts sent could not be recorded: they may be sent again", "error", err)
		}
	}
	if len(sent) < len(due.Messages) {
		retry := l.opt.Machine.Now() + alertRetry.Nanoseconds()
		if l.passDue == nil || *l.passDue > retry {
			l.passDue = &retry
		}
	}
}

// hash reads new files' hashes for a slice of time, under the catalog's lock (specs.md §15.13).
func (l *loop) hash(ctx context.Context) {
	left := false
	_, err := l.locked(ctx, func(db *sql.DB) (*jobs.Outcome, error) {
		var err error
		left, err = watch.HashNew(ctx, db, l.opt.Config, l.opt.Machine, l.opt.Machine.Now()+hashSlice.Nanoseconds())
		return nil, err
	})
	if err != nil && ctx.Err() == nil {
		l.log.Warn("reading new files' hashes failed: the next pass tries again", "error", err)
	}
	l.hashPending = left && err == nil
}

// scrub reads the copies due for a slice of time, under the catalog's lock, within what is left of the budget of the
// stretch of integrity.window that began at opened, and moves a corrupt copy to the trash when another is healthy:
// starting the daemon is the consent (specs.md §15.13, §16). A corrupt copy brings a pass, which sends its alert, and
// makes the copy moved again.
func (l *loop) scrub(ctx context.Context, opened int64) {
	if l.scrubOpened != opened {
		l.scrubOpened, l.scrubSpent = opened, 0
	}
	slice := min(hashSlice, l.set.scrubBudget-l.scrubSpent)
	start := l.opt.Machine.Now()
	var out *jobs.ScrubOutcome
	_, err := l.locked(ctx, func(db *sql.DB) (*jobs.Outcome, error) {
		var err error
		out, err = jobs.Scrub(ctx, db, l.opt.Config, l.opt.Machine, jobs.ScrubOptions{Budget: slice, Apply: true, Quarantine: true})
		return nil, err
	})
	l.scrubSpent += time.Duration(l.opt.Machine.Now() - start)
	if err != nil {
		if ctx.Err() == nil {
			l.log.Warn("the scrub failed: it goes on later", "error", err)
			// Within the budget still, but not at once: a slice of the budget is spent.
			l.scrubSpent += hashSlice
		}
		return
	}
	left, corrupt := false, 0
	for i := range out.Disks {
		d := &out.Disks[i]
		left = left || d.Left
		corrupt += len(d.Corrupt)
		if d.Read > 0 || len(d.Corrupt) > 0 || len(d.Changed) > 0 {
			l.log.Info("scrubbed", "disk", d.ID, "copies", d.Read, "bytes", d.Bytes, "corrupt", len(d.Corrupt), "repaired", len(d.Repaired), "quarantined", len(d.Quarantined),
				"kept", len(d.Kept), "changed", len(d.Changed))
		}
	}
	if !left {
		l.scrubIdle = opened
	}
	if corrupt > 0 {
		now := l.opt.Machine.Now()
		l.passDue = &now
	}
}

// backup backs the catalog up, under its lock, once for the stretch of maintenance.window that began at opened, and
// with maintenance.catalog_on_disks copies it to the disks (specs.md §9, §15.13). One that fails is not tried again
// before the next stretch: a line of the log says so.
func (l *loop) backup(ctx context.Context, opened int64) {
	l.backupOpened = opened
	var out *jobs.BackupOutcome
	_, err := l.locked(ctx, func(db *sql.DB) (*jobs.Outcome, error) {
		var err error
		out, err = jobs.BackupCatalog(ctx, db, l.opt.Config, l.opt.Machine)
		return nil, err
	})
	if err != nil {
		if ctx.Err() == nil {
			l.log.Error("the catalog backup failed: the next stretch of the maintenance window tries again", "error", err)
		}
		return
	}
	l.log.Info("the catalog was backed up", "path", out.Path, "removed", len(out.Removed))
	for _, d := range out.Disks {
		if d.Err != nil {
			l.log.Warn("the catalog backup did not reach a disk", "disk", d.ID, "error", d.Err)
		}
	}
}

// purge purges the trash after a pass, under the same lock, as atoll trash purge --yes does: what has reached its
// time, then what goes for room (specs.md §15.13, §17). It reads the catalog first, and checks no disk when nothing is
// due. A dry run purges nothing.
func (l *loop) purge(ctx context.Context, db *sql.DB) {
	if !l.opt.Apply {
		return
	}
	due, err := trash.PurgeDue(ctx, db, l.opt.Config, l.opt.Machine.Now())
	if err != nil || !due {
		if err != nil && ctx.Err() == nil {
			l.log.Warn("the trash could not be read: the next pass tries again", "error", err)
		}
		return
	}
	s, err := trash.Open(ctx, db, l.opt.Config, l.opt.Machine, true)
	if err != nil {
		if ctx.Err() == nil {
			l.log.Warn("purging the trash failed: the next pass tries again", "error", err)
		}
		return
	}
	defer s.Close()
	var purged, stayed int
	summary, err := s.Purge(ctx, nil, func(e *trash.EntryItem) error {
		if e.Outcome == trash.OutcomePurged {
			purged++
		} else {
			stayed++
		}
		return nil
	})
	if err == nil {
		err = s.Finish(ctx)
	}
	if err != nil {
		if ctx.Err() == nil {
			l.log.Warn("purging the trash failed: the next pass tries again", "error", err)
		}
		return
	}
	l.log.Info("the trash was purged", "entries", purged, "bytes", summary.Bytes, "stayed", stayed, "full", summary.Full)
}

// locked runs fn on the catalog, under its lock, which it takes after the others' and lets go of last.
func (l *loop) locked(ctx context.Context, fn func(db *sql.DB) (*jobs.Outcome, error)) (outcome *jobs.Outcome, err error) {
	release, err := fsx.WaitLockFile(ctx, l.set.catalog, func() { l.log.Info("waiting for the catalog: a command holds it") })
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer release()
	db, err := catalog.OpenPool(ctx, l.set.catalog, l.opt.Config.PoolID)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	// Closed before the lock goes: closing another descriptor of the file would drop SQLite's own locks.
	defer func() { err = errors.Join(err, db.Close()) }()
	return fn(db)
}

// branches writes mergerfs's runtime branches when they should change: every branch of the configuration but
// those of the disks missing, or not walked since they came back (specs.md §15.13).
func (l *loop) branches(ctx context.Context, db *sql.DB) {
	if !l.opt.Apply || l.set.viewMount == "" {
		return
	}
	current, err := view.ReadBranches(l.set.viewMount)
	if err != nil {
		if !errors.Is(err, view.ErrNoMergerfs) {
			l.log.Warn("cannot read mergerfs's branches", "error", err)
		}
		return
	}
	list, err := catalog.Disks(ctx, db)
	if err != nil {
		l.log.Warn("cannot read the disks' capacities", "error", err)
		return
	}
	capacities := map[string]int64{}
	for _, d := range list {
		if d.HasCapacity {
			capacities[d.ID] = d.Capacity
		}
	}
	m, err := view.NewMergerfs(l.opt.Config, l.set.minFree, capacities)
	if err != nil {
		l.log.Warn("no mergerfs configuration", "error", err)
		return
	}
	want := m.Runtime(func(disk string) bool {
		st, d := l.disks[disk], l.set.disksByID[disk]
		// A failed disk is read-only and never walked: present is enough.
		return st != nil && st.state != disks.Missing && (st.walked || d != nil && d.State == config.DiskFailed)
	})
	if want == "" || want == current {
		return
	}
	if err := view.WriteBranches(l.set.viewMount, want); err != nil {
		l.log.Warn("cannot write mergerfs's branches", "error", err)
		return
	}
	l.log.Info("mergerfs's branches changed", "from", current, "to", want)
}

// ClosedPaths gives the paths of the ripe groups that say a file was closed after a write by someone other than
// Atoll, in the order of the groups: Atoll's own copies close files too, and a folder is no file (specs.md §14.1).
func ClosedPaths(ripe []watch.Group) []string {
	var out []string
	for i := range ripe {
		g := &ripe[i]
		if g.Folder || !slices.Contains(g.Kinds, watch.KindWritten) || !slices.ContainsFunc(g.Origins, func(o string) bool { return o != watch.OriginAtoll }) {
			continue
		}
		out = append(out, g.Path)
	}
	return out
}
