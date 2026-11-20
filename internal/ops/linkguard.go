package ops

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/layout"
)

// ErrLastName says that a link of the guard is the last name of its data, which removing it would lose: it stays,
// and goes to the trash instead when nothing else names that data (specs.md §17).
var ErrLastName = errors.New("the link is the last name of its data")

// linksPlace is the links folder of the disk (specs.md §6, §17).
func (d *Disk) linksPlace() []string {
	return components(d.config.Workdir, layout.LinksFolder)
}

// linkPlace is where the link of a file sits.
func (d *Disk) linkPlace(fileID int64) []string {
	return append(d.linksPlace(), strconv.FormatInt(fileID, 10))
}

// LinkEntry is an entry of the links folder, and what a look found there that the guard uses: whether it is a regular
// file, the inode it leads to, and its size. No more of the look is kept: a disk has a link for each of its copies, and
// they are all held while they are decided (specs.md §26).
type LinkEntry struct {
	Name string
	File bool
	Ino  uint64
	Size int64
}

// Links reads the links folder: each entry, and what it leads to, by name in byte order. A disk with no such folder
// has none.
func (d *Disk) Links() ([]LinkEntry, error) {
	list, own, err := d.links()
	if err == nil {
		d.ownMu.Lock()
		d.own = own
		d.ownMu.Unlock()
	}
	return list, err
}

// links reads the links folder, and what leads to each inode.
func (d *Disk) links() ([]LinkEntry, map[uint64][]int64, error) {
	dir, err := d.open(d.linksPlace())
	switch {
	case fsx.IsAbsent(err):
		return nil, map[uint64][]int64{}, nil
	case err != nil:
		return nil, nil, classify(err)
	}
	defer dir.close()
	names, err := dir.Names()
	if err != nil {
		return nil, nil, classify(fmt.Errorf("list the links: %w", err))
	}
	list := make([]LinkEntry, 0, len(names))
	own := make(map[uint64][]int64, len(names))
	for _, name := range names {
		st, err := dir.Stat(name)
		switch {
		case fsx.IsAbsent(err):
			continue
		case err != nil:
			return nil, nil, classify(err)
		}
		list = append(list, LinkEntry{Name: name, File: st.Kind == fsx.KindFile, Ino: st.Ino, Size: st.Size})
		if id, ok := layout.LinkID(name); ok && st.Kind == fsx.KindFile {
			own[st.Ino] = append(own[st.Ino], id)
		}
	}
	slices.SortFunc(list, func(a, b LinkEntry) int { return cmp.Compare(a.Name, b.Name) })
	return list, own, nil
}

// HighestLinkID gives the highest file id the links folder names, 0 when there is none: a new catalog starts its files
// past it, so that an old link never names a new file (specs.md §9, §17).
func (d *Disk) HighestLinkID() (int64, error) {
	dir, err := d.open(d.linksPlace())
	switch {
	case fsx.IsAbsent(err):
		return 0, nil
	case err != nil:
		return 0, classify(err)
	}
	defer dir.close()
	names, err := dir.Names()
	if err != nil {
		return 0, classify(fmt.Errorf("list the links: %w", err))
	}
	highest := int64(0)
	for _, name := range names {
		if id, ok := layout.LinkID(name); ok {
			highest = max(highest, id)
		}
	}
	return highest, nil
}

// errLinksHidden says that another mount covers the links folder: the links below it are there still, and cannot be
// counted. It does not wrap ErrOtherMount, which says that nothing moved: what cannot count them may come after a move.
var errLinksHidden = errors.New("another mount covers the links folder, and hides the links below it")

// ownLinks gives the files whose link of the guard leads to an inode: names of Atoll's, which no count of a file's names
// includes (specs.md §17). The links folder is read once a session; the guard's own operations keep the count.
func (d *Disk) ownLinks(ino uint64) ([]int64, error) {
	d.ownMu.Lock()
	defer d.ownMu.Unlock()
	if err := d.readOwn(); err != nil {
		return nil, err
	}
	return slices.Clone(d.own[ino]), nil
}

// readOwn reads the links folder, unless this session did, ownMu held. A folder that is not there holds no link; one
// that cannot be read is never taken for one with no link, which would let a purge go on while a link keeps the purged
// data (invariant 4): a disk's error is the disk lost, and any other says nothing, for the caller to stop on. Nothing
// of a read that failed is kept, so the next look reads the folder again.
func (d *Disk) readOwn() error {
	if d.own != nil {
		return nil
	}
	_, own, err := d.links()
	switch {
	case errors.Is(err, ErrOtherMount):
		return fmt.Errorf("the guard's links cannot be counted: %w", errLinksHidden)
	case err != nil:
		return fmt.Errorf("the guard's links cannot be counted: %w", err)
	}
	d.own = own
	return nil
}

