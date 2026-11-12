package alerts

import (
	"cmp"
	"fmt"
	"slices"
)

// Why an alert is sent (specs.md §23).
const (
	WhyOnset    = "onset"
	WhyMore     = "more"
	WhyReminder = "reminder"
)

// Holding is a condition that holds after a pass: its key and its count.
type Holding struct {
	Key   string
	Count int64
}

// Stored is what the table alerts keeps of a condition sent.
type Stored struct {
	Key     string
	FirstNS int64
	SentNS  int64
	Count   int64
}

// Send is an alert to send.
type Send struct {
	Key string
	Why string
}

// Plan says what to send, and what the table keeps: Store once those are sent, Keep for what holds and is not
// sent now, and Drop for what ended. Each is in key order.
type Plan struct {
	Send  []Send
	Store []Stored
	Keep  []Stored
	Drop  []string
}

// PlanAlerts works out what to send at a time, from the conditions that hold and what was sent before (specs.md
// §23): a condition that begins is sent, one that holds is sent again when its count grows or remindNS has gone
// by since it was last sent, and one that ends is dropped. It does no I/O.
func PlanAlerts(holding []Holding, stored []Stored, nowNS, remindNS int64) (Plan, error) {
	if remindNS <= 0 {
		return Plan{}, fmt.Errorf("alerts: a reminder every %d ns", remindNS)
	}
	byKey := make(map[string]Stored, len(stored))
	for _, s := range stored {
		byKey[s.Key] = s
	}
	held := make(map[string]bool, len(holding))
	list := slices.Clone(holding)
	slices.SortFunc(list, func(a, b Holding) int { return cmp.Compare(a.Key, b.Key) })
	var p Plan
	for _, h := range list {
		if held[h.Key] {
			return Plan{}, fmt.Errorf("alerts: the key %q holds twice", h.Key)
		}
		if h.Count < 0 {
			return Plan{}, fmt.Errorf("alerts: %q counts %d", h.Key, h.Count)
		}
		held[h.Key] = true
		s, known := byKey[h.Key]
		switch {
		case !known:
			p.Send = append(p.Send, Send{h.Key, WhyOnset})
			p.Store = append(p.Store, Stored{Key: h.Key, FirstNS: nowNS, SentNS: nowNS, Count: h.Count})
		case h.Count > s.Count:
			p.Send = append(p.Send, Send{h.Key, WhyMore})
			p.Store = append(p.Store, Stored{Key: h.Key, FirstNS: s.FirstNS, SentNS: nowNS, Count: h.Count})
		case nowNS-s.SentNS >= remindNS:
			p.Send = append(p.Send, Send{h.Key, WhyReminder})
			p.Store = append(p.Store, Stored{Key: h.Key, FirstNS: s.FirstNS, SentNS: nowNS, Count: h.Count})
		default:
			// A count that went down is kept so, so that one more later is sent.
			p.Keep = append(p.Keep, Stored{Key: h.Key, FirstNS: s.FirstNS, SentNS: s.SentNS, Count: h.Count})
		}
	}
	for _, s := range stored {
		if !held[s.Key] {
			p.Drop = append(p.Drop, s.Key)
		}
	}
	slices.Sort(p.Drop)
	return p, nil
}
