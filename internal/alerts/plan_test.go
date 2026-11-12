package alerts

import (
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

func TestPlanVectors(t *testing.T) {
	type stored struct {
		Key     string `json:"key"`
		FirstNS int64  `json:"first_ns"`
		SentNS  int64  `json:"sent_ns"`
		Count   int64  `json:"count"`
	}
	type vector struct {
		Holding []struct {
			Key   string `json:"key"`
			Count int64  `json:"count"`
		} `json:"holding"`
		Stored   []stored `json:"stored"`
		NowNS    int64    `json:"now_ns"`
		RemindNS int64    `json:"remind_ns"`
		Send     []struct {
			Key string `json:"key"`
			Why string `json:"why"`
		} `json:"send"`
		Store []stored `json:"store"`
		Keep  []stored `json:"keep"`
		Drop  []string `json:"drop"`
		Error bool     `json:"error"`
		Note  string   `json:"note"`
	}
	rows := func(list []stored) []Stored {
		out := []Stored{}
		for _, s := range list {
			out = append(out, Stored(s))
		}
		return out
	}
	for i, v := range vectors.Load[vector](t, "alerts_plan.json") {
		var holding []Holding
		for _, h := range v.Holding {
			holding = append(holding, Holding{h.Key, h.Count})
		}
		p, err := PlanAlerts(holding, rows(v.Stored), v.NowNS, v.RemindNS)
		if v.Error {
			if err == nil {
				t.Errorf("case %d (%s): want an error", i, v.Note)
			}
			continue
		}
		if err != nil {
			t.Errorf("case %d (%s): %v", i, v.Note, err)
			continue
		}
		send := []Send{}
		for _, s := range v.Send {
			send = append(send, Send{s.Key, s.Why})
		}
		if !slices.Equal(orEmpty(p.Send), send) || !slices.Equal(orEmpty(p.Store), rows(v.Store)) || !slices.Equal(orEmpty(p.Keep), rows(v.Keep)) ||
			!slices.Equal(orEmpty(p.Drop), orEmpty(v.Drop)) {
			t.Errorf("case %d (%s): %+v", i, v.Note, p)
		}
	}
}

func orEmpty[T any](list []T) []T {
	if list == nil {
		return []T{}
	}
	return list
}
