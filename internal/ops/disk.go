package ops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/layout"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/watch"
)

// What can become of a write to a disk (specs.md §17.1).
var (
	// ErrDiskLost says that the disk cannot be relied on any more, and that the operation stays
	// planned: only a reprise can tell where the file is (invariant 4).
	ErrDiskLost = errors.New("the disk cannot be relied on")
	// ErrNameRefused says that the driver refuses the name, and that nothing moved.
	ErrNameRefused = errors.New("the driver refuses this name")
	// ErrPathTaken says that something is at the target, and that nothing moved.
	ErrPathTaken = errors.New("the path is taken")
	// ErrGone says that the name no longer holds the file it held, and that nothing moved.
	ErrGone = errors.New("the name no longer holds that file")
	// ErrOtherMount says that a folder on the way is another mount, which no check confirmed.
	ErrOtherMount = errors.New("another mount on the way")
)

// Disk is a disk open for writing, through a handle on its top, which a mount check found to be the
// disk it should be (invariant 3).
type Disk struct {
	ID      string
	config  *config.Disk
	machine disks.Machine
	top     fsx.Dir
	mountID uint64
	dev     uint64
	ino     uint64
}

// Open opens the top of a disk. The caller confirms it afterwards, then calls Verify, so that what
// was confirmed is what the handle leads to (specs.md §10).
func Open(m disks.Machine, d *config.Disk) (*Disk, error) {
	top, err := m.OpenTop(d.Mount)
	if err != nil {
		return nil, fmt.Errorf("disk %q: %w", d.ID, err)
	}
	info, err := top.Info()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("disk %q: %w", d.ID, err), top.Close())
	}
	return &Disk{ID: d.ID, config: d, machine: m, top: top, mountID: info.MountID, dev: info.Dev, ino: info.Ino}, nil
}

// Close lets go of the disk.
func (d *Disk) Close() {
	// A folder whose close failing says nothing: what must be durable was synced.
	_ = d.top.Close()
}

// Verify checks that the mount path leads to this handle's mount, device and folder (specs.md §10).
// It asks the cache, so it wakes nothing.
func (d *Disk) Verify() error {
	info, err := fsx.StatPath(d.config.Mount)
	if err != nil {
		return lost(err)
	}
	if info.MountID != d.mountID || info.Dev != d.dev || info.Ino != d.ino {
		return fmt.Errorf("%w: another mount is at %q", ErrDiskLost, d.config.Mount)
	}
	return nil
}

func lost(err error) error {
	return fmt.Errorf("%w: %w", ErrDiskLost, err)
}

// classify keeps what says the disk is gone apart from what says only this write failed.
func classify(err error) error {
	if fsx.IsDiskLost(err) {
		return lost(err)
	}
	return err
}

func components(paths ...string) []string {
	var out []string
	for _, p := range paths {
		out = append(out, strings.Split(p, "/")...)
	}
	return out
}

func (d *Disk) rootPlace(logical string) []string {
	return components(d.config.Root, logical)
}

func (d *Disk) trashPlace(trashPath string) []string {
	return components(d.config.Workdir, layout.TrashFolder, trashPath)
}

// folder is an open folder, with what it took to reach it.
type folder struct {
	fsx.Dir
	opened []fsx.Dir
}

func (f *folder) close() {
	for _, d := range f.opened {
		// A folder whose close failing says nothing: what must be durable was synced.
		_ = d.Close()
	}
}

// sameMount refuses a folder on another mount, as a walk does not enter one (specs.md §15.10): no
// check confirmed it, and what it holds is on another disk.
func (d *Disk) sameMount(dir fsx.Dir) error {
	info, err := dir.Info()
	if err != nil {
		return classify(err)
	}
	if info.MountID != d.mountID {
		return ErrOtherMount
	}
	return nil
}

// open reaches a folder through handles, so that no symlink is followed on the way.
func (d *Disk) open(path []string) (*folder, error) {
	f := &folder{Dir: d.top}
	for _, name := range path {
		next, err := f.OpenDir(name)
		if err != nil {
			f.close()
			return nil, fmt.Errorf("open %q: %w", name, err)
		}
		f.opened = append(f.opened, next)
		f.Dir = next
		if err := d.sameMount(next); err != nil {
			f.close()
			return nil, err
		}
	}
	return f, nil
}

