package writer

import (
	"context"
	"errors"
	"sync"
	"syscall"

	"github.com/peondusud/atoll/internal/fsx"
)

// File is a file the view opened for writing, and for a new file in immediate mode the copies written in parallel
// that follow what is written to it (specs.md §12.3). A new file the view made moves to another disk when a write runs
// out of space, with view.move_on_enospc (specs.md §21.2): whatever reaches its handle holds mu shared, and the move
// holds it alone, so that nothing reaches the handle it leaves once it moved, and nothing is written meanwhile.
type File struct {
	fsx.WritableFile
	Disk, Path string
	par        *parallel

	mu sync.RWMutex
	// w, ctx and who are the writer, the context and the caller of the Create that made it, for a move: nil w for a
	// file that never moves. ctx is the create's, which no interrupt of the kernel's cancels.
	w   *Writer
	ctx context.Context
	who Caller
	// movable says that a write that runs out of space may still move it, under mu; again, that it was opened again to
	// be written, under the writer's mu; moves counts its moves, under mu.
	movable bool
	again   bool
	moves   int
	// held is the row of view_ops this handle holds open, holder its writer, heldCtx the context of the open that holds
	// it, which no interrupt of the kernel's cancels: 0 and nil for none (specs.md §21.2, flaw 10).
	held    int64
	holder  *Writer
	heldCtx context.Context
}

// OnDisk gives the disk the file is on now.
func (f *File) OnDisk() string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.Disk
}

// noRoom says that a disk had no room for a write: no block left, or the quota of its owner reached.
func noRoom(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}

// WriteAt writes to the original, then gives the parts what it took. A write the original's disk has no room for moves
// a new file to another disk, where it is made: what that write answers is what the program gets (specs.md §21.2).
func (f *File) WriteAt(data []byte, off int64) (int, error) {
	f.mu.RLock()
	moves := f.moves
	n, err := f.WritableFile.WriteAt(data, off)
	if f.par != nil {
		if err != nil {
			f.par.abandon("a write to the original failed")
		} else {
			f.par.mirror(data[:n], off)
		}
	}
	tryMove := err != nil && noRoom(err) && f.w != nil && f.movable
	f.mu.RUnlock()
	if !tryMove {
		return n, err //nolint:wrapcheck // fsx says what failed
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.moves == moves {
		if !f.movable {
			return n, err //nolint:wrapcheck // fsx says what failed
		}
		if moveErr := f.w.move(f, len(data)); moveErr != nil {
			f.w.log.Info("a file whose write ran out of space stays on its disk", "disk", f.Disk, "path", f.Path, "reason", moveErr)
			return n, err //nolint:wrapcheck // fsx says what failed
		}
	}
	// Moved, by this write or by one at the same time: the write is made where the file is now.
	return f.WritableFile.WriteAt(data, off) //nolint:wrapcheck // fsx says what failed
}

// Write is no write the parts can follow, as it names no place, and no write a move can make again on another disk,
// whose handle would not be at the same place: the file never moves from then on.
func (f *File) Write(data []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.movable = false
	if f.par != nil {
		f.par.abandon("written without a place")
	}
	return f.WritableFile.Write(data) //nolint:wrapcheck // fsx says what failed
}

// Truncate drops the parts: a file truncated while it is written is no new file written once.
func (f *File) Truncate(size int64) error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.par != nil {
		f.par.abandon("truncated")
	}
	return f.WritableFile.Truncate(size) //nolint:wrapcheck // fsx says what failed
}

// Close closes the original, then ends the parts: the program closed the file. A move under way ends first.
func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	size := int64(-1)
	if f.par != nil {
		if st, err := f.WritableFile.Info(); err == nil {
			size = st.Size
		}
	}
	err := f.WritableFile.Close()
	if f.par != nil {
		if err != nil {
			f.par.abandon("the original did not close cleanly")
		} else {
			f.par.release(size)
		}
	}
	f.movable = false
	if f.w != nil {
		f.w.closed(f)
	}
	if f.holder != nil {
		f.holder.closeHandle(f.heldCtx, f.held)
		f.holder = nil
	}
	return err //nolint:wrapcheck // fsx says what failed
}

// closed forgets a file Create made, once closed.
func (w *Writer) closed(f *File) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writing[f.Path] == f {
		delete(w.writing, f.Path)
	}
}

// What else reaches the handle, held shared.

func (f *File) ReadAt(data []byte, off int64) (int, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.WritableFile.ReadAt(data, off) //nolint:wrapcheck // fsx says what failed
}

func (f *File) Info() (fsx.Stat, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.WritableFile.Info() //nolint:wrapcheck // fsx says what failed
}

func (f *File) Sync() error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.WritableFile.Sync() //nolint:wrapcheck // fsx says what failed
}

func (f *File) SetTimes(atime, mtime fsx.Time) error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.WritableFile.SetTimes(atime, mtime) //nolint:wrapcheck // fsx says what failed
}

func (f *File) SetMode(mode uint32) error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.WritableFile.SetMode(mode) //nolint:wrapcheck // fsx says what failed
}

func (f *File) SetOwner(uid, gid uint32) error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.WritableFile.SetOwner(uid, gid) //nolint:wrapcheck // fsx says what failed
}

func (f *File) DropCache() error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.WritableFile.DropCache() //nolint:wrapcheck // fsx says what failed
}

func (f *File) Xattrs(namespace string) (map[string][]byte, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.WritableFile.Xattrs(namespace) //nolint:wrapcheck // fsx says what failed
}

func (f *File) SetXattr(attr string, value []byte, flags int) error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.WritableFile.SetXattr(attr, value, flags) //nolint:wrapcheck // fsx says what failed
}

func (f *File) RemoveXattr(attr string) error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.WritableFile.RemoveXattr(attr) //nolint:wrapcheck // fsx says what failed
}
