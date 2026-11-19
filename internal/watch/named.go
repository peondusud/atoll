package watch

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/solver"
)

// Named is a name the watcher gave a pass to look at (specs.md §15.13): a logical path below the root, never the root
// itself, which only a walk of the whole disk looks at.
type Named struct {
	Path string
	// Folder says that an event named a folder there: the look walks whatever is below it.
	Folder bool
	// Deleted says that an event said the name was deleted: a copy no longer found at it, or below it, is gone.
	Deleted bool
}

// namedLook is what a look limited to names compares against: the names it read in the end, and what the events said
// of them.
type namedLook struct {
	names   []string
	deleted []string
}

// isDeleted says that an event said a name was deleted, the name of a copy or a folder above it.
func (n *namedLook) isDeleted(path string) bool {
	for _, d := range n.deleted {
		if path == d || strings.HasPrefix(path, d+"/") {
			return true
		}
	}
	return false
}

// lookNamed looks at the names a pass was given on one disk, and those of the inodes found there that the catalog
// holds elsewhere on it, and compares them with the rows at or below those names alone (specs.md §15.13). A file found
// at a name with no row, whose inode a row elsewhere holds, may have come from there: that name is looked at too, so
// that a rename whose old name no event gave is read as one, and not as a new file.
func lookNamed(ctx context.Context, db *sql.DB, c *config.Config, m disks.Machine, d *config.Disk, r *Report, look *Look, now int64) (*catalog.Walk, error) {
	root, err := m.OpenRoot(d.Mount, d.Root)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInterrupted, err)
	}
	opened, err := root.Info()
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("%w: %w", ErrInterrupted, err)
	}
	named := &namedLook{}
	given := map[string]*Named{}
	for i := range look.Names {
		n := look.Names[i]
		if n.Path == "" {
			continue
		}
		if was := given[n.Path]; was != nil {
			was.Folder, was.Deleted = was.Folder || n.Folder, was.Deleted || n.Deleted
			continue
		}
		given[n.Path] = &n
	}
	list := make([]Named, 0, len(given))
	for _, n := range given {
		list = append(list, *n)
		if n.Deleted {
			named.deleted = append(named.deleted, n.Path)
		}
	}
	slices.SortFunc(list, func(a, b Named) int { return strings.Compare(a.Path, b.Path) })
	w := &Walked{}
	read := &nameReader{root: root, top: &opened, w: w, entries: map[string]bool{}, limits: LimitsOf(c, d)}
	err = read.names(ctx, list)
	if err == nil {
		var pulled []Named
		if pulled, err = pullNames(ctx, db, d.ID, w, list); err == nil {
			err = read.names(ctx, pulled)
			list = append(list, pulled...)
		}
	}
	// Folders only read, whose close failing changes nothing that was read from them.
	_ = root.Close()
	if err != nil {
		return nil, err
	}
	if err := walkedTheDisk(m, d, &opened); err != nil {
		return nil, err
	}
	if err := withoutOwnLinks(m, d, w); err != nil {
		return nil, err
	}
	for i := range list {
		named.names = append(named.names, list[i].Path)
	}
	named.names = catalog.TopNames(named.names)
	r.Looked = named.names
	found, err := compareWalk(ctx, db, c, d, w, r, look, now, named)
	if err != nil {
		return nil, err
	}
	found.Names = named.names
	return found, nil
}

