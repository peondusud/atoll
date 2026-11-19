package writer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/view"
)

// What the view checks on a disk before it acts there for a caller (specs.md §21.2; view.Access): the kernel checked
// the caller against what the view presents; where the disk the operation reaches holds what it acts on otherwise,
// as after a chmod or a chown made directly on that disk, the caller is checked again against what that disk holds.
// Each check looks at a disk the operation reaches anyway, before anything is journaled, and wakes no other.

// Caller is who asks an operation, as the kernel names it: the account and group what it makes is given to, and the
// thread that asks, whose other groups and capabilities /proc gives when a check on a disk reads them.
type Caller struct {
	Owner
	// PID is the thread that asks, 0 when the kernel names none.
	PID uint32
	// Creds, when set, are its other groups and its capabilities, as a test gives them, its account and group being
	// the Owner's; otherwise /proc gives them, when a check needs them.
	Creds *view.Credentials
}

// Asked is who asks a change of attributes, and how, which the kernel's checks of it read.
type Asked struct {
	Caller
	// Open says that it asks through a file it opened for writing, which a truncation needs nothing more of.
	Open bool
	// Now says that the times it gives are the time now, as touch gives them, which write access allows.
	Now bool
}

// asker is who asks an operation, and its credentials once a check read them.
type asker struct {
	who   Caller
	creds *view.Credentials
}

func asking(who Caller) *asker { return &asker{who: who} }

// credentials gives the caller's credentials, read once.
func (a *asker) credentials() *view.Credentials {
	if a.creds == nil {
		if a.who.Creds != nil {
			c := *a.who.Creds
			c.UID, c.GID = a.who.UID, a.who.GID
			a.creds = &c
		} else {
			a.creds = credentialsOf(a.who)
		}
	}
	return a.creds
}

// credentialsOf reads from /proc the credentials of the thread that asks: its other groups, and its capabilities when
// it runs in the view's own user namespace, where the kernel takes them as they are (proc_status.json). When /proc
// gives none of that, or another account than the kernel named, as a thread gone since, the caller is its account
// and group alone, the strictest it can be.
func credentialsOf(who Caller) *view.Credentials {
	out := &view.Credentials{UID: who.UID, GID: who.GID}
	if who.PID == 0 {
		return out
	}
	pid := strconv.FormatUint(uint64(who.PID), 10)
	data, err := fsx.ReadFile("/proc/" + pid + "/task/" + pid + "/status")
	if err != nil {
		return out
	}
	st, err := view.ParseProcStatus(data)
	if err != nil || st.FSUID != who.UID || st.FSGID != who.GID {
		return out
	}
	out.Groups = st.Groups
	theirs, err := fsx.ReadFile("/proc/" + pid + "/uid_map")
	if err != nil {
		return out
	}
	ours, err := fsx.ReadFile("/proc/self/uid_map")
	if err == nil && bytes.Equal(theirs, ours) {
		out.Caps = st.CapEff
	}
	return out
}

// protectedHardlinks says whether fs.protected_hardlinks is on, as systemd sets it; on, too, when it cannot be read.
func protectedHardlinks() bool {
	data, err := fsx.ReadFile("/proc/sys/fs/protected_hardlinks")
	return err != nil || strings.TrimSpace(string(data)) != "0"
}

// allowed runs a check of an operation on what a disk holds: what the kernel allowed already where nothing differs,
// and otherwise what Linux would allow there, EACCES or EPERM when it would not. It gives the mode a chmod applies.
func (w *Writer) allowed(a *asker, disk, path string, acc *view.Access) (uint32, error) {
	if !acc.Differs() {
		return acc.Mode, nil
	}
	refused, mode := acc.Check(a.credentials())
	if refused != 0 {
		w.log.Info("the view refuses what a disk refuses: it holds it otherwise than the catalog says, until a pass records it",
			"disk", disk, "path", path, "asks", string(acc.Kind), "uid", a.who.UID, "error", refused.Error())
		return 0, fmt.Errorf("%s on disk %s: %w", acc.Kind, disk, refused)
	}
	return mode, nil
}

// held gives what a disk holds at a logical path, as a check reads it, "" for the root; false when it holds nothing.
func held(d *ops.Disk, path string) (view.Attrs, bool, error) {
	st, found, err := d.StatUser(path)
	if err != nil || !found {
		return view.Attrs{}, false, err //nolint:wrapcheck // ops says what failed
	}
	return view.AttrsOf(view.NodeOfStat(d.ID, &st, "")), true, nil
}

// presented gives what the view presents at a logical path, as a check reads it.
func (w *Writer) presented(ctx context.Context, path string) (view.Attrs, error) {
	n, err := w.tree.Lookup(ctx, path)
	switch {
	case err != nil:
		return view.Attrs{}, fmt.Errorf("%w", err)
	case n == nil:
		return view.Attrs{}, ErrNotFound
	}
	return view.AttrsOf(n), nil
}

