package ops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/solver"
)

// The extended attributes of the user. namespace: those Atoll's own view writes on a user's behalf, on one disk, and
// a pass gives the other copies of a file (specs.md §21.2).

// ErrXattrRefused says that a copy's filesystem refused an extended attribute it was to take: it holds none, a name
// or a value too long for it, or no room for it. The copy keeps what it held of it (specs.md §21.2).
var ErrXattrRefused = errors.New("the filesystem refuses this extended attribute")

// UserXattrs gives the user. attributes of what is at a logical path, a file or a folder, with what a look finds of
// it. ErrGone means that nothing is there.
func (d *Disk) UserXattrs(logical string) (map[string][]byte, fsx.Stat, error) {
	path := d.rootPlace(logical)
	parent, err := d.open(path[:len(path)-1])
	switch {
	case fsx.IsAbsent(err), errors.Is(err, ErrOtherMount):
		return nil, fsx.Stat{}, fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return nil, fsx.Stat{}, classify(err)
	}
	defer parent.close()
	name := path[len(path)-1]
	st, err := parent.Stat(name)
	switch {
	case fsx.IsAbsent(err):
		return nil, fsx.Stat{}, fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return nil, fsx.Stat{}, classify(err)
	}
	attrs, err := parent.Xattrs(name, fsx.UserNamespace)
	switch {
	case fsx.IsAbsent(err):
		return nil, fsx.Stat{}, fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return nil, fsx.Stat{}, classify(err)
	}
	return attrs, st, nil
}

// SetUserXattr sets an extended attribute of what is at a logical path, a file or a folder, with the flags of
// setxattr(2), as a user asked through Atoll's own view, then makes it durable (specs.md §21.2). What the filesystem
// answers of the attribute, as EEXIST, ENODATA or ENOTSUP, passes as it is.
func (d *Disk) SetUserXattr(logical, attr string, value []byte, flags int) error {
	return d.changeUserXattr(logical, func(parent fsx.Dir, name string) error { return parent.SetXattr(name, attr, value, flags) })
}

// RemoveUserXattr removes an extended attribute of what is at a logical path, as SetUserXattr sets one.
func (d *Disk) RemoveUserXattr(logical, attr string) error {
	return d.changeUserXattr(logical, func(parent fsx.Dir, name string) error { return parent.RemoveXattr(name, attr) })
}

func (d *Disk) changeUserXattr(logical string, change func(parent fsx.Dir, name string) error) error {
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
	err = change(parent.Dir, name)
	switch {
	case fsx.IsDiskLost(err):
		return lost(err)
	case fsx.IsAbsent(err):
		return fmt.Errorf("%w: %w", ErrGone, err)
	case err != nil:
		return fmt.Errorf("%w", err)
	}
	return syncEntry(parent.Dir, name)
}

// GiveXattrs gives a copy the user. attributes of another, after a look that its name still holds what its row says:
// each it lacks or holds with another value is set, each the other does not hold removed, then the file is synced
// (specs.md §21.2). It says what it gave and what the file is then. ErrGone means that the name no longer holds that
// copy, and that nothing was written; ErrXattrRefused that its filesystem refused one, what came before it given.
func (d *Disk) GiveXattrs(row *catalog.KnownCopy, want map[string][]byte) (solver.XattrPlan, fsx.Stat, error) {
	f, _, err := d.OpenSource(row.Path, row)
	if err != nil {
		return solver.XattrPlan{}, fsx.Stat{}, err
	}
	// A file whose close failing changes nothing: what must be durable was synced.
	defer func() { _ = f.Close() }()
	have, err := f.Xattrs(fsx.UserNamespace)
	if err != nil {
		return solver.XattrPlan{}, fsx.Stat{}, classify(err)
	}
	plan := solver.PlanXattrs(want, have)
	var refused error
	for _, name := range plan.Remove {
		if err := f.RemoveXattr(name); err != nil && !fsx.IsNoXattr(err) {
			if refused = xattrError(err); !errors.Is(refused, ErrXattrRefused) {
				return plan, fsx.Stat{}, refused
			}
			break
		}
	}
	for _, name := range plan.Set {
		if refused != nil {
			break
		}
		if err := f.SetXattr(name, want[name], 0); err != nil {
			if refused = xattrError(err); !errors.Is(refused, ErrXattrRefused) {
				return plan, fsx.Stat{}, refused
			}
		}
	}
	if !plan.Empty() {
		if err := f.Sync(); err != nil {
			return plan, fsx.Stat{}, lost(err)
		}
		d.machine.Checkpoint("xattrs-set")
	}
	after, err := f.Info()
	if err != nil {
		return plan, fsx.Stat{}, classify(err)
	}
	return plan, after, refused
}

// xattrError says what an error giving an attribute means: a disk that cannot be relied on, a refusal of its
// filesystem, or anything else.
func xattrError(err error) error {
	switch {
	case fsx.IsDiskLost(err):
		return lost(err)
	case fsx.IsXattrRefused(err):
		return fmt.Errorf("%w: %w", ErrXattrRefused, err)
	}
	return fmt.Errorf("%w", err)
}

// recoverXattrs resolves an operation xattr left planned (specs.md §17.2): nothing says which of the attributes it gave
// reached the copy, so it is undone, and the pass that takes the view's operation in compares them again and gives
// what still differs; a copy that is no longer the one its row says writes an op-undone event, as it changed since.
func (d *Disk) recoverXattrs(ctx context.Context, db *sql.DB, op *Op, nowNS int64) error {
	_, _, same, err := d.metadataCopyNow(ctx, db, op)
	if err != nil {
		return err
	}
	return d.Undo(ctx, db, op, !same, nowNS)
}
