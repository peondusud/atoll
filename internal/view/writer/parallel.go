package writer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/rules"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/watch"
)

// Copies written in parallel (specs.md §12.3, §21.2): a new file whose policy says immediate, with replicas wanted, is
// written as the program writes it to its original and to a part in <workdir>/tmp/ on each replica's target, the
// targets the solver's, so that its bytes are on each when the program closes it. A pass's job then adopts each part,
// and the rest of the copy pipeline makes it a replica. A part never holds the program back: what it waits to have
// written is bounded, and one that falls behind, fails, or would eat into min_free is dropped at once, its replica left
// to the ordinary job.

// What a part may wait to have written: past partLimit, or past viewLimit for every part of the view at once, a part
// that falls behind is dropped. partQueue bounds the writes it may wait for, however small. Variables, so that a test
// can make a part fall behind.
var (
	partLimit int64 = 32 << 20
	viewLimit int64 = 128 << 20
)

const partQueue = 4096

// chunk is a write a part waits to have, its own copy of the bytes.
type chunk struct {
	data []byte
	off  int64
}

// part is one copy in parallel, on one target.
type part struct {
	disk  string
	view  *catalog.ViewOp
	op    *catalog.Op
	file  *ops.Part
	queue chan chunk
	// drained is closed once the part wrote or dropped every write it took.
	drained chan struct{}
	// What the part waits to have written, the largest end it was given, and why it was dropped, under parallel.mu
	// but err, which its writer sets too, under errMu.
	waiting int64
	end     int64
	errMu   sync.Mutex
	err     error
}

func (p *part) failed() error {
	p.errMu.Lock()
	defer p.errMu.Unlock()
	return p.err
}

func (p *part) fail(err error) {
	p.errMu.Lock()
	defer p.errMu.Unlock()
	if p.err == nil {
		p.err = err
	}
}

// parallel is the parts of one file being written, from its creation to its close.
type parallel struct {
	w *Writer
	// ctx is what its end runs under: the create's, which no interrupt of the kernel's cancels, as the parts end
	// apart from the requests that write and close the file.
	ctx   context.Context
	path  string
	mu    sync.Mutex
	parts []*part
	// over says that the file was closed, or the parts dropped: nothing more reaches them.
	over bool
}

// roomOf is what the view's parts may still fill on a disk past min_free, as its filesystem said when the first of
// those open there began, and how many are open there.
type roomOf struct {
	room int64
	open int
}

// parts holds what the writer knows of its copies written in parallel: each file's, by path, what every part waits to
// have written, and each target's room.
type parts struct {
	mu      sync.Mutex
	files   map[string]*parallel
	waiting int64
	rooms   map[string]*roomOf
	// finishing counts what closing a file's parts still does, which Wait waits for.
	finishing sync.WaitGroup
}

// Wait waits until the parts of every file closed are finished with: synced and handed to the passes, or dropped.
func (w *Writer) Wait() {
	w.par.finishing.Wait()
}

// ErrFellBehind says that a part waited to have more written than it may, so that the program never waits for it.
var ErrFellBehind = errors.New("the copy written in parallel fell behind")

// ErrNoRoomPast says that a part would fill its disk past min_free.
var ErrNoRoomPast = errors.New("the copy written in parallel would fill its disk past min_free")

// maxParallelOn is how many parts the view writes on a disk at once, at most: as many as the daemon makes copies there
// at once, by the disk's kind or max_parallel_per_disk (specs.md §14). The view's parts are the user's own writes, at the
// pace the program writes them; they count on their own, in the view, apart from the daemon's copies.
func (w *Writer) maxParallelOn(d *config.Disk) int {
	return disks.CopyLimit(w.machine, d, w.config.CopyLimits())
}

// startParallel begins the copies in parallel of a new file the view made on disk at path, when its policy says so: in
// immediate mode with replicas wanted, read before its size is known, on no SSD cache, with no file of the catalog at
// that path, the targets the solver chooses, each a data disk the view may write to now, online, not in standby, as a
// part held back by a disk waking would fall behind, and holding fewer parts of the view than its limit of copies at
// once (specs.md §14).
// What cannot begin is left to the ordinary job.
func (w *Writer) startParallel(ctx context.Context, disk, path string, who Caller) *parallel {
	if enabled, _ := w.config.Effective()["replication.enabled"].(bool); !enabled {
		return nil
	}
	policy := w.config.Policy(path, nil)
	if policy.Action == rules.Ignore || policy.Mode != "immediate" || policy.Replicas == 0 {
		return nil
	}
	if f, err := catalog.FileAt(ctx, w.db, path); err != nil || f != nil {
		return nil
	}
	rows, err := catalog.ViewDisks(ctx, w.db)
	if err != nil {
		w.log.Warn("no copy in parallel: the catalog cannot be read", "path", path, "error", err)
		return nil
	}
	state := map[string]catalog.ViewDisk{}
	for _, r := range rows {
		state[r.ID] = r
	}
	if state[disk].Cache {
		// A new file on the SSD cache stays there, resident: its copies come with its transfer (specs.md §19).
		return nil
	}
	free := func(id string) (int64, bool) {
		d := w.disk(id)
		if d == nil {
			return 0, false
		}
		_, f, err := w.machine.Space(d.Mount)
		return f, err == nil
	}
	// The plan is the pool's, standby disks among its candidates as for any replica: a target it chooses in standby is
	// left to the ordinary job by startPart, rather than another chosen in its place.
	writable := func(id string) bool {
		r := state[id]
		return w.handles.Has(id) && !r.Cache
	}
	targets, err := watch.ParallelTargets(ctx, w.db, w.config, w.now(), path, disk, free, writable)
	if err != nil {
		w.log.Warn("no copy in parallel: no plan", "path", path, "error", err)
		return nil
	}
	par := &parallel{w: w, path: path, ctx: context.WithoutCancel(ctx)}
	for _, id := range targets {
		if p := w.startPart(ctx, id, disk, path, who); p != nil {
			par.parts = append(par.parts, p)
		}
	}
	if len(par.parts) == 0 {
		return nil
	}
	w.par.mu.Lock()
	w.par.files[path] = par
	w.par.mu.Unlock()
	return par
}

