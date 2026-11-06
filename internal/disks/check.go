package disks

import (
	"context"
	"fmt"
	"slices"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/names"
)

// State is what a mount check concludes about a disk (specs.md §4, §10). The other disk states
// come from elsewhere: standby from SMART, draining, failed and retired from the catalog.
type State string

// The states a check gives.
const (
	Online   State = "online"
	Readonly State = "readonly"
	Missing  State = "missing"
)

// Reason says why a disk is not online, from a closed list (specs.md §10, §29.3).
type Reason string

// The reasons of a disk that is missing.
const (
	NotMounted      Reason = "not-mounted"
	IOError         Reason = "io-error"
	PartialMount    Reason = "partial-mount"
	WrongFilesystem Reason = "wrong-filesystem"
	DeviceGone      Reason = "device-gone"
	DeviceUnread    Reason = "device-unreadable"
	WrongIdentifier Reason = "wrong-identifier"
	NoRoot          Reason = "no-root"
)

// The reasons of a disk that is read-only.
const (
	UnsupportedDriver Reason = "unsupported-driver"
	ADS               Reason = "ads"
	NoCase            Reason = "nocase"
	IOCharset         Reason = "iocharset"
	IgnoreCase        Reason = "ignore-case"
	Casefold          Reason = "casefold"
	ASCIICI           Reason = "ascii-ci"
	ReadOnlyMount     Reason = "read-only-mount"
)

// Verdict is the result of a mount check. Reason and Message are empty on a disk that is online.
type Verdict struct {
	State   State
	Reason  Reason
	Message string
}

// PathAnswer is what the mount path gave.
type PathAnswer string

// The answers about a mount path.
const (
	PathOK      PathAnswer = "ok"
	PathAbsent  PathAnswer = "absent"
	PathIOError PathAnswer = "io-error"
)

// DeviceAnswer is what the device gave.
type DeviceAnswer string

// The answers about a device.
const (
	DeviceOK         DeviceAnswer = "ok"
	DeviceGoneAway   DeviceAnswer = "gone"
	DeviceUnreadable DeviceAnswer = "unreadable"
)

// FolderAnswer is what the managed root gave.
type FolderAnswer string

// The answers about the managed root.
const (
	FolderOK         FolderAnswer = "ok"
	FolderAbsent     FolderAnswer = "absent"
	FolderNotAFolder FolderAnswer = "not-a-folder"
	FolderOtherMount FolderAnswer = "other-mount"
	FolderIOError    FolderAnswer = "io-error"
)

// MountSeen answers questions 1 and 2. Entry is nil when the table has no entry for the mount id
// of the path.
type MountSeen struct {
	Path  PathAnswer
	Entry *MountEntry
	Note  string
}

// DeviceSeen answers questions 5 to 7. Identity is nil when the head holds no such filesystem.
type DeviceSeen struct {
	Device   DeviceAnswer
	Identity *Identity
	Note     string
}

// RootSeen answers question 10, and whether the root also answers in the other case.
type RootSeen struct {
	Folder    FolderAnswer
	OtherCase bool
	Note      string
}

// Observer answers what a check asks of a machine. A check asks in order and stops at the first
// failure, so that nothing past it is read.
type Observer interface {
	Mount(mount string) MountSeen
	Device(fs names.Filesystem, entry *MountEntry) DeviceSeen
	Root(mount, root string) RootSeen
}

// Machine is what the rest of Atoll asks of the machine it runs on, about its disks and the time.
// A conformance build answers from a control file (specs.md §29.4), and the delivered binary has
// none of that code.
type Machine interface {
	Observer
	// Now is the time, in Unix nanoseconds.
	Now() int64
	// Space says how large a disk is, and what a program may fill.
	Space(mount string) (capacity, free int64, err error)
	// OpenRoot opens the managed root of a disk that passed its check.
	OpenRoot(mount, root string) (fsx.Dir, error)
	// OpenTop opens the top of a disk that passed its check, for what writes to it.
	OpenTop(mount string) (fsx.Dir, error)
	// ReadPastCache answers questions 5 to 7 again, past the page cache (specs.md §10).
	ReadPastCache(fs names.Filesystem, entry *MountEntry) DeviceSeen
	// Checkpoint marks a point of an operation, where a conformance build may stop at once as
	// kill -9 would (specs.md §29.4). It does nothing in the delivered binary.
	Checkpoint(point string)
}

// CheckAll checks each disk, in the order given.
func CheckAll(ctx context.Context, observer Observer, list []config.Disk) ([]Verdict, error) {
	verdicts := make([]Verdict, 0, len(list))
	for i := range list {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("mount check: %w", err)
		}
		verdicts = append(verdicts, Check(observer, &list[i]))
	}
	return verdicts, nil
}

