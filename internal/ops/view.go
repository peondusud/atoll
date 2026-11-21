package ops

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
)

// What Atoll's own view does on a disk on a user's behalf (specs.md §21.2). Unlike Atoll's own operations, these leave
// each folder's time as the change makes it, as a user's change on that disk would, so that a media server sees the
// folder changed. As Atoll's own do, they reach what they change through handles from the verified top, follow no
// symbolic link, never replace anything, and make each name they make or move durable before they return, so that
// what the view's journal says done is on the disk.

// Like gives the mode and owner of a folder the view presents, for one that a disk lacks on the way to what the view
// makes there.
type Like func(folder string) (*fsx.Stat, error)

// UserAttrs is a change of attributes a user makes through the view: a nil field is left as it is.
type UserAttrs struct {
	Mode         *uint32
	UID, GID     *uint32
	Size         *int64
	Atime, Mtime *fsx.Time
}

// userParent reaches the folder a logical path lands in, making each folder missing on the way below the root with the
// mode and owner like gives of it. It gives that folder, with the path's last name.
func (d *Disk) userParent(logical string, like Like) (*folder, string, error) {
	parts := strings.Split(logical, "/")
	f, err := d.open(components(d.config.Root))
	if err != nil {
		return nil, "", classify(err)
	}
	for i, name := range parts[:len(parts)-1] {
		next, err := f.OpenDir(name)
		if fsx.IsAbsent(err) {
			st, likeErr := like(strings.Join(parts[:i+1], "/"))
			if likeErr != nil {
				f.close()
				return nil, "", likeErr
			}
			if err := d.makeUserFolder(f.Dir, name, st); err != nil {
				f.close()
				return nil, "", err
			}
			next, err = f.OpenDir(name)
		}
		switch {
		case fsx.IsNotAFolder(err):
			f.close()
			return nil, "", fmt.Errorf("%w: %w", ErrPathTaken, err)
		case err != nil:
			f.close()
			return nil, "", classify(fmt.Errorf("open %q: %w", name, err))
		}
		f.opened = append(f.opened, next)
		f.Dir = next
		if err := d.sameMount(next); err != nil {
			f.close()
			return nil, "", err
		}
	}
	return f, parts[len(parts)-1], nil
}

// makeUserFolder makes a folder with a mode and owner, then makes its name durable.
func (d *Disk) makeUserFolder(parent fsx.Dir, name string, like *fsx.Stat) error {
	err := parent.Mkdir(name, like.Mode)
	switch {
	case fsx.IsExisting(err):
		return fmt.Errorf("%w: %w", ErrPathTaken, err)
	case fsx.IsNameRefused(err):
		return fmt.Errorf("%w: %w", ErrNameRefused, err)
	case err != nil:
		return classify(err)
	}
	if err := adopt(parent, name, like); err != nil {
		return err
	}
	return d.durable(parent, name)
}

// durable makes a name made or moved in a folder durable, what it leads to first (specs.md §17.1).
func (d *Disk) durable(parent fsx.Dir, name string) error {
	if err := syncEntry(parent, name); err != nil {
		return err
	}
	if err := parent.Sync(); err != nil {
		return lost(err)
	}
	return nil
}

// owned gives what is made the owner a user asked for. A driver that keeps its own, as ntfs-3g without permissions
// does, or a process that may not give a file away, leaves it as it is, with a warning (specs.md §15.4).
func owned(err error, name string) error {
	switch {
	case err == nil:
		return nil
	case fsx.IsDiskLost(err):
		return lost(err)
	default:
		slog.Warn("what the view made keeps another owner", "name", name, "error", err)
		return nil
	}
}

// CreateUserFile makes a regular file at a logical path with a mode and an owner, after the folders missing on the way,
// and gives it open for writing.
func (d *Disk) CreateUserFile(logical string, mode, uid, gid uint32, like Like) (fsx.WritableFile, fsx.Stat, error) {
	if err := d.Verify(); err != nil {
		return nil, fsx.Stat{}, err
	}
	parent, name, err := d.userParent(logical, like)
	if err != nil {
		return nil, fsx.Stat{}, err
	}
	defer parent.close()
	w, err := parent.CreateFile(name, mode)
	switch {
	case fsx.IsExisting(err):
		return nil, fsx.Stat{}, fmt.Errorf("%w: %w", ErrPathTaken, err)
	case fsx.IsNameRefused(err):
		return nil, fsx.Stat{}, fmt.Errorf("%w: %w", ErrNameRefused, err)
	case err != nil:
		return nil, fsx.Stat{}, classify(err)
	}
	st, err := d.madeFile(parent, name, w, mode, uid, gid)
	if err != nil {
		_ = w.Close()
		return nil, fsx.Stat{}, err
	}
	return w, st, nil
}