// startPart journals and makes one part on a target, and starts what writes it, or says nil.
func (w *Writer) startPart(ctx context.Context, disk, source, path string, who Caller) *part {
	d := w.disk(disk)
	if d == nil || !w.writableNow(ctx, disk) {
		return nil
	}
	if !w.openPart(d) {
		return nil
	}
	op, v, err := catalog.BeginViewPart(ctx, w.db, disk, path, source, w.now(), who.asker())
	if err != nil {
		w.closePart(disk)
		w.log.Warn("no copy in parallel: the journal cannot be written", "disk", disk, "path", path, "error", err)
		return nil
	}
	var file *ops.Part
	err = w.handles.Use(disk, func(h *ops.Disk) error {
		var err error
		if file, err = h.MakePart(op); err != nil {
			return err //nolint:wrapcheck // ops says what failed
		}
		return catalog.SetCreateKey(ctx, w.db, op, fmt.Sprintf("%d", file.Stat.Ino)) //nolint:wrapcheck // the catalog says what failed
	})
	if err != nil {
		if file != nil {
			_ = file.Close()
		}
		w.log.Warn("no copy in parallel", "disk", disk, "path", path, "error", err)
		w.dropPart(ctx, &part{disk: disk, view: v, op: op}, err)
		return nil
	}
	p := &part{disk: disk, view: v, op: op, file: file, queue: make(chan chunk, partQueue), drained: make(chan struct{})}
	go p.write(w)
	return p
}

// writableNow says whether a disk takes a part now: the view has a handle on it, and the catalog says it is online.
func (w *Writer) writableNow(ctx context.Context, disk string) bool {
	if !w.handles.Has(disk) {
		return false
	}
	rows, err := catalog.ViewDisks(ctx, w.db)
	if err != nil {
		return false
	}
	for _, r := range rows {
		if r.ID == disk {
			return !r.Cache && r.State == string(solver.DiskOnline)
		}
	}
	return false
}

// openPart counts a part open on a disk, under its limit of copies at once, and measures its room past min_free when it is
// the first open there.
func (w *Writer) openPart(d *config.Disk) bool {
	w.par.mu.Lock()
	defer w.par.mu.Unlock()
	r := w.par.rooms[d.ID]
	if r != nil && r.open >= w.maxParallelOn(d) {
		return false
	}
	if r == nil {
		capacity, free, err := w.machine.Space(d.Mount)
		if err != nil {
			return false
		}
		reserved, err := w.minFree.Reserved(capacity)
		if err != nil || free <= reserved {
			return false
		}
		r = &roomOf{room: free - reserved}
		w.par.rooms[d.ID] = r
	}
	r.open++
	return true
}

// closePart counts a part no longer open on a disk; with none left there, the room is measured again for the next.
func (w *Writer) closePart(disk string) {
	w.par.mu.Lock()
	defer w.par.mu.Unlock()
	if r := w.par.rooms[disk]; r != nil {
		if r.open--; r.open <= 0 {
			delete(w.par.rooms, disk)
		}
	}
}

// write writes what a part is given, in order, until its queue is closed; once it failed, what comes is let go.
func (p *part) write(w *Writer) {
	defer close(p.drained)
	for c := range p.queue {
		if p.failed() == nil {
			if _, err := p.file.WriteAt(c.data, c.off); err != nil {
				p.fail(fmt.Errorf("write: %w", err))
			}
		}
		w.par.mu.Lock()
		w.par.waiting -= int64(len(c.data))
		p.waiting -= int64(len(c.data))
		w.par.mu.Unlock()
	}
}

