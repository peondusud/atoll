// Package writer is what Atoll's own view writes on a user's behalf (specs.md §21.2): each operation on a file on one
// disk, a folder on every disk that holds it, each journaled in view_ops before it runs, and the passes of the daemon
// take it to the other copies.
package writer

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/value"
	"github.com/peondusud/atoll/internal/view"
	"github.com/peondusud/atoll/internal/watch"
)

// What the view answers a user, each the error a filesystem gives.
var (
	ErrExists = errors.New("a name is there")
	// ErrBusy says that a move of Atoll's own of the copy an open to write would write stayed planned past the wait
	// (specs.md §21.2, flaw 10): EBUSY, for the program to try again.
	ErrBusy      = errors.New("a move of Atoll's own of that copy is under way")
	ErrNotFound  = errors.New("no such name")
	ErrNotFolder = errors.New("not a folder")
	ErrIsFolder  = errors.New("a folder")
	ErrNotEmpty  = errors.New("the folder holds something")
	// ErrRefused says that a disk that holds a copy refuses the name, would hold two that collide, or a path past its
	// branch's limit: nothing changes (specs.md §15.7, §15.8, §21.2).
	ErrRefused = errors.New("a disk that holds a copy refuses that name")
	// ErrCrossDisk says that a rename would land on a name another disk presents: as across two filesystems, a
	// program copies instead (specs.md §21.2).
	ErrCrossDisk = errors.New("that name is presented from another disk")
	ErrNoSpace   = errors.New("no disk the view may write to has room")
	// ErrInvalid says that a rename would move a folder into itself.
	ErrInvalid = errors.New("a folder cannot move into itself")
)

// Owner is who asks: the owner of what is made.
type Owner = view.Owner

// Writer writes what Atoll's own view is asked to, on a user's behalf.
type Writer struct {
	db        *sql.DB
	config    *config.Config
	machine   disks.Machine
	tree      *view.Tree
	handles   *Handles
	retention time.Duration
	minFree   value.MinFree
	log       *slog.Logger
	// mu lets one change of names run at a time: what each looks at before it changes must still hold when it does.
	mu sync.Mutex
	// par holds the copies written in parallel (specs.md §12.3).
	par *parts
	// moveOnENOSPC is view.move_on_enospc: a new file whose write runs out of space moves to another disk (specs.md
	// §21.2). writing holds, by path, each file Create made that is still open through the handle it gave, under mu.
	moveOnENOSPC bool
	writing      map[string]*File
	// held holds the rows of view_ops that work under way still answers for, though the writer's lock does not cover
	// it: a part being written or closed, a move during its copy. A check while the view runs resolves none of them
	// (Recheck).
	heldMu sync.Mutex
	held   map[int64]bool
	// opened counts, by row of view_ops, the handles open to write on the file it made or writes, under heldMu: the row
	// stays held open until the last of them closes, and no pass takes it in meanwhile (specs.md §21.2, flaw 10).
	opened map[int64]int
}

// openHandle counts a handle open to write on the file of a row held open, opened with ctx.
func (w *Writer) openHandle(ctx context.Context, f *File, id int64) {
	w.heldMu.Lock()
	defer w.heldMu.Unlock()
	w.opened[id]++
	f.held, f.holder, f.heldCtx = id, w, context.WithoutCancel(ctx)
}

// closeHandle counts a handle closed, and makes the row done once none is open on its file any more.
func (w *Writer) closeHandle(ctx context.Context, id int64) {
	w.heldMu.Lock()
	w.opened[id]--
	last := w.opened[id] <= 0
	if last {
		delete(w.opened, id)
	}
	w.heldMu.Unlock()
	if !last {
		return
	}
	if err := catalog.ReleaseViewOp(ctx, w.db, id); err != nil {
		// It stays held open until the view's next start, which lets go of every row so (Reprise).
		w.log.Warn("an operation of the view stays held open", "id", id, "error", err)
	}
}

// hold says that work under way answers for a row of the view's journal, until release.
func (w *Writer) hold(id int64) {
	w.heldMu.Lock()
	defer w.heldMu.Unlock()
	w.held[id] = true
}

func (w *Writer) release(id int64) {
	w.heldMu.Lock()
	defer w.heldMu.Unlock()
	delete(w.held, id)
}

func (w *Writer) isHeld(id int64) bool {
	w.heldMu.Lock()
	defer w.heldMu.Unlock()
	return w.held[id]
}

// New makes the writer of a view.
func New(db *sql.DB, c *config.Config, m disks.Machine, tree *view.Tree, handles *Handles, log *slog.Logger) (*Writer, error) {
	text, _ := c.Effective()["trash.retention"].(string)
	retention, err := value.ParseDuration(text)
	if err != nil {
		return nil, fmt.Errorf("trash.retention: %w", err)
	}
	minFree, err := c.MinFree()
	if err != nil {
		return nil, fmt.Errorf("placement.min_free: %w", err)
	}
	moveOnENOSPC, _ := c.Effective()["view.move_on_enospc"].(bool)
	return &Writer{db: db, config: c, machine: m, tree: tree, handles: handles, retention: retention, minFree: minFree, log: log,
		par: &parts{files: map[string]*parallel{}, rooms: map[string]*roomOf{}}, moveOnENOSPC: moveOnENOSPC, writing: map[string]*File{},
		held: map[int64]bool{}, opened: map[int64]int{}}, nil
}

// Tree gives what the writer looks at what the view presents through.
func (w *Writer) Tree() *view.Tree { return w.tree }

func (w *Writer) disk(id string) *config.Disk {
	for i := range w.config.Disks {
		if w.config.Disks[i].ID == id {
			return &w.config.Disks[i]
		}
	}
	return nil
}

func split(path string) (dir, name string) {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i], path[i+1:]
	}
	return "", path
}

// folder says that a path presents a folder, as what is made or moved there needs.
func (w *Writer) folder(ctx context.Context, path string) error {
	n, err := w.tree.Lookup(ctx, path)
	switch {
	case err != nil:
		return fmt.Errorf("%w", err)
	case n == nil:
		return ErrNotFound
	case n.Kind != view.NodeDir:
		return ErrNotFolder
	}
	return nil
}

