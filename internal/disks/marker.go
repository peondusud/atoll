package disks

import (
	"context"
	"fmt"
	"io"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/layout"
)

// MarkerAnswer is what the top of a disk holds at <workdir>/disk.toml, its marker (specs.md §10).
type MarkerAnswer string

// The answers about a disk's marker.
const (
	MarkerAbsent MarkerAnswer = "absent"
	MarkerFile   MarkerAnswer = "file"
	// MarkerNotAFile is a symbolic link, which is never followed, a folder, or anything else that is no regular file.
	MarkerNotAFile MarkerAnswer = "not-a-file"
	// MarkerIOError is an error that says the disk is unknown (invariant 4).
	MarkerIOError MarkerAnswer = "io-error"
	// MarkerUnreadable is any other error, as a permission refused.
	MarkerUnreadable MarkerAnswer = "unreadable"
)

// MarkerLimit is more than any marker holds: a file past it is read that far and one byte more, and is no marker.
const MarkerLimit = 64 << 10

// MarkerSeen is what a read of a disk's marker gave: a file's text, MarkerLimit bytes at most and one byte more, or
// why none.
type MarkerSeen struct {
	File MarkerAnswer
	Text []byte
	Note string
}

// settled says whether an answer holds for as long as its mount does: a reading, not an error.
func (s MarkerSeen) settled() bool {
	return s.File == MarkerAbsent || s.File == MarkerFile || s.File == MarkerNotAFile
}

func (MarkerSeen) withError(err error) MarkerSeen { return markerError(err) }

func (MarkerSeen) changedMount() MarkerSeen {
	return MarkerSeen{File: MarkerUnreadable, Note: "the mount changed while it was looked at"}
}

// MarkerSeenOf gives what a read of a marker gave as a MarkerSeen: its text and whether there was one, or the error
// that kept it from being read.
func MarkerSeenOf(text []byte, found bool, err error) MarkerSeen {
	switch {
	case err != nil:
		return markerError(err)
	case found:
		return MarkerSeen{File: MarkerFile, Text: text}
	default:
		return MarkerSeen{File: MarkerAbsent}
	}
}

// markerError is what an error says of a marker: none there, something there that is no regular file, the disk
// unknown, or nothing.
func markerError(err error) MarkerSeen {
	switch {
	case fsx.IsAbsent(err):
		return MarkerSeen{File: MarkerAbsent}
	case fsx.IsUnreadableName(err):
		return MarkerSeen{File: MarkerNotAFile, Note: err.Error()}
	case fsx.IsDiskLost(err):
		return MarkerSeen{File: MarkerIOError, Note: err.Error()}
	default:
		return MarkerSeen{File: MarkerUnreadable, Note: err.Error()}
	}
}

// ReadMarker reads <workdir>/disk.toml through a handle on the top of a disk's mount, and writes nothing: one component
// at a time, no link followed, a regular file only, opened without waiting, MarkerLimit bytes and one more at most, as
// whoever writes the disk can put anything at that name (specs.md §10).
func ReadMarker(top fsx.Dir, workdir string) MarkerSeen {
	file, err := fsx.OpenFileAt(top, workdir+"/"+layout.MarkerName)
	if err != nil {
		return markerError(err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, MarkerLimit+1))
	if err != nil {
		return markerError(err)
	}
	return MarkerSeen{File: MarkerFile, Text: data}
}

// JudgeMarker is what a check makes of a disk's marker, and whether it is the disk's own (specs.md §10): a file there
// as MarkerVerdict says; something there that is no regular file, no marker this version reads; an error that says
// the disk is unknown, the disk missing, never a marker absent (invariant 4); any other error, nothing.
func JudgeMarker(v Verdict, s MarkerSeen, seen bool, poolID string, d *config.Disk) (Verdict, bool) {
	if v.State == Missing {
		return v, false
	}
	switch s.File {
	case MarkerAbsent, MarkerFile:
		return MarkerVerdict(v, s.Text, s.File == MarkerFile, seen, poolID, d)
	case MarkerNotAFile:
		if v.State != Online {
			return v, false
		}
		verdict := readOnly(OtherMarker, fmt.Sprintf("its %s is no regular file, and no marker this version reads: %s", layout.MarkerName, s.Note))
		verdict.TimePrecisionNS, verdict.MountOwner, verdict.Source = v.TimePrecisionNS, v.MountOwner, v.Source
		return verdict, false
	case MarkerIOError:
		return missing(IOError, fmt.Sprintf("its marker %s does not answer: %s", layout.MarkerName, s.Note)), false
	case MarkerUnreadable:
		return v, false
	default:
		return v, false
	}
}