// mirror gives each part of a file a write the original took, never waiting: a part that would wait past its bound, or
// fill its disk past min_free, is dropped.
func (par *parallel) mirror(data []byte, off int64) {
	if len(data) == 0 {
		return
	}
	par.mu.Lock()
	defer par.mu.Unlock()
	if par.over {
		return
	}
	w := par.w
	for _, p := range par.parts {
		if p.failed() != nil {
			continue
		}
		end := off + int64(len(data))
		w.par.mu.Lock()
		grow := max(end-p.end, 0)
		r := w.par.rooms[p.disk]
		switch {
		case r == nil || r.room < grow:
			p.fail(ErrNoRoomPast)
		case p.waiting+int64(len(data)) > partLimit, w.par.waiting+int64(len(data)) > viewLimit:
			p.fail(ErrFellBehind)
		}
		if p.failed() != nil {
			w.par.mu.Unlock()
			continue
		}
		c := chunk{data: append([]byte(nil), data...), off: off}
		select {
		case p.queue <- c:
			r.room -= grow
			w.par.waiting += int64(len(data))
			p.waiting += int64(len(data))
			p.end = max(p.end, end)
		default:
			p.fail(ErrFellBehind)
		}
		w.par.mu.Unlock()
	}
}

// abandon drops every part of a file: something the parts cannot follow happened to it, as a write that failed on the
// original, a truncation, a rename, a deletion, or another open to write it. Ordinary rules apply from then on.
func (par *parallel) abandon(why string) {
	par.mu.Lock()
	if par.over {
		par.mu.Unlock()
		return
	}
	par.over = true
	parts := par.parts
	for _, p := range parts {
		p.fail(errors.New(why))
		close(p.queue)
	}
	par.mu.Unlock()
	par.w.forget(par)
	par.finish(parts, -1)
}

// release ends the parts of a file the program closed: each that took every write and holds the original's size is
// synced and handed to the passes, done; any other is dropped. size is the original's, -1 when it cannot be told.
func (par *parallel) release(size int64) {
	par.mu.Lock()
	if par.over {
		par.mu.Unlock()
		return
	}
	par.over = true
	parts := par.parts
	for _, p := range parts {
		close(p.queue)
	}
	par.mu.Unlock()
	par.w.forget(par)
	par.finish(parts, size)
}

func (w *Writer) forget(par *parallel) {
	w.par.mu.Lock()
	defer w.par.mu.Unlock()
	if w.par.files[par.path] == par {
		delete(w.par.files, par.path)
	}
}

// finish waits, apart from the program, for each part to have written what it took, then hands it over or drops it.
func (par *parallel) finish(parts []*part, size int64) {
	w := par.w
	w.par.finishing.Add(1)
	go func() {
		defer w.par.finishing.Done()
		ctx := par.ctx
		for _, p := range parts {
			<-p.drained
			err := p.failed()
			if err == nil && size < 0 {
				err = errors.New("the original's size cannot be told")
			}
			if err == nil {
				err = p.seal(size)
			}
			if err != nil {
				w.log.Info("a copy written in parallel is dropped: the ordinary job makes that replica", "disk", p.disk, "path", par.path, "reason", err)
				w.dropPart(ctx, p, err)
				continue
			}
			if err := catalog.ViewPartWritten(ctx, w.db, p.view); err != nil {
				w.log.Warn("a copy written in parallel stays planned: the journal cannot be written", "disk", p.disk, "path", par.path, "error", err)
			}
			w.closePart(p.disk)
		}
	}()
}

// seal makes a part that holds the original's size durable, and closes it.
func (p *part) seal(size int64) error {
	defer func() { _ = p.file.Close() }()
	st, err := p.file.Info()
	switch {
	case err != nil:
		return fmt.Errorf("look: %w", err)
	case st.Size != size:
		return fmt.Errorf("it holds %d bytes, the original %d", st.Size, size)
	}
	if err := p.file.Sync(); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	return nil
}

// dropPart removes a part and undoes its operations; on a disk that cannot be relied on they stay planned, for the
// view's reprise at its next start (specs.md §17.2).
func (w *Writer) dropPart(ctx context.Context, p *part, why error) {
	if p.file != nil {
		_ = p.file.Close()
	}
	w.closePart(p.disk)
	err := w.handles.Use(p.disk, func(h *ops.Disk) error { return h.DropPart(p.op.Path2) }) //nolint:wrapcheck // ops says what failed
	switch {
	case errors.Is(err, ops.ErrDiskLost), errors.Is(err, ErrNoHandle):
		w.log.Warn("a copy written in parallel stays planned: its disk cannot be relied on", "disk", p.disk, "error", err, "reason", why)
		return
	case err != nil:
		w.log.Warn("a copy written in parallel stays planned: its part cannot be removed", "disk", p.disk, "error", err, "reason", why)
		return
	}
	if err := catalog.FinishViewOp(ctx, w.db, p.view, catalog.OpUndone, false, w.now()); err != nil {
		w.log.Warn("a copy written in parallel stays planned: the journal cannot be written", "disk", p.disk, "error", err)
	}
}

// abandonAt drops the parts of every file at or below a path being written in parallel.
func (w *Writer) abandonAt(path, why string) {
	w.par.mu.Lock()
	var list []*parallel
	for p, par := range w.par.files {
		if p == path || strings.HasPrefix(p, path+"/") {
			list = append(list, par)
		}
	}
	w.par.mu.Unlock()
	for _, par := range list {
		par.abandon(why)
	}
}