// like gives the mode and owner the view presents of a folder, for one a disk lacks on the way to what is made there.
func (w *Writer) like(ctx context.Context) ops.Like {
	return func(folder string) (*fsx.Stat, error) {
		n, err := w.tree.Lookup(ctx, folder)
		switch {
		case err != nil:
			return nil, fmt.Errorf("%w", err)
		case n == nil:
			return nil, ErrNotFound
		case n.Kind != view.NodeDir:
			return nil, ErrNotFolder
		}
		return &fsx.Stat{Kind: fsx.KindDir, Mode: n.Mode, Uid: n.UID, Gid: n.GID}, nil
	}
}

func (w *Writer) now() int64 { return w.machine.Now() }

// journal writes an operation of the view before it runs (invariant 12), with who asked it (specs.md §9).
func (w *Writer) journal(ctx context.Context, v *catalog.ViewOp, who Caller) error {
	v.AtNS = w.now()
	v.Asker = who.asker()
	if err := catalog.BeginViewOp(ctx, w.db, v); err != nil {
		return fmt.Errorf("%w", err)
	}
	return nil
}

// askedBy gives what a log line of an operation of the view says of who asked it: its uid and pid, as the journal
// records them, or nothing (specs.md §21.2).
func askedBy(v *catalog.ViewOp) []any {
	if v.Asker == nil {
		return nil
	}
	if v.Asker.PID == 0 {
		return []any{"uid", v.Asker.UID}
	}
	return []any{"uid", v.Asker.UID, "pid", v.Asker.PID}
}

// finish says what became of an operation: done, or undone when err says nothing happened. A disk that cannot be
// relied on leaves it planned, for the view's reprise (specs.md §17.1).
func (w *Writer) finish(ctx context.Context, v *catalog.ViewOp, err error) error {
	if errors.Is(err, ops.ErrDiskLost) {
		w.log.Warn("an operation of the view stays planned: its disk cannot be relied on",
			append([]any{"disk", v.Disk, "path", v.Path, "error", err}, askedBy(v)...)...)
		return err
	}
	state := catalog.OpDone
	if err != nil {
		state = catalog.OpUndone
	}
	if ferr := catalog.FinishViewOp(ctx, w.db, v, state, false, w.now()); ferr != nil {
		return errors.Join(err, fmt.Errorf("%w", ferr))
	}
	return err
}

// finishHeld is finish for an operation whose file the view then holds open: held, rather than done.
func (w *Writer) finishHeld(ctx context.Context, v *catalog.ViewOp, err error) error {
	if err != nil {
		return w.finish(ctx, v, err)
	}
	if ferr := catalog.FinishViewOp(ctx, w.db, v, catalog.ViewHeld, false, w.now()); ferr != nil {
		return fmt.Errorf("%w", ferr)
	}
	return nil
}

// Create makes a new file, on the SSD cache when its policy says so, or on the disk placement.new_files picks
// (specs.md §12.4, §19, §21.2), and gives it open for writing, the caller's. The folder it is made in is checked on
// that disk, when it holds it.
func (w *Writer) Create(ctx context.Context, path string, mode uint32, who Caller) (*File, *view.Node, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.free(ctx, path); err != nil {
		return nil, nil, err
	}
	disk, err := w.landing(ctx, path)
	if err != nil {
		return nil, nil, err
	}
	cached := disk != ""
	if !cached {
		if disk, err = w.place(ctx, path); err != nil {
			return nil, nil, err
		}
	}
	if err := w.mayCreate(ctx, asking(who), disk, path); err != nil {
		return nil, nil, err
	}
	v := &catalog.ViewOp{Kind: catalog.ViewCreate, Disk: disk, Path: path}
	if err := w.journal(ctx, v, who); err != nil {
		return nil, nil, err
	}
	var f fsx.WritableFile
	var st fsx.Stat
	err = w.handles.Use(disk, func(d *ops.Disk) error {
		var err error
		f, st, err = d.CreateUserFile(path, mode, who.UID, who.GID, w.like(ctx))
		return err //nolint:wrapcheck // ops says what failed
	})
	// Made, the row is held open, as the file is through the handle it gives, until the last one closes (flaw 10).
	if err := w.finishHeld(ctx, v, err); err != nil {
		return nil, nil, err
	}
	file := &File{WritableFile: f, Disk: disk, Path: path}
	w.openHandle(ctx, file, v.ID)
	if !cached {
		file.par = w.startParallel(ctx, disk, path, who)
	}
	if w.moveOnENOSPC {
		// A new file, open through this handle alone: a write that runs out of space may move it (specs.md §21.2).
		file.w, file.ctx, file.who, file.movable = w, context.WithoutCancel(ctx), who, true
		w.writing[path] = file
	}
	return file, view.NodeOfStat(disk, &st, ""), nil
}