// makeFolders reaches a folder of the trash, creating what is missing on the way, each with the
// mode and owner like gives for its level (specs.md §17.1). Each is durable before anything goes
// through it. It is never given a path below the root, whose folders are operations of their own.
func (d *Disk) makeFolders(path []string, like map[int]fsx.Stat) (*folder, error) {
	f := &folder{Dir: d.top}
	for i, name := range path {
		parent := f.Dir
		next, err := parent.OpenDir(name)
		if fsx.IsAbsent(err) {
			if err := d.makeFolder(parent, name, like, i); err != nil {
				f.close()
				return nil, err
			}
			next, err = parent.OpenDir(name)
		}
		if err != nil {
			f.close()
			return nil, classify(fmt.Errorf("open %q: %w", name, err))
		}
		f.opened = append(f.opened, next)
		f.Dir = next
		if err := d.sameMount(next); err != nil {
			f.close()
			return nil, err
		}
	}
	return f, nil
}

func (d *Disk) makeFolder(parent fsx.Dir, name string, like map[int]fsx.Stat, level int) error {
	st, known := like[level]
	var mode *fsx.Stat
	if known {
		mode = &st
	}
	err := parent.Mkdir(name, modeLike(mode))
	switch {
	case fsx.IsExisting(err):
		return nil
	case fsx.IsNameRefused(err):
		return fmt.Errorf("%w: %w", ErrNameRefused, err)
	case err != nil:
		return classify(err)
	}
	if known {
		if err := adopt(parent, name, &st); err != nil {
			return err
		}
	}
	if err := parent.Sync(); err != nil {
		return lost(err)
	}
	return nil
}

// modeLike is the mode a new folder is made with: already the one it stands in for, so that a crash
// before adopt, or a driver that refuses adopt, leaves it private rather than open.
func modeLike(like *fsx.Stat) uint32 {
	if like != nil {
		return like.Mode
	}
	return 0o755
}

// adopt gives a new folder the mode and owner it stands in for, where the driver allows it. A driver
// that refuses only leaves a folder as Mkdir made it, and only a disk that is gone makes this matter.
func adopt(parent fsx.Dir, name string, like *fsx.Stat) error {
	err := parent.Adopt(name, like)
	switch {
	case err == nil:
		return nil
	case fsx.IsDiskLost(err):
		return lost(err)
	default:
		slog.Warn("a new folder keeps the default mode or owner", "name", name, "error", err)
		return nil
	}
}

// holds says whether a name holds the file an operation is about, by what is known of that file, and
// gives what it saw.
func holds(dir fsx.Dir, name string, is func(*fsx.Stat) bool) (fsx.Stat, error) {
	st, err := dir.Stat(name)
	switch {
	case fsx.IsAbsent(err):
		return st, fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return st, classify(err)
	case st.Kind != fsx.KindFile || !is(&st):
		return st, ErrGone
	}
	return st, nil
}

// byKey knows a file by its inode, which is all an entry of the trash says of it.
func byKey(fileKey string) func(*fsx.Stat) bool {
	return func(st *fsx.Stat) bool { return keyOf(st) == fileKey }
}

// untouched knows a copy as a scan does, by what its row says (specs.md §15.10).
func untouched(row *catalog.KnownCopy) func(*fsx.Stat) bool {
	return func(st *fsx.Stat) bool {
		found, err := watch.FileFrom(row.Path, st)
		return err == nil && watch.Unchanged(&found, row)
	}
}

// linked knows a copy once link has moved its change time, so that only its inode, size and
// modification time can tell.
func linked(row *catalog.KnownCopy) func(*fsx.Stat) bool {
	return func(st *fsx.Stat) bool {
		found, err := watch.FileFrom(row.Path, st)
		if err != nil {
			return false
		}
		known := *row
		known.CtimeNS = sql.NullInt64{Int64: found.CtimeNS, Valid: true}
		return watch.Unchanged(&found, &known)
	}
}

