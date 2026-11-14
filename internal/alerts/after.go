package alerts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
)

// Remind is how often an alert is sent again while its condition lasts (specs.md §23).
const Remind = 24 * time.Hour

// Due is what a pass leaves to send, in key order, and what to record of each once it went.
type Due struct {
	Messages []*Message
	// RemindNS is when the next reminder of a condition that holds is due, 0 when none holds (specs.md §23): the
	// daemon comes back then, as nothing else may bring a pass.
	RemindNS int64
	rows     map[string]catalog.AlertRow
}

// Prepare works out, under the catalog's lock, what to send after a pass. With write, it records at once what needs
// no send: a condition that ended is forgotten, and a count that went down is kept, so that one more is sent.
func Prepare(ctx context.Context, db *sql.DB, f *Facts, nowNS int64, write bool) (*Due, error) {
	rows, err := catalog.ReadAlerts(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	stored := make([]Stored, 0, len(rows))
	for _, r := range rows {
		stored = append(stored, Stored(r))
	}
	holding := Holdings(f)
	plan, err := PlanAlerts(holding, stored, nowNS, Remind.Nanoseconds())
	if err != nil {
		return nil, err
	}
	if write {
		keep := make([]catalog.AlertRow, 0, len(plan.Keep))
		for _, s := range plan.Keep {
			keep = append(keep, catalog.AlertRow(s))
		}
		if err := catalog.WriteAlerts(ctx, db, keep, plan.Drop); err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
	}
	counts := make(map[string]int64, len(holding))
	for _, h := range holding {
		counts[h.Key] = h.Count
	}
	due := &Due{rows: make(map[string]catalog.AlertRow, len(plan.Store))}
	for _, s := range plan.Store {
		due.rows[s.Key] = catalog.AlertRow(s)
	}
	// What holds is sent again a day after it last was: what is sent now, a day from now.
	for _, list := range [][]Stored{plan.Keep, plan.Store} {
		for _, s := range list {
			if at := s.SentNS + Remind.Nanoseconds(); due.RemindNS == 0 || at < due.RemindNS {
				due.RemindNS = at
			}
		}
	}
	for _, s := range plan.Send {
		m, err := Describe(ctx, db, f, Holding{Key: s.Key, Count: counts[s.Key]})
		if err != nil {
			return nil, err
		}
		if s.Why == WhyReminder {
			since := time.Unix(0, due.rows[s.Key].FirstNS).Format("2006-01-02 15:04 MST")
			m.Body += "\nThis has lasted since " + since + "."
		}
		due.Messages = append(due.Messages, m)
	}
	return due, nil
}

// Unsent gives what is left of what a pass left to send once the alerts of those keys went: the others, for another
// try, each recorded as its pass worked it out once it goes.
func (d *Due) Unsent(sent []string) *Due {
	left := &Due{RemindNS: d.RemindNS, rows: d.rows}
	for _, m := range d.Messages {
		if !slices.Contains(sent, m.Key) {
			left.Messages = append(left.Messages, m)
		}
	}
	return left
}

// RetryAfter gives how long the daemon waits before it tries again, alone, alerts that could not be sent, after that
// many tries in a row that left some unsent (specs.md §23): 5 minutes, then twice as long each time, up to an hour.
func RetryAfter(tries int) time.Duration {
	wait := 5 * time.Minute
	for i := 1; i < tries && wait < time.Hour; i++ {
		wait *= 2
	}
	return min(wait, time.Hour)
}

// Record writes, under the catalog's lock, what was sent: an alert not sent is tried again later.
func Record(ctx context.Context, db *sql.DB, due *Due, sent []string) error {
	put := make([]catalog.AlertRow, 0, len(sent))
	for _, key := range sent {
		if r, ok := due.rows[key]; ok {
			put = append(put, r)
		}
	}
	if err := catalog.WriteAlerts(ctx, db, put, nil); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	return nil
}

// SendAll sends each message through each channel and gives the keys of those that went through one at least. A
// channel that cannot be reached is not tried again for the messages after it, as each would wait as long; one whose
// server answered with a refusal is, as the refusal may be the message's own. failed is told of each failure.
func SendAll(ctx context.Context, channels []Channel, messages []*Message, failed func(m *Message, c Channel, err error)) []string {
	down := make([]bool, len(channels))
	var sent []string
	for _, m := range messages {
		went := false
		for i, c := range channels {
			if down[i] {
				continue
			}
			if err := c.Send(ctx, m); err != nil {
				var answered *AnswerError
				down[i] = !errors.As(err, &answered)
				failed(m, c, err)
				continue
			}
			went = true
		}
		if went {
			sent = append(sent, m.Key)
		}
	}
	return sent
}
