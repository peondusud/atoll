package watch

import (
	"context"
	"errors"
	"fmt"
	"strconv"

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
	Mode    int64
	// UID and GID are its owner, 32 bits each, as a walk holds one of these for every file of a disk.
	UID, GID uint32
	// What comparing a walk with the rows says of it: the next file with its inode, how many rows moved to
	// it, and that one did. They are here, and not in maps, as a rename of a whole pool has one of each for
	// every file.
	sameInode *FoundFile
	claims    int
	isTarget  bool
	// rowless says that no row holds its name, straddled that it was renamed while the walk went, and home
	// that the row at its name is this file.
	rowless   bool
	straddled bool
	home      bool
}

// FoundEntry is a folder, symlink or other file a walk found. Kind is never solver.EntryIgnored
// for a regular file: whether a rule ignores one is decided later. A folder has its identity
// (specs.md §15.12).
type FoundEntry struct {
	Path    string
	Kind    solver.EntryKind
	Mode    int64
	MtimeNS int64
	Target  *string
	FileKey *string
	BtimeNS *int64
}

// Walked is what one walk found (specs.md §15.10).
type Walked struct {
	Files       []FoundFile
	Entries     []FoundEntry
	Unreadable  []string
	OtherMounts []string
	// Refused holds the names of Unreadable that refused the account that asked.
	Refused []string
}

// unreachable says whether an answer about a listed name says only that the walk cannot reach it by that name: gone
// or replaced since it was listed, a name the driver cannot read, or one that refuses the account that asks. None
// says anything about the disk (specs.md §15.10).
func unreachable(err error) bool {
	return fsx.IsUnreadableName(err) || fsx.IsAccessRefused(err)
}

// skip reports a name the walk could not reach, and tells one that refused the account apart: that one lasts.
func (w *Walked) skip(path string, err error) {
	w.Unreadable = append(w.Unreadable, path)
	if fsx.IsAccessRefused(err) {
		w.Refused = append(w.Refused, path)
	}
}

// ErrInterrupted says that a walk stopped because the disk did not answer: nothing can be
// concluded from what it found (invariant 4).
var ErrInterrupted = errors.New("the disk did not answer")

// errFolderGone says that a folder went between its opening and its listing, or refused to be listed: walkBelow
// reports it. It wraps what the listing answered.
var errFolderGone = errors.New("the folder cannot be listed")

// Walk reads everything below a root, through handles, never following a symlink. A listed name
// that can no longer be reached is reported, and any other error stops the walk.
func Walk(ctx context.Context, root fsx.Dir) (*Walked, error) {
	top, err := root.Info()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInterrupted, err)
	}
	w := &Walked{}
	if err := walkFolder(ctx, root, "", &top, w); err != nil {
		return nil, err
	}
	return w, nil
}

// walkFolder walks a folder of the root top. A folder of another mount, or of another device under the same mount,
// as a nested btrfs subvolume is, is listed and not walked (specs.md §15.10).
func walkFolder(ctx context.Context, dir fsx.Dir, prefix string, top *fsx.Stat, w *Walked) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("walk: %w", err)
	}
	names, err := dir.Names()
	switch {
	case prefix != "" && unreachable(err):
		// Removed once it was opened, or refusing to be listed: it can no longer be reached by its name, which says
		// nothing of the disk. The root is no such folder: one that is gone, or cannot be listed, is the disk's.
		return fmt.Errorf("%w: %w", errFolderGone, err)
	case err != nil:
		return fmt.Errorf("%w: %q: %w", ErrInterrupted, prefix, err)
	}
	for _, name := range names {
		path := prefix + name
		st, err := dir.Stat(name)
		switch {
		case unreachable(err):
			w.skip(path, err)
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
			found := FoundEntry{Path: path, Kind: solver.EntryDir, Mode: int64(st.Mode), MtimeNS: mtime}
			if err := setIdentity(&found, &st); err != nil {
				return fmt.Errorf("%w: %q: %w", ErrInterrupted, path, err)
			}
			w.Entries = append(w.Entries, found)
			if st.MountID != top.MountID || st.Dev != top.Dev {
				w.OtherMounts = append(w.OtherMounts, path)
				continue
			}
			if err := walkBelow(ctx, dir, name, path, top, w); err != nil {
				return err
			}
		case fsx.KindSymlink:
			target, err := dir.ReadLink(name)
			switch {
			case unreachable(err):
				w.skip(path, err)
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

func walkBelow(ctx context.Context, dir fsx.Dir, name, path string, top *fsx.Stat, w *Walked) error {
	below, err := dir.OpenDir(name)
	switch {
	case unreachable(err):
		// Reported, with no row: its entry was the last one added.
		w.Entries = w.Entries[:len(w.Entries)-1]
		w.skip(path, err)
		return nil
	case err != nil:
		return fmt.Errorf("%w: %q: %w", ErrInterrupted, path, err)
	}
	walkErr := walkFolder(ctx, below, path+"/", top, w)
	// A folder only read, whose close failing changes nothing that was read from it.
	_ = below.Close()
	if errors.Is(walkErr, errFolderGone) {
		// As one that could not be opened: reported, with no row, its entry being the last one added, as nothing of
		// it was listed.
		w.Entries = w.Entries[:len(w.Entries)-1]
		w.skip(path, walkErr)
		return nil
	}
	return walkErr
}

// setIdentity gives a folder what tells it from another: a rename keeps both (specs.md §15.12).
func setIdentity(e *FoundEntry, st *fsx.Stat) error {
	key := strconv.FormatUint(st.Ino, 10)
	e.FileKey = &key
	if st.HasBirth {
		birth, ok, err := solver.CreationTimeNS(st.Birth.Sec, st.Birth.Nsec)
		if err != nil {
			return fmt.Errorf("creation time: %w", err)
		}
		if ok {
			e.BtimeNS = &birth
		}
	}
	return nil
}

// FileFrom says what a walk would say of a regular file.
func FileFrom(path string, st *fsx.Stat) (FoundFile, error) {
	mtime, err := solver.TimeNS(st.Mtime.Sec, st.Mtime.Nsec)
	if err != nil {
		return FoundFile{}, fmt.Errorf("modification time: %w", err)
	}
	return foundFile(path, st, mtime)
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
		Mode:    int64(st.Mode),
		UID:     st.Uid,
		GID:     st.Gid,
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
