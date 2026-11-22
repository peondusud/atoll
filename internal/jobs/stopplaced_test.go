package jobs

import (
	"context"
	"errors"
	"testing"
)

// A run stopped as SIGTERM or a low battery stops it, right as a copy moves to its name, still records the change
// time the look after the rename found: the copy is at its name already, and the record is what lets the reprise
// tell a write since from the rename itself (specs.md §14.2). Without it the reprise records the copy unverified,
// which a crash may leave but a stop never should.
func TestAStopAsACopyMovesKeepsItsRecord(t *testing.T) {
	t.Parallel()
	p := newParallel(t, "", map[string]string{"t1": "ssd", "t2": "ssd"}, "t1", "t2")
	paths := p.fill("t1", 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p.fault("t2", func(call, _ string) error {
		if call == "rename" {
			// The rename goes on; the stop comes as it does.
			cancel()
		}
		return nil
	})
	_, err := Replicate(ctx, p.db, p.config, p, ReplicateOptions{Now: true}, func(*ReplicateItem) error { return nil })
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("the run gave %v", err)
	}
	p.fault("t2", nil)
	if got := p.count("SELECT count(*) FROM jobs WHERE json_extract(payload, '$.placed.ctime_ns') IS NOT NULL"); got != 1 {
		t.Errorf("%d copies with their change time recorded after the stop, want 1", got)
	}
	p.now += 3_600_000_000_000
	for range 3 {
		if _, err := p.replicateAll(); err != nil {
			t.Fatal(err)
		}
		if p.count("SELECT count(*) FROM jobs") == 0 {
			break
		}
		p.now += 3_600_000_000_000
	}
	// The reprise finds the copy as its record says, and records it verified.
	p.copied("t1", "t2", paths)
}