// MarkerVerdict is what a check makes of the disk's marker, <workdir>/disk.toml, once it was read (specs.md §10), and
// whether it is the disk's own, which the catalog then records seen. None, on a disk whose own marker was seen, makes
// the disk missing, no-marker, whatever the second table says, as for a wrong identifier: a filesystem with its
// identifier and none of its marker, a clone made before disk add, is not that disk, and nothing it holds counts. One
// that names another pool, another disk of this pool or another filesystem, or a file there this version does not
// read, as disk add refuses each, makes a disk online readonly, other-marker. What the check said otherwise stays: a
// missing disk is missing, and records nothing, and a readonly one keeps its reason. One that says the same changes
// nothing, and nor does none on a disk whose marker no one saw, as in a pool from before.
func MarkerVerdict(v Verdict, text []byte, found, seen bool, poolID string, d *config.Disk) (Verdict, bool) {
	if v.State == Missing {
		return v, false
	}
	if !found {
		if !seen {
			return v, false
		}
		return missing(NoMarker, fmt.Sprintf("its own marker %s/%s was seen before, and is not there: a filesystem with its identifier "+
			"is not taken for it, as a clone made before disk add, and nothing it holds counts; atoll disk mark %s writes it again "+
			"once it is that disk", d.Workdir, layout.MarkerName, d.ID)), false
	}
	m, err := layout.ParseMarker(text)
	if err == nil && m.PoolID == poolID && m.DiskID == d.ID && m.FSUUID == d.UUID {
		return v, true
	}
	if v.State != Online {
		return v, false
	}
	var why string
	switch {
	case err != nil:
		why = fmt.Sprintf("its %s is not a marker this version reads: %v", layout.MarkerName, err)
	case m.PoolID != poolID:
		why = fmt.Sprintf("its marker says it is a disk of the pool %q", m.PoolID)
	case m.DiskID != d.ID:
		why = fmt.Sprintf("its marker says it is the disk %q of this pool", m.DiskID)
	default:
		why = fmt.Sprintf("its marker names the filesystem %s, copied from another disk", m.FSUUID)
	}
	verdict := readOnly(OtherMarker, why)
	verdict.TimePrecisionNS, verdict.MountOwner, verdict.Source = v.TimePrecisionNS, v.MountOwner, v.Source
	return verdict, false
}

// MarkerObserver is a machine that reads a disk's marker itself, as the delivered one does once per mount for as long
// as it runs, as it reads hiberfil.sys (specs.md §10). again says that a marker found absent is to be read again rather
// than taken from what was kept: a disk whose own marker was seen, for which atoll disk mark may have written it since.
type MarkerObserver interface {
	Marker(mount, workdir string, entry *MountEntry, again bool) MarkerSeen
}

// Seen says which disks' own marker the catalog records seen, by id, each on the identifier it was seen on: what a
// check needs of the catalog to ask the question of the marker (specs.md §9, §10).
type Seen map[string]string

// Of says whether a disk's own marker was seen on the filesystem the configuration gives it.
func (s Seen) Of(d *config.Disk) bool {
	uuid, ok := s[d.ID]
	return ok && uuid == d.UUID
}

// CheckKnown is the mount check, then, when its first table passes, the question of the marker: what the check of a
// command, a session, the daemon or Atoll's own view, which know the catalog, makes of a disk (specs.md §10). It wakes
// no more than the check does where the machine keeps what it read of each mount.
func CheckKnown(m Machine, d *config.Disk, poolID string, seen bool) Verdict {
	v := Check(m, d)
	if v.State == Missing || d.State == config.DiskFailed {
		// A failed disk is never read again (specs.md §18).
		return v
	}
	v, _ = JudgeMarker(v, markerOn(m, d, seen), seen, poolID, d)
	return v
}

// ConfirmKnown is Confirm, then the question of the marker, as CheckKnown asks it: what a scan confirms after its walk
// (specs.md §10, §15.10).
func ConfirmKnown(m Machine, d *config.Disk, poolID string, seen bool) Verdict {
	v := Confirm(m, d)
	if v.State == Missing {
		return v
	}
	v, _ = JudgeMarker(v, markerOn(m, d, seen), seen, poolID, d)
	return v
}

// CheckAllKnown is CheckKnown of each disk, in the order given.
func CheckAllKnown(ctx context.Context, m Machine, list []config.Disk, poolID string, seen Seen) ([]Verdict, error) {
	verdicts := make([]Verdict, 0, len(list))
	for i := range list {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("mount check: %w", err)
		}
		verdicts = append(verdicts, CheckKnown(m, &list[i], poolID, seen.Of(&list[i])))
	}
	return verdicts, nil
}

// markerOn reads the marker of a disk its check found there, through the machine: the delivered one, which keeps what
// it read of each mount, and the conformance build's. A machine that reads none, as a test's that stands for a few
// questions alone, leaves the question unasked.
func markerOn(m Machine, d *config.Disk, seen bool) MarkerSeen {
	mo, ok := m.(MarkerObserver)
	if !ok {
		return MarkerSeen{File: MarkerUnreadable, Note: "this machine reads no marker"}
	}
	entry := m.Mount(d.Mount).Entry
	if entry == nil || entry.MountPoint != d.Mount {
		return MarkerSeen{File: MarkerUnreadable, Note: "the mount changed while it was looked at"}
	}
	return mo.Marker(d.Mount, d.Workdir, entry, seen)
}

// ReadMarkerAt reads a disk's marker through a handle on the folder at its mount point, kept nowhere: what a machine
// that answers from folders, as the conformance build's, gives as its Marker.
func ReadMarkerAt(mount, workdir string) MarkerSeen {
	top, err := fsx.OpenDir(mount)
	if err != nil {
		return markerError(err)
	}
	defer func() { _ = top.Close() }()
	return ReadMarker(top, workdir)
}
