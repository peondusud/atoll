package watch

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
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
	// Size and its owner are what Atoll's own view presents of it (specs.md §21.2).
	Size     int64
	UID, GID uint32
}

// Walked is what one walk found (specs.md §15.10).
type Walked struct {
	Files       []FoundFile
	Entries     []FoundEntry
	Unreadable  []string
	OtherMounts []string
	// Refused holds the names of Unreadable that refused the account that asked.
	Refused []string
	// TooLong holds the names the walk neither recorded nor went below, as their path is past the limit of their disk's
	// mergerfs branch while their folder's is not (specs.md §15.8), in the walk's order.
	TooLong []string
}

// NoBound is the Limits.MaxPath of a walk that goes below every name: walk.long_paths = "walk" (specs.md §15.8).
const NoBound = -1

// Limits says how far a walk goes, and how many folders it holds open at once (specs.md §15.8, §24). A name whose
// logical path is longer than MaxPath bytes is neither recorded nor gone below, and is reported in Walked.TooLong;
// NoBound walks every name. With MaxOpen, a walk holds at most that many folders open at once, its root included, and
// opens a folder again from one it holds when it comes back to it; 0 holds every folder it is below.
type Limits struct {
	MaxPath int
	MaxOpen int
}

// Unbounded walks everything, holding every folder it is below: a walk of Atoll's own folders, whose depth the
// library's bounds, or of a test.
var Unbounded = Limits{MaxPath: NoBound}

// LimitsOf gives the limits of the walks of a disk, as walk.long_paths and walk.max_open say (specs.md §15.8, §24).
// With "skip", the default, a walk goes no further than what a mergerfs branch can present: <mount>/<root>/<logical
// path> in at most 4095 bytes, PATH_MAX with its NUL. Past it, mergerfs lists a copy that it cannot open, which
// protects nothing.
func LimitsOf(c *config.Config, d *config.Disk) Limits {
	if all, maxOpen := c.Walk(); all {
		return Limits{MaxPath: NoBound, MaxOpen: maxOpen}
	}
	return Limits{MaxPath: max(names.MaxBranchPathBytes-len(d.Mount)-1-len(d.Root)-1, 0)}
}

// ownSlack is how much longer than a logical path a place of the trash, below <workdir>/trash/, is at most: a date
// and a slash, an operation's id with .chunks and a slash, and a slash and a chunk's index (specs.md §17).
const ownSlack = len("2006-01-02/") + 19 + len(".chunks/") + len("/") + 19

// Own gives the limits of a walk of a disk's own folders, <workdir>/trash/ and tmp/, from those of its root: what
// Atoll puts there is a copy it found below the root, below a date and an operation's folder, so a path a walk of
// the root does not leave is not left there either.
func (l Limits) Own() Limits {
	if l.MaxPath == NoBound {
		return l
	}
	return Limits{MaxPath: l.MaxPath + ownSlack, MaxOpen: l.MaxOpen}
}