// IsSameFile says whether a look is of the file a row speaks of (specs.md §15.1).
func IsSameFile(row *catalog.KnownCopy, st *fsx.Stat) bool {
	return st.Kind == fsx.KindFile && isSameAsRow(row, st)
}

// IsUntouched says whether a look found what a row says (specs.md §15.10).
func IsUntouched(row *catalog.KnownCopy, st *fsx.Stat) bool {
	return untouched(row)(st)
}

// TimeOf gives a change time as the catalog holds it, with nothing valid in it if it cannot.
func TimeOf(t fsx.Time) sql.NullInt64 {
	return timeOf(t)
}

// timeOf gives a change time as the catalog holds it, with nothing valid in it if it cannot.
func timeOf(t fsx.Time) sql.NullInt64 {
	ns, err := solver.TimeNS(t.Sec, t.Nsec)
	return sql.NullInt64{Int64: ns, Valid: err == nil}
}

func keyOf(st *fsx.Stat) string {
	return strconv.FormatUint(st.Ino, 10)
}

// stat looks at a name, and says whether it is there. Only an error that says it is not there means
// that: any other says nothing about it (invariant 4).
func (d *Disk) stat(path []string) (fsx.Stat, bool, error) {
	parent, err := d.open(path[:len(path)-1])
	switch {
	case fsx.IsAbsent(err):
		return fsx.Stat{}, false, nil
	case err != nil:
		return fsx.Stat{}, false, classify(err)
	}
	defer parent.close()
	st, err := parent.Stat(path[len(path)-1])
	switch {
	case fsx.IsAbsent(err):
		return fsx.Stat{}, false, nil
	case err != nil:
		return fsx.Stat{}, false, classify(err)
	}
	return st, true, nil
}

// StatRoot looks at a logical path on this disk.
func (d *Disk) StatRoot(logical string) (fsx.Stat, bool, error) {
	return d.stat(d.rootPlace(logical))
}

// LookAt says what stands in the way of a logical path: whether something is there or a parent of
// it is not a folder, and which of its parent folders are missing, from the top (specs.md §17.4).
func (d *Disk) LookAt(logical string) (taken bool, missing []string, err error) {
	parts := strings.Split(logical, "/")
	dir, err := d.open(components(d.config.Root))
	if err != nil {
		return false, nil, classify(err)
	}
	defer dir.close()
	for i, name := range parts {
		st, err := dir.Stat(name)
		switch {
		case fsx.IsAbsent(err):
			for j := i; j < len(parts)-1; j++ {
				missing = append(missing, strings.Join(parts[:j+1], "/"))
			}
			return false, missing, nil
		case err != nil:
			return false, nil, classify(err)
		case i == len(parts)-1 || st.Kind != fsx.KindDir:
			return true, nil, nil
		}
		next, err := dir.OpenDir(name)
		if err != nil {
			return false, nil, classify(err)
		}
		dir.opened = append(dir.opened, next)
		dir.Dir = next
		if err := d.sameMount(next); err != nil {
			return false, nil, err
		}
	}
	return false, nil, nil
}

