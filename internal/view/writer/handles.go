package writer

import (
	"slices"
	"sync"

	"github.com/peondusud/atoll/internal/ops"
)

// Handles holds each disk the view may write to, as its last mount check found it online, through a handle on its top
// (invariant 3). A handle is replaced or dropped only once no use of it is under way, and each disk has a lock of its
// own, so that an operation that hangs on a disk in trouble holds back no other disk.
type Handles struct {
	mu    sync.Mutex
	disks map[string]*handle
}

type handle struct {
	mu sync.RWMutex
	d  *ops.Disk
}

// ErrNoHandle says that the view may not write to that disk now.
var ErrNoHandle = errNoHandle{}

type errNoHandle struct{}

func (errNoHandle) Error() string { return "the view may not write to that disk now" }

// NewHandles makes an empty set of handles.
func NewHandles() *Handles {
	return &Handles{disks: map[string]*handle{}}
}

func (h *Handles) slot(disk string) *handle {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.disks[disk]
	if !ok {
		s = &handle{}
		h.disks[disk] = s
	}
	return s
}

// Use runs use with the handle of a disk, and ErrNoHandle when it has none.
func (h *Handles) Use(disk string, use func(*ops.Disk) error) error {
	s := h.slot(disk)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.d == nil {
		return ErrNoHandle
	}
	return use(s.d)
}

// Has says whether a disk has a handle now.
func (h *Handles) Has(disk string) bool {
	s := h.slot(disk)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.d != nil
}

// Set gives a disk the handle its last check confirmed, and closes the one it had; Drop takes it away.
func (h *Handles) Set(disk string, d *ops.Disk) { h.slot(disk).replace(d) }

// Drop takes the handle of a disk its check did not confirm writable, and closes it.
func (h *Handles) Drop(disk string) { h.slot(disk).replace(nil) }

// Disks gives the disks that have a handle now, in byte order.
func (h *Handles) Disks() []string {
	h.mu.Lock()
	ids := make([]string, 0, len(h.disks))
	for id := range h.disks {
		ids = append(ids, id)
	}
	h.mu.Unlock()
	var out []string
	for _, id := range ids {
		if h.Has(id) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// Close drops every handle.
func (h *Handles) Close() {
	h.mu.Lock()
	list := make([]*handle, 0, len(h.disks))
	for _, s := range h.disks {
		list = append(list, s)
	}
	h.mu.Unlock()
	for _, s := range list {
		s.replace(nil)
	}
}

func (s *handle) replace(d *ops.Disk) {
	s.mu.Lock()
	old := s.d
	s.d = d
	s.mu.Unlock()
	if old != nil {
		old.Close()
	}
}