// Mkdir makes a new folder, where a new file would go.
func (w *Writer) Mkdir(ctx context.Context, path string, mode uint32, who Caller) (*view.Node, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.free(ctx, path); err != nil {
		return nil, err
	}
	disk, err := w.place(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := w.mayCreate(ctx, asking(who), disk, path); err != nil {
		return nil, err
	}
	v := &catalog.ViewOp{Kind: catalog.ViewMkdir, Disk: disk, Path: path, Folder: true}
	if err := w.journal(ctx, v, who); err != nil {
		return nil, err
	}
	var st fsx.Stat
	err = w.handles.Use(disk, func(d *ops.Disk) error {
		var err error
		st, err = d.MakeUserFolder(path, mode, who.UID, who.GID, w.like(ctx))
		return err //nolint:wrapcheck // ops says what failed
	})
	if err := w.finish(ctx, v, err); err != nil {
		return nil, err
	}
	return view.NodeOfStat(disk, &st, ""), nil
}

// Symlink makes a new symbolic link, where a new file would go.
func (w *Writer) Symlink(ctx context.Context, path, target string, who Caller) (*view.Node, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.free(ctx, path); err != nil {
		return nil, err
	}
	disk, err := w.place(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := w.mayCreate(ctx, asking(who), disk, path); err != nil {
		return nil, err
	}
	v := &catalog.ViewOp{Kind: catalog.ViewSymlink, Disk: disk, Path: path}
	if err := w.journal(ctx, v, who); err != nil {
		return nil, err
	}
	var st fsx.Stat
	err = w.handles.Use(disk, func(d *ops.Disk) error {
		var err error
		st, err = d.MakeUserSymlink(path, target, who.UID, who.GID, w.like(ctx))
		return err //nolint:wrapcheck // ops says what failed
	})
	if err := w.finish(ctx, v, err); err != nil {
		return nil, err
	}
	return view.NodeOfStat(disk, &st, target), nil
}

// free says that a path's folder presents a folder, and that nothing presents at the path.
func (w *Writer) free(ctx context.Context, path string) error {
	dir, _ := split(path)
	if dir != "" {
		if err := w.folder(ctx, dir); err != nil {
			return err
		}
	}
	n, err := w.tree.Lookup(ctx, path)
	switch {
	case err != nil:
		return fmt.Errorf("%w", err)
	case n != nil:
		return ErrExists
	}
	return nil
}

// landing gives the cache disk a new file lands on (specs.md §19): with Atoll's own view, cache.enabled and its policy's
// cache, read before its size is known, a cache disk the view may write to now, online or standby and neither readonly,
// draining nor failed, with room past min_free, whose filesystem takes the name with no collision and whose branch
// reaches it; the most room, then by position. "" when there is none: the cache is full, or not for this file, and it
// goes to a data disk. Only a file lands there: a folder or a symbolic link has no residency.
func (w *Writer) landing(ctx context.Context, path string) (string, error) {
	settings, err := w.config.Cache()
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}
	if !settings.InView || !settings.Enabled || !w.config.Policy(path, nil).Cache {
		return "", nil
	}
	rows, err := catalog.ViewDisks(ctx, w.db)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}
	state := map[string]catalog.ViewDisk{}
	for _, r := range rows {
		state[r.ID] = r
	}
	best, bestRoom, bestPosition := "", int64(0), 0
	for i := range w.config.Disks {
		d := &w.config.Disks[i]
		r, known := state[d.ID]
		if !known || !r.Cache || d.State != "" || !w.handles.Has(d.ID) {
			continue
		}
		if r.State != string(solver.DiskOnline) && r.State != string(solver.DiskStandby) {
			continue
		}
		refused, err := w.refusedOn(ctx, d, path, "")
		if err != nil {
			return "", err
		}
		if refused {
			continue
		}
		capacity, free, err := w.machine.Space(d.Mount)
		if err != nil {
			w.log.Warn("the space of a disk cannot be read: nothing new goes there", "disk", d.ID, "error", err)
			continue
		}
		reserved, err := w.minFree.Reserved(capacity)
		if err != nil {
			return "", fmt.Errorf("%w", err)
		}
		room := free - reserved
		if room > 0 && (best == "" || room > bestRoom || room == bestRoom && r.Position < bestPosition) {
			best, bestRoom, bestPosition = d.ID, room, r.Position
		}
	}
	return best, nil
}

// candidate is a disk a new file may go to: the room it has past min_free and what it holds, for new_files, and how many
// files of its group it holds, for colocate.
type candidate struct {
	id       string
	fill     solver.Fill
	together int
	standby  bool
}

// place picks the disk of something new, as for a replica's target (specs.md §12.4): a data disk the view may write to
// now, online or standby and neither readonly, draining nor failed, with room past min_free, whose filesystem takes the
// name with no collision and whose branch reaches it; the one that holds the most files of its group with colocate,
// then as placement.new_files says, online before standby, then by position.
func (w *Writer) place(ctx context.Context, path string) (string, error) {
	return w.placeFor(ctx, path, "", 0)
}

// placeFor is place, but for the disk except, and among disks whose room past min_free exceeds need: where a new file
// whose write ran out of space on except moves to (specs.md §21.2).
func (w *Writer) placeFor(ctx context.Context, path, except string, need int64) (string, error) {
	rows, err := catalog.ViewDisks(ctx, w.db)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}
	newFiles, fillTo, err := watch.NewFilesOf(w.config)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}
	together := map[string]int{}
	colocate := w.config.Policy(path, nil).Colocate
	if folder, ok := solver.GroupFolder(path, colocate); ok {
		// A file counts with its own rule's colocate, as in the plan.
		same := func(other string) bool { return w.config.Policy(other, nil).Colocate == colocate }
		if together, err = catalog.GroupHolders(ctx, w.db, folder, colocate == solver.ColocateShow, same); err != nil {
			return "", fmt.Errorf("%w", err)
		}
	}
	state := map[string]catalog.ViewDisk{}
	for _, r := range rows {
		state[r.ID] = r
	}
	var list []candidate
	for i := range w.config.Disks {
		d := &w.config.Disks[i]
		r, known := state[d.ID]
		if !known || r.Cache || d.State != "" || d.ID == except || !w.handles.Has(d.ID) {
			continue
		}
		if r.State != string(solver.DiskOnline) && r.State != string(solver.DiskStandby) {
			continue
		}
		refused, err := w.refusedOn(ctx, d, path, "")
		if err != nil {
			return "", err
		}
		if refused {
			continue
		}
		capacity, free, err := w.machine.Space(d.Mount)
		if err != nil {
			w.log.Warn("the space of a disk cannot be read: nothing new goes there", "disk", d.ID, "error", err)
			continue
		}
		reserved, err := w.minFree.Reserved(capacity)
		if err != nil {
			return "", fmt.Errorf("%w", err)
		}
		if room := free - reserved; room > max(need, 0) {
			list = append(list, candidate{id: d.ID, fill: solver.Fill{Room: room, Used: capacity - free, Capacity: capacity, Position: r.Position},
				together: together[d.ID], standby: r.State == string(solver.DiskStandby)})
		}
	}
	if len(list) == 0 {
		return "", ErrNoSpace
	}
	slices.SortFunc(list, func(a, b candidate) int {
		if c := cmp.Compare(b.together, a.together); c != 0 {
			return c
		}
		if c := solver.CompareNew(newFiles, fillTo, 0, a.fill, b.fill); c != 0 {
			return c
		}
		if a.standby != b.standby {
			if a.standby {
				return 1
			}
			return -1
		}
		return cmp.Compare(a.fill.Position, b.fill.Position)
	})
	return list[0].id, nil
}

