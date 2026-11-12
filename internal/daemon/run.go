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

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/jobs"
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
	checkDue                    int64
	passDue                     *int64
	lastEnd                     *int64
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
		opt: &opt, set: set, log: opt.Log, grouping: grouping, disks: map[string]*diskState{}, first: true,
		watcher: watch.NewWatcher(watch.WatcherOptions{ViewMount: set.viewMount, Now: opt.Machine.Now, Inotify: opt.Inotify}),
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
		var origins []string
		for _, g := range ripe {
			for _, o := range g.Origins {
				if !slices.Contains(origins, o) {
					origins = append(origins, o)
				}
			}
		}
		in := WakeInput{NowNS: now, CheckDueNS: l.checkDue, First: l.first, Reloaded: l.reloaded, Gone: l.gone, Back: l.back, PassDueNS: l.passDue,
			RipeOrigins: origins, LastPassEndNS: l.lastEnd, RescanEveryNS: l.set.rescanEvery.Nanoseconds(), Window: l.set.window, Location: l.set.location}
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
	for id := range l.disks {
		if set.disksByID[id] == nil {
			l.unwatch(id)
			delete(l.disks, id)
		}
	}
	l.reloaded = true
	l.checkDue = l.opt.Machine.Now()
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
		if st.state == disks.Missing && st.reset {
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
	if st == nil || d == nil || st.watched {
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
	outcome, err := l.locked(ctx, func(db *sql.DB) (*jobs.Outcome, error) {
		outcome, err := jobs.RunTo(ctx, db, l.opt.Config, l.opt.Machine, l.opt.Apply, nil)
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
	l.passDue = nil
	if outcome.NextDue.Valid {
		next := outcome.NextDue.Int64
		l.passDue = &next
	}
	for id, st := range l.disks {
		if st.walked && st.state != disks.Missing {
			l.watch(ctx, id)
		}
	}
	l.log.Info("a pass ended", "because", because, "seconds", float64(end-start)/1e9, "decisions", len(outcome.Decisions),
		"operations", len(outcome.Operations), "waiting", outcome.Waiting, "partial", outcome.Partial, "held", outcome.Held)
	if l.opt.OnPass != nil {
		l.opt.OnPass(because, outcome)
	}
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
		st := l.disks[disk]
		return st != nil && st.state != disks.Missing && st.walked
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
