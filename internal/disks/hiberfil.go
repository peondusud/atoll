package disks

import (
	"sync"

	"github.com/peondusud/atoll/internal/fsx"
)

// HiberfilName is the file Windows hibernates into, at the top of the volume it starts from, as Fast Startup does too
// (specs.md §10, §22).
const HiberfilName = "hiberfil.sys"

// HiberfilHeader is how much of its start says whether Windows is hibernated: its first 4 KiB.
const HiberfilHeader = 4096

// HiberfilAnswer is what the top of an NTFS disk's mount gave at hiberfil.sys.
type HiberfilAnswer string

// The answers about hiberfil.sys.
const (
	// HiberfilAbsent: no such name.
	HiberfilAbsent HiberfilAnswer = "absent"
	// HiberfilFile: a regular file, whose size and start were read.
	HiberfilFile HiberfilAnswer = "file"
	// HiberfilNotAFile: a folder, a symbolic link or anything else at that name.
	HiberfilNotAFile HiberfilAnswer = "not-a-file"
	// HiberfilIOError: an error that says the disk cannot be relied on, as EIO (invariant 4).
	HiberfilIOError HiberfilAnswer = "io-error"
	// HiberfilUnreadable: any other error, as a permission refused: nothing can be told.
	HiberfilUnreadable HiberfilAnswer = "unreadable"
)

// HiberfilSeen answers the question of a hibernated NTFS volume (specs.md §10): what hiberfil.sys at the top of the
// mount holds. Head is what a read of its start gave, its first 4 KiB or fewer.
type HiberfilSeen struct {
	File HiberfilAnswer
	Size int64
	Head []byte
	Note string
}

// Hibernated says whether what was seen stands for Windows hibernated, by the rule of the source of the kernel's driver
// of the type ntfs (check_windows_hibernation_status): a hiberfil.sys smaller than 4 KiB, or whose first 4 KiB are not
// all zeros, as the magic hibr or HIBR of a hibernation; anything else at that name is read by its size, and so as
// hibernated. All zeros is what Windows leaves once it resumed. An answer that is no reading says nothing.
func (s HiberfilSeen) Hibernated() bool {
	switch s.File {
	case HiberfilFile:
		if s.Size < HiberfilHeader || len(s.Head) < HiberfilHeader {
			return true
		}
		for _, b := range s.Head[:HiberfilHeader] {
			if b != 0 {
				return true
			}
		}
		return false
	case HiberfilNotAFile:
		return true
	case HiberfilAbsent, HiberfilIOError, HiberfilUnreadable:
	}
	return false
}

// settled says whether an answer holds for as long as its mount does: a reading, not an error.
func (s HiberfilSeen) settled() bool {
	return s.File == HiberfilAbsent || s.File == HiberfilFile || s.File == HiberfilNotAFile
}

// judgeHiberfil makes a verdict of what hiberfil.sys gave: online when it says nothing of a hibernation. An error that
// says the disk cannot be relied on makes it missing, never a file absent (invariant 4); any other leaves whether Windows
// is hibernated unknown, and the disk is written to no more until a read tells.
func judgeHiberfil(s HiberfilSeen) Verdict {
	switch {
	case s.File == HiberfilIOError:
		return missing(IOError, HiberfilName+" at its top does not answer: "+s.Note)
	case s.File == HiberfilAbsent, s.settled() && !s.Hibernated():
		return Verdict{State: Online}
	case s.File == HiberfilNotAFile:
		return readOnly(Hibernated, HiberfilName+" at its top is not a file, which the kernel's NTFS driver takes for Windows hibernated: "+
			"rename it if Windows did not make it, then mount the disk again")
	case s.settled():
		return readOnly(Hibernated, "Windows left this volume hibernated, as Fast Startup does, and what is written to it is lost when "+
			"Windows resumes: start Windows and shut it down fully, or turn Fast Startup off, then mount it again")
	default:
		return readOnly(Hibernated, "whether Windows left this volume hibernated cannot be told, as "+HiberfilName+" at its top cannot be read: "+s.Note)
	}
}

