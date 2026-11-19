package jobs

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// waitFor waits until a condition holds, for some time at most, and says whether it did.
func waitFor(cond func() bool, most time.Duration) bool {
	for end := time.Now().Add(most); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

// What makes or removes a name on a disk is done one copy at a time there (specs.md §14): two copies to one folder that
// is missing never both make it, which would end the second as path-taken, and the time of a folder two copies land in
// is put back once, to what it was, not to what the first copy's rename made it. Each case holds the first copy at the
// point where the second would come in between, for a while: one copy at a time, the second never does.
func TestCopiesAtOnceTakeTheirNamesOneAtATime(t *testing.T) {
	t.Run("a folder made once", func(t *testing.T) {
		p := newParallel(t, "", map[string]string{"t1": "ssd", "t2": "ssd"}, "t1", "t2")
		paths := p.fill("t1", 2)
		var mu sync.Mutex
		looks := map[uint64]int{}
		held := false
		p.fault("t2", func(call, name string) error {
			if name != "F" {
				return nil
			}
			switch call {
			case "stat":
				g := goroutine()
				mu.Lock()
				// A copy looks at its target first before anything is journaled, and again as it moves there.
				looks[g]++
				mu.Unlock()
			case "mkdir":
				mu.Lock()
				first := !held
				held = true
				mu.Unlock()
				if first {
					// Held until the other copy looks at its target as it moves there, which one copy at a time it never
					// does while this one makes the folder.
					self := goroutine()
					waitFor(func() bool { mu.Lock(); defer mu.Unlock(); return otherInPlace(looks, self) }, 500*time.Millisecond)
				}
			}
			return nil
		})
		items, err := p.replicateAll()
		if err != nil {
			t.Fatal(err)
		}
		if got := outcomes(items); got[OutcomeReplicated] != 2 {
			t.Fatalf("%+v", items)
		}
		p.copied("t1", "t2", paths)
	})
	t.Run("a folder's time put back once", func(t *testing.T) {
		p := newParallel(t, "", map[string]string{"t1": "ssd", "t2": "ssd"}, "t1", "t2")
		paths := p.fill("t1", 2)
		folder := filepath.Join(p.dirs["t2"], "media", "F")
		if err := os.MkdirAll(folder, 0o755); err != nil {
			t.Fatal(err)
		}
		was := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
		if err := os.Chtimes(folder, was, was); err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		renames := 0
		p.fault("t2", func(call, _ string) error {
			if call == "rename" {
				mu.Lock()
				renames++
				mu.Unlock()
			}
			return nil
		})
		var once sync.Once
		p.stopAt = func(point string) {
			if point == "moved" {
				once.Do(func() {
					waitFor(func() bool { mu.Lock(); defer mu.Unlock(); return renames >= 2 }, 500*time.Millisecond)
				})
			}
		}
		items, err := p.replicateAll()
		if err != nil {
			t.Fatal(err)
		}
		if got := outcomes(items); got[OutcomeReplicated] != 2 {
			t.Fatalf("%+v", items)
		}
		st, err := os.Stat(folder)
		if err != nil {
			t.Fatal(err)
		}
		if !st.ModTime().Equal(was) {
			t.Errorf("the folder's time is %v, want %v", st.ModTime(), was)
		}
		p.copied("t1", "t2", paths)
	})
}

// otherInPlace says whether a copy other than self looks at its target as it moves there.
func otherInPlace(looks map[uint64]int, self uint64) bool {
	for g, n := range looks {
		if g != self && n >= 2 {
			return true
		}
	}
	return false
}