// refusedOn says whether a disk refuses a name: its filesystem, the limit of its branch, and on NTFS a name it holds
// that collides with it, but for except and what is below it, which leaves as it comes (specs.md §15.7, §15.8).
func (w *Writer) refusedOn(ctx context.Context, d *config.Disk, path, except string) (bool, error) {
	problems, err := names.CheckPath(path, d.FS)
	if err != nil {
		return false, fmt.Errorf("%w", err)
	}
	fits, err := names.FitsBranch(d.Mount, d.Root, path)
	if err != nil {
		return false, fmt.Errorf("%w", err)
	}
	if len(problems) > 0 || !fits {
		return true, nil
	}
	if d.FS != names.NTFS {
		return false, nil
	}
	parts := strings.Split(path, "/")
	for i := range parts {
		taken, err := catalog.KeyTakenExcept(ctx, w.db, d.ID, strings.Join(parts[:i+1], "/"), except)
		if err != nil {
			return false, fmt.Errorf("%w", err)
		}
		if taken {
			return true, nil
		}
	}
	return false, nil
}

// moveWait is how long an open to write waits, a try at a time, while a move of Atoll's own of the copy it would write
// is planned: a move and its syncs take milliseconds, and one a stop left planned waits for the daemon's reprise.
var moveWait, moveTry = 10 * time.Second, 20 * time.Millisecond

