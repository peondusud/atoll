package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/metrics"
	"github.com/peondusud/atoll/internal/ups"
	"github.com/peondusud/atoll/internal/value"
)

const (
	// upsEvery is how often the daemon reads the UPS, and upsDead how long it may go unheard: upsmon's POLLFREQ and
	// DEADTIME, by default (specs.md §20).
	upsEvery = 5 * time.Second
	upsDead  = 15 * time.Second
	// flushWait is how long the disks have to write out what they hold, as the battery runs low: a disk hung on a dead
	// USB link holds its syncfs for good, and the daemon waits no longer for it.
	flushWait = time.Minute
)

// power reads the UPS through NUT, every upsEvery, in a goroutine of its own, and keeps what it makes of it for the
// loop (specs.md §20). A nil power reads no UPS, which counts as on line power, as for a loop a test makes by hand.
type power struct {
	log   *slog.Logger
	now   func() int64
	read  func(context.Context, value.NUT) (string, error)
	every time.Duration
	dead  time.Duration

	mu sync.Mutex
	// enabled is ups.enabled, and nut the UPS it names, nil when ups.nut is no address NUT can have: nothing is asked
	// then, and the UPS goes unheard.
	enabled bool
	nut     *value.NUT
	// reading is what the daemon makes of the UPS, and answeredNS when it last said something: the start, or the
	// reload that enabled it, until it does.
	reading    ups.Reading
	answeredNS int64
	// cancel ends the step of the loop under way: the battery running low ends it at once.
	cancel context.CancelFunc
	// changed tells the loop that the reading changed.
	changed chan struct{}
	// metrics is told of each reading that changes, under mu (specs.md §23.1).
	metrics *metrics.Recorder
}

func newPower(log *slog.Logger, now func() int64, every, dead time.Duration) *power {
	if every <= 0 {
		every = upsEvery
	}
	if dead <= 0 {
		dead = upsDead
	}
	return &power{log: log, now: now, read: ups.Read, every: every, dead: dead, reading: ups.Reading{Power: ups.Online}, changed: make(chan struct{}, 1)}
}

// configure takes ups.enabled and ups.nut, at the start and at each reload. A UPS no longer read counts as on line
// power, and one just enabled has dead time to answer.
func (p *power) configure(c *config.Config) {
	if p == nil {
		return
	}
	effective := c.Effective()
	enabled, _ := effective["ups.enabled"].(bool)
	address, _ := effective["ups.nut"].(string)
	p.mu.Lock()
	defer p.mu.Unlock()
	defer p.record()
	if enabled && !p.enabled {
		p.answeredNS = p.now()
	}
	p.enabled, p.nut = enabled, nil
	if !enabled {
		p.set(ups.Reading{Power: ups.Online})
		return
	}
	n, err := value.ParseNUT(address)
	if err != nil {
		p.log.Error("ups.nut is no address NUT can have: the UPS goes unheard", "nut", address, "error", err)
		return
	}
	p.nut = &n
}

// run reads the UPS every p.every until ctx is done.
func (p *power) run(ctx context.Context) {
	ticker := time.NewTicker(p.every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.poll(ctx)
		}
	}
}

