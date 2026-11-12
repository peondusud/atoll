package ops

import "fmt"

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

// ResolveTrash decides for a copy going to the trash. changed says that the file both names lead to
// is no longer what its row says, by size or modification time, as link moved its change time.
func ResolveTrash(target, source Seen, changed bool) (Decision, error) {
	if err := knownSeen(target, source); err != nil {
		return Decision{}, err
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