// MakeRootFolder makes one folder below the root, for a restore, and keeps the time of the folder it
// lands in (specs.md §6.1, item 8). like is the folder of the trash at that level, when there is
// one. It says what it made. The root itself is never made: its absence is an error (invariant 3).
func (d *Disk) MakeRootFolder(logical string, like *fsx.Stat) (fsx.Stat, error) {
	if err := d.Verify(); err != nil {
		return fsx.Stat{}, err
	}
	path := d.rootPlace(logical)
	parent, err := d.open(path[:len(path)-1])
	switch {
	case fsx.IsNotAFolder(err):
		return fsx.Stat{}, fmt.Errorf("%w: %w", ErrPathTaken, err)
	case err != nil:
		return fsx.Stat{}, classify(err)
	}
	defer parent.close()
	before, err := parent.Info()
	if err != nil {
		return fsx.Stat{}, classify(err)
	}
	name := path[len(path)-1]
	err = parent.Mkdir(name, modeLike(like))
	switch {
	case fsx.IsExisting(err):
		return fsx.Stat{}, fmt.Errorf("%w: %w", ErrPathTaken, err)
	case fsx.IsNameRefused(err):
		return fsx.Stat{}, fmt.Errorf("%w: %w", ErrNameRefused, err)
	case err != nil:
		return fsx.Stat{}, classify(err)
	}
	if like != nil {
		if err := adopt(parent, name, like); err != nil {
			return fsx.Stat{}, err
		}
	}
	d.machine.Checkpoint("folder-made")
	if err := restoreTime(parent, before.Mtime); err != nil {
		return fsx.Stat{}, err
	}
	if err := parent.Sync(); err != nil {
		return fsx.Stat{}, lost(err)
	}
	st, err := parent.Stat(name)
	if err != nil {
		return fsx.Stat{}, classify(err)
	}
	return st, nil
}

// TrashFolderInfo looks at a folder of the trash, to make one like it.
func (d *Disk) TrashFolderInfo(trashPath string) (*fsx.Stat, error) {
	st, there, err := d.stat(d.trashPlace(trashPath))
	if err != nil {
		return nil, err
	}
	if !there || st.Kind != fsx.KindDir {
		return nil, nil //nolint:nilnil // no folder to be like is an answer
	}
	return &st, nil
}

// moveNoReplace is renameat2 with RENAME_NOREPLACE, or link then unlink where that gives EINVAL or
// ENOSYS. Whether EINVAL was about the flag or the name, only link can tell (measured). Unlike a
// rename, those are two calls, so the link is made durable before the source's name goes: a device
// that fails between them must not leave the file with no name at all. is says what the source's name
// must still hold once the link is durable, which that fsync can make long.
func (d *Disk) moveNoReplace(source fsx.Dir, name string, target fsx.Dir, newName string, is func(*fsx.Stat) bool) error {
	err := source.Rename(name, target, newName)
	switch {
	case err == nil:
		return nil
	case fsx.IsAbsent(err):
		return fmt.Errorf("%w: %w", ErrGone, err)
	case fsx.IsExisting(err):
		return fmt.Errorf("%w: %w", ErrPathTaken, err)
	case !fsx.IsFlagRefused(err):
		return classify(err)
	}
	err = source.Link(name, target, newName)
	switch {
	case err == nil:
	case fsx.IsAbsent(err):
		return fmt.Errorf("%w: %w", ErrGone, err)
	case fsx.IsExisting(err):
		return fmt.Errorf("%w: %w", ErrPathTaken, err)
	case fsx.IsNameRefused(err):
		return fmt.Errorf("%w: %w", ErrNameRefused, err)
	default:
		return classify(err)
	}
	d.machine.Checkpoint("linked")
	if err := target.Sync(); err != nil {
		return lost(err)
	}
	switch _, err := holds(source, name, is); {
	case err == nil:
	case fsx.IsAbsent(err):
		// The source's name went by itself: the link is the file's name now, and may be the only one.
		// The move is done, as a reprise would find it (specs.md §17.2).
		return nil
	case errors.Is(err, ErrGone):
		// A replacement or an edit since the look must not lose its name for what was linked.
		if err := target.Unlink(newName); err != nil {
			return classify(err)
		}
		return err
	default:
		return err
	}
	// Both names lead to the file now, which a reprise knows how to finish.
	if err := source.Unlink(name); err != nil {
		return classify(err)
	}
	return nil
}

// Moved says what a move left behind it.
type Moved struct {
	// Stat is the file at the target, whose change time and link count the move may have changed.
	Stat fsx.Stat
	// PriorNS is the change time the file had just before the move.
	PriorNS sql.NullInt64
}

// Links says what the other names of the file must record, when it has any.
func (m *Moved) Links() *catalog.Links {
	links := linksOf(&m.Stat)
	if links != nil {
		links.PriorNS = m.PriorNS
	}
	return links
}