// poll reads the UPS once, and keeps what the daemon makes of it.
func (p *power) poll(ctx context.Context) {
	p.mu.Lock()
	enabled, nut := p.enabled, p.nut
	p.mu.Unlock()
	if !enabled {
		return
	}
	var status *string
	if nut != nil {
		s, err := p.read(ctx, *nut)
		switch {
		case err == nil:
			status = &s
		case ctx.Err() != nil:
			return
		default:
			p.log.Debug("the UPS gave no answer", "error", err)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.enabled || p.nut != nut {
		// Read again meanwhile: this reading is of a UPS no longer asked about.
		return
	}
	now := p.now()
	r := ups.Next(p.reading.Power, p.answeredNS, now, p.dead.Nanoseconds(), status)
	if r.Answered {
		p.answeredNS, r.Answered = now, false
	}
	p.set(r)
}

// set keeps a reading, p.mu held, and tells the loop when it changed. The battery running low ends the loop's step
// under way at once, as SIGTERM would.
func (p *power) set(r ups.Reading) {
	if r == p.reading {
		return
	}
	was := p.reading
	p.reading = r
	p.record()
	if r.Power == ups.Low && was.Power != ups.Low && p.cancel != nil {
		p.cancel()
	}
	select {
	case p.changed <- struct{}{}:
	default:
	}
}

// record gives the metrics what the daemon makes of the UPS, nothing without ups.enabled (specs.md §23.1). Under mu.
func (p *power) record() {
	var u *metrics.UPS
	if p.enabled {
		u = &metrics.UPS{Power: string(p.reading.Power), Unreachable: p.reading.Unreachable}
	}
	p.metrics.Update(func(s *metrics.Snapshot) { s.UPS = u })
}

// get gives what the daemon makes of the UPS.
func (p *power) get() ups.Reading {
	if p == nil {
		return ups.Reading{Power: ups.Online}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reading
}

// onBattery says whether a copy may start: not on battery, nor with the battery low (specs.md §20).
func (p *power) onBattery() bool {
	return p.get().Power != ups.Online
}

// turn gives the context of one step of the loop, which the battery running low ends, and what to call once the step
// is over.
func (p *power) turn(ctx context.Context) (context.Context, func()) {
	turn, cancel := context.WithCancel(ctx)
	if p == nil {
		return turn, cancel
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancel = cancel
	if p.reading.Power == ups.Low {
		cancel()
	}
	return turn, func() {
		p.mu.Lock()
		p.cancel = nil
		p.mu.Unlock()
		cancel()
	}
}

// signal tells of a change of the reading, nil when no UPS is read.
func (p *power) signal() <-chan struct{} {
	if p == nil {
		return nil
	}
	return p.changed
}

// powerChanged says in the log what changed of the UPS since the last turn, and makes due what it calls for: back on
// line power, the copies the passes held; and a pass, whose alerts tell whether the UPS can be read (specs.md §20, §23).
func (l *loop) powerChanged() {
	r, was := l.power.get(), l.lastPower
	if r == was {
		return
	}
	l.lastPower = r
	now := l.opt.Machine.Now()
	due := func() {
		if l.passDue == nil || *l.passDue > now {
			l.passDue = &now
		}
	}
	switch {
	case r.Power == ups.Battery && was.Power == ups.Online:
		l.log.Warn("the UPS runs on battery: no copy starts until line power is back")
		l.notify("STATUS=On battery: no copy starts")
	case r.Power == ups.Online && was.Power != ups.Online:
		l.log.Info("the UPS is on line power again: what waited for it is due")
		l.notify("STATUS=On line power")
		due()
	}
	if r.Unreachable != was.Unreachable {
		if r.Unreachable {
			l.log.Warn("the UPS cannot be read: NUT has said nothing of it", "for", l.power.dead.String())
		} else {
			l.log.Info("the UPS can be read again")
		}
		due()
	}
}

// park stops the daemon's work as the UPS's battery runs low (specs.md §20): the step under way has ended, at its next
// step, as SIGTERM ends it, and the daemon writes out every disk, then only reads the UPS, hears the watcher and SIGHUP,
// until line power is back, and refuses what the local API hands it at once (specs.md §25.1). It then takes its work
// up again as at a start. It says whether the daemon was asked to stop meanwhile.
func (l *loop) park(ctx context.Context) bool {
	l.log.Warn("the UPS's battery runs low: the daemon stops its work, writes out the disks, and waits for line power")
	l.notify("STATUS=Low battery: stopped until line power is back")
	l.flush()
	for l.power.get().Power == ups.Low {
		select {
		case <-ctx.Done():
			return true
		case e := <-l.watcher.Events():
			l.add(e)
		case <-l.opt.Reload:
			l.reload()
		case w := <-l.opt.Catalog:
			w.done <- ErrParked
		case <-l.power.signal():
		}
	}
	// Anything may have changed while it did nothing, and the pass the battery stopped may have been one that walked
	// every disk.
	l.first = true
	l.log.Info("line power is back: the daemon takes its work up again, as at a start")
	return false
}

// flush asks each disk the last check found, a failed one aside, to write out what its filesystem holds in memory,
// syncfs(2), all at once and for flushWait at most: a disk hung on a dead USB link would hold the others (specs.md §20).
func (l *loop) flush() {
	type result struct {
		id  string
		err error
	}
	results := make(chan result, len(l.opt.Config.Disks))
	waiting := map[string]bool{}
	for i := range l.opt.Config.Disks {
		d := &l.opt.Config.Disks[i]
		if st := l.disks[d.ID]; st == nil || st.state == disks.Missing || d.State == config.DiskFailed {
			continue
		}
		waiting[d.ID] = true
		go func(id, mount string) {
			results <- result{id: id, err: syncDisk(l.opt.Machine, mount)}
		}(d.ID, d.Mount)
	}
	wait := l.opt.FlushWait
	if wait <= 0 {
		wait = flushWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for len(waiting) > 0 {
		select {
		case r := <-results:
			delete(waiting, r.id)
			if r.err != nil {
				l.log.Warn("a disk could not write out what it holds", "disk", r.id, "error", r.err)
			}
		case <-timer.C:
			l.log.Warn("disks did not finish writing out in time", "disks", slices.Sorted(maps.Keys(waiting)))
			return
		}
	}
	l.log.Info("the disks wrote out what they held")
}

// syncDisk writes out the filesystem of a disk's mount.
func syncDisk(m disks.Machine, mount string) error {
	top, err := m.OpenTop(mount)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	return errors.Join(top.SyncFS(), top.Close())
}