// mayCreate checks the folder a new name is made in, on the disk it is made on, when that disk holds it: a folder it
// lacks is made as the view presents it.
func (w *Writer) mayCreate(ctx context.Context, a *asker, disk, path string) error {
	dir, _ := split(path)
	folder, err := w.presented(ctx, dir)
	if err != nil {
		return err
	}
	return w.handles.Use(disk, func(d *ops.Disk) error {
		h, found, err := held(d, dir)
		if err != nil || !found || h.Kind != view.NodeDir {
			return err
		}
		_, err = w.allowed(a, disk, dir, &view.Access{Kind: view.AccessCreate, Presented: folder, Disk: h})
		return err
	})
}

// mayDelete checks the removal of a name from its folder on a disk that holds it, as a deletion or a rename asks it.
func (w *Writer) mayDelete(ctx context.Context, a *asker, disk, path string, n *view.Node) error {
	dir, _ := split(path)
	folder, err := w.presented(ctx, dir)
	if err != nil {
		return err
	}
	return w.handles.Use(disk, func(d *ops.Disk) error {
		entry, found, err := held(d, path)
		if err != nil || !found {
			return err
		}
		parent, found, err := held(d, dir)
		if err != nil || !found {
			return err
		}
		_, err = w.allowed(a, disk, path, &view.Access{Kind: view.AccessDelete, Presented: view.AttrsOf(n), Disk: entry,
			PresentedFolder: folder, DiskFolder: parent})
		return err
	})
}

// mayLink checks a hard link on h, the disk of the file, which the caller holds: the file at from, as
// fs.protected_hardlinks asks, and the folder the new name is made in, when that disk holds it.
func (w *Writer) mayLink(ctx context.Context, a *asker, h *ops.Disk, from string, n *view.Node, st *fsx.Stat, to string) error {
	source := &view.Access{Kind: view.AccessLink, Presented: view.AttrsOf(n), Disk: view.AttrsOf(view.NodeOfStat(h.ID, st, ""))}
	if source.Differs() && protectedHardlinks() {
		if _, err := w.allowed(a, h.ID, from, source); err != nil {
			return err
		}
	}
	dir, _ := split(to)
	folder, err := w.presented(ctx, dir)
	if err != nil {
		return err
	}
	f, found, err := held(h, dir)
	if err != nil || !found || f.Kind != view.NodeDir {
		return err
	}
	_, err = w.allowed(a, h.ID, dir, &view.Access{Kind: view.AccessCreate, Presented: folder, Disk: f})
	return err
}

// mayRename checks a rename on each disk it reaches: the name removed from its folder; the new name made in its
// folder, or, onto a name, that one removed from it; and a folder moved to another folder, which writes its own entry.
func (w *Writer) mayRename(ctx context.Context, a *asker, acting []string, from, to string, n, t *view.Node) error {
	fromDir, _ := split(from)
	toDir, _ := split(to)
	for _, id := range acting {
		err := w.mayDelete(ctx, a, id, from, n)
		switch {
		case err != nil:
		case t != nil:
			err = w.mayDelete(ctx, a, id, to, t)
		default:
			err = w.mayCreate(ctx, a, id, to)
		}
		if err == nil && n.Kind == view.NodeDir && fromDir != toDir {
			err = w.mayOn(a, id, from, n, view.AccessWrite)
		}
		if err != nil && !errors.Is(err, ErrNoHandle) {
			return err
		}
	}
	return nil
}

// mayOn checks an operation on what a disk holds at a path, as the view presents it there, when the disk holds
// something there; kind gives the check of what it holds.
func (w *Writer) mayOn(a *asker, disk, path string, n *view.Node, kind view.AccessKind) error {
	return w.handles.Use(disk, func(d *ops.Disk) error {
		h, found, err := held(d, path)
		if err != nil || !found {
			return err
		}
		_, err = w.allowed(a, disk, path, &view.Access{Kind: kind, Presented: view.AttrsOf(n), Disk: h})
		return err
	})
}

// mayChange checks a change of attributes on what a disk holds, each as the kernel checks it, and gives the change
// to make there: the mode without setgid when the caller may not keep it there.
func (w *Writer) mayChange(a *asker, disk, path string, n *view.Node, change *ops.UserAttrs, how *Asked) (*ops.UserAttrs, error) {
	out := *change
	err := w.handles.Use(disk, func(d *ops.Disk) error {
		h, found, err := held(d, path)
		if err != nil || !found {
			return err
		}
		check := func(acc *view.Access) (uint32, error) {
			acc.Presented, acc.Disk = view.AttrsOf(n), h
			return w.allowed(a, disk, path, acc)
		}
		if change.Size != nil && !how.Open {
			if _, err := check(&view.Access{Kind: view.AccessWrite}); err != nil {
				return err
			}
		}
		if change.UID != nil || change.GID != nil {
			if _, err := check(&view.Access{Kind: view.AccessChown, UID: change.UID, GID: change.GID}); err != nil {
				return err
			}
		}
		if change.Mode != nil {
			mode, err := check(&view.Access{Kind: view.AccessChmod, Mode: *change.Mode, GID: change.GID})
			if err != nil {
				return err
			}
			out.Mode = &mode
		}
		if change.Atime != nil || change.Mtime != nil {
			kind := view.AccessTimes
			if how.Now {
				kind = view.AccessTouch
			}
			if _, err := check(&view.Access{Kind: kind}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
