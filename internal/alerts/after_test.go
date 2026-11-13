package alerts

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
)

func missing(ids ...string) *Facts {
	f := &Facts{Files: &catalog.FileAlerts{ByState: map[string]int64{}}}
	for _, id := range ids {
		f.Disks = append(f.Disks, jobs.DiskLine{ID: id, Verdict: disks.Verdict{State: disks.Missing}})
	}
	return f
}

func keys(due *Due) []string {
	var out []string
	for _, m := range due.Messages {
		out = append(out, m.Key)
	}
	return out
}

// What a pass leaves to send, then what is recorded once sent: an alert that went is not sent again while its
// condition holds, until the reminder; one that did not go is sent at the next pass; one that ends is forgotten
// (specs.md §23).
func TestPrepareAndRecord(t *testing.T) {
	db := newCatalog(t, nil)
	ctx := t.Context()
	const hour = int64(3600e9)
	due, err := Prepare(ctx, db, missing("t1", "t2"), 1000, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := keys(due); !slices.Equal(got, []string{"disk-missing:t1", "disk-missing:t2"}) {
		t.Fatalf("%q", got)
	}
	// Only t1 went.
	if err := Record(ctx, db, due, []string{"disk-missing:t1"}); err != nil {
		t.Fatal(err)
	}
	if due, _ = Prepare(ctx, db, missing("t1", "t2"), 1000+hour, true); !slices.Equal(keys(due), []string{"disk-missing:t2"}) {
		t.Errorf("an hour later: %q", keys(due))
	}
	if err := Record(ctx, db, due, keys(due)); err != nil {
		t.Fatal(err)
	}
	due, _ = Prepare(ctx, db, missing("t1", "t2"), 1000+24*hour, true)
	if !slices.Equal(keys(due), []string{"disk-missing:t1"}) || !strings.Contains(due.Messages[0].Body, "This has lasted since") {
		t.Errorf("a day later: %q", keys(due))
	}
	// A dry run records nothing, not even what ended.
	if due, _ = Prepare(ctx, db, missing(), 1000+25*hour, false); len(due.Messages) != 0 {
		t.Errorf("all ended: %q", keys(due))
	}
	if rows, _ := catalog.ReadAlerts(ctx, db); len(rows) != 2 {
		t.Errorf("a dry run wrote: %+v", rows)
	}
	if _, err := Prepare(ctx, db, missing(), 1000+25*hour, true); err != nil {
		t.Fatal(err)
	}
	if rows, _ := catalog.ReadAlerts(ctx, db); len(rows) != 0 {
		t.Errorf("ended, still there: %+v", rows)
	}
}

// channel is a channel that fails on the messages it is told to.
type channel struct {
	name string
	fail map[string]string
	got  []string
}

func (c *channel) Name() string { return c.name }

func (c *channel) Send(_ context.Context, m *Message) error {
	c.got = append(c.got, m.Key)
	switch c.fail[m.Key] {
	case "down":
		return errors.New("down")
	case "refused":
		return fmt.Errorf("x: %w", &AnswerError{Status: "413", Text: "too large"})
	default:
		return nil
	}
}

// A message went when one channel at least took it. A channel that cannot be reached is not tried again in the same
// send; one whose server refused a message is, for the next.
func TestSendAll(t *testing.T) {
	a := &channel{name: "a", fail: map[string]string{"2": "down"}}
	b := &channel{name: "b", fail: map[string]string{"1": "refused", "2": "down"}}
	var failures []string
	sent := SendAll(t.Context(), []Channel{a, b}, []*Message{{Key: "1"}, {Key: "2"}, {Key: "3"}}, func(m *Message, c Channel, err error) {
		failures = append(failures, c.Name()+":"+m.Key)
	})
	if !slices.Equal(sent, []string{"1"}) || !slices.Equal(a.got, []string{"1", "2"}) || !slices.Equal(b.got, []string{"1", "2"}) ||
		!slices.Equal(failures, []string{"b:1", "a:2", "b:2"}) {
		t.Errorf("sent %q, a got %q, b got %q, failures %q", sent, a.got, b.got, failures)
	}
}

// What a pass leaves says when the next reminder is due: 24 h after the alert of a condition that holds was last
// sent, the earliest of them, and never with nothing that holds. The daemon comes back then: on a pool where nothing
// changes no pass was due for days, and the reminder with it (specs.md §23).
func TestPrepareSaysWhenToRemind(t *testing.T) {
	db := newCatalog(t, nil)
	ctx := t.Context()
	const hour = int64(3600e9)
	day := Remind.Nanoseconds()
	due, err := Prepare(ctx, db, missing("t1"), 1000, true)
	if err != nil {
		t.Fatal(err)
	}
	if due.RemindNS != 1000+day {
		t.Errorf("an alert to send now: remind at %d, want %d", due.RemindNS, 1000+day)
	}
	if err := Record(ctx, db, due, keys(due)); err != nil {
		t.Fatal(err)
	}
	// Ten hours later another condition begins: the first one's reminder comes first.
	due, err = Prepare(ctx, db, missing("t1", "t2"), 1000+10*hour, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(keys(due), []string{"disk-missing:t2"}) || due.RemindNS != 1000+day {
		t.Errorf("ten hours later: %q, remind at %d, want %d", keys(due), due.RemindNS, 1000+day)
	}
	if err := Record(ctx, db, due, keys(due)); err != nil {
		t.Fatal(err)
	}
	// Once the first ended, the second's.
	if due, err = Prepare(ctx, db, missing("t2"), 1000+11*hour, true); err != nil || len(due.Messages) != 0 || due.RemindNS != 1000+10*hour+day {
		t.Errorf("then: %q, remind at %d, %v", keys(due), due.RemindNS, err)
	}
	if due, err = Prepare(ctx, db, missing(), 1000+12*hour, true); err != nil || due.RemindNS != 0 {
		t.Errorf("with nothing that holds: remind at %d, %v", due.RemindNS, err)
	}
}
