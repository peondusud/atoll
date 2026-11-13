package ops

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/layout"
)

// The parts of a create: a copy in progress in <workdir>/tmp/, and what a job does with it (specs.md §14.2).

// tmpPlace gives the place of a name in <workdir>/tmp/.
func (d *Disk) tmpPlace(name string) []string {
	return components(d.config.Workdir, layout.TmpFolder, name)
}

// Part is a copy in progress, open through its own handle.
type Part struct {
	fsx.WritableFile
	// Stat is what it was when it was opened.
	Stat fsx.Stat
}

// private closes a folder of Atoll's to every account but its owner's, one found open included: the folder of the
// parts, which an earlier version left open, and the links folder. A part carries its source's mode and owner while
// it is read again, what a repair saves there is a file's own bytes, and a link reaches a file whatever the folders
// above it in the root forbid: in a folder anyone may enter, a file that only its folders kept private would be open
// to all. A driver that holds no mode leaves the folder as it is.
func private(dir fsx.Dir) error {
	st, err := dir.Info()
	if err != nil {
		return classify(err)
	}
	if st.Mode&0o077 == 0 {
		return nil
	}
	switch err := dir.SetMode(st.Mode &^ 0o077); {
	case err == nil:
		return nil
	case fsx.IsDiskLost(err):
		return lost(err)
	default:
		slog.Warn("a folder of Atoll's stays open to other accounts", "error", err)
		return nil
	}
}

// MakePart makes the part an operation names: <workdir>/tmp/ where it is missing, each folder synced, then
// its parent, and closed to other accounts; the part with mode 0600, never over anything, synced, then its folder
// (specs.md §14.2).
func (d *Disk) MakePart(op *Op) (*Part, error) {
	if err := d.Verify(); err != nil {
		return nil, err
	}
	path := d.tmpPlace(op.Path2)
	dir, err := d.makeFolders(path[:len(path)-1], nil)
	if err != nil {
		return nil, err
	}
	defer dir.close()
	if err := private(dir.Dir); err != nil {
		return nil, err
	}
	name := path[len(path)-1]
	f, err := dir.CreateFile(name, 0o600)
	switch {
	case fsx.IsExisting(err):
		return nil, fmt.Errorf("%w: %w", ErrPathTaken, err)
	case err != nil:
		return nil, classify(err)
	}
	part, err := partOf(f)
	if err != nil {
		return nil, err
	}
	if err := part.Sync(); err != nil {
		_ = part.Close()
		return nil, lost(err)
	}
	if err := dir.Sync(); err != nil {
		_ = part.Close()
		return nil, lost(err)
	}
	return part, nil
}

// OpenPart opens the part an operation resumes, and gives ErrGone when it is not there.
func (d *Disk) OpenPart(op *Op) (*Part, error) {
	if err := d.Verify(); err != nil {
		return nil, err
	}
	path := d.tmpPlace(op.Path2)
	dir, err := d.open(path[:len(path)-1])
	switch {
	case fsx.IsAbsent(err), errors.Is(err, ErrOtherMount):
		return nil, fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return nil, classify(err)
	}
	defer dir.close()
	f, err := dir.OpenWritable(path[len(path)-1])
	switch {
	case fsx.IsUnreadableName(err):
		return nil, fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return nil, classify(err)
	}
	return partOf(f)
}

func partOf(f fsx.WritableFile) (*Part, error) {
	st, err := f.Info()
	if err != nil {
		_ = f.Close()
		return nil, classify(err)
	}
	return &Part{WritableFile: f, Stat: st}, nil
}