// forget says that a link of the guard is gone.
func (d *Disk) forget(ino uint64, fileID int64) {
	d.ownMu.Lock()
	defer d.ownMu.Unlock()
	if d.own != nil {
		d.own[ino] = slices.DeleteFunc(d.own[ino], func(id int64) bool { return id == fileID })
	}
}

// OwnLinksOf gives the files whose link of the guard leads to an entry of the trash, which must have another name for
// one to: Atoll sent that data there itself, and a purge removes those links first (specs.md §17).
func (d *Disk) OwnLinksOf(trashPath, fileKey string) ([]int64, error) {
	path := d.trashPlace(trashPath)
	parent, err := d.open(path[:len(path)-1])
	switch {
	case fsx.IsAbsent(err):
		return nil, nil
	case err != nil:
		return nil, classify(err)
	}
	defer parent.close()
	st, err := parent.Stat(path[len(path)-1])
	switch {
	case fsx.IsAbsent(err):
		return nil, nil
	case err != nil:
		return nil, classify(err)
	case st.Kind != fsx.KindFile || keyOf(&st) != fileKey || st.Nlink < 2:
		// With one name, the entry is all there is: no link leads to it, and the folder need not be read.
		return nil, nil
	}
	return d.ownLinks(st.Ino)
}

// withoutOwn takes the guard's links out of what a look counts of a file's names. A file with one name has none of
// them, and spares reading the folder. A folder that cannot be read fails the look (readOwn).
func (d *Disk) withoutOwn(st *fsx.Stat) error {
	if st.Kind != fsx.KindFile || st.Nlink < 2 {
		return nil
	}
	own, err := d.ownLinks(st.Ino)
	if err != nil {
		return err
	}
	if n := uint32(len(own)); n > 0 {
		st.Nlink -= min(n, st.Nlink-1)
	}
	return nil
}

// LinksFolder is the links folder, open to make or remove links in it, made if need be: only on a disk confirmed in this
// session, with the mode 0700, as each link reaches a file whatever the folders above it in the root forbid.
type LinksFolder struct {
	dir *folder
}

// Close lets go of the folder.
func (f *LinksFolder) Close() {
	f.dir.close()
}

// OpenLinks opens the links folder, and makes it, and the work folder, when create says so and they are missing.
// Without create, a disk with no links folder gives nil.
func (d *Disk) OpenLinks(create bool) (*LinksFolder, error) {
	if err := d.Verify(); err != nil {
		return nil, err
	}
	place := d.linksPlace()
	dir, err := d.open(place)
	if err == nil {
		// Found open, it is closed before a link is made in it. A run that makes none writes nothing.
		if create {
			if err := private(dir.Dir); err != nil {
				dir.close()
				return nil, err
			}
		}
		return &LinksFolder{dir: dir}, nil
	}
	if !fsx.IsAbsent(err) {
		return nil, classify(err)
	}
	if !create {
		return nil, nil //nolint:nilnil // no folder, and none wanted
	}
	parent, err := d.makeFolders(place[:len(place)-1], nil)
	if err != nil {
		return nil, err
	}
	defer parent.close()
	name := place[len(place)-1]
	switch err := parent.Mkdir(name, 0o700); {
	case err == nil:
		if err := syncEntry(parent.Dir, name); err != nil {
			return nil, err
		}
		if err := parent.Sync(); err != nil {
			return nil, lost(err)
		}
	case !fsx.IsExisting(err):
		return nil, classify(err)
	}
	dir, err = d.open(place)
	if err != nil {
		return nil, classify(err)
	}
	// One that came there since the look above, made by someone else, is closed too.
	if err := private(dir.Dir); err != nil {
		dir.close()
		return nil, err
	}
	return &LinksFolder{dir: dir}, nil
}

// Sync makes the links made or removed in the folder durable.
func (f *LinksFolder) Sync() error {
	if err := f.dir.Sync(); err != nil {
		return lost(err)
	}
	return nil
}