// Open opens a file the view presents for writing: its original, the copy it presents, alone (specs.md §21.2), once
// the caller may write it there. The write is journaled once, until a pass takes it in, and held open until the last
// handle on it closes. While a move of Atoll's own of that copy is planned, as the SSD cache's transfer journals one
// (§19), it waits, then opens the copy the view presents once that move is over, another disk's when it went through:
// a write to a copy on its way to the trash would be lost with it (conformance/models/README.md, flaw 10).
func (w *Writer) Open(ctx context.Context, path string, n *view.Node, who Caller) (*File, error) {
	if n.Kind == view.NodeDir {
		return nil, ErrIsFolder
	}
	for waited := time.Duration(0); ; waited += moveTry {
		f, err := w.open(ctx, path, n, who)
		if !errors.Is(err, catalog.ErrMovePlanned) {
			return f, err
		}
		if waited >= moveWait {
			return nil, fmt.Errorf("%w: %w", ErrBusy, err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w", ctx.Err())
		case <-time.After(moveTry):
		}
		again, err := w.tree.Lookup(ctx, path)
		switch {
		case err != nil:
			return nil, fmt.Errorf("%w", err)
		case again == nil:
			return nil, ErrNotFound
		}
		n = again
	}
}

// open is one try of Open, under the writer's lock.
func (w *Writer) open(ctx context.Context, path string, n *view.Node, who Caller) (*File, error) {
	if n.Kind == view.NodeDir {
		return nil, ErrIsFolder
	}
	// One change of names at a time: a move of the file this opens again (specs.md §21.2) is over, or never starts.
	w.mu.Lock()
	defer w.mu.Unlock()
	if f := w.writing[path]; f != nil {
		// Opened again to be written, a new file is no longer one handle's: it never moves.
		f.again = true
		if f.Disk != n.Disk {
			// It moved since the kernel looked it up: what is opened is where it is now.
			moved, err := w.tree.Lookup(ctx, path)
			switch {
			case err != nil:
				return nil, fmt.Errorf("%w", err)
			case moved == nil:
				return nil, ErrNotFound
			}
			n = moved
		}
	}
	if err := w.mayOn(asking(who), n.Disk, path, n, view.AccessWrite); err != nil {
		return nil, err
	}
	// Opened again to be written, a file is no new file written once: its copies in parallel go.
	w.abandonAt(path, "opened again to be written")
	id, err := w.journalWrite(ctx, path, n, who, true)
	if err != nil {
		return nil, err
	}
	var f fsx.WritableFile
	err = w.handles.Use(n.Disk, func(d *ops.Disk) error {
		var err error
		f, _, err = d.OpenUserFile(path)
		return err //nolint:wrapcheck // ops says what failed
	})
	file := &File{WritableFile: f, Disk: n.Disk, Path: path}
	// Counted before what failed is said, so that a row held open for nothing is let go of at once.
	if id != 0 {
		w.openHandle(ctx, file, id)
	}
	if err != nil {
		if id != 0 {
			w.closeHandle(ctx, id)
		}
		return nil, err
	}
	return file, nil
}

// journalWrite journals a write on a path's presented copy, unless one is open already that presents it from there.
// With held, for an open to write, the row is held open, and its id given: one done already is held open again, and
// a new one waits while a move of Atoll's own of that copy is planned (catalog.ErrMovePlanned).
func (w *Writer) journalWrite(ctx context.Context, path string, n *view.Node, who Caller, held bool) (int64, error) {
	open, err := catalog.OpenViewOps(ctx, w.db)
	if err != nil {
		return 0, fmt.Errorf("%w", err)
	}
	for i := len(open) - 1; i >= 0; i-- {
		v := &open[i]
		if v.Kind == catalog.ViewPart {
			// A part is in tmp/ of another disk, and nothing at the path.
			continue
		}
		touches := v.Path == path || v.Path2 == path || strings.HasPrefix(path, v.Path+"/") || (v.Path2 != "" && strings.HasPrefix(path, v.Path2+"/"))
		if !touches {
			continue
		}
		if (v.Kind == catalog.ViewWrite || v.Kind == catalog.ViewCreate) && v.Path == path && v.Disk == n.Disk {
			if !held {
				return 0, nil
			}
			if v.State == catalog.OpDone {
				if err := catalog.HoldViewOp(ctx, w.db, v.ID); err != nil {
					return 0, fmt.Errorf("%w", err)
				}
			}
			return v.ID, nil
		}
		break
	}
	v := &catalog.ViewOp{Kind: catalog.ViewWrite, Disk: n.Disk, Path: path, Folder: n.Kind == view.NodeDir}
	if held && !v.Folder {
		v.State = catalog.ViewHeld
	}
	if err := w.journal(ctx, v, who); err != nil {
		return 0, err
	}
	if v.State != catalog.ViewHeld {
		return 0, nil
	}
	return v.ID, nil
}

// Setattr changes what a user may change of a path: the owner, the mode, the size and the times. A file or a symbolic
// link changes on the disk it is presented from, a folder on every disk that holds it (specs.md §21.2), each once
// the caller may change it there.
func (w *Writer) Setattr(ctx context.Context, path string, n *view.Node, a *ops.UserAttrs, how Asked) (*view.Node, error) {
	if n.Kind == view.NodeDir && a.Size != nil {
		return nil, ErrIsFolder
	}
	if a.Size != nil {
		w.abandonAt(path, "truncated")
	}
	targets := []string{n.Disk}
	if n.Kind == view.NodeDir {
		w.mu.Lock()
		defer w.mu.Unlock()
		holders, err := w.folderHolders(ctx, path)
		if err != nil {
			return nil, err
		}
		targets = holders
	}
	if len(targets) == 0 {
		return nil, ErrNotFound
	}
	asker := asking(how.Caller)
	changes := map[string]*ops.UserAttrs{}
	for _, id := range targets {
		change, err := w.mayChange(asker, id, path, n, a, &how)
		switch {
		case errors.Is(err, ErrNoHandle):
		case err != nil:
			return nil, err
		default:
			changes[id] = change
		}
	}
	if _, err := w.journalWrite(ctx, path, &view.Node{Kind: n.Kind, Disk: targets[0]}, how.Caller, false); err != nil {
		return nil, err
	}
	var first *fsx.Stat
	for _, id := range targets {
		change := changes[id]
		if change == nil {
			change = a
		}
		err := w.handles.Use(id, func(d *ops.Disk) error {
			st, err := d.SetUserAttrs(path, change)
			if err == nil && first == nil {
				first = &st
			}
			return err //nolint:wrapcheck // ops says what failed
		})
		switch {
		case errors.Is(err, ops.ErrGone), errors.Is(err, ErrNoHandle):
		case err != nil:
			return nil, err
		}
	}
	if first == nil {
		return nil, ErrNotFound
	}
	target := ""
	if n.Kind == view.NodeSymlink {
		target = n.Target
	}
	return view.NodeOfStat(targets[0], first, target), nil
}

// Link gives the file a path presents a second name, on the disk it is presented from, as a hard link on that disk
// (specs.md §21.2): the folders above the new name that the disk lacks are made as for what is new. ErrIsFolder for a
// folder, as on any filesystem; ErrCrossDisk when it cannot be made there, as across two filesystems, so that a
// program copies instead: a symbolic link, a disk the view may not write to now, a disk whose driver moves the file's
// modification time as it links it, a name that disk refuses, collides with one it holds, or puts past its branch's
// limit, or a driver that makes no such name. Each name is then a file of
// its own, which the passes replicate as any other, never as a link (specs.md §14, §15.1).
func (w *Writer) Link(ctx context.Context, from, to string, who Caller) (*view.Node, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.tree.Lookup(ctx, from)
	switch {
	case err != nil:
		return nil, fmt.Errorf("%w", err)
	case n == nil:
		return nil, ErrNotFound
	case n.Kind == view.NodeDir:
		return nil, ErrIsFolder
	case n.Kind != view.NodeFile:
		return nil, ErrCrossDisk
	}
	if err := w.free(ctx, to); err != nil {
		return nil, err
	}
	d := w.disk(n.Disk)
	if d == nil || !w.handles.Has(n.Disk) || w.linkMovesTimes(d) {
		return nil, ErrCrossDisk
	}
	refused, err := w.refusedOn(ctx, d, to, "")
	if err != nil {
		return nil, err
	}
	if refused {
		return nil, ErrCrossDisk
	}
	var made fsx.Stat
	err = w.handles.Use(n.Disk, func(h *ops.Disk) error {
		st, found, err := h.StatRoot(from)
		switch {
		case err != nil:
			return err //nolint:wrapcheck // ops says what failed
		case !found:
			return ErrNotFound
		case st.Kind != fsx.KindFile:
			return ErrCrossDisk
		}
		if err := w.mayLink(ctx, asking(who), h, from, n, &st, to); err != nil {
			return err
		}
		v := &catalog.ViewOp{Kind: catalog.ViewLink, Disk: n.Disk, Path: from, Path2: to, FileKey: fmt.Sprintf("%d", st.Ino)}
		if err := w.journal(ctx, v, who); err != nil {
			return err
		}
		made, err = h.LinkUser(from, to, v.FileKey, w.like(ctx))
		return w.finish(ctx, v, err)
	})
	switch {
	case errors.Is(err, ops.ErrNameRefused), errors.Is(err, ops.ErrNoLink), errors.Is(err, ErrNoHandle), errors.Is(err, ops.ErrPathTaken):
		// What the disk holds at the new name, as a stale copy the view does not present, takes it there too.
		return nil, fmt.Errorf("%w: %w", ErrCrossDisk, err)
	case err != nil:
		return nil, err
	}
	return view.NodeOfStat(n.Disk, &made, ""), nil
}

// ErrXattrOnLink says that a symbolic link takes no extended attribute of the user. namespace, as Linux answers.
var ErrXattrOnLink = errors.New("a symbolic link takes no user. attribute")

// ErrXattrOfRoot says that the view keeps no extended attribute of its root, which stands for every disk's.
var ErrXattrOfRoot = errors.New("the view keeps no attribute of its root")

// SetXattr sets an extended attribute of the user. namespace on what a path presents, on a user's behalf, with the flags
// of setxattr(2) (specs.md §21.2): a file's on the copy it is presented from, its original, alone, a folder's on every
// disk that holds it. The pass that takes it in gives the file's other copies the same set. What the disk answers of
// the attribute, as EEXIST, ENODATA, ENOTSUP or E2BIG, passes as it is.
func (w *Writer) SetXattr(ctx context.Context, path string, n *view.Node, attr string, value []byte, flags int, who Caller) error {
	return w.changeXattr(ctx, path, n, who, func(d *ops.Disk) error { return d.SetUserXattr(path, attr, value, flags) })
}

// RemoveXattr removes an extended attribute of the user. namespace from what a path presents, as SetXattr sets one.
func (w *Writer) RemoveXattr(ctx context.Context, path string, n *view.Node, attr string, who Caller) error {
	return w.changeXattr(ctx, path, n, who, func(d *ops.Disk) error { return d.RemoveUserXattr(path, attr) })
}

func (w *Writer) changeXattr(ctx context.Context, path string, n *view.Node, who Caller, change func(*ops.Disk) error) error {
	switch {
	case path == "":
		return ErrXattrOfRoot
	case n.Kind == view.NodeSymlink:
		return ErrXattrOnLink
	}
	targets := []string{n.Disk}
	if n.Kind == view.NodeDir {
		w.mu.Lock()
		defer w.mu.Unlock()
		holders, err := w.folderHolders(ctx, path)
		if err != nil {
			return err
		}
		targets = holders
	}
	if len(targets) == 0 {
		return ErrNotFound
	}
	asker := asking(who)
	for _, id := range targets {
		if err := w.mayOn(asker, id, path, n, view.AccessXattr); err != nil && !errors.Is(err, ErrNoHandle) {
			return err
		}
	}
	v := &catalog.ViewOp{Kind: catalog.ViewXattr, Disk: targets[0], Path: path, Folder: n.Kind == view.NodeDir}
	if n.Kind != view.NodeDir {
		// The file is found by its inode on that disk when a pass gives its other copies its attributes, wherever a
		// rename took it since.
		err := w.handles.Use(n.Disk, func(d *ops.Disk) error {
			st, found, err := d.StatRoot(path)
			switch {
			case err != nil:
				return err //nolint:wrapcheck // ops says what failed
			case !found:
				return ErrNotFound
			}
			v.FileKey = fmt.Sprintf("%d", st.Ino)
			return nil
		})
		if err != nil {
			return err
		}
	}
	if err := w.journal(ctx, v, who); err != nil {
		return err
	}
	var first error
	for i, id := range targets {
		err := w.handles.Use(id, change)
		switch {
		case i == 0:
			first = err
		case err == nil, errors.Is(err, ops.ErrGone), errors.Is(err, ErrNoHandle):
		default:
			// A folder's attributes are no file's: no pass takes them to a disk that missed them (specs.md §21.2).
			w.log.Warn("a folder keeps an extended attribute on a disk", "disk", id, "path", path, "error", err)
		}
	}
	return w.finish(ctx, v, first)
}

// linkMovesTimes says whether a hard link made on a disk would move its file's modification time, as the kernel's ntfs
// driver of Linux 7.1 does when mounted with the type ntfs (specs.md §22): the view refuses such a link with EXDEV, so
// that the program copies, as the next pass would otherwise give every other copy that new time. A disk whose mount
// cannot be read now is refused too. Merge note: beta's disks.LinkMovesTimes(fstype), or (*ops.Disk).LinkMovesTimes(),
// says the same, and takes this one's place.
func (w *Writer) linkMovesTimes(d *config.Disk) bool {
	seen := w.machine.Mount(d.Mount)
	return seen.Entry == nil || seen.Entry.FSType == "ntfs"
}

// Unlink deletes what a path presents, but a folder: a file goes to the trash from the disk it is presented from,
// `deleted`, and the pass sends its other copies (specs.md §15.5, §21.2); a symbolic link is removed.
func (w *Writer) Unlink(ctx context.Context, path string, who Caller) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.abandonAt(path, "deleted")
	n, err := w.tree.Lookup(ctx, path)
	switch {
	case err != nil:
		return fmt.Errorf("%w", err)
	case n == nil:
		return ErrNotFound
	case n.Kind == view.NodeDir:
		return ErrIsFolder
	}
	if err := w.mayDelete(ctx, asking(who), n.Disk, path, n); err != nil {
		return err
	}
	if n.Kind == view.NodeSymlink {
		v := &catalog.ViewOp{Kind: catalog.ViewUnlink, Disk: n.Disk, Path: path}
		if err := w.journal(ctx, v, who); err != nil {
			return err
		}
		err := w.handles.Use(n.Disk, func(d *ops.Disk) error { return d.RemoveUser(path, false) }) //nolint:wrapcheck // ops says what failed
		return w.finish(ctx, v, err)
	}
	v := &catalog.ViewOp{Kind: catalog.ViewUnlink, Disk: n.Disk, Path: path, Asker: who.asker()}
	return w.toTrash(ctx, v, path, "deleted")
}