// pullNames gives the names the catalog holds elsewhere on the disk for what a look found at names with no row: the
// inode of a file, or of a folder, found where no row of it is. Each is looked at as the events' names are.
func pullNames(ctx context.Context, db *sql.DB, disk string, w *Walked, given []Named) ([]Named, error) {
	paths := make([]string, 0, len(given))
	for i := range given {
		paths = append(paths, given[i].Path)
	}
	rowAt := map[string]bool{}
	err := catalog.EachKnownCopyAt(ctx, db, disk, paths, func(k *catalog.KnownCopy) { rowAt[k.Path] = true })
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	folderAt := map[string]bool{}
	if err := catalog.EachFolderRowAt(ctx, db, disk, paths, func(e *catalog.EntryIdentity) { folderAt[e.Path] = true }); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	var fileKeys, folderKeys []string
	for i := range w.Files {
		if !rowAt[w.Files[i].Path] {
			fileKeys = append(fileKeys, w.Files[i].FileKey)
		}
	}
	for i := range w.Entries {
		if e := &w.Entries[i]; e.Kind == solver.EntryDir && e.FileKey != nil && !folderAt[e.Path] && atOrBelow(pathSet(paths), e.Path) {
			folderKeys = append(folderKeys, *e.FileKey)
		}
	}
	covered := pathSet(paths)
	var out []Named
	add := func(path string, folder bool) {
		if !atOrBelow(covered, path) {
			covered[path] = true
			out = append(out, Named{Path: path, Folder: folder})
		}
	}
	rows, err := catalog.KnownCopiesByKey(ctx, db, disk, fileKeys)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	for i := range rows {
		add(rows[i].Path, false)
	}
	folders, err := catalog.FolderRowsByKey(ctx, db, disk, folderKeys)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	for i := range folders {
		add(folders[i].Path, true)
	}
	slices.SortFunc(out, func(a, b Named) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// nameReader looks at names below a root, through handles opened from it one name at a time, never following a
// symbolic link.
type nameReader struct {
	root fsx.Dir
	top  *fsx.Stat
	w    *Walked
	// entries holds the folders and other entries already found, and walked the folders walked whole: a name below one
	// of these was read with it.
	entries map[string]bool
	walked  map[string]bool
	// limits are the disk's walks' (specs.md §15.8).
	limits Limits
}

// names looks at each name: a file alone, a folder whole below it, and the folder that holds it, whose entry Atoll's
// own view presents. A name that holds nothing gives nothing: what was there is no longer at it. A name the look cannot
// reach is reported, as a walk's is, and says nothing.
func (n *nameReader) names(ctx context.Context, list []Named) error {
	if n.walked == nil {
		n.walked = map[string]bool{}
	}
	for i := range list {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("look: %w", err)
		}
		if atOrBelow(n.walked, list[i].Path) {
			continue
		}
		if err := n.name(ctx, list[i].Path); err != nil {
			return err
		}
	}
	return nil
}

// name looks at one name, from the root one folder at a time, holding only the folder it is in: the one above it can
// go once it is open. A name past the limit of the disk's mergerfs branch is reported as such, and not read further,
// as a walk would leave it (specs.md §15.8).
func (n *nameReader) name(ctx context.Context, path string) error {
	parts := strings.Split(path, "/")
	dir, prefix := n.root, ""
	defer func() {
		if dir != n.root {
			// A folder only read, whose close failing changes nothing that was read from it.
			_ = dir.Close()
		}
	}()
	for i, part := range parts {
		at := prefix + part
		st, err := dir.Stat(part)
		switch {
		case fsx.IsAbsent(err):
			return nil
		case unreachable(err):
			n.w.skip(at, err)
			return nil
		case err != nil:
			return fmt.Errorf("%w: %q: %w", ErrInterrupted, at, err)
		case !n.limits.Fits(at):
			if !slices.Contains(n.w.TooLong, at) {
				n.w.TooLong = append(n.w.TooLong, at)
			}
			return nil
		}
		last := i == len(parts)-1
		if !last {
			// A name below a symbolic link or a file is no name of the library: a walk never follows one.
			if st.Kind != fsx.KindDir {
				return nil
			}
			if err := n.entry(at, &st); err != nil {
				return err
			}
			if st.MountID != n.top.MountID || st.Dev != n.top.Dev {
				if !slices.Contains(n.w.OtherMounts, at) {
					n.w.OtherMounts = append(n.w.OtherMounts, at)
				}
				return nil
			}
			below, err := dir.OpenDir(part)
			switch {
			case fsx.IsAbsent(err):
				return nil
			case unreachable(err):
				n.w.skip(at, err)
				return nil
			case err != nil:
				return fmt.Errorf("%w: %q: %w", ErrInterrupted, at, err)
			}
			if dir != n.root {
				_ = dir.Close()
			}
			dir, prefix = below, at+"/"
			continue
		}
		return n.leaf(ctx, dir, prefix, part, at, &st)
	}
	return nil
}

// entry adds the entry of a folder that holds a name, once: only the folder itself, none of what else it holds.
func (n *nameReader) entry(path string, st *fsx.Stat) error {
	if n.entries[path] {
		return nil
	}
	n.entries[path] = true
	mtime, err := solver.TimeNS(st.Mtime.Sec, st.Mtime.Nsec)
	if err != nil {
		return fmt.Errorf("%w: %q: %w", ErrInterrupted, path, err)
	}
	found := FoundEntry{Path: path, Kind: solver.EntryDir, Mode: int64(st.Mode), MtimeNS: mtime, Size: st.Size, UID: st.Uid, GID: st.Gid}
	if err := setIdentity(&found, st); err != nil {
		return fmt.Errorf("%w: %q: %w", ErrInterrupted, path, err)
	}
	n.w.Entries = append(n.w.Entries, found)
	return nil
}

// leaf looks at the name itself, as a walk would at that name, in the folder dir at prefix.
func (n *nameReader) leaf(ctx context.Context, dir fsx.Dir, prefix, name, path string, st *fsx.Stat) error {
	mtime, err := solver.TimeNS(st.Mtime.Sec, st.Mtime.Nsec)
	if err != nil {
		return fmt.Errorf("%w: %q: %w", ErrInterrupted, path, err)
	}
	switch st.Kind {
	case fsx.KindFile:
		file, err := foundFile(path, st, mtime)
		if err != nil {
			return fmt.Errorf("%w: %q: %w", ErrInterrupted, path, err)
		}
		n.w.Files = append(n.w.Files, file)
	case fsx.KindDir:
		n.walked[path] = true
		if n.entries[path] {
			// Found as the folder of another name first: its entry is there, and it is walked below now.
			n.w.Entries = slices.DeleteFunc(n.w.Entries, func(e FoundEntry) bool { return e.Path == path })
		}
		n.entries[path] = true
		found := FoundEntry{Path: path, Kind: solver.EntryDir, Mode: int64(st.Mode), MtimeNS: mtime, Size: st.Size, UID: st.Uid, GID: st.Gid}
		if err := setIdentity(&found, st); err != nil {
			return fmt.Errorf("%w: %q: %w", ErrInterrupted, path, err)
		}
		n.w.Entries = append(n.w.Entries, found)
		if st.MountID != n.top.MountID || st.Dev != n.top.Dev {
			n.w.OtherMounts = append(n.w.OtherMounts, path)
			return nil
		}
		k := newWalker(ctx, dir, strings.TrimSuffix(prefix, "/"), n.top, n.w, n.limits)
		return k.walkBelow(k.stack[0], name, path, st)
	case fsx.KindSymlink:
		target, err := dir.ReadLink(name)
		switch {
		case fsx.IsAbsent(err):
			return nil
		case unreachable(err):
			n.w.skip(path, err)
			return nil
		case err != nil:
			return fmt.Errorf("%w: %q: %w", ErrInterrupted, path, err)
		}
		n.w.Entries = append(n.w.Entries, FoundEntry{Path: path, Kind: solver.EntrySymlink, Mode: int64(st.Mode), MtimeNS: mtime, Target: &target,
			Size: st.Size, UID: st.Uid, GID: st.Gid})
	case fsx.KindOther:
		n.w.Entries = append(n.w.Entries, FoundEntry{Path: path, Kind: solver.EntryIgnored, Mode: int64(st.Mode), MtimeNS: mtime, Size: st.Size, UID: st.Uid, GID: st.Gid})
	}
	return nil
}