// ToTrash moves a copy to where its operation says, and says once that is durable (specs.md §17.1).
// row is what the catalog says of the copy.
func (d *Disk) ToTrash(op *Op, row *catalog.KnownCopy) (*Moved, error) {
	if err := d.Verify(); err != nil {
		return nil, err
	}
	src := d.rootPlace(op.Path)
	dst := d.trashPlace(op.Path2)
	source, err := d.open(src[:len(src)-1])
	switch {
	case fsx.IsAbsent(err), errors.Is(err, ErrOtherMount):
		return nil, fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return nil, classify(err)
	}
	defer source.close()
	name := src[len(src)-1]
	if _, err := holds(source, name, byKey(op.FileKey)); err != nil {
		return nil, err
	}
	like, err := d.likeSource(source, src, dst)
	if err != nil {
		return nil, err
	}
	target, err := d.makeFolders(dst[:len(dst)-1], like)
	if err != nil {
		return nil, err
	}
	defer target.close()
	d.machine.Checkpoint("trash-folders")
	newName := dst[len(dst)-1]
	// Looked at again right before the move, as making those folders can be long, and an edit in place
	// keeps the inode: it must not leave the library for the trash unseen.
	seen, err := holds(source, name, untouched(row))
	if err != nil {
		return nil, err
	}
	before, err := source.Info()
	if err != nil {
		return nil, classify(err)
	}
	if err := d.moveNoReplace(source.Dir, name, target.Dir, newName, linked(row)); err != nil {
		return nil, err
	}
	// A file that took the name between the look and the move goes back: resolving the operation does
	// that, as a reprise would (specs.md §17.1, §17.2).
	if _, err := holds(target.Dir, newName, byKey(op.FileKey)); err != nil {
		return nil, err
	}
	d.machine.Checkpoint("moved")
	if err := target.Sync(); err != nil {
		return nil, lost(err)
	}
	d.machine.Checkpoint("target-synced")
	if err := restoreTime(source.Dir, before.Mtime); err != nil {
		return nil, err
	}
	if err := source.Sync(); err != nil {
		return nil, lost(err)
	}
	d.machine.Checkpoint("source-synced")
	return d.movedFrom(target.Dir, newName, &seen)
}

// likeSource gives, for each level of the target that stands for a folder of the logical path, that
// folder's mode and owner, so that a private folder stays private in the trash (specs.md §17.1). The
// operation's own folder stands for the root.
func (d *Disk) likeSource(source *folder, src, dst []string) (map[int]fsx.Stat, error) {
	like := map[int]fsx.Stat{}
	root := len(components(d.config.Root))
	folders := len(src) - 1 - root
	for i := -1; i < folders; i++ {
		st, err := source.opened[root+i].Info()
		if err != nil {
			return nil, classify(err)
		}
		// The last levels of dst are the operation's folder, the parents of the logical path, then its
		// name.
		like[len(dst)-1-folders+i] = st
	}
	return like, nil
}

func (d *Disk) movedFrom(dir fsx.Dir, name string, seen *fsx.Stat) (*Moved, error) {
	st, err := dir.Stat(name)
	if err != nil {
		return nil, classify(err)
	}
	return &Moved{Stat: st, PriorNS: timeOf(seen.Ctime)}, nil
}