// toTrash journals an operation of the view that sends the copy a path presents to the trash, then sends it, and gives
// its folder the time now, as a deletion on that disk would (specs.md §17.1, §21.2).
func (w *Writer) toTrash(ctx context.Context, v *catalog.ViewOp, path, reason string) error {
	return w.handles.Use(v.Disk, func(d *ops.Disk) error {
		st, found, err := d.StatRoot(path)
		switch {
		case err != nil:
			return err //nolint:wrapcheck // ops says what failed
		case !found:
			return ErrNotFound
		case st.Kind != fsx.KindFile:
			return ErrIsFolder
		}
		row, err := knownOf(path, &st)
		if err != nil {
			return err
		}
		now := w.now()
		v.FileKey, v.AtNS = row.FileKey.String, now
		op, err := catalog.BeginViewTrash(ctx, w.db, v, &catalog.TrashStart{
			Disk: v.Disk, Path: path, FileKey: row.FileKey.String, Size: row.Size, Reason: reason, NowNS: now, ExpiresNS: expires(now, w.retention),
		})
		if err != nil {
			return fmt.Errorf("%w", err)
		}
		if _, err := d.ToTrash(op, row); err != nil {
			if moved, err := w.afterFailedTrash(ctx, d, v, op, row, err); !moved {
				return err
			}
		}
		if err := d.TouchParent(path, fsx.TimeOf(now)); err != nil {
			w.log.Warn("a folder keeps its time", "path", path, "error", err)
		}
		return w.finish(ctx, v, nil)
	})
}

// afterFailedTrash resolves a move of the view to the trash that failed, as the daemon's session does
// (trash.Session.afterFailedMove): a disk that cannot be relied on leaves it planned, for the view's reprise, and one
// on another mount moved nothing; anything else is resolved at once by the table of §17.2, against the look right
// before the move (specs.md §17.1, §21.2). It says whether the copy reached the trash after all, the operation then to
// finish; otherwise what the program gets, the operation undone or left planned.
func (w *Writer) afterFailedTrash(ctx context.Context, d *ops.Disk, v *catalog.ViewOp, op *ops.Op, row *catalog.KnownCopy, moveErr error) (bool, error) {
	if errors.Is(moveErr, ops.ErrDiskLost) || errors.Is(moveErr, ops.ErrOtherMount) {
		return false, w.finish(ctx, v, moveErr)
	}
	done, warn, err := d.ResolveViewTrash(ctx, w.db, op, row)
	switch {
	case errors.Is(err, ops.ErrDiskLost):
		return false, w.finish(ctx, v, err)
	case err != nil:
		w.log.Warn("an operation of the view stays planned", append([]any{"disk", v.Disk, "path", v.Path, "error", err}, askedBy(v)...)...)
		return false, errors.Join(moveErr, fmt.Errorf("%w", err))
	case done:
		return true, nil
	}
	if ferr := catalog.FinishViewOp(ctx, w.db, v, catalog.OpUndone, warn, w.now()); ferr != nil {
		return false, errors.Join(moveErr, fmt.Errorf("%w", ferr))
	}
	return false, moveErr
}

