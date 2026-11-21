package ops

import (
	"fmt"

	"github.com/peondusud/atoll/internal/catalog"
)

// Seen says what a name holds, compared with what an operation moved or made.
type Seen string

// What a name can hold.
const (
	SeenAbsent Seen = "absent"
	SeenSame   Seen = "same"
	SeenOther  Seen = "other"
)

// Decision is what a reprise does with an operation left planned (specs.md §17.2).
type Decision struct {
	Finish       bool // otherwise it is undone
	RemoveSource bool // the source's name must go: link was interrupted
	RemoveTarget bool // the target's name must go: the file was edited in place since
	MoveBack     bool // the file at the target goes back: it is not what the look found
	Warn         bool // what was found is not what an operation of Atoll leaves
}

// ResolveTrash decides for a copy going to the trash, from what each name holds by the copy's inode. changed says that
// the file at the target is no longer what the copy's row says, by size or modification time, as link and the move
// moved its change time (specs.md §17.2). With the source holding that inode too, they are one file with two names,
// edited in place since: the name in the trash goes. Alone, it is not the copy, whatever has its inode number, as a
// filesystem gives the inode of a file just removed to the next one made: a file that took the copy's name before the
// move, or the copy edited then, which goes back where it was, or stays where it is when that name is taken.
func ResolveTrash(target, source Seen, changed bool) (Decision, error) {
	if err := knownSeen(target, source); err != nil {
		return Decision{}, err
	}
	if target == SeenSame && source != SeenSame && changed {
		target = SeenOther
	}
	switch {
	case target == SeenSame && source == SeenSame && changed:
		return Decision{RemoveTarget: true}, nil
	case target == SeenSame:
		return Decision{Finish: true, RemoveSource: source == SeenSame}, nil
	case target == SeenAbsent && source == SeenSame:
		return Decision{}, nil
	case target == SeenOther && source == SeenAbsent:
		return Decision{MoveBack: true, Warn: true}, nil
	default:
		return Decision{Warn: true}, nil
	}
}

// ResolveRestore decides for an entry going back to its path.
func ResolveRestore(target, source Seen) (Decision, error) {
	if err := knownSeen(target, source); err != nil {
		return Decision{}, err
	}
	switch {
	case target == SeenSame:
		return Decision{Finish: true, RemoveSource: source == SeenSame}, nil
	case target == SeenAbsent && source == SeenSame:
		return Decision{}, nil
	default:
		return Decision{Warn: true}, nil
	}
}

// ResolveRename decides for a rename on a disk: as for a restore, as the target is a logical path that
// anyone can write to, so what is found there stays (specs.md §17.2).
func ResolveRename(target, source Seen) (Decision, error) {
	return ResolveRestore(target, source)
}

// ResolveUnlink decides for a purge, from what the file in the trash holds.
func ResolveUnlink(source Seen) (Decision, error) {
	if err := knownSeen(source); err != nil {
		return Decision{}, err
	}
	switch source {
	case SeenAbsent:
		return Decision{Finish: true}, nil
	case SeenSame:
		return Decision{}, nil
	case SeenOther:
	}
	return Decision{Warn: true}, nil
}

// ResolveRmdir decides for a folder removed below the root, from what its name holds: same says that it is the folder,
// by inode (specs.md §17.2, §17.5).
func ResolveRmdir(source Seen) (Decision, error) {
	return ResolveUnlink(source)
}

// ResolveMkdir decides for a folder made below the root: same says that it is a folder.
func ResolveMkdir(target Seen) (Decision, error) {
	if err := knownSeen(target); err != nil {
		return Decision{}, err
	}
	switch target {
	case SeenSame:
		return Decision{Finish: true}, nil
	case SeenAbsent:
		return Decision{}, nil
	case SeenOther:
	}
	return Decision{Warn: true}, nil
}

func knownSeen(list ...Seen) error {
	for _, s := range list {
		if s != SeenAbsent && s != SeenSame && s != SeenOther {
			return fmt.Errorf("reprise: %q", s)
		}
	}
	return nil
}

// CreatedRow says what the row of a copy made records, once a look at its name found it there, right after its
// rename or in a reprise (specs.md §14.2): what its part was when it was verified, so that a write into it once its name
// is in the library reads as an edit at the next look. Only the change time is the look's, as the rename moved it, and
// only when the look found nothing else moved; otherwise it is the part's, which the next look finds moved, and later
// than the row says, as a modification and not a conflict (§15.4).
func CreatedRow(verified, found *catalog.PartLook) catalog.PartLook {
	row := *verified
	if onlyCtimeMoved(verified, found) {
		row.CtimeNS = found.CtimeNS
	}
	return row
}

// RecoveredRow says what the row of a copy a reprise finds at its name records, and whether it is recorded unverified
// (specs.md §14.2): placed is the change time the job recorded right after the rename, in a commit of its own, and nil
// when the crash came between the rename and that commit. With it, a change time found other than it is a write since
// the rename, which the row reads as an edit at the next look: the row is the part's, whose change time the look finds
// moved. Without it, a write that moved only the change time cannot be told from the rename: the row is what CreatedRow
// gives, unverified when the change time found is not the one the part was verified with, so that a mismatch read later
// is a conflict, never a corruption a repair would overwrite. A job an earlier version verified recorded no look: what is
// found, unverified.
func RecoveredRow(verified *catalog.PartLook, placed *int64, found *catalog.PartLook) (catalog.PartLook, bool) {
	switch {
	case verified == nil:
		return *found, true
	case placed == nil:
		return CreatedRow(verified, found), found.CtimeNS != verified.CtimeNS
	case found.CtimeNS != *placed:
		return *verified, false
	}
	return CreatedRow(verified, found), false
}

// onlyCtimeMoved says whether two looks at one inode differ in nothing but their change time. A creation time on one
// alone is a difference.
func onlyCtimeMoved(a, b *catalog.PartLook) bool {
	sameBirth := (a.BtimeNS == nil) == (b.BtimeNS == nil) && (a.BtimeNS == nil || *a.BtimeNS == *b.BtimeNS)
	return sameBirth && a.Size == b.Size && a.MtimeNS == b.MtimeNS && a.Mode == b.Mode && a.UID == b.UID && a.GID == b.GID &&
		a.Nlink == b.Nlink
}
