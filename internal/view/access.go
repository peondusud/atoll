package view

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"syscall"
)

// What Atoll's own view checks on a disk before it acts there for a caller (specs.md §21.2;
// conformance/vectors/view_access.json). Mounted with default_permissions, the view has the kernel check each caller
// against what it presents, from the catalog; then it acts with CAP_DAC_OVERRIDE, CAP_CHOWN and CAP_FOWNER. A disk
// can hold what an operation acts on otherwise, as after a chmod or a chown made directly on it that no pass has
// recorded yet: there the view checks the caller again, against what that disk holds, as Linux would check it there,
// so that it never lets a caller do on a disk more than the disk itself allows.

// Cap is a capability of Linux that passes a check of a file's permissions, numbered as Linux numbers it.
type Cap uint8

// The capabilities a check of permissions reads.
const (
	CapChown         Cap = 0 // CAP_CHOWN: gives any file to any account and group
	CapDACOverride   Cap = 1 // CAP_DAC_OVERRIDE: reads and writes any file, searches any folder
	CapDACReadSearch Cap = 2 // CAP_DAC_READ_SEARCH: reads any file, reads and searches any folder
	CapFowner        Cap = 3 // CAP_FOWNER: passes the checks that ask for a file's owner
	CapFsetid        Cap = 4 // CAP_FSETID: keeps setuid and setgid as it writes, and setgid of another group
)

// Caps is a set of capabilities, bit n for capability n, as /proc's CapEff gives them.
type Caps uint64

// Has says whether the set holds a capability.
func (c Caps) Has(one Cap) bool { return c&(1<<one) != 0 }

// Credentials are who asks an operation, as Linux checks it: the account and group it acts on files as, its other
// groups, and its effective capabilities.
type Credentials struct {
	UID, GID uint32
	Groups   []uint32
	Caps     Caps
}

func (c *Credentials) inGroup(gid uint32) bool { return gid == c.GID || slices.Contains(c.Groups, gid) }

// Attrs are what a check reads of what a path holds: its kind, its mode with setuid, setgid and sticky, and its owner.
type Attrs struct {
	Kind     NodeKind
	Mode     uint32
	UID, GID uint32
}

// AttrsOf gives what a node presents that a check reads.
func AttrsOf(n *Node) Attrs {
	return Attrs{Kind: n.Kind, Mode: n.Mode & 0o7777, UID: n.UID, GID: n.GID}
}

// AccessKind is what an operation asks of what it acts on.
type AccessKind string

// What an operation asks, each as Linux checks it.
const (
	// AccessWrite: to write a file, open or truncated by its name, or a folder's own entry, as a folder moved to
	// another folder writes it. A file that holds setuid, or setgid with the group's execution, a write takes it off.
	AccessWrite AccessKind = "write"
	// AccessXattr: to set or remove an extended attribute of the user. namespace.
	AccessXattr AccessKind = "xattr"
	// AccessCreate: to make a name in a folder, the folder being what it acts on.
	AccessCreate AccessKind = "create"
	// AccessDelete: to remove a name from a folder, or move it out of it: the folder and the sticky bit.
	AccessDelete AccessKind = "delete"
	// AccessChmod: to change the mode. A mode that changes nothing but takes setuid or setgid off is never refused, as
	// the kernel asks one of any writer of such a file.
	AccessChmod AccessKind = "chmod"
	// AccessChown: to change the owner or the group.
	AccessChown AccessKind = "chown"
	// AccessTimes: to give the times.
	AccessTimes AccessKind = "times"
	// AccessTouch: to set the times to the time now, as touch does.
	AccessTouch AccessKind = "touch"
	// AccessLink: to give a file another name, with fs.protected_hardlinks on.
	AccessLink AccessKind = "link"
)

// Access is one check of an operation on what one disk holds.
type Access struct {
	Kind AccessKind
	// Presented is what the operation acts on as the view presents it, which the kernel checked, and Disk what the
	// disk holds there: the file, the folder or the symbolic link, or, for AccessCreate, the folder a name is made in.
	Presented, Disk Attrs
	// PresentedFolder and DiskFolder are, for AccessDelete, the folder the name is removed from.
	PresentedFolder, DiskFolder Attrs
	// Mode is the mode AccessChmod asks. UID and GID are the owner and group AccessChown asks, nil for one it leaves;
	// and for AccessChmod the group given with the mode, if any.
	Mode     uint32
	UID, GID *uint32
}