// knownOf is what a look found of a copy, as the row an operation compares it with right before it moves it.
func knownOf(path string, st *fsx.Stat) (*catalog.KnownCopy, error) {
	found, err := watch.FileFrom(path, st)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	return &catalog.KnownCopy{
		Path: path, FilePath: path,
		Size:    sql.NullInt64{Int64: found.Size, Valid: true},
		MtimeNS: sql.NullInt64{Int64: found.MtimeNS, Valid: true},
		CtimeNS: sql.NullInt64{Int64: found.CtimeNS, Valid: true},
		BtimeNS: nullOf(found.BtimeNS),
		FileKey: sql.NullString{String: found.FileKey, Valid: true},
	}, nil
}

func nullOf(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

// expires is when an entry sent to the trash at now leaves it, or the largest time §9 holds when that is past it.
func expires(now int64, retention time.Duration) int64 {
	if int64(retention) > math.MaxInt64-now {
		return math.MaxInt64
	}
	return now + int64(retention)
}

// Rmdir removes a folder that presents nothing below it, from every disk where it is empty; a pass removes it from
// the others once nothing is left there (specs.md §21.2).
func (w *Writer) Rmdir(ctx context.Context, path string, who Caller) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.tree.Lookup(ctx, path)
	switch {
	case err != nil:
		return fmt.Errorf("%w", err)
	case n == nil:
		return ErrNotFound
	case n.Kind != view.NodeDir:
		return ErrNotFolder
	}
	children, err := w.tree.List(ctx, path)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	if len(children) > 0 {
		return ErrNotEmpty
	}
	holders, err := w.folderHolders(ctx, path)
	if err != nil {
		return err
	}
	if len(holders) == 0 {
		return ErrNotFound
	}
	asker := asking(who)
	for _, id := range holders {
		if err := w.mayDelete(ctx, asker, id, path, n); err != nil && !errors.Is(err, ErrNoHandle) {
			return err
		}
	}
	v := &catalog.ViewOp{Kind: catalog.ViewRmdir, Disk: holders[0], Path: path, Folder: true}
	if err := w.journal(ctx, v, who); err != nil {
		return err
	}
	for _, id := range holders {
		err := w.handles.Use(id, func(d *ops.Disk) error { return d.RemoveUser(path, true) }) //nolint:wrapcheck // ops says what failed
		switch {
		case err == nil, errors.Is(err, ops.ErrGone), errors.Is(err, ops.ErrNotEmpty):
		case errors.Is(err, ops.ErrDiskLost):
			return w.finish(ctx, v, err)
		default:
			w.log.Warn("a folder removed through the view stays on a disk, for a pass", "disk", id, "path", path, "error", err)
		}
	}
	return w.finish(ctx, v, nil)
}

// Rename moves what a path presents to another (specs.md §21.2): a file on the disk it is presented from, a folder on
// every disk that holds it. It is refused when a disk that holds a copy refuses the new name. Onto a name presented
// from the same disk, the copy there goes to the trash first, `overwritten`; from another disk, ErrCrossDisk.
func (w *Writer) Rename(ctx context.Context, from, to string, noReplace bool, who Caller) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.tree.Lookup(ctx, from)
	switch {
	case err != nil:
		return fmt.Errorf("%w", err)
	case n == nil:
		return ErrNotFound
	case from == to:
		return nil
	case n.Kind == view.NodeDir && strings.HasPrefix(to, from+"/"):
		return ErrInvalid
	}
	w.abandonAt(from, "renamed")
	if dir, _ := split(to); dir != "" {
		if err := w.folder(ctx, dir); err != nil {
			return err
		}
	}
	t, err := w.tree.Lookup(ctx, to)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	replace := t != nil
	switch {
	case replace && noReplace, replace && n.Kind == view.NodeDir:
		return ErrExists
	case replace && t.Kind == view.NodeDir:
		return ErrIsFolder
	case replace && t.Disk != n.Disk:
		return ErrCrossDisk
	}
	var holders []string
	if n.Kind == view.NodeDir {
		holders, err = w.folderHolders(ctx, from)
	} else {
		holders, err = w.fileHolders(ctx, from, n)
	}
	if err != nil {
		return err
	}
	if err := w.refusedBy(ctx, holders, from, to, n.Kind == view.NodeDir); err != nil {
		return err
	}
	acting := []string{n.Disk}
	if n.Kind == view.NodeDir {
		acting = holders
	}
	if err := w.mayRename(ctx, asking(who), acting, from, to, n, t); err != nil {
		return err
	}
	if n.Kind == view.NodeDir {
		return w.renameFolder(ctx, holders, from, to, who)
	}
	return w.renameFile(ctx, n.Disk, from, to, replace, who)
}

// refusedBy refuses a rename that a disk holding a copy at or below the old name would refuse: the new name, and below
// a folder the longest path it holds there, which must still reach its branch.
func (w *Writer) refusedBy(ctx context.Context, holders []string, from, to string, isFolder bool) error {
	for _, id := range holders {
		d := w.disk(id)
		if d == nil {
			continue
		}
		refused, err := w.refusedOn(ctx, d, to, from)
		if err != nil {
			return err
		}
		if !refused && isFolder {
			longest, err := catalog.LongestBelow(ctx, w.db, id, from)
			if err != nil {
				return fmt.Errorf("%w", err)
			}
			refused = longest > 0 && len(d.Mount)+1+len(d.Root)+1+longest-len(from)+len(to) > names.MaxBranchPathBytes
		}
		if refused {
			return ErrRefused
		}
	}
	return nil
}

