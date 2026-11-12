package ops

import (
	"errors"
	"fmt"

	"github.com/peondusud/atoll/internal/fsx"
)

// OpenTrashSource opens the file of a trash entry for reading, after a look that its name holds the entry's file, by
// inode (specs.md §17.4). ErrGone means it no longer does.
func (d *Disk) OpenTrashSource(trashPath, fileKey string) (fsx.File, fsx.Stat, error) {
	if err := d.Verify(); err != nil {
		return nil, fsx.Stat{}, err
	}
	path := d.trashPlace(trashPath)
	dir, err := d.open(path[:len(path)-1])
	switch {
	case fsx.IsAbsent(err), errors.Is(err, ErrOtherMount):
		return nil, fsx.Stat{}, fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return nil, fsx.Stat{}, classify(err)
	}
	defer dir.close()
	name := path[len(path)-1]
	st, err := holds(dir.Dir, name, byKey(fileKey))
	if err != nil {
		return nil, fsx.Stat{}, err
	}
	f, err := dir.OpenFile(name)
	switch {
	case fsx.IsUnreadableName(err):
		return nil, fsx.Stat{}, fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return nil, fsx.Stat{}, classify(err)
	}
	opened, err := f.Info()
	if err != nil {
		_ = f.Close()
		return nil, fsx.Stat{}, classify(err)
	}
	if keyOf(&opened) != keyOf(&st) {
		_ = f.Close()
		return nil, fsx.Stat{}, ErrGone
	}
	return f, st, nil
}

// TrashSourceStill says whether the file of a trash entry is still the one read, unchanged: its size, modification
// time, change time and inode, as a copy's source is looked at again (specs.md §14.2).
func (d *Disk) TrashSourceStill(trashPath string, read *fsx.Stat) (bool, error) {
	st, there, err := d.stat(d.trashPlace(trashPath))
	switch {
	case err != nil:
		return false, err
	case !there || st.Kind != fsx.KindFile:
		return false, nil
	}
	return st.Ino == read.Ino && st.Size == read.Size && st.Mtime == read.Mtime && st.Ctime == read.Ctime, nil
}