// FromTrash moves an entry of the trash back to its path. Every parent folder is there by now, each
// made as an operation of its own, and the root is never made here (invariant 3).
func (d *Disk) FromTrash(op *Op) (*Moved, error) {
	if err := d.Verify(); err != nil {
		return nil, err
	}
	src := d.trashPlace(op.Path2)
	dst := d.rootPlace(op.Path)
	source, err := d.open(src[:len(src)-1])
	switch {
	case fsx.IsAbsent(err):
		return nil, fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return nil, classify(err)
	}
	defer source.close()
	name := src[len(src)-1]
	seen, err := holds(source, name, byKey(op.FileKey))
	if err != nil {
		return nil, err
	}
	target, err := d.open(dst[:len(dst)-1])
	switch {
	case fsx.IsNotAFolder(err):
		return nil, fmt.Errorf("%w: %w", ErrPathTaken, err)
	case err != nil:
		return nil, classify(err)
	}
	defer target.close()
	before, err := target.Info()
	if err != nil {
		return nil, classify(err)
	}
	newName := dst[len(dst)-1]
	if err := d.moveNoReplace(source.Dir, name, target.Dir, newName, byKey(op.FileKey)); err != nil {
		return nil, err
	}
	// What took the name in the trash since the look must not pass for the entry. It stays at the
	// path: nothing goes back, as nothing moves a live file into the trash (specs.md §17.2).
	if _, err := holds(target.Dir, newName, byKey(op.FileKey)); err != nil {
		return nil, err
	}
	d.machine.Checkpoint("moved")
	if err := restoreTime(target.Dir, before.Mtime); err != nil {
		return nil, err
	}
	if err := target.Sync(); err != nil {
		return nil, lost(err)
	}
	d.machine.Checkpoint("target-synced")
	if err := source.Sync(); err != nil {
		return nil, lost(err)
	}
	d.machine.Checkpoint("source-synced")
	return d.movedFrom(target.Dir, newName, &seen)
}

// isSameAsRow says whether a look is of the file a row speaks of (specs.md §15.1).
func isSameAsRow(row *catalog.KnownCopy, st *fsx.Stat) bool {
	found, err := watch.FileFrom(row.Path, st)
	if err != nil {
		return false
	}
	look, known := watch.FoundLook(&found), watch.RowLook(row)
	return solver.SameFile(&known, &look)
}

// sameFileAs knows a copy by its identity, edited or not (specs.md §15.1): a rename loses nothing.
func sameFileAs(row *catalog.KnownCopy) func(*fsx.Stat) bool {
	return func(st *fsx.Stat) bool { return isSameAsRow(row, st) }
}

// RenameFile moves a copy to where its operation says, both logical paths on this disk, and says once
// that is durable (specs.md §15.12). The target's parents are there by now, each an operation of its own.
func (d *Disk) RenameFile(op *Op, row *catalog.KnownCopy) (*Moved, *fsx.Stat, error) {
	if err := d.Verify(); err != nil {
		return nil, nil, err
	}
	src := d.rootPlace(op.Path)
	dst := d.rootPlace(op.Path2)
	source, err := d.open(src[:len(src)-1])
	switch {
	case fsx.IsAbsent(err), errors.Is(err, ErrOtherMount):
		return nil, nil, fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return nil, nil, classify(err)
	}
	defer source.close()
	target, err := d.open(dst[:len(dst)-1])
	switch {
	case fsx.IsNotAFolder(err):
		return nil, nil, fmt.Errorf("%w: %w", ErrPathTaken, err)
	case err != nil:
		return nil, nil, classify(err)
	}
	defer target.close()
	name, newName := src[len(src)-1], dst[len(dst)-1]
	seen, err := holds(source.Dir, name, sameFileAs(row))
	if err != nil {
		return nil, nil, err
	}
	sourceTime, err := source.Info()
	if err != nil {
		return nil, nil, classify(err)
	}
	targetTime, err := target.Info()
	if err != nil {
		return nil, nil, classify(err)
	}
	if err := d.moveNoReplace(source.Dir, name, target.Dir, newName, sameFileAs(row)); err != nil {
		return nil, nil, err
	}
	if _, err := holds(target.Dir, newName, byKey(op.FileKey)); err != nil {
		return nil, nil, err
	}
	d.machine.Checkpoint("moved")
	if err := target.Sync(); err != nil {
		return nil, nil, lost(err)
	}
	d.machine.Checkpoint("target-synced")
	if err := d.restoreBoth(source.Dir, target.Dir, sourceTime.Mtime, targetTime.Mtime); err != nil {
		return nil, nil, err
	}
	if err := source.Sync(); err != nil {
		return nil, nil, lost(err)
	}
	d.machine.Checkpoint("source-synced")
	moved, err := d.movedFrom(target.Dir, newName, &seen)
	if err != nil {
		return nil, nil, err
	}
	return moved, &seen, nil
}