// Check runs the mount check of specs.md §10 on a disk.
func Check(observer Observer, d *config.Disk) Verdict {
	seen := observer.Mount(d.Mount)
	switch seen.Path {
	case PathOK:
	case PathAbsent:
		return missing(NotMounted, "the mount path does not exist")
	case PathIOError:
		return missing(IOError, "the mount path does not answer: "+seen.Note)
	}
	e := seen.Entry
	switch {
	case e == nil:
		return missing(NotMounted, "nothing is mounted at that path")
	case e.MountPoint != d.Mount:
		return missing(NotMounted, fmt.Sprintf("the mount is at %q: write that path, not one through a symlink", e.MountPoint))
	case e.Root != "/":
		return missing(PartialMount, fmt.Sprintf("this mount shows the folder %q, not the top of the filesystem", e.Root))
	case !typeAllowed(d.FS, e.FSType):
		return missing(WrongFilesystem, fmt.Sprintf("the type there is %q, for a disk of %s", e.FSType, d.FS))
	}
	device := observer.Device(d.FS, e)
	switch device.Device {
	case DeviceOK:
	case DeviceGoneAway:
		return missing(DeviceGone, fmt.Sprintf("its device %q is gone: %s", e.Source, device.Note))
	case DeviceUnreadable:
		return missing(DeviceUnread, fmt.Sprintf("its device %q cannot be read, which takes root or the disk group: %s", e.Source, device.Note))
	}
	switch {
	case device.Identity == nil:
		return missing(WrongFilesystem, fmt.Sprintf("its device %q holds no %s", e.Source, d.FS))
	case device.Identity.ID != d.UUID:
		return missing(WrongIdentifier, fmt.Sprintf("another disk is mounted there: %s", device.Identity.ID))
	case slices.Contains(e.SuperOptions, "shutdown"):
		return missing(IOError, "the filesystem has shut down")
	}
	root := observer.Root(d.Mount, d.Root)
	switch root.Folder {
	case FolderOK:
	case FolderAbsent:
		return missing(NoRoot, fmt.Sprintf("it has no folder %q", d.Root))
	case FolderNotAFolder:
		return missing(NoRoot, fmt.Sprintf("%q is not a folder", d.Root))
	case FolderOtherMount:
		return missing(NoRoot, fmt.Sprintf("%q is another mount", d.Root))
	case FolderIOError:
		return missing(IOError, fmt.Sprintf("%q does not answer: %s", d.Root, root.Note))
	}
	return judgeWrites(e, device.Identity.Variant, root.OtherCase, d.Root)
}

// Confirm runs a mount check, then reads the head past the page cache, so that a disk whose answers
// are still cached does not pass (specs.md §10). A session does this once per disk.
func Confirm(m Machine, d *config.Disk) Verdict {
	verdict := Check(m, d)
	if verdict.State == Missing {
		return verdict
	}
	seen := m.Mount(d.Mount)
	if seen.Entry == nil {
		return missing(NotMounted, "nothing is mounted at that path")
	}
	device := m.ReadPastCache(d.FS, seen.Entry)
	switch device.Device {
	case DeviceOK:
	case DeviceGoneAway:
		return missing(DeviceGone, fmt.Sprintf("its device %q is gone: %s", seen.Entry.Source, device.Note))
	case DeviceUnreadable:
		return missing(DeviceUnread, fmt.Sprintf("its device %q does not answer: %s", seen.Entry.Source, device.Note))
	}
	switch {
	case device.Identity == nil:
		return missing(WrongFilesystem, fmt.Sprintf("its device %q holds no %s", seen.Entry.Source, d.FS))
	case device.Identity.ID != d.UUID:
		return missing(WrongIdentifier, fmt.Sprintf("another disk is mounted there: %s", device.Identity.ID))
	}
	return verdict
}

func missing(reason Reason, message string) Verdict {
	return Verdict{State: Missing, Reason: reason, Message: message}
}

func readOnly(reason Reason, message string) Verdict {
	return Verdict{State: Readonly, Reason: reason, Message: message}
}

// typeAllowed says whether the mount table's type may hold a disk of fs.
func typeAllowed(fs names.Filesystem, fstype string) bool {
	switch fs {
	case names.Ext4:
		return fstype == "ext4"
	case names.XFS:
		return fstype == "xfs"
	case names.Btrfs:
		return fstype == "btrfs"
	case names.NTFS:
		return fstype == "ntfs3" || fstype == "fuseblk" || fstype == "ntfs"
	default:
		return false
	}
}

// judgeWrites decides whether Atoll may write to a disk it has identified: the second table of
// specs.md §10, in its order.
func judgeWrites(e *MountEntry, variant Variant, otherCase bool, root string) Verdict {
	ntfs3 := e.FSType == "ntfs3"
	switch {
	case e.FSType == "ntfs":
		return readOnly(UnsupportedDriver, "mounted with a driver whose name rules were never measured: use ntfs3 or ntfs-3g")
	case ntfs3 && slices.Contains(e.SuperOptions, "ads"):
		return readOnly(ADS, "ntfs3 with ads can destroy another file: remount with ads=0")
	case ntfs3 && slices.Contains(e.SuperOptions, "nocase"):
		return readOnly(NoCase, "ntfs3 with nocase changes name rules: remount without it")
	case ntfs3 && !slices.Contains(e.SuperOptions, "iocharset=utf8"):
		return readOnly(IOCharset, "ntfs3 without iocharset=utf8 does not keep names as they are: remount with it")
	case variant == VariantCasefold:
		return readOnly(Casefold, "this ext4 has the casefold feature, which changes name rules")
	case variant == VariantASCIICI:
		return readOnly(ASCIICI, "this XFS has the ascii-ci feature, which changes name rules")
	case otherCase:
		return readOnly(IgnoreCase, fmt.Sprintf("%q also answers in the other case: remount without ignore_case", root))
	case slices.Contains(e.MountOptions, "ro"), slices.Contains(e.SuperOptions, "ro"), slices.Contains(e.SuperOptions, "emergency_ro"):
		return readOnly(ReadOnlyMount, "it is mounted read-only")
	default:
		return Verdict{State: Online}
	}
}