func (d *Disk) madeFile(parent *folder, name string, w fsx.WritableFile, mode, uid, gid uint32) (fsx.Stat, error) {
	if err := owned(w.SetOwner(uid, gid), name); err != nil {
		return fsx.Stat{}, err
	}
	// The mode asked for, whatever the umask of this process, and after the owner, which clears setuid and setgid.
	if err := w.SetMode(mode); err != nil {
		return fsx.Stat{}, classify(err)
	}
	if err := d.durable(parent.Dir, name); err != nil {
		return fsx.Stat{}, err
	}
	st, err := w.Info()
	if err != nil {
		return fsx.Stat{}, classify(err)
	}
	return st, nil
}

// MakeUserFolder makes a folder at a logical path with a mode and an owner, after the folders missing on the way.
func (d *Disk) MakeUserFolder(logical string, mode, uid, gid uint32, like Like) (fsx.Stat, error) {
	if err := d.Verify(); err != nil {
		return fsx.Stat{}, err
	}
	parent, name, err := d.userParent(logical, like)
	if err != nil {
		return fsx.Stat{}, err
	}
	defer parent.close()
	if err := d.makeUserFolder(parent.Dir, name, &fsx.Stat{Mode: mode, Uid: uid, Gid: gid}); err != nil {
		return fsx.Stat{}, err
	}
	st, err := parent.Stat(name)
	if err != nil {
		return fsx.Stat{}, classify(err)
	}
	return st, nil
}

