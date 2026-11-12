package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
)

// smartEvery is how often the daemon reads the disks' SMART (specs.md §23): smartctl -n standby wakes none of them.
const smartEvery = time.Hour

// smartFirst says when a daemon that starts first reads SMART: at once, after its first pass, when its machine runs
// smartctl; never otherwise.
func smartFirst(m disks.Machine) *int64 {
	if _, runs := m.(disks.SmartReader); !runs {
		return nil
	}
	now := m.Now()
	return &now
}

// readSmart reads the SMART of each disk the last check found mounted, unless it is failed, and records each reading
// of a disk awake; a disk asleep is left so, with the reading it had (specs.md §23). A reading that tells of more
// trouble than the one before brings a pass, whose alerts tell of it. Without smartctl, nothing is read again until
// the configuration is.
func (l *loop) readSmart(ctx context.Context) {
	now := l.opt.Machine.Now()
	next := now + smartEvery.Nanoseconds()
	l.smartDue = &next
	readings := map[string]disks.Smart{}
	for i := range l.opt.Config.Disks {
		d := &l.opt.Config.Disks[i]
		if st := l.disks[d.ID]; st == nil || st.state == disks.Missing || d.State == config.DiskFailed {
			continue
		}
		s, err := disks.ReadSmart(ctx, l.opt.Machine, d)
		switch {
		case errors.Is(err, disks.ErrNoSmart), errors.Is(err, exec.ErrNotFound):
			l.log.Warn("SMART is not read: smartctl is not there; the configuration read again looks for it again", "error", err)
			l.smartDue = nil
			return
		case err != nil:
			if ctx.Err() == nil {
				l.log.Warn("SMART could not be read", "disk", d.ID, "error", err)
			}
		case !s.Asleep:
			readings[d.ID] = s
		}
	}
	if len(readings) == 0 {
		return
	}
	var worse []string
	_, err := l.locked(ctx, func(db *sql.DB) (*jobs.Outcome, error) {
		var err error
		worse, err = recordSmart(ctx, db, readings, now, l.opt.Apply)
		return nil, err
	})
	if err != nil {
		if ctx.Err() == nil {
			l.log.Warn("the readings of SMART could not be recorded: the next reading tries again", "error", err)
		}
		return
	}
	for _, id := range worse {
		s := readings[id]
		l.log.Warn("SMART tells of trouble", "disk", id, "failing", s.Failing, "reallocated", s.Reallocated, "pending", s.Pending,
			"uncorrectable", s.Uncorrectable, "temperature", s.Temperature)
	}
	// A pass works the alerts out, from what was just recorded: a dry run recorded nothing.
	if len(worse) > 0 && l.opt.Apply && len(l.set.channels) > 0 && (l.passDue == nil || *l.passDue > now) {
		l.passDue = &now
	}
}

// recordSmart records each reading at readNS, with apply, and gives the disks whose reading tells of more trouble
// than the one the catalog held, by the catalog's order of the disks. A disk the catalog does not hold yet is left
// for the next reading.
func recordSmart(ctx context.Context, db *sql.DB, readings map[string]disks.Smart, readNS int64, apply bool) ([]string, error) {
	list, err := catalog.Disks(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	var worse []string
	for i := range list {
		d := &list[i]
		s, read := readings[d.ID]
		if !read {
			continue
		}
		var earlier *disks.Smart
		var r disks.SmartRecord
		if d.SmartJSON != "" && json.Unmarshal([]byte(d.SmartJSON), &r) == nil {
			earlier = &r.Smart
		}
		if s.Worse(earlier) {
			worse = append(worse, d.ID)
		}
		if !apply {
			continue
		}
		data, err := json.Marshal(disks.SmartRecord{ReadNS: readNS, Smart: s})
		if err != nil {
			return nil, fmt.Errorf("disk %q: %w", d.ID, err)
		}
		if err := catalog.SetSmart(ctx, db, d.ID, data); err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
	}
	return worse, nil
}