// OpenSource opens the copy a job reads, after a look that its name still holds what its row says
// (specs.md §14.2). ErrGone means it no longer does.
func (d *Disk) OpenSource(logical string, row *catalog.KnownCopy) (fsx.File, fsx.Stat, error) {
	if err := d.Verify(); err != nil {
		return nil, fsx.Stat{}, err
	}
	path := d.rootPlace(logical)
	dir, err := d.open(path[:len(path)-1])
	switch {
	case fsx.IsAbsent(err), errors.Is(err, ErrOtherMount):
		return nil, fsx.Stat{}, fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return nil, fsx.Stat{}, classify(err)
	}
	defer dir.close()
	name := path[len(path)-1]
	st, err := holds(dir.Dir, name, untouched(row))
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

// SourceStill says whether a copy's name still holds the file a job read, unchanged: its size,
// modification time, change time and inode (specs.md §14.2).
func (d *Disk) SourceStill(logical string, read *fsx.Stat) (bool, error) {
	st, there, err := d.stat(d.rootPlace(logical))
	switch {
	case err != nil:
		return false, err
	case !there || st.Kind != fsx.KindFile:
		return false, nil
	}
	return st.Ino == read.Ino && st.Size == read.Size && st.Mtime == read.Mtime && st.Ctime == read.Ctime, nil
}

// PlacePart moves a verified part to a logical path, whose parents are there by now, each an operation of
// its own: the rename without replacing, the file synced at its new name, then the target folder, whose
// time is restored, then the folder of the part (specs.md §14.2). ErrPathTaken with IsExisting means
// that something took the name since the look.
func (d *Disk) PlacePart(op *Op, logical string) (*Moved, error) {
	if err := d.Verify(); err != nil {
		return nil, err
	}
	src := d.tmpPlace(op.Path2)
	dst := d.rootPlace(logical)
	source, err := d.open(src[:len(src)-1])
	switch {
	case fsx.IsAbsent(err), errors.Is(err, ErrOtherMount):
		return nil, fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return nil, classify(err)
	}
	defer source.close()
	target, err := d.open(dst[:len(dst)-1])
	switch {
	case fsx.IsNotAFolder(err):
		return nil, fmt.Errorf("%w: %w", ErrPathTaken, err)
	case fsx.IsAbsent(err), errors.Is(err, ErrOtherMount):
		return nil, fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return nil, classify(err)
	}
	defer target.close()
	name, newName := src[len(src)-1], dst[len(dst)-1]
	seen, err := holds(source.Dir, name, byKey(op.FileKey))
	if err != nil {
		return nil, err
	}
	before, err := target.Info()
	if err != nil {
		return nil, classify(err)
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
	if err := restoreTime(target.Dir, before.Mtime); err != nil {
		return nil, err
	}
	if err := source.Sync(); err != nil {
		return nil, lost(err)
	}
	d.machine.Checkpoint("source-synced")
	return d.movedFrom(target.Dir, newName, &seen)
}

// What stands at a copy's target (specs.md §14.1).
const (
	TargetFree     = ""
	TargetTaken    = "taken"    // something is at the path itself: a conflict once copied
	TargetNoFolder = "nofolder" // a parent of the path is not a folder
	TargetRefused  = "refused"  // a name on the way is the short name of another entry, which no driver creates
)

// LookAtTarget says what stands at a logical path where a copy will land, and which of its parent folders
// are missing, from the top.
func (d *Disk) LookAtTarget(logical string) (string, []string, error) {
	parts := strings.Split(logical, "/")
	dir, err := d.open(components(d.config.Root))
	if err != nil {
		return TargetFree, nil, classify(err)
	}
	defer dir.close()
	for i, name := range parts {
		// On NTFS, what answers for a name is no proof that it is there, nor what does not that it may be made: the
		// short name of another entry is refused before anything is copied (specs.md §15.8).
		if alias, err := d.isAlias(dir.Dir, name); err != nil {
			return TargetFree, nil, err
		} else if alias {
			return TargetRefused, nil, nil
		}
		st, err := dir.Stat(name)
		switch {
		case fsx.IsAbsent(err):
			var missing []string
			for j := i; j < len(parts)-1; j++ {
				missing = append(missing, strings.Join(parts[:j+1], "/"))
			}
			return TargetFree, missing, nil
		case err != nil:
			return TargetFree, nil, classify(err)
		case i == len(parts)-1:
			return TargetTaken, nil, nil
		case st.Kind != fsx.KindDir:
			return TargetNoFolder, nil, nil
		}
		next, err := dir.OpenDir(name)
		if err != nil {
			return TargetFree, nil, classify(err)
		}
		dir.opened = append(dir.opened, next)
		dir.Dir = next
		if err := d.sameMount(next); err != nil {
			return TargetFree, nil, err
		}
	}
	return TargetFree, nil, nil
}

// DropPart removes a part, and is fine with one already gone.
func (d *Disk) DropPart(name string) error {
	if err := d.Verify(); err != nil {
		return err
	}
	path := d.tmpPlace(name)
	dir, err := d.open(path[:len(path)-1])
	switch {
	case fsx.IsAbsent(err), errors.Is(err, ErrOtherMount):
		return nil
	case err != nil:
		return classify(err)
	}
	defer dir.close()
	if err := dir.Unlink(path[len(path)-1]); err != nil && !fsx.IsAbsent(err) {
		return classify(err)
	}
	if err := dir.Sync(); err != nil {
		return lost(err)
	}
	return nil
}

// TmpNames gives the names <workdir>/tmp/ holds, none when it does not exist.
func (d *Disk) TmpNames() ([]string, error) {
	if err := d.Verify(); err != nil {
		return nil, err
	}
	place := d.tmpPlace("-")
	dir, err := d.open(place[:len(place)-1])
	switch {
	case fsx.IsAbsent(err), errors.Is(err, ErrOtherMount):
		return nil, nil
	case err != nil:
		return nil, classify(err)
	}
	defer dir.close()
	names, err := dir.Names()
	if err != nil {
		return nil, classify(err)
	}
	return names, nil
}

// CleanTmp removes the parts of <workdir>/tmp/ that an operation done or undone names and that nothing
// keeps: no job owns them, and no operation left planned names them. Nothing else there is touched
// (specs.md §14.2).
func (d *Disk) CleanTmp(named map[string]string, keep map[string]bool) error {
	if err := d.Verify(); err != nil {
		return err
	}
	place := d.tmpPlace("-")
	dir, err := d.open(place[:len(place)-1])
	switch {
	case fsx.IsAbsent(err), errors.Is(err, ErrOtherMount):
		return nil
	case err != nil:
		return classify(err)
	}
	defer dir.close()
	names, err := dir.Names()
	if err != nil {
		return classify(err)
	}
	removed := false
	for _, name := range names {
		state, ok := named[name]
		if !ok || keep[name] || state == "planned" {
			continue
		}
		if err := dir.Unlink(name); err != nil && !fsx.IsAbsent(err) {
			return classify(err)
		}
		removed = true
	}
	if removed {
		if err := dir.Sync(); err != nil {
			return lost(err)
		}
	}
	return nil
}
