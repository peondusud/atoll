package ops

import (
	"fmt"
	"slices"
	"strings"

	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
)

// ErrNameCollides says that, on an NTFS disk, a name Atoll was to make, or a folder on the way to it, has the lpath_key
// of another name of its folder, whoever made that one, which Windows reads as the same name (specs.md §15.7). It is a
// name refused: nothing was written.
var ErrNameCollides = fmt.Errorf("%w: another name of its folder has its key", ErrNameRefused)

// collidesIn lists a folder of an NTFS disk that a name is made in, goes through or stops at: whether it lists that
// name with its bytes, and the names it lists with its key and other bytes, but but, the name a case-only rename
// leaves, in byte order. The disk is the truth here, a name no walk recorded yet included: the catalog's keys answer
// for the plan, and this, right before, for each name Atoll makes on that disk (specs.md §15.7; conformance/models/
// README.md, flaws 24 and 25, the maintainer's remedy disk-key-look, 2026-11-25). A listing that fails says the disk
// unknown, never that nothing collides (invariant 4).
func (d *Disk) collidesIn(dir fsx.Dir, name, but string) (listed bool, with []string, err error) {
	all, err := dir.Names()
	if err != nil {
		return false, nil, classify(err)
	}
	return slices.Contains(all, name), names.Colliding(name, all, but), nil
}

// refuseColliding refuses, on an NTFS disk, a name Atoll is about to make in a folder, a folder, a file's name or a
// rename's new name, when the folder lists another with its key: ErrNameCollides, before anything is written. On
// another disk it lists nothing: ext4, XFS and btrfs hold both names (specs.md §15.7).
func (d *Disk) refuseColliding(dir fsx.Dir, name, but string) error {
	if d.config.FS != names.NTFS {
		return nil
	}
	_, with, err := d.collidesIn(dir, name, but)
	if err != nil {
		return err
	}
	if len(with) > 0 {
		return fmt.Errorf("%w: %q beside %q", ErrNameCollides, name, with[0])
	}
	return nil
}

// lookThrough says, on an NTFS disk, whether a look may take what it finds at a name of a folder, one it goes through
// or stops at, for that name: ErrNameCollides when the folder lists another name with its key, as a write below it
// would collide there, and a driver that finds a name by its case would give the other one; ErrNameRefused when the
// folder does not list it while a look at it, or at it in capitals, finds an entry, the short name of another, which no
// driver lists, each finds in capitals, ntfs3 in any case, and ntfs-3g makes beside it in small letters, a name Windows
// reads as the same (docs/research/ntfs-driver.md). So no existence there is decided by a look alone (specs.md §15.7,
// §15.8). One listing serves both; a short name is looked for only in its own form. but is the name a case-only
// rename leaves there. nil on another disk.
func (d *Disk) lookThrough(dir fsx.Dir, name, but string) error {
	if d.config.FS != names.NTFS {
		return nil
	}
	listed, with, err := d.collidesIn(dir, name, but)
	switch {
	case err != nil:
		return err
	case len(with) > 0:
		return fmt.Errorf("%w: %q beside %q", ErrNameCollides, name, with[0])
	case listed || !names.ShortForm(name):
		return nil
	}
	for _, n := range []string{name, strings.ToUpper(name)} {
		switch _, err := dir.Stat(n); {
		case err == nil:
			return fmt.Errorf("%w: %q is the short name of another entry", ErrNameRefused, name)
		case !fsx.IsAbsent(err):
			return classify(err)
		}
	}
	return nil
}

// leftBy gives the name a rename from one logical path to another leaves in the folder of its new name, old, its own
// name, when both are in one folder: a case-only rename does not collide with its own old name (specs.md §15.7). ""
// when they are in two.
func leftBy(from, to, old string) string {
	if from != "" && from[:max(strings.LastIndexByte(from, '/'), 0)] == to[:max(strings.LastIndexByte(to, '/'), 0)] {
		return old
	}
	return ""
}

// lastName gives the last name of a logical path.
func lastName(logical string) string {
	return logical[strings.LastIndexByte(logical, '/')+1:]
}