// MakeLink makes the link of a copy, an operation kind = link: the name at op.Path must hold op.FileKey, and the link
// is made without replacing anything. It says what the rows of the file's names must record then, as the link moved its
// change time. ErrGone says that the name no longer holds that copy, and ErrPathTaken that the link's name is taken;
// nothing was made then.
func (d *Disk) MakeLink(op *Op, links *LinksFolder) (*catalog.Links, error) {
	name := strconv.FormatInt(op.Link, 10)
	src := d.rootPlace(op.Path)
	source, err := d.open(src[:len(src)-1])
	switch {
	case fsx.IsAbsent(err), errors.Is(err, ErrOtherMount):
		return nil, fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return nil, classify(err)
	}
	defer source.close()
	from := src[len(src)-1]
	before, err := holds(source, from, byKey(op.FileKey))
	if err != nil {
		return nil, err
	}
	// The links already made are read before this one is, so that a folder that cannot be read stops it before anything
	// is made: what the names of its file record takes them out (specs.md §17).
	d.ownMu.Lock()
	err = d.readOwn()
	d.ownMu.Unlock()
	if err != nil {
		return nil, err
	}
	switch err := source.Link(from, links.dir.Dir, name); {
	case err == nil:
	case fsx.IsAbsent(err):
		return nil, fmt.Errorf("%w: %w", ErrGone, err)
	case fsx.IsExisting(err):
		return nil, fmt.Errorf("%w: %w", ErrPathTaken, err)
	default:
		return nil, classify(err)
	}
	st, err := links.dir.Stat(name)
	if err != nil {
		return nil, classify(err)
	}
	if keyOf(&st) != op.FileKey {
		// The copy's name took another file between the look and the link. That one has its name in the root still,
		// so the link that leads to it goes; were it its last name, it stays, for the next pass to send to the trash.
		if st.Nlink >= 2 {
			if err := links.dir.Unlink(name); err != nil && !fsx.IsAbsent(err) {
				return nil, classify(err)
			}
		}
		return nil, fmt.Errorf("%w: the copy's name holds inode %d", ErrGone, st.Ino)
	}
	d.machine.Checkpoint("linked")
	if err := syncEntry(links.dir.Dir, name); err != nil {
		return nil, err
	}
	d.ownMu.Lock()
	if d.own != nil {
		d.own[st.Ino] = append(d.own[st.Ino], op.Link)
	}
	d.ownMu.Unlock()
	// Its own row records the change time too, which the link moved: what it had just before says whether it holds a
	// change nobody has looked at. The folder was read above, so this reads nothing; were it to, the link is made, and
	// only a reprise can say what its names record.
	if err := d.withoutOwn(&st); err != nil {
		return nil, lost(err)
	}
	left := linksFrom(&st)
	if left != nil {
		left.PriorNS = timeOf(before.Ctime)
	}
	return left, nil
}

// DropLink removes the link of a file, an operation kind = unlink with a link: it must lead to op.FileKey, which must
// have another name. It gives the change time the file had just before, as removing a name moves it for the others. A
// link already gone is fine. ErrGone says that it leads to another file, and ErrLastName that it is its data's last
// name; it stays then.
func (d *Disk) DropLink(op *Op, links *LinksFolder) (sql.NullInt64, error) {
	name := strconv.FormatInt(op.Link, 10)
	st, err := links.dir.Stat(name)
	switch {
	case fsx.IsAbsent(err):
		return sql.NullInt64{}, nil
	case err != nil:
		return sql.NullInt64{}, classify(err)
	case st.Kind != fsx.KindFile || keyOf(&st) != op.FileKey:
		return sql.NullInt64{}, ErrGone
	case st.Nlink < 2:
		return sql.NullInt64{}, ErrLastName
	}
	if err := links.dir.Unlink(name); err != nil && !fsx.IsAbsent(err) {
		return sql.NullInt64{}, classify(err)
	}
	d.machine.Checkpoint("unlinked")
	d.forget(st.Ino, op.Link)
	return timeOf(st.Ctime), nil
}

// RemoveLinksFolder removes the links folder once it is empty, as the guard turned off leaves it. One that is not
// empty, or anything else that keeps it, leaves it there: an empty folder of Atoll's harms nothing.
func (d *Disk) RemoveLinksFolder() {
	place := d.linksPlace()
	parent, err := d.open(place[:len(place)-1])
	if err != nil {
		return
	}
	defer parent.close()
	if err := parent.Rmdir(place[len(place)-1]); err == nil {
		// A folder that comes back after a crash is empty, and goes at the next pass.
		_ = parent.Sync()
	}
}

