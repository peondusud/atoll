package watch

import (
	"context"
	"errors"
	"fmt"

	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/solver"
)

// FoundFile is a regular file a walk found.
type FoundFile struct {
	Path    string
	Size    int64
	MtimeNS int64
	CtimeNS int64
	BtimeNS *int64 // nil when the creation time is absent (specs.md §7)
	FileKey string
	Nlink   int64
}

// FoundEntry is a folder, symlink or other file a walk found. Kind is never solver.EntryIgnored
// for a regular file: whether a rule ignores one is decided later.
type FoundEntry struct {
	Path    string
	Kind    solver.EntryKind
	Mode    int64
	MtimeNS int64
	Target  *string
}

// Walked is what one walk found (specs.md §15.10).
type Walked struct {
	Files       []FoundFile
	Entries     []FoundEntry
	Unreadable  []string
	OtherMounts []string
}

// ErrInterrupted says that a walk stopped because the disk did not answer: nothing can be
// concluded from what it found (invariant 4).
var ErrInterrupted = errors.New("the disk did not answer")

// Walk reads everything below a root, through handles, never following a symlink. A listed name
// that can no longer be reached is reported, and any other error stops the walk.
func Walk(ctx context.Context, root fsx.Dir) (*Walked, error) {
	top, err := root.Info()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInterrupted, err)
	}
	w := &Walked{}
	if err := walkFolder(ctx, root, "", top.MountID, w); err != nil {
		return nil, err
	}
	return w, nil
}

func walkFolder(ctx context.Context, dir fsx.Dir, prefix string, mountID uint64, w *Walked) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("walk: %w", err)
	}
	names, err := dir.Names()
	if err != nil {
		return fmt.Errorf("%w: %q: %w", ErrInterrupted, prefix, err)
	}
	for _, name := range names {
		path := prefix + name
		st, err := dir.Stat(name)
		switch {
		case fsx.IsUnreadableName(err):
			w.Unreadable = append(w.Unreadable, path)
			continue
		case err != nil:
			return fmt.Errorf("%w: %q: %w", ErrInterrupted, path, err)
		}
		mtime, err := solver.TimeNS(st.Mtime.Sec, st.Mtime.Nsec)
		if err != nil {
			return fmt.Errorf("%w: %q: %w", ErrInterrupted, path, err)
		}
		switch st.Kind {
		case fsx.KindFile:
			file, err := foundFile(path, &st, mtime)
			if err != nil {
				return fmt.Errorf("%w: %q: %w", ErrInterrupted, path, err)
			}
			w.Files = append(w.Files, file)
		case fsx.KindDir:
			w.Entries = append(w.Entries, FoundEntry{Path: path, Kind: solver.EntryDir, Mode: int64(st.Mode), MtimeNS: mtime})
			if st.MountID != mountID {
				w.OtherMounts = append(w.OtherMounts, path)
				continue
			}
			if err := walkBelow(ctx, dir, name, path, mountID, w); err != nil {
				return err
			}
		case fsx.KindSymlink:
			target, err := dir.ReadLink(name)
			switch {
			case fsx.IsUnreadableName(err):
				w.Unreadable = append(w.Unreadable, path)
				continue
			case err != nil:
				return fmt.Errorf("%w: %q: %w", ErrInterrupted, path, err)
			}
			w.Entries = append(w.Entries, FoundEntry{Path: path, Kind: solver.EntrySymlink, Mode: int64(st.Mode), MtimeNS: mtime, Target: &target})
		case fsx.KindOther:
			w.Entries = append(w.Entries, FoundEntry{Path: path, Kind: solver.EntryIgnored, Mode: int64(st.Mode), MtimeNS: mtime})
		}
	}
	return nil
}

func walkBelow(ctx context.Context, dir fsx.Dir, name, path string, mountID uint64, w *Walked) error {
	below, err := dir.OpenDir(name)
	switch {
	case fsx.IsUnreadableName(err):
		w.Unreadable = append(w.Unreadable, path)
		return nil
	case err != nil:
		return fmt.Errorf("%w: %q: %w", ErrInterrupted, path, err)
	}
	walkErr := walkFolder(ctx, below, path+"/", mountID, w)
	// A folder only read, whose close failing changes nothing that was read from it.
	_ = below.Close()
	return walkErr
}

func foundFile(path string, st *fsx.Stat, mtime int64) (FoundFile, error) {
	ctime, err := solver.TimeNS(st.Ctime.Sec, st.Ctime.Nsec)
	if err != nil {
		return FoundFile{}, fmt.Errorf("change time: %w", err)
	}
	file := FoundFile{
		Path:    path,
		Size:    st.Size,
		MtimeNS: mtime,
		CtimeNS: ctime,
		FileKey: fmt.Sprintf("%d", st.Ino),
		Nlink:   int64(st.Nlink),
	}
	if st.HasBirth {
		birth, ok, err := solver.CreationTimeNS(st.Birth.Sec, st.Birth.Nsec)
		if err != nil {
			return FoundFile{}, fmt.Errorf("creation time: %w", err)
		}
		if ok {
			file.BtimeNS = &birth
		}
	}
	return file, nil
}