// restoreBoth gives both folders of a move their times back. They may be one folder, and then the time
// it had before is the one to give.
func (d *Disk) restoreBoth(source, target fsx.Dir, sourceTime, targetTime fsx.Time) error {
	if err := restoreTime(target, targetTime); err != nil {
		return err
	}
	if err := restoreTime(source, sourceTime); err != nil {
		return err
	}
	if err := target.Sync(); err != nil {
		return lost(err)
	}
	return nil
}

// RenameFolder moves a folder and everything below it to where its operation says (specs.md §15.12).
// Where the flag is refused, and a folder has no link, it is a look at the target and a plain rename,
// whose only risk, measured, is replacing an empty folder that appeared since the look.
func (d *Disk) RenameFolder(op *Op) error {
	if err := d.Verify(); err != nil {
		return err
	}
	src := d.rootPlace(op.Path)
	dst := d.rootPlace(op.Path2)
	source, err := d.open(src[:len(src)-1])
	switch {
	case fsx.IsAbsent(err), errors.Is(err, ErrOtherMount):
		return fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return classify(err)
	}
	defer source.close()
	target, err := d.open(dst[:len(dst)-1])
	switch {
	case fsx.IsNotAFolder(err):
		return fmt.Errorf("%w: %w", ErrPathTaken, err)
	case err != nil:
		return classify(err)
	}
	defer target.close()
	name, newName := src[len(src)-1], dst[len(dst)-1]
	if _, err := holdsFolder(source.Dir, name, op.FileKey); err != nil {
		return err
	}
	sourceTime, err := source.Info()
	if err != nil {
		return classify(err)
	}
	targetTime, err := target.Info()
	if err != nil {
		return classify(err)
	}
	if err := moveFolderNoReplace(source.Dir, name, target.Dir, newName); err != nil {
		return err
	}
	if _, err := holdsFolder(target.Dir, newName, op.FileKey); err != nil {
		return err
	}
	d.machine.Checkpoint("moved")
	if err := target.Sync(); err != nil {
		return lost(err)
	}
	d.machine.Checkpoint("target-synced")
	if err := d.restoreBoth(source.Dir, target.Dir, sourceTime.Mtime, targetTime.Mtime); err != nil {
		return err
	}
	if err := source.Sync(); err != nil {
		return lost(err)
	}
	d.machine.Checkpoint("source-synced")
	return nil
}

func holdsFolder(dir fsx.Dir, name, fileKey string) (fsx.Stat, error) {
	st, err := dir.Stat(name)
	switch {
	case fsx.IsAbsent(err):
		return st, fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return st, classify(err)
	case st.Kind != fsx.KindDir || keyOf(&st) != fileKey:
		return st, ErrGone
	}
	return st, nil
}

func moveFolderNoReplace(source fsx.Dir, name string, target fsx.Dir, newName string) error {
	err := source.Rename(name, target, newName)
	switch {
	case err == nil:
		return nil
	case fsx.IsAbsent(err):
		return fmt.Errorf("%w: %w", ErrGone, err)
	case fsx.IsExisting(err):
		return fmt.Errorf("%w: %w", ErrPathTaken, err)
	case !fsx.IsFlagRefused(err):
		return classify(err)
	}
	switch _, err := target.Stat(newName); {
	case err == nil:
		return fmt.Errorf("%w: %q", ErrPathTaken, newName)
	case !fsx.IsAbsent(err):
		return classify(err)
	}
	err = source.RenamePlain(name, target, newName)
	switch {
	case err == nil:
		return nil
	case fsx.IsAbsent(err):
		return fmt.Errorf("%w: %w", ErrGone, err)
	case fsx.IsExisting(err), fsx.IsNotEmpty(err), fsx.IsNotAFolder(err):
		return fmt.Errorf("%w: %w", ErrPathTaken, err)
	case fsx.IsNameRefused(err):
		return fmt.Errorf("%w: %w", ErrNameRefused, err)
	default:
		return classify(err)
	}
}