// Fits says whether a walk records a logical path, and goes below it.
func (l Limits) Fits(path string) bool {
	return l.MaxPath == NoBound || len(path) <= l.MaxPath
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

// errNotTheFolder says that a folder a walk opened again is not the one it had left: another one at that name, or one
// on another device or mount (specs.md §15.8).
var errNotTheFolder = errors.New("no longer the folder the walk was in")

// abandoned says that a walk could not open again a folder it had left, as it was removed, renamed, replaced or refuses
// the account that asks since: what was below it is left aside, as a name that cannot be reached is, and the walk goes
// on beside it. It says nothing of the disk, nor of what was below it (specs.md §15.10).
type abandoned struct {
	at  *frame
	err error
}

func (a *abandoned) Error() string {
	return fmt.Sprintf("%q: %v", a.at.path, a.err)
}

func (a *abandoned) Unwrap() error { return a.err }

// frame is a folder a walk is in: its name in its folder, what it is, and its handle while the walk holds one.
type frame struct {
	dir  fsx.Dir
	name string
	// path is its logical path, "" for the root.
	path              string
	dev, ino, mountID uint64
}

// walker walks below a folder. Its stack holds the folders it is in, from its root, which it never closes. With
// limits.MaxOpen, it holds at most that many of them open: the root and the deepest, as the deepest are where it comes
// back first. lo is the shallowest of them but the root that may hold a handle: none of those between the root and lo
// does.
type walker struct {
	ctx    context.Context
	top    *fsx.Stat
	w      *Walked
	limits Limits
	stack  []*frame
	open   int
	lo     int
}

func newWalker(ctx context.Context, root fsx.Dir, prefix string, top *fsx.Stat, w *Walked, limits Limits) *walker {
	return &walker{ctx: ctx, top: top, w: w, limits: limits, stack: []*frame{{dir: root, path: prefix}}, open: 1, lo: 1}
}

// Walk reads everything below a root, through handles, never following a symlink, as far as limits say. A listed name
// that can no longer be reached is reported, and any other error stops the walk.
func Walk(ctx context.Context, root fsx.Dir, limits Limits) (*Walked, error) {
	top, err := root.Info()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInterrupted, err)
	}
	w := &Walked{}
	k := newWalker(ctx, root, "", &top, w, limits)
	k.stack[0].dev, k.stack[0].ino, k.stack[0].mountID = top.Dev, top.Ino, top.MountID
	if err := k.walkFolder(k.stack[0], ""); err != nil {
		return nil, err
	}
	return w, nil
}

// handle gives a folder's handle, opening it again from the nearest folder above it the walk holds when the walk let
// it go, one name at a time, each the folder it was: same device, inode and mount. One that is not, or that can no
// longer be opened by its name, is abandoned; an error that says nothing of a name stops the walk.
func (k *walker) handle(f *frame) (fsx.Dir, error) {
	if f.dir != nil {
		return f.dir, nil
	}
	at := len(k.stack) - 1
	for k.stack[at] != f {
		at--
	}
	from := at
	for k.stack[from].dir == nil {
		from--
	}
	k.lo = min(k.lo, from+1)
	for i := from + 1; i <= at; i++ {
		parent, next := k.stack[i-1], k.stack[i]
		k.makeRoom(parent)
		dir, err := parent.dir.OpenDir(next.name)
		switch {
		case unreachable(err):
			return nil, &abandoned{at: next, err: err}
		case err != nil:
			return nil, fmt.Errorf("%w: %q: %w", ErrInterrupted, next.path, err)
		}
		st, err := dir.Info()
		if err != nil {
			_ = dir.Close()
			return nil, fmt.Errorf("%w: %q: %w", ErrInterrupted, next.path, err)
		}
		if st.Dev != next.dev || st.Ino != next.ino || st.MountID != next.mountID {
			_ = dir.Close()
			return nil, &abandoned{at: next, err: errNotTheFolder}
		}
		next.dir = dir
		k.open++
	}
	return f.dir, nil
}

// makeRoom lets go of the shallowest folders the walk holds, but its root and keep, the folder it is about to open
// another from, until it can open one more within limits.MaxOpen: keep is the deepest it holds, so what it lets go of
// is what it comes back to last.
func (k *walker) makeRoom(keep *frame) {
	if k.limits.MaxOpen <= 0 {
		return
	}
	closedBelow := true
	for j := k.lo; k.open >= k.limits.MaxOpen && j < len(k.stack); j++ {
		f := k.stack[j]
		switch {
		case f.dir == nil:
		case f == keep:
			closedBelow = false
		default:
			// A folder only read, whose close failing changes nothing that was read from it.
			_ = f.dir.Close()
			f.dir = nil
			k.open--
			if closedBelow {
				k.lo = j + 1
			}
		}
	}
}