// MakeUserSymlink makes a symbolic link at a logical path with a target and an owner, after the folders missing on
// the way.
func (d *Disk) MakeUserSymlink(logical, target string, uid, gid uint32, like Like) (fsx.Stat, error) {
	if err := d.Verify(); err != nil {
		return fsx.Stat{}, err
	}
	parent, name, err := d.userParent(logical, like)
	if err != nil {
		return fsx.Stat{}, err
	}
	defer parent.close()
	err = parent.Symlink(target, name)
	switch {
	case fsx.IsExisting(err):
		return fsx.Stat{}, fmt.Errorf("%w: %w", ErrPathTaken, err)
	case fsx.IsNameRefused(err):
		return fsx.Stat{}, fmt.Errorf("%w: %w", ErrNameRefused, err)
	case err != nil:
		return fsx.Stat{}, classify(err)
	}
	if err := owned(parent.Lchown(name, uid, gid), name); err != nil {
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

// OpenUserFile opens the regular file at a logical path for writing.
func (d *Disk) OpenUserFile(logical string) (fsx.WritableFile, fsx.Stat, error) {
	if err := d.Verify(); err != nil {
		return nil, fsx.Stat{}, err
	}
	path := d.rootPlace(logical)
	parent, err := d.open(path[:len(path)-1])
	switch {
	case fsx.IsAbsent(err), errors.Is(err, ErrOtherMount):
		return nil, fsx.Stat{}, fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return nil, fsx.Stat{}, classify(err)
	}
	defer parent.close()
	w, err := parent.OpenWritable(path[len(path)-1])
	switch {
	case fsx.IsAbsent(err):
		return nil, fsx.Stat{}, fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return nil, fsx.Stat{}, classify(err)
	}
	st, err := w.Info()
	if err != nil {
		_ = w.Close()
		return nil, fsx.Stat{}, classify(err)
	}
	return w, st, nil
}

// RenameUser moves a file or a folder from one logical path to another on this disk, after the folders missing on the
// way to the new one, and never onto anything. fileKey is the inode of what moves, as a look right before found it.
func (d *Disk) RenameUser(from, to string, isFolder bool, fileKey string, like Like) error {
	if err := d.Verify(); err != nil {
		return err
	}
	src := d.rootPlace(from)
	source, err := d.open(src[:len(src)-1])
	switch {
	case fsx.IsAbsent(err), errors.Is(err, ErrOtherMount):
		return fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return classify(err)
	}
	defer source.close()
	target, newName, err := d.userParent(to, like)
	if err != nil {
		return err
	}
	defer target.close()
	name := src[len(src)-1]
	if isFolder {
		if _, err := holdsFolder(source.Dir, name, fileKey); err != nil {
			return err
		}
		err = d.moveFolderNoReplace(source.Dir, name, target.Dir, newName)
	} else {
		if _, err := holds(source.Dir, name, byKey(fileKey)); err != nil {
			return err
		}
		err = d.moveNoReplace(source.Dir, name, target.Dir, newName, byKey(fileKey))
	}
	if err != nil {
		return err
	}
	if err := d.durable(target.Dir, newName); err != nil {
		return err
	}
	if err := source.Sync(); err != nil {
		return lost(err)
	}
	return nil
}

// PlaceUserPart moves the part an operation create names to a logical path, after the folders missing on the way,
// each made with the mode and owner like gives of it, and never onto anything: a new file the view moves to this disk
// once a write ran out of space on another (specs.md §21.2). Its name, then its folder, are synced, then the folder of
// the part. As what the view does for a user, the folder it lands in keeps the time the new name gives it. It says what
// is at the path then. ErrPathTaken with IsExisting means that something took the name.
func (d *Disk) PlaceUserPart(op *Op, logical string, like Like) (fsx.Stat, error) {
	if err := d.Verify(); err != nil {
		return fsx.Stat{}, err
	}
	src := d.tmpPlace(op.Path2)
	source, err := d.open(src[:len(src)-1])
	switch {
	case fsx.IsAbsent(err), errors.Is(err, ErrOtherMount):
		return fsx.Stat{}, fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return fsx.Stat{}, classify(err)
	}
	defer source.close()
	name := src[len(src)-1]
	if _, err := holds(source.Dir, name, byKey(op.FileKey)); err != nil {
		return fsx.Stat{}, err
	}
	target, newName, err := d.userParent(logical, like)
	if err != nil {
		return fsx.Stat{}, err
	}
	defer target.close()
	if err := d.moveNoReplace(source.Dir, name, target.Dir, newName, byKey(op.FileKey)); err != nil {
		return fsx.Stat{}, err
	}
	placed, err := holds(target.Dir, newName, byKey(op.FileKey))
	if err != nil {
		return fsx.Stat{}, err
	}
	d.machine.Checkpoint("moved")
	if err := d.durable(target.Dir, newName); err != nil {
		return fsx.Stat{}, err
	}
	d.machine.Checkpoint("target-synced")
	if err := source.Sync(); err != nil {
		return fsx.Stat{}, lost(err)
	}
	d.machine.Checkpoint("source-synced")
	return placed, nil
}

// RemoveLeft removes, at its path, the copy a move of Atoll's own view left on a disk whose trash had no room for it
// (specs.md §21.2): an operation unlink with no path in the trash. The name must still hold that copy as row says, its
// inode, size, modification time and change time, looked at right before, as a move to the trash compares a copy
// (§17.1, step 4); ErrGone says that it does not, and it stays. The folder it leaves gets its time back, as Atoll's own
// operations give it, and is synced.
func (d *Disk) RemoveLeft(op *Op, row *catalog.KnownCopy) error {
	if err := d.Verify(); err != nil {
		return err
	}
	path := d.rootPlace(op.Path)
	parent, err := d.open(path[:len(path)-1])
	switch {
	case fsx.IsAbsent(err), errors.Is(err, ErrOtherMount):
		return fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return classify(err)
	}
	defer parent.close()
	name := path[len(path)-1]
	before, err := parent.Info()
	if err != nil {
		return classify(err)
	}
	if _, err := holds(parent.Dir, name, func(st *fsx.Stat) bool { return keyOf(st) == op.FileKey && untouched(row)(st) }); err != nil {
		return err
	}
	if err := parent.Unlink(name); err != nil {
		if fsx.IsAbsent(err) {
			return fmt.Errorf("%w: %w", ErrGone, err)
		}
		return classify(err)
	}
	d.machine.Checkpoint("unlinked")
	if err := restoreTime(parent.Dir, before.Mtime); err != nil {
		return err
	}
	if err := parent.Sync(); err != nil {
		return lost(err)
	}
	d.machine.Checkpoint("source-synced")
	return nil
}

// LeftAt says, for the reprise of an operation RemoveLeft runs, whether the copy it removes is gone from its path, once
// the folder of that path is synced, as a purge's reprise asks of the trash (specs.md §17.2): gone, it finished; the
// same inode, nothing happened; another file, nothing happened either, and what is there is not what it expected.
func (d *Disk) LeftAt(op *Op) (gone, other bool, err error) {
	if err := d.Verify(); err != nil {
		return false, false, err
	}
	path := d.rootPlace(op.Path)
	if err := d.syncFolder(path[:len(path)-1]); err != nil {
		return false, false, err
	}
	st, there, err := d.stat(path)
	switch {
	case err != nil:
		return false, false, err
	case !there:
		return true, false, nil
	}
	return false, st.Kind != fsx.KindFile || keyOf(&st) != op.FileKey, nil
}

// ErrNoLink says that the driver would not give the file another name: hard links it does not make, or too many of
// them already. Nothing was made, and a program copies instead (specs.md §21.2).
var ErrNoLink = errors.New("the driver makes no other name for this file")

// LinkUser gives the regular file at a logical path a second name at another, on this disk, after the folders missing
// on the way to the new one, and never over anything: a hard link a user asked for (specs.md §21.2). fileKey is the
// inode of the file, as a look right before found it. It says what is at the new name then.
func (d *Disk) LinkUser(from, to string, fileKey string, like Like) (fsx.Stat, error) {
	if err := d.Verify(); err != nil {
		return fsx.Stat{}, err
	}
	src := d.rootPlace(from)
	source, err := d.open(src[:len(src)-1])
	switch {
	case fsx.IsAbsent(err), errors.Is(err, ErrOtherMount):
		return fsx.Stat{}, fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return fsx.Stat{}, classify(err)
	}
	defer source.close()
	name := src[len(src)-1]
	if _, err := holds(source.Dir, name, byKey(fileKey)); err != nil {
		return fsx.Stat{}, err
	}
	target, newName, err := d.userParent(to, like)
	if err != nil {
		return fsx.Stat{}, err
	}
	defer target.close()
	err = source.Link(name, target.Dir, newName)
	switch {
	case fsx.IsExisting(err):
		return fsx.Stat{}, fmt.Errorf("%w: %w", ErrPathTaken, err)
	case fsx.IsNameRefused(err):
		return fsx.Stat{}, fmt.Errorf("%w: %w", ErrNameRefused, err)
	case fsx.IsDiskLost(err):
		return fsx.Stat{}, lost(err)
	case fsx.IsAbsent(err):
		return fsx.Stat{}, fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		// EPERM, EMLINK, EOPNOTSUPP: no such name on this driver, as across two filesystems.
		return fsx.Stat{}, fmt.Errorf("%w: %w", ErrNoLink, err)
	}
	// What a name made leads to first, then its folder: on NTFS a file keeps its names in its own record too (§17.1).
	// A name that no longer leads to that file was taken since by someone else's, and stays theirs.
	st, err := holds(target.Dir, newName, byKey(fileKey))
	if err != nil {
		return fsx.Stat{}, err
	}
	if err := d.durable(target.Dir, newName); err != nil {
		return fsx.Stat{}, err
	}
	return st, nil
}

// RemoveUser removes a folder that holds nothing, or a symbolic link, at a logical path: a regular file goes to the
// trash, never this way (specs.md §21.2).
func (d *Disk) RemoveUser(logical string, isFolder bool) error {
	if err := d.Verify(); err != nil {
		return err
	}
	path := d.rootPlace(logical)
	parent, err := d.open(path[:len(path)-1])
	switch {
	case fsx.IsAbsent(err), errors.Is(err, ErrOtherMount):
		return fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return classify(err)
	}
	defer parent.close()
	name := path[len(path)-1]
	st, err := parent.Stat(name)
	switch {
	case fsx.IsAbsent(err):
		return fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return classify(err)
	case isFolder && st.Kind != fsx.KindDir, !isFolder && st.Kind != fsx.KindSymlink:
		return ErrGone
	}
	if isFolder {
		err = parent.Rmdir(name)
	} else {
		err = parent.Unlink(name)
	}
	switch {
	case fsx.IsAbsent(err):
		return fmt.Errorf("%w: %w", ErrGone, err)
	case fsx.IsNotEmpty(err), fsx.IsExisting(err):
		return fmt.Errorf("%w: %w", ErrNotEmpty, err)
	case err != nil:
		return classify(err)
	}
	if err := parent.Sync(); err != nil {
		return lost(err)
	}
	return nil
}

// TouchParent gives the folder of a logical path the time now: what Atoll's own move to the trash keeps, a user's
// deletion through the view changes, as one on the disk would.
func (d *Disk) TouchParent(logical string, now fsx.Time) error {
	path := d.rootPlace(logical)
	parent, err := d.open(path[:len(path)-1])
	if err != nil {
		return classify(err)
	}
	defer parent.close()
	return restoreTime(parent.Dir, now)
}

// SetUserAttrs changes the owner, the mode, the size and the times of what is at a logical path, as a user asked:
// the owner first, which clears setuid and setgid; a symbolic link takes only an owner, and a folder no size. It says
// what is there then.
func (d *Disk) SetUserAttrs(logical string, a *UserAttrs) (fsx.Stat, error) {
	if err := d.Verify(); err != nil {
		return fsx.Stat{}, err
	}
	path := d.rootPlace(logical)
	parent, err := d.open(path[:len(path)-1])
	switch {
	case fsx.IsAbsent(err), errors.Is(err, ErrOtherMount):
		return fsx.Stat{}, fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return fsx.Stat{}, classify(err)
	}
	defer parent.close()
	name := path[len(path)-1]
	st, err := parent.Stat(name)
	switch {
	case fsx.IsAbsent(err):
		return fsx.Stat{}, fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return fsx.Stat{}, classify(err)
	}
	uid, gid := st.Uid, st.Gid
	if a.UID != nil {
		uid = *a.UID
	}
	if a.GID != nil {
		gid = *a.GID
	}
	switch st.Kind {
	case fsx.KindSymlink:
		if a.UID != nil || a.GID != nil {
			if err := owned(parent.Lchown(name, uid, gid), name); err != nil {
				return fsx.Stat{}, err
			}
		}
	case fsx.KindDir:
		err = d.setFolderAttrs(parent.Dir, name, a, uid, gid)
	case fsx.KindFile, fsx.KindOther:
		err = d.setFileAttrs(parent.Dir, name, a, uid, gid)
	}
	if err != nil {
		return fsx.Stat{}, err
	}
	after, err := parent.Stat(name)
	if err != nil {
		return fsx.Stat{}, classify(err)
	}
	return after, nil
}

func (d *Disk) setFileAttrs(parent fsx.Dir, name string, a *UserAttrs, uid, gid uint32) error {
	w, err := parent.OpenWritable(name)
	if err != nil {
		return classify(err)
	}
	defer func() { _ = w.Close() }()
	if a.UID != nil || a.GID != nil {
		if err := owned(w.SetOwner(uid, gid), name); err != nil {
			return err
		}
	}
	if a.Mode != nil {
		if err := w.SetMode(*a.Mode); err != nil {
			return classify(err)
		}
	}
	if a.Size != nil {
		if err := w.Truncate(*a.Size); err != nil {
			return classify(err)
		}
	}
	if a.Atime != nil || a.Mtime != nil {
		atime, mtime := fsx.OmitTime, fsx.OmitTime
		if a.Atime != nil {
			atime = *a.Atime
		}
		if a.Mtime != nil {
			mtime = *a.Mtime
		}
		if err := w.SetTimes(atime, mtime); err != nil {
			return classify(err)
		}
	}
	return nil
}

func (d *Disk) setFolderAttrs(parent fsx.Dir, name string, a *UserAttrs, uid, gid uint32) error {
	dir, err := parent.OpenDir(name)
	if err != nil {
		return classify(err)
	}
	defer func() { _ = dir.Close() }()
	if err := d.sameMount(dir); err != nil {
		return err
	}
	if a.UID != nil || a.GID != nil {
		if err := owned(dir.SetOwner(uid, gid), name); err != nil {
			return err
		}
	}
	if a.Mode != nil {
		if err := dir.SetMode(*a.Mode); err != nil {
			return classify(err)
		}
	}
	if a.Mtime != nil {
		if err := dir.SetMtime(*a.Mtime); err != nil {
			return classify(err)
		}
	}
	return nil
}

// StatUser looks at a logical path on this disk, as the view checks what is there before it acts there for a user
// (specs.md §21.2): "" is the root itself, the folder <root>/ of the disk.
func (d *Disk) StatUser(logical string) (fsx.Stat, bool, error) {
	if logical == "" {
		return d.stat(components(d.config.Root))
	}
	return d.StatRoot(logical)
}

// StatTrash looks at a place of the trash, as the reprise of an operation that sent a copy there looks at it.
func (d *Disk) StatTrash(trashPath string) (fsx.Stat, bool, error) {
	return d.stat(d.trashPlace(trashPath))
}

// ReadLinkRoot gives the target of the symbolic link at a logical path.
func (d *Disk) ReadLinkRoot(logical string) (string, error) {
	path := d.rootPlace(logical)
	parent, err := d.open(path[:len(path)-1])
	if err != nil {
		return "", classify(err)
	}
	defer parent.close()
	target, err := parent.ReadLink(path[len(path)-1])
	if err != nil {
		return "", classify(err)
	}
	return target, nil
}