// The bits of a mode a check reads beyond the permissions.
const (
	setuid   = 0o4000
	setgid   = 0o2000
	sticky   = 0o1000
	groupRun = 0o010
	mayRead  = 4
	mayWrite = 2
	mayRun   = 1
)

func same(x, y *Attrs) bool {
	return x.Mode&0o7777 == y.Mode&0o7777 && x.UID == y.UID && x.GID == y.GID
}

// Differs says whether the disk holds what the check reads otherwise than the view presents it: the mode, setuid,
// setgid and sticky included, or the owner or the group, of what the operation acts on, and for a deletion of its
// folder. Only then is the caller checked again: the kernel checked it already against what the view presents.
func (a *Access) Differs() bool {
	return !same(&a.Presented, &a.Disk) || a.Kind == AccessDelete && !same(&a.PresentedFolder, &a.DiskFolder)
}

// Check says what Linux would answer the caller on the disk: 0 when it allows the operation, EACCES or EPERM when it
// does not; and, for AccessChmod, the mode it applies. A disk that holds what the check reads as the view presents it
// allows what the kernel allowed already, and a chmod applies the mode asked.
func (a *Access) Check(who *Credentials) (syscall.Errno, uint32) {
	if !a.Differs() {
		return 0, a.Mode
	}
	d := &a.Disk
	switch a.Kind {
	case AccessWrite:
		if !permitted(who, d, mayWrite) {
			return syscall.EACCES, 0
		}
		// The kernel takes these off at the first write of a caller without CAP_FSETID, of the bits it presents; the
		// view writes with CAP_FSETID, which takes nothing off.
		if d.Kind == NodeFile && privileged(d.Mode)&^privileged(a.Presented.Mode) != 0 && !who.Caps.Has(CapFsetid) {
			return syscall.EPERM, 0
		}
	case AccessXattr:
		switch {
		case d.Kind != NodeFile && d.Kind != NodeDir:
			return syscall.EPERM, 0
		case d.Kind == NodeDir && d.Mode&sticky != 0 && !owns(who, d):
			return syscall.EPERM, 0
		case !permitted(who, d, mayWrite):
			return syscall.EACCES, 0
		}
	case AccessCreate:
		if !permitted(who, d, mayWrite|mayRun) {
			return syscall.EACCES, 0
		}
	case AccessDelete:
		f := &a.DiskFolder
		switch {
		case !permitted(who, f, mayWrite|mayRun):
			return syscall.EACCES, 0
		case f.Mode&sticky != 0 && d.UID != who.UID && f.UID != who.UID && !who.Caps.Has(CapFowner):
			return syscall.EPERM, 0
		}
	case AccessChmod:
		return chmod(a, who)
	case AccessChown:
		owner := who.UID == d.UID
		switch {
		case who.Caps.Has(CapChown):
		case a.UID != nil && (!owner || *a.UID != d.UID):
			return syscall.EPERM, 0
		case a.GID != nil && (!owner || *a.GID != d.GID && !who.inGroup(*a.GID)):
			return syscall.EPERM, 0
		}
	case AccessTimes:
		if !owns(who, d) {
			return syscall.EPERM, 0
		}
	case AccessTouch:
		if !owns(who, d) && !permitted(who, d, mayWrite) {
			return syscall.EACCES, 0
		}
	case AccessLink:
		if !owns(who, d) && !safeSource(who, d) {
			return syscall.EPERM, 0
		}
	}
	return 0, a.Mode
}

// chmod checks a change of mode: its owner or CAP_FOWNER, but for a mode that only takes setuid or setgid off; and
// setgid is taken off the mode asked when the caller is neither in the file's group, the one given with the mode if
// any, nor holds CAP_FSETID.
func chmod(a *Access, who *Credentials) (syscall.Errno, uint32) {
	d := &a.Disk
	asked := a.Mode & 0o7777
	if asked&^(setuid|setgid) == d.Mode&^(setuid|setgid)&0o7777 && asked&^d.Mode&(setuid|setgid) == 0 {
		return 0, asked
	}
	if !owns(who, d) {
		return syscall.EPERM, 0
	}
	gid := d.GID
	if a.GID != nil {
		gid = *a.GID
	}
	if !who.inGroup(gid) && !who.Caps.Has(CapFsetid) {
		asked &^= setgid
	}
	return 0, asked
}

// privileged gives the bits of a mode a write takes off: setuid, and setgid with the group's execution.
func privileged(mode uint32) uint32 {
	bits := mode & setuid
	if mode&(setgid|groupRun) == setgid|groupRun {
		bits |= setgid
	}
	return bits
}