// LinkToTrash moves the link of a file to the trash, an operation kind = trash with a link, as ToTrash moves a copy
// (specs.md §17.1): its data lost every name Atoll knows of in the root. The folders of the target stand for the
// root and the folders of the logical path, as they do for a copy: each is made like the one it stands for, while
// that one is still there, and otherwise private, and the file's owner's.
func (d *Disk) LinkToTrash(op *Op) (*Moved, error) {
	if err := d.Verify(); err != nil {
		return nil, err
	}
	src := d.linkPlace(op.Link)
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
	held, err := holds(source, name, byKey(op.FileKey))
	if err != nil {
		return nil, err
	}
	like, err := d.likeLogical(op.Path, dst, &held)
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
	seen, err := holds(source, name, byKey(op.FileKey))
	if err != nil {
		return nil, err
	}
	if err := d.moveNoReplace(source.Dir, name, target.Dir, newName, byKey(op.FileKey)); err != nil {
		return nil, err
	}
	if _, err := holds(target.Dir, newName, byKey(op.FileKey)); err != nil {
		return nil, err
	}
	d.machine.Checkpoint("moved")
	if err := syncEntry(target.Dir, newName); err != nil {
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
	d.forget(seen.Ino, op.Link)
	return d.movedFrom(target.Dir, newName, &seen)
}

// likeLogical gives, for each level of a target in the trash that stands for the root or a folder of a logical path,
// the mode and owner of that folder below the root while it is still there. What the guard sends to the trash may
// have lost its folders, and nothing says any more how open one that went was: it is private in the trash, and so
// is every one below it, with the owner of the file. The last folder on the way that is still there may be open to
// every account, as the root is, and a private folder must not come back open (specs.md §17.1).
func (d *Disk) likeLogical(logical string, dst []string, file *fsx.Stat) (map[int]fsx.Stat, error) {
	parts := strings.Split(logical, "/")
	folders := len(parts) - 1
	// The last levels of dst are the operation's folder, the folders of the logical path, then its name.
	first := len(dst) - 2 - folders
	root, err := d.open(components(d.config.Root))
	if err != nil {
		return nil, classify(err)
	}
	defer root.close()
	last, err := root.Info()
	if err != nil {
		return nil, classify(err)
	}
	like := map[int]fsx.Stat{first: last}
	at := root.Dir
	var opened []fsx.Dir
	defer func() {
		for _, o := range opened {
			_ = o.Close()
		}
	}()
	for i := range folders {
		if at != nil {
			next, err := at.OpenDir(parts[i])
			switch {
			case err == nil:
				opened = append(opened, next)
				st, err := next.Info()
				if err != nil {
					return nil, classify(err)
				}
				at, last = next, st
			case fsx.IsAbsent(err), fsx.IsNotAFolder(err):
				at, last = nil, fsx.Stat{Kind: fsx.KindDir, Mode: 0o700, Uid: file.Uid, Gid: file.Gid}
			default:
				return nil, classify(err)
			}
		}
		like[first+1+i] = last
	}
	return like, nil
}

// recoverMakeLink resolves a link left planned: made, it is done; not there, or another file there, nothing of it
// happened. Either way nothing else depends on it: the next pass looks at the links again.
func (d *Disk) recoverMakeLink(ctx context.Context, db *sql.DB, op *Op) error {
	seen, st, err := d.seen(d.linkPlace(op.Link), op)
	if err != nil {
		return err
	}
	if seen != SeenSame {
		return d.endLink(ctx, db, op, nil, false)
	}
	// What the file had just before is gone with the crash: its names take what it has now, as after any reprise.
	return d.endLink(ctx, db, op, linksFrom(&st), true)
}

// endLink ends an operation on a link, done with what the names of its file record, or undone.
func (d *Disk) endLink(ctx context.Context, db *sql.DB, op *Op, links *catalog.Links, done bool) error {
	var err error
	if done {
		err = catalog.EndLinks(ctx, db, op.Disk, []catalog.LinkEnd{{ID: op.ID, Links: links}}, nil)
	} else {
		err = catalog.EndLinks(ctx, db, op.Disk, nil, []int64{op.ID})
	}
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	return nil
}

// recoverDropLink resolves a removal of a link left planned: gone, it is done; still there, or another file there, it
// did not happen.
func (d *Disk) recoverDropLink(ctx context.Context, db *sql.DB, op *Op) error {
	seen, _, err := d.seen(d.linkPlace(op.Link), op)
	if err != nil {
		return err
	}
	if seen != SeenAbsent {
		return d.endLink(ctx, db, op, nil, false)
	}
	links, err := d.LinksLeft(ctx, db, op.FileKey, sql.NullInt64{})
	if err != nil {
		return err
	}
	return d.endLink(ctx, db, op, links, true)
}