// OwnFiles gives what this disk's own trash and tmp hold of those inodes: a copy that is there was an
// interrupted operation of Atoll's, not a user's deletion (specs.md §15.5).
func (d *Disk) OwnFiles(ctx context.Context, inodes map[string]bool) (map[string][]solver.FileLook, error) {
	out := map[string][]solver.FileLook{}
	for _, folder := range []string{layout.TrashFolder, layout.TmpFolder} {
		dir, err := d.open(components(d.config.Workdir, folder))
		switch {
		case fsx.IsAbsent(err):
			continue
		case err != nil:
			// That disk's trouble, as a walk of it that fails is: nothing can be said of what is absent.
			return nil, lost(err)
		}
		walked, err := watch.Walk(ctx, dir.Dir)
		dir.close()
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrDiskLost, err)
		}
		for i := range walked.Files {
			if f := &walked.Files[i]; inodes[f.FileKey] {
				out[f.FileKey] = append(out[f.FileKey], watch.FoundLook(f))
			}
		}
	}
	return out, nil
}

// FolderInfo looks at a folder below the root, to make one like it, and says nil for anything else.
func (d *Disk) FolderInfo(logical string) (*fsx.Stat, error) {
	st, there, err := d.stat(d.rootPlace(logical))
	if err != nil {
		return nil, err
	}
	if !there || st.Kind != fsx.KindDir {
		return nil, nil //nolint:nilnil // no folder to be like is an answer
	}
	return &st, nil
}

// restoreTime gives a folder its time back. Only a disk that is gone makes it matter: a folder's
// date protects nothing, and the next scan records what is there.
func restoreTime(f fsx.Dir, t fsx.Time) error {
	err := f.SetMtime(t)
	switch {
	case err == nil:
		return nil
	case fsx.IsDiskLost(err):
		return lost(err)
	default:
		slog.Warn("a folder keeps a new time", "error", err)
		return nil
	}
}

// Purged says what a purge found of the file: how many names it had, and the change time it had
// before one went.
type Purged struct {
	Nlink   uint32
	PriorNS sql.NullInt64
}

// PurgeEntry deletes a file of the trash, and is fine with one that is already gone. ErrGone means that
// the name holds another file, which stays.
func (d *Disk) PurgeEntry(op *Op) (Purged, error) {
	if err := d.Verify(); err != nil {
		return Purged{}, err
	}
	path := d.trashPlace(op.Path2)
	parent, err := d.open(path[:len(path)-1])
	switch {
	case fsx.IsAbsent(err):
		return Purged{}, nil
	case err != nil:
		return Purged{}, classify(err)
	}
	defer parent.close()
	name := path[len(path)-1]
	st, err := parent.Stat(name)
	switch {
	case fsx.IsAbsent(err):
		return Purged{}, nil
	case err != nil:
		return Purged{}, classify(err)
	case st.Kind != fsx.KindFile || keyOf(&st) != op.FileKey:
		return Purged{}, ErrGone
	}
	if err := parent.Unlink(name); err != nil && !fsx.IsAbsent(err) {
		return Purged{}, classify(err)
	}
	d.machine.Checkpoint("moved")
	if err := parent.Sync(); err != nil {
		return Purged{}, lost(err)
	}
	d.machine.Checkpoint("source-synced")
	return Purged{Nlink: st.Nlink, PriorNS: timeOf(st.Ctime)}, nil
}

// CleanTrash removes the folders of a location of the trash that are left empty, down to and
// including the date. It may stop at any point: an empty folder in the trash harms nothing.
func (d *Disk) CleanTrash(trashPath string) {
	place := d.trashPlace(trashPath)
	keep := len(components(d.config.Workdir, layout.TrashFolder))
	for end := len(place) - 1; end > keep; end-- {
		parent, err := d.open(place[:end-1])
		if fsx.IsAbsent(err) {
			continue // a folder further down was never made: the ones above may have been
		}
		if err != nil {
			return
		}
		err = parent.Rmdir(place[end-1])
		parent.close()
		if err != nil && !fsx.IsAbsent(err) {
			return
		}
	}
}