// owns says whether the caller passes a check that asks for the owner: it is, or it holds CAP_FOWNER.
func owns(who *Credentials, a *Attrs) bool { return who.UID == a.UID || who.Caps.Has(CapFowner) }

// permitted checks the bits of the mode as Linux does, without ACLs: the owner's bits for the owner, the group's for a
// member of the group, the others' for any other; then the capabilities that pass them.
func permitted(who *Credentials, a *Attrs, mask uint32) bool {
	bits := a.Mode
	switch {
	case who.UID == a.UID:
		bits >>= 6
	case who.inGroup(a.GID):
		bits >>= 3
	}
	if mask&^bits&7 == 0 {
		return true
	}
	if a.Kind == NodeDir {
		// A folder is read and searched with CAP_DAC_READ_SEARCH, and anything with CAP_DAC_OVERRIDE.
		return mask&mayWrite == 0 && who.Caps.Has(CapDACReadSearch) || who.Caps.Has(CapDACOverride)
	}
	if mask == mayRead && who.Caps.Has(CapDACReadSearch) {
		return true
	}
	// A file is read and written with CAP_DAC_OVERRIDE, and run when anyone may run it.
	return (mask&mayRun == 0 || a.Mode&0o111 != 0) && who.Caps.Has(CapDACOverride)
}

// safeSource says whether fs.protected_hardlinks lets someone other than the owner link a file: a regular file, neither
// setuid nor setgid with the group's execution, that the caller may read and write.
func safeSource(who *Credentials, a *Attrs) bool {
	return a.Kind == NodeFile && privileged(a.Mode) == 0 && permitted(who, a, mayRead|mayWrite)
}

// ProcStatus is what the view reads of a thread from /proc/<pid>/task/<pid>/status (specs.md §21.2;
// conformance/vectors/proc_status.json): the user and group ids it acts on files as, the fourth field of Uid and Gid;
// its other groups; and its effective capabilities. UID is its real account, the first field of Uid, which the watcher
// tells the process behind an event by (§15.1).
type ProcStatus struct {
	UID          uint32
	FSUID, FSGID uint32
	Groups       []uint32
	CapEff       Caps
}

// ParseProcStatus reads the status of a thread, as /proc gives it.
func ParseProcStatus(data []byte) (*ProcStatus, error) {
	out := &ProcStatus{}
	seen := map[string]bool{}
	for line := range bytes.Lines(data) {
		key, value, ok := bytes.Cut(bytes.TrimSuffix(line, []byte("\n")), []byte(":"))
		name := string(key)
		if !ok || name != "Uid" && name != "Gid" && name != "Groups" && name != "CapEff" {
			continue
		}
		if seen[name] {
			return nil, fmt.Errorf("status: %s twice", name)
		}
		seen[name] = true
		fields := bytes.Fields(value)
		var err error
		switch name {
		case "Uid":
			if out.FSUID, err = fourth(fields); err == nil {
				out.UID, err = realID(fields)
			}
		case "Gid":
			out.FSGID, err = fourth(fields)
		case "Groups":
			out.Groups = make([]uint32, 0, len(fields))
			for _, f := range fields {
				g, perr := strconv.ParseUint(string(f), 10, 32)
				if perr != nil {
					return nil, fmt.Errorf("status: Groups: %w", perr)
				}
				out.Groups = append(out.Groups, uint32(g))
			}
		case "CapEff":
			if len(fields) != 1 {
				return nil, errors.New("status: CapEff: not one field")
			}
			var c uint64
			c, err = strconv.ParseUint(string(fields[0]), 16, 64)
			out.CapEff = Caps(c)
		}
		if err != nil {
			return nil, fmt.Errorf("status: %s: %w", name, err)
		}
	}
	for _, name := range []string{"Uid", "Gid", "Groups", "CapEff"} {
		if !seen[name] {
			return nil, fmt.Errorf("status: no %s", name)
		}
	}
	return out, nil
}

// realID gives the first of four ids, the real one.
func realID(fields [][]byte) (uint32, error) {
	id, err := strconv.ParseUint(string(fields[0]), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%w", err)
	}
	return uint32(id), nil
}

// fourth gives the fourth of four ids, the filesystem's.
func fourth(fields [][]byte) (uint32, error) {
	if len(fields) != 4 {
		return 0, fmt.Errorf("%d fields, want 4", len(fields))
	}
	id, err := strconv.ParseUint(string(fields[3]), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%w", err)
	}
	return uint32(id), nil
}
