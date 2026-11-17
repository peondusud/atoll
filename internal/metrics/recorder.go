package metrics

import (
	"slices"
	"sync"
)

// Recorder holds the daemon's Snapshot: the daemon's steps change it, each request to /metrics writes it. A nil
// Recorder records nothing, for a daemon run with no metrics, as in tests.
type Recorder struct {
	mu sync.Mutex
	s  Snapshot
}

// NewRecorder gives a Recorder for a daemon built as version.
func NewRecorder(version string) *Recorder {
	return &Recorder{s: Snapshot{Version: version}}
}

// Update changes the snapshot, under its lock: fn must not keep s.
func (r *Recorder) Update(fn func(s *Snapshot)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(&r.s)
}

// Text writes the snapshot as it stands (specs.md §23.1).
func (r *Recorder) Text() ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Render(&r.s)
}

// Configure sets the disks of the configuration, in its order, from what each says of itself: a disk that stays keeps
// what was recorded of it, and one that went is forgotten.
func (s *Snapshot) Configure(disks []Disk) {
	next := make([]Disk, len(disks))
	for i := range disks {
		next[i] = disks[i]
		if was := s.DiskOf(disks[i].ID); was != nil {
			kept := *was
			kept.FS, kept.Role, kept.Mount = disks[i].FS, disks[i].Role, disks[i].Mount
			next[i] = kept
		}
	}
	s.Disks = next
}

// DiskOf gives the disk of that id, nil when the configuration has none.
func (s *Snapshot) DiskOf(id string) *Disk {
	at := slices.IndexFunc(s.Disks, func(d Disk) bool { return d.ID == id })
	if at < 0 {
		return nil
	}
	return &s.Disks[at]
}

// AddOperations adds counts of operations, by kind and outcome.
func (s *Snapshot) AddOperations(counts map[[2]string]int64) {
	for key, n := range counts {
		at := slices.IndexFunc(s.Operations, func(op Operation) bool { return op.Kind == key[0] && op.Outcome == key[1] })
		if at < 0 {
			s.Operations = append(s.Operations, Operation{Kind: key[0], Outcome: key[1]})
			at = len(s.Operations) - 1
		}
		s.Operations[at].Count += n
	}
}