func (w *Writer) renameFile(ctx context.Context, disk, from, to string, replace bool, who Caller) error {
	return w.handles.Use(disk, func(d *ops.Disk) error {
		st, found, err := d.StatRoot(from)
		switch {
		case err != nil:
			return err //nolint:wrapcheck // ops says what failed
		case !found:
			return ErrNotFound
		}
		key := fmt.Sprintf("%d", st.Ino)
		if !replace {
			v := &catalog.ViewOp{Kind: catalog.ViewRename, Disk: disk, Path: from, Path2: to, FileKey: key}
			if err := w.journal(ctx, v, who); err != nil {
				return err
			}
			return w.finish(ctx, v, d.RenameUser(from, to, false, key, w.like(ctx)))
		}
		return w.replace(ctx, d, from, to, key, who)
	})
}

// replace sends the copy presented at to to the trash, `overwritten`, then renames from onto its name, as one
// operation of the view: a rename that fails once the copy is in the trash leaves it planned, for the view's reprise.
func (w *Writer) replace(ctx context.Context, d *ops.Disk, from, to, key string, who Caller) error {
	st, found, err := d.StatRoot(to)
	switch {
	case err != nil:
		return err //nolint:wrapcheck // ops says what failed
	case !found:
		return ErrNotFound
	case st.Kind != fsx.KindFile:
		return ErrIsFolder
	}
	row, err := knownOf(to, &st)
	if err != nil {
		return err
	}
	now := w.now()
	v := &catalog.ViewOp{Kind: catalog.ViewReplace, Disk: d.ID, Path: from, Path2: to, FileKey: key, AtNS: now, Asker: who.asker()}
	op, err := catalog.BeginViewTrash(ctx, w.db, v, &catalog.TrashStart{
		Disk: d.ID, Path: to, FileKey: row.FileKey.String, Size: row.Size, Reason: "overwritten", NowNS: now, ExpiresNS: expires(now, w.retention),
	})
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	if _, err := d.ToTrash(op, row); err != nil {
		if moved, err := w.afterFailedTrash(ctx, d, v, op, row, err); !moved {
			return err
		}
	}
	err = d.RenameUser(from, to, false, key, w.like(ctx))
	if err != nil {
		w.log.Warn("a rename onto a name stays planned: what was there is in the trash", "from", from, "to", to, "error", err)
		return fmt.Errorf("%w: %w", ops.ErrDiskLost, err)
	}
	return w.finish(ctx, v, nil)
}

func (w *Writer) renameFolder(ctx context.Context, holders []string, from, to string, who Caller) error {
	v := &catalog.ViewOp{Kind: catalog.ViewRename, Disk: holders[0], Path: from, Path2: to, Folder: true}
	if err := w.journal(ctx, v, who); err != nil {
		return err
	}
	moved := 0
	var failed error
	for _, id := range holders {
		err := w.handles.Use(id, func(d *ops.Disk) error {
			st, found, err := d.StatRoot(from)
			switch {
			case err != nil:
				return err //nolint:wrapcheck // ops says what failed
			case !found || st.Kind != fsx.KindDir:
				return ops.ErrGone
			}
			return d.RenameUser(from, to, true, fmt.Sprintf("%d", st.Ino), w.like(ctx)) //nolint:wrapcheck // ops says what failed
		})
		switch {
		case err == nil:
			moved++
		case errors.Is(err, ops.ErrGone), errors.Is(err, ErrNoHandle):
		default:
			w.log.Warn("a folder renamed through the view stays at its old name on a disk", "disk", id, "from", from, "to", to, "error", err)
			failed = errors.Join(failed, err)
		}
	}
	if moved == 0 {
		return w.finish(ctx, v, cmp.Or(failed, ErrNotFound))
	}
	return w.finish(ctx, v, nil)
}

// fileHolders gives the disks that hold a copy of the file a path presents: those the catalog's rows say hold it, and
// the disk of what the view made or wrote there.
func (w *Writer) fileHolders(ctx context.Context, path string, n *view.Node) ([]string, error) {
	paths, _, err := w.resolved(ctx, path)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{n.Disk: true}
	if len(paths) > 0 {
		rows, err := catalog.ViewRowsOf(ctx, w.db, paths)
		if err != nil {
			return nil, fmt.Errorf("%w", err)
		}
		for _, p := range paths {
			if f := rows[p].File; f != nil {
				for _, c := range f.Copies {
					if c.State != string(solver.CopyPending) {
						set[c.Disk] = true
					}
				}
			}
		}
	}
	return sortedKeys(set), nil
}

// folderHolders gives the disks that hold a folder the view presents: those whose entry the catalog holds, and those
// the view made it or something below it on.
func (w *Writer) folderHolders(ctx context.Context, path string) ([]string, error) {
	paths, open, err := w.resolved(ctx, path)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	if len(paths) > 0 {
		rows, err := catalog.ViewRowsOf(ctx, w.db, paths)
		if err != nil {
			return nil, fmt.Errorf("%w", err)
		}
		for _, p := range paths {
			for _, e := range rows[p].Entries {
				if e.Kind == string(solver.EntryDir) {
					set[e.Disk] = true
				}
			}
		}
	}
	for k := range open {
		o := &open[k]
		target := o.Path
		if o.Kind == view.OpRename || o.Kind == view.OpReplace {
			target = o.Path2
		}
		if o.Kind == view.OpUnlink || o.Kind == view.OpRmdir || o.Kind == catalog.ViewPart {
			continue
		}
		for _, newer := range open[k+1:] {
			if newer.Kind == view.OpRename && newer.Folder && (target == newer.Path || strings.HasPrefix(target, newer.Path+"/")) {
				target = newer.Path2 + target[len(newer.Path):]
			}
		}
		if target == path || strings.HasPrefix(target, path+"/") {
			set[o.Disk] = true
		}
	}
	return sortedKeys(set), nil
}

// resolved gives the catalog paths whose rows hold what a path presents, with the view's open operations: those it
// resolves to, and for one the view made or wrote on a disk the path itself, whose rows hold the other copies still on
// their disks until a pass takes the change to them; none for one that presents nothing.
func (w *Writer) resolved(ctx context.Context, path string) ([]string, []view.Op, error) {
	rows, err := catalog.OpenViewOps(ctx, w.db)
	if err != nil {
		return nil, nil, fmt.Errorf("%w", err)
	}
	open := make([]view.Op, 0, len(rows))
	for i := range rows {
		open = append(open, view.OpOf(&rows[i]))
	}
	switch s := view.Resolve(open, path); s.Kind {
	case view.FromCatalog:
		return s.Paths, open, nil
	case view.FromDisk:
		return []string{path}, open, nil
	case view.Absent:
	}
	return nil, open, nil
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
