// Package readonly runs atoll mount: Atoll's own view, read only, at a point beside mergerfs, for the media servers,
// until it is stopped (specs.md §21.2). It reads the catalog and the disks, and writes to neither.
package readonly

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sys/unix"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/view"
	"github.com/peondusud/atoll/internal/view/fusefs"
	"github.com/peondusud/atoll/internal/view/writer"
)

// CheckEvery is the time between two checks of the disks' mounts (specs.md §21.2).
const CheckEvery = 30 * time.Second

// Options is what Run needs.
type Options struct {
	Config  *config.Config
	DB      *sql.DB
	Machine disks.Machine
	// Point is where the view is mounted: a folder that exists, apart from view.mount and the disks.
	Point string
	// Every is the time between two checks of the disks' mounts; CheckEvery when zero.
	Every time.Duration
	// Ready, when set, is called once the view is mounted.
	Ready func()
	Log   *slog.Logger
	// Writes says that the view writes on a user's behalf, at view.mount in place of mergerfs, through a handle on each
	// disk its check finds online (specs.md §21.2).
	Writes bool
	// mount serves the view at the point: fusefs.Mount when nil, as in the delivered binary; a unit test's otherwise,
	// where no FUSE device can be opened.
	mount func(tree *view.Tree, point string, w *writer.Writer) (served, error)
}

// served is a view being served, as fusefs.Mount gives it.
type served interface {
	Done() <-chan struct{}
	Unmount() error
}

// mountFUSE serves the view through the kernel's FUSE device.
func mountFUSE(tree *view.Tree, point string, w *writer.Writer) (served, error) {
	m, err := fusefs.Mount(tree, point, w)
	if err != nil {
		return nil, err //nolint:wrapcheck // Run says where
	}
	return m, nil
}

// ErrUnmounted says that the view was unmounted by something else than Run.
var ErrUnmounted = errors.New("the view was unmounted by another process")

// staleTries bounds the mounts of the view, one on another, that Run detaches at its start.
const staleTries = 8

// Run mounts the view at the point, then serves it until ctx is done, and unmounts it then. It checks the disks'
// mounts at its start and every Every: the root of a disk its check finds online or readonly is open for reading,
// and a read of a copy on any other fails with EIO. It fails when the view is unmounted by another process.
func Run(ctx context.Context, o Options) error {
	if err := clearPoint(o.Point, o.Log); err != nil {
		return err
	}
	info, err := fsx.StatPath(o.Point)
	switch {
	case fsx.IsAbsent(err):
		return fmt.Errorf("mount point %s: it does not exist, and Atoll makes no folder outside its disks", o.Point)
	case err != nil:
		return fmt.Errorf("mount point: %w", err)
	case !info.IsDir:
		return fmt.Errorf("mount point %s: not a folder", o.Point)
	}
	roots := view.NewDiskRoots()
	defer roots.Close()
	c := &checker{o: o, roots: roots, readable: map[string]bool{}}
	if o.Writes {
		c.handles = writer.NewHandles()
		defer c.handles.Close()
	}
	c.check(ctx)
	tree := view.NewTree(o.DB, roots, view.Owner{UID: uint32(unix.Geteuid()), GID: uint32(unix.Getegid())}, o.Machine.Now())
	// Which copy a file is read from (specs.md §7): read.policy and read.classes, as the configuration says at the start.
	prefer, classes := o.Config.Read()
	tree.WithRead(view.ReadPolicy{PreferSpinning: prefer, Classes: classes})
	// The cache disks are part of Atoll's own view, with view.engine = "atoll" (specs.md §19); with mergerfs, they are
	// not, as its line leaves them out, and the view beside it presents what it does.
	settings, err := o.Config.Cache()
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	tree.WithCache(settings.InView)
	var w *writer.Writer
	if o.Writes {
		if w, err = writer.New(o.DB, o.Config, o.Machine, tree, c.handles, o.Log); err != nil {
			return fmt.Errorf("%w", err)
		}
		// What a stop left planned is resolved before anything reads through the view (specs.md §21.2).
		if err := w.Reprise(ctx); err != nil {
			return fmt.Errorf("%w", err)
		}
	}
	mount := o.mount
	if mount == nil {
		mount = mountFUSE
	}
	m, err := mount(tree, o.Point, w)
	if err != nil {
		return fmt.Errorf("mount at %s: %w", o.Point, err)
	}
	if o.Writes {
		o.Log.Info("the view is mounted, and writes", "point", o.Point)
	} else {
		o.Log.Info("the view is mounted, read only", "point", o.Point)
	}
	if o.Ready != nil {
		o.Ready()
	}
	every := o.Every
	if every <= 0 {
		every = CheckEvery
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := m.Unmount(); err != nil {
				return fmt.Errorf("unmount %s: %w", o.Point, err)
			}
			o.Log.Info("the view is unmounted", "point", o.Point)
			return nil
		case <-m.Done():
			return fmt.Errorf("%s: %w", o.Point, ErrUnmounted)
		case <-ticker.C:
			c.check(ctx)
			if w != nil {
				// What a disk lost left planned is resolved once it is there again, not at the next start alone.
				if err := w.Recheck(ctx); err != nil {
					o.Log.Warn("what the view left planned is not resolved now", "error", err)
				}
			}
		}
	}
}