// walkFolder walks a folder of the root top. A folder of another mount, or of another device under the same mount,
// as a nested btrfs subvolume is, is listed and not walked (specs.md §15.10).
func (k *walker) walkFolder(f *frame, prefix string) error {
	if err := k.ctx.Err(); err != nil {
		return fmt.Errorf("walk: %w", err)
	}
	dir, err := k.handle(f)
	if err != nil {
		return err
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
	w := k.w
	for _, name := range names {
		path := prefix + name
		if !k.limits.Fits(path) {
			// Past what a mergerfs branch presents: neither recorded nor gone below, and nothing is concluded of it
			// (specs.md §15.8).
			w.TooLong = append(w.TooLong, path)
			continue
		}
		// What is below the last name may have let go of this folder.
		if dir, err = k.handle(f); err != nil {
			return err
		}
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
			found := FoundEntry{Path: path, Kind: solver.EntryDir, Mode: int64(st.Mode), MtimeNS: mtime, Size: st.Size, UID: st.Uid, GID: st.Gid}
			if err := setIdentity(&found, &st); err != nil {
				return fmt.Errorf("%w: %q: %w", ErrInterrupted, path, err)
			}
			w.Entries = append(w.Entries, found)
			if st.MountID != k.top.MountID || st.Dev != k.top.Dev {
				w.OtherMounts = append(w.OtherMounts, path)
				continue
			}
			if err := k.walkBelow(f, name, path, &st); err != nil {
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
			w.Entries = append(w.Entries, FoundEntry{Path: path, Kind: solver.EntrySymlink, Mode: int64(st.Mode), MtimeNS: mtime, Target: &target,
				Size: st.Size, UID: st.Uid, GID: st.Gid})
		case fsx.KindOther:
			w.Entries = append(w.Entries, FoundEntry{Path: path, Kind: solver.EntryIgnored, Mode: int64(st.Mode), MtimeNS: mtime, Size: st.Size, UID: st.Uid, GID: st.Gid})
		}
	}
	return nil
}

// walkMark is how much a walk had found when it went below a folder: what it finds below goes after it.
type walkMark struct {
	files, entries, unreadable, refused, otherMounts, tooLong int
}

func (w *Walked) mark() walkMark {
	return walkMark{len(w.Files), len(w.Entries), len(w.Unreadable), len(w.Refused), len(w.OtherMounts), len(w.TooLong)}
}

// leaveAside forgets what a walk found since it went below a folder, and the folder's own entry, the last one added
// before, and reports the folder as a name it could not reach.
func (w *Walked) leaveAside(at walkMark, path string, err error) {
	w.Files = w.Files[:at.files]
	w.Entries = w.Entries[:at.entries-1]
	w.Unreadable, w.Refused = w.Unreadable[:at.unreadable], w.Refused[:at.refused]
	w.OtherMounts, w.TooLong = w.OtherMounts[:at.otherMounts], w.TooLong[:at.tooLong]
	w.skip(path, err)
}

// walkBelow walks a folder found in another, st as its listing gave it, its entry the last one added.
func (k *walker) walkBelow(parent *frame, name, path string, st *fsx.Stat) error {
	dir, err := k.handle(parent)
	if err != nil {
		return err
	}
	k.makeRoom(parent)
	below, err := dir.OpenDir(name)
	switch {
	case unreachable(err):
		// Reported, with no row: its entry was the last one added.
		k.w.Entries = k.w.Entries[:len(k.w.Entries)-1]
		k.w.skip(path, err)
		return nil
	case err != nil:
		return fmt.Errorf("%w: %q: %w", ErrInterrupted, path, err)
	}
	f := &frame{dir: below, name: name, path: path, dev: st.Dev, ino: st.Ino, mountID: st.MountID}
	at := k.w.mark()
	k.stack = append(k.stack, f)
	k.open++
	walkErr := k.walkFolder(f, path+"/")
	if f.dir != nil {
		// A folder only read, whose close failing changes nothing that was read from it.
		_ = f.dir.Close()
		f.dir = nil
		k.open--
	}
	k.stack = k.stack[:len(k.stack)-1]
	k.lo = min(k.lo, len(k.stack))
	var left *abandoned
	switch {
	case errors.Is(walkErr, errFolderGone):
		// As one that could not be opened: reported, with no row, its entry being the last one added, as nothing of
		// it was listed.
		k.w.leaveAside(at, path, walkErr)
		return nil
	case errors.As(walkErr, &left) && left.at == f:
		// As one that cannot be reached: what was found below it goes, and so does its entry.
		k.w.leaveAside(at, path, left.err)
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