// ReadHiberfil looks at hiberfil.sys through a handle on the top of a disk's mount, and writes nothing: its size and the
// first 4 KiB, read without changing its access time where the process may (specs.md §10).
func ReadHiberfil(top fsx.Dir) HiberfilSeen {
	size, head, err := fsx.ReadStart(top, HiberfilName, HiberfilHeader)
	switch {
	case err == nil:
		return HiberfilSeen{File: HiberfilFile, Size: size, Head: head}
	case fsx.IsAbsent(err):
		return HiberfilSeen{File: HiberfilAbsent}
	case fsx.IsUnreadableName(err):
		// A symbolic link, which is not followed, a folder, or anything else that is no regular file.
		return HiberfilSeen{File: HiberfilNotAFile, Note: err.Error()}
	default:
		return hiberfilError(err)
	}
}

// hiberfilError is what an error says of hiberfil.sys: the disk unknown, or nothing.
func hiberfilError(err error) HiberfilSeen {
	if fsx.IsDiskLost(err) {
		return HiberfilSeen{File: HiberfilIOError, Note: err.Error()}
	}
	return HiberfilSeen{File: HiberfilUnreadable, Note: err.Error()}
}

// mountKey is what tells one mount from another at a path: the id of the mount table, which a mount made once another
// is gone may be given again, the device, and the unique id of Linux 6.8 and later, 0 before.
type mountKey struct {
	id           uint64
	major, minor uint32
	unique       uint64
}

// reading is what a machine that runs for long reads once per mount at the top of an NTFS disk, hiberfil.sys or
// $LogFile: whether it is a reading to keep, and what it gives when the mount at a path cannot be told, or changed.
type reading[T any] interface {
	settled() bool
	withError(err error) T
	changedMount() T
}

// onceAMount keeps what a machine that runs for long read of one file at the top of each mount, so that the daemon's
// check, every 30 s, wakes no disk for it (specs.md §10). It is read again once the disk is mounted anew, and an error
// is never kept: the next check asks again.
type onceAMount[T reading[T]] struct {
	mu   sync.Mutex
	seen map[string]keptReading[T]
}

type keptReading[T any] struct {
	key  mountKey
	seen T
}

// hiberfilCache keeps what was read of hiberfil.sys, and logFileCache what was read of $LogFile.
type (
	hiberfilCache = onceAMount[HiberfilSeen]
	logFileCache  = onceAMount[LogFileSeen]
)

// mountChanged says that the mount at a path changed while hiberfil.sys was read there: nothing can be told of it.
var mountChanged = HiberfilSeen{File: HiberfilUnreadable, Note: "the mount changed while it was looked at"}

func (HiberfilSeen) withError(err error) HiberfilSeen { return hiberfilError(err) }

func (HiberfilSeen) changedMount() HiberfilSeen { return mountChanged }

// answer gives what the file holds on the mount that entry names at mount: what was kept of that same mount, or what
// read gives, kept when it is a reading. unique gives the unique id of the mount at a path, false where the kernel has
// none. read reads through a handle whose mount is entry's, and gives changedMount otherwise; the unique id must still
// be the same once it answered, or nothing is kept.
func (c *onceAMount[T]) answer(mount string, entry *MountEntry, unique func(string) (uint64, bool, error), read func() T) T {
	var none T
	uniqueOf := func() (uint64, error) {
		id, ok, err := unique(mount)
		if !ok {
			id = 0
		}
		return id, err
	}
	key := mountKey{id: entry.ID, major: entry.Major, minor: entry.Minor}
	var err error
	if key.unique, err = uniqueOf(); err != nil {
		return none.withError(err)
	}
	c.mu.Lock()
	kept, found := c.seen[mount]
	c.mu.Unlock()
	if found && kept.key == key {
		return kept.seen
	}
	seen := read()
	if !seen.settled() {
		return seen
	}
	if after, err := uniqueOf(); err != nil || after != key.unique {
		return none.changedMount()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[string]keptReading[T]{}
	}
	c.seen[mount] = keptReading[T]{key: key, seen: seen}
	return seen
}

// forget drops what was kept of the mount at a path, once a check found nothing mounted there: whatever is mounted there
// next is another mount, even where the kernel gives it the same id.
func (c *onceAMount[T]) forget(mount string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.seen, mount)
}