// clearPoint detaches what a view whose process stopped short left mounted at the point, which the kernel answers
// ENOTCONN or ECONNREFUSED for until it is unmounted (fsx.IsNotConnected). Anything else mounted at the point, a view
// a process still serves included, is refused.
func clearPoint(point string, log *slog.Logger) error {
	for range staleTries {
		data, err := fsx.ReadMountTable()
		if err != nil {
			return fmt.Errorf("mount point: %w", err)
		}
		var top *disks.MountEntry
		table := disks.ParseMountInfo(data)
		for i := range table.Entries {
			if table.Entries[i].MountPoint == point {
				top = &table.Entries[i]
			}
		}
		if top == nil {
			return nil
		}
		if top.FSType != fusefs.Type {
			return fmt.Errorf("mount point %s: %s is mounted there", point, top.FSType)
		}
		if !gone(point) {
			return fmt.Errorf("mount point %s: another process serves the view there", point)
		}
		log.Warn("a view whose process stopped short is detached", "point", point)
		if err := fsx.DetachMount(point); err != nil {
			return fmt.Errorf("mount point %s: the view left there: %w", point, err)
		}
	}
	return fmt.Errorf("mount point %s: still mounted after %d views detached", point, staleTries)
}

// gone says whether no process serves the FUSE mount at a point any more. Opening its root asks the process, where a
// stat may not: the kernel keeps the root's attributes for a second (measured).
func gone(point string) bool {
	dir, err := fsx.OpenDir(point)
	if err == nil {
		_ = dir.Close()
		return false
	}
	return fsx.IsNotConnected(err)
}

// checker keeps the root of each disk as its last mount check found it, and for a view that writes a handle on the top
// of each disk it found online.
type checker struct {
	o       Options
	roots   *view.DiskRoots
	handles *writer.Handles
	// readable says which disks the last check gave a root, to log a change only.
	readable map[string]bool
	// seen is which disks' own marker the catalog recorded seen, as the check read it last (specs.md §10).
	seen disks.Seen
}

// check runs the mount check of every disk, with the question of its marker, which the machine reads once per mount
// (specs.md §10). Each disk found online or readonly gets its root opened again, so that a disk mounted again, after a
// USB reset, is read from its new mount; any other has none: a disk whose own marker went is missing, and nothing it
// holds is presented.
func (c *checker) check(ctx context.Context) {
	seen, err := catalog.MarkersSeen(ctx, c.o.DB)
	if err != nil {
		c.o.Log.Warn("which disks' own marker was seen is not read now: the check takes what it read last", "error", err)
	} else {
		c.seen = disks.Seen(seen)
	}
	for i := range c.o.Config.Disks {
		d := &c.o.Config.Disks[i]
		v := disks.CheckKnown(c.o.Machine, d, c.o.Config.PoolID, c.seen.Of(d))
		root, why := c.rootOf(d, v)
		if c.handles != nil {
			c.handleOf(d, v, root != nil)
		}
		if root == nil {
			c.roots.Drop(d.ID)
			if was, seen := c.readable[d.ID]; was || !seen {
				c.o.Log.Warn("a disk cannot be read: what the view presents there fails with EIO", "disk", d.ID, "reason", why)
			}
			c.readable[d.ID] = false
			continue
		}
		c.roots.Set(d.ID, root)
		if was, seen := c.readable[d.ID]; seen && !was {
			c.o.Log.Info("a disk can be read again", "disk", d.ID)
		}
		c.readable[d.ID] = true
	}
}

// handleOf opens a handle on the top of a disk its check found online, and confirms it there (invariant 3); a disk the
// view cannot write to has none, readonly by another's marker included, and what the view writes there fails with
// EROFS.
func (c *checker) handleOf(d *config.Disk, v disks.Verdict, readable bool) {
	if !readable || d.State == config.DiskFailed || v.State != disks.Online {
		c.handles.Drop(d.ID)
		return
	}
	h, err := ops.Open(c.o.Machine, d, 0)
	if err == nil {
		if err = h.Verify(); err != nil {
			h.Close()
		}
	}
	if err != nil {
		c.o.Log.Warn("the view cannot write to a disk", "disk", d.ID, "error", err)
		c.handles.Drop(d.ID)
		return
	}
	c.handles.Set(d.ID, h)
}

// rootOf opens the root of a disk its check finds online or readonly, or says why it does not.
func (c *checker) rootOf(d *config.Disk, v disks.Verdict) (fsx.Dir, string) {
	if d.State == config.DiskFailed {
		return nil, "failed"
	}
	if v.State != disks.Online && v.State != disks.Readonly {
		return nil, v.Message
	}
	root, err := c.o.Machine.OpenRoot(d.Mount, d.Root)
	if err != nil {
		return nil, err.Error()
	}
	return root, ""
}
