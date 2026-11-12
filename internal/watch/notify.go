package watch

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// The fanotify and inotify values the watcher uses (specs.md §15.1, docs/research/watcher.md), spelled out
// rather than taken from x/sys, so that what is asked of the kernel is in one place.
const (
	fanCloexec         = 0x1
	fanNonblock        = 0x2
	fanUnlimitedMarks  = 0x20
	fanReportFID       = 0x200
	fanReportDirFID    = 0x400
	fanReportName      = 0x800
	fanReportTargetFID = 0x1000
	fanReportDFIDName  = fanReportDirFID | fanReportName

	fanMarkAdd        = 0x1
	fanMarkFilesystem = 0x100

	fanAttrib     = 0x4
	fanCloseWrite = 0x8
	fanMovedFrom  = 0x40
	fanMovedTo    = 0x80
	fanCreate     = 0x100
	fanDelete     = 0x200
	fanQOverflow  = 0x4000
	fanOnChild    = 0x08000000
	fanRename     = 0x10000000
	fanOnDir      = 0x40000000

	infoFID         = 1
	infoDFIDName    = 2
	infoDFID        = 3
	infoOldDFIDName = 10
	infoNewDFIDName = 12

	fanMetadataSize = 24
	fanVersion      = 3
)

// fsMask is asked of a filesystem mark: a rename as one event with both names, and a file closed after a
// write rather than each write, so that the bounded queue lasts (specs.md §15.1).
const fsMask = fanCreate | fanDelete | fanRename | fanCloseWrite | fanAttrib | fanOnDir

// foldersMask is asked of a mark per folder, whose renames come as two events (measured).
const foldersMask = fanCreate | fanDelete | fanMovedFrom | fanMovedTo | fanCloseWrite | fanAttrib | fanOnDir | fanOnChild

// fanRecord is one fanotify event as the kernel wrote it.
type fanRecord struct {
	mask  uint64
	fd    int32
	pid   int32
	infos []fanInfo
}

// fanInfo is one record of a file handle: of the folder and a name in it, or of the file itself.
type fanInfo struct {
	kind       uint8
	handleType int32
	handle     []byte
	name       string
}

var errMalformed = errors.New("a malformed event")

// parseFanotify reads what one read of a fanotify group gave.
func parseFanotify(buf []byte) ([]fanRecord, error) {
	var out []fanRecord
	for off := 0; off < len(buf); {
		if len(buf)-off < fanMetadataSize {
			return out, fmt.Errorf("%w: %d bytes left", errMalformed, len(buf)-off)
		}
		length := int(binary.NativeEndian.Uint32(buf[off:]))
		version := buf[off+4]
		metaLen := int(binary.NativeEndian.Uint16(buf[off+6:]))
		if version != fanVersion || metaLen < fanMetadataSize || length < metaLen || off+length > len(buf) {
			return out, fmt.Errorf("%w: length %d, metadata %d, version %d", errMalformed, length, metaLen, version)
		}
		r := fanRecord{
			mask: binary.NativeEndian.Uint64(buf[off+8:]),
			fd:   int32(binary.NativeEndian.Uint32(buf[off+16:])),
			pid:  int32(binary.NativeEndian.Uint32(buf[off+20:])),
		}
		for i := off + metaLen; i < off+length; {
			if off+length-i < 4 {
				return out, fmt.Errorf("%w: an info header cut short", errMalformed)
			}
			kind, infoLen := buf[i], int(binary.NativeEndian.Uint16(buf[i+2:]))
			if infoLen < 4 || i+infoLen > off+length {
				return out, fmt.Errorf("%w: an info of %d bytes", errMalformed, infoLen)
			}
			if info, ok, err := parseFanInfo(kind, buf[i+4:i+infoLen]); err != nil {
				return out, err
			} else if ok {
				r.infos = append(r.infos, info)
			}
			i += infoLen
		}
		out = append(out, r)
		off += length
	}
	return out, nil
}

// parseFanInfo reads one info of a handle: the fsid, the handle, and for a folder with a name, the name up to
// its NUL. An info of another kind is skipped.
func parseFanInfo(kind uint8, body []byte) (fanInfo, bool, error) {
	switch kind {
	case infoFID, infoDFIDName, infoDFID, infoOldDFIDName, infoNewDFIDName:
	default:
		return fanInfo{}, false, nil
	}
	if len(body) < 8+8 {
		return fanInfo{}, false, fmt.Errorf("%w: a handle info of %d bytes", errMalformed, len(body))
	}
	h := body[8:]
	size := int(binary.NativeEndian.Uint32(h))
	if 8+size > len(h) {
		return fanInfo{}, false, fmt.Errorf("%w: a handle of %d bytes in %d", errMalformed, size, len(h)-8)
	}
	info := fanInfo{kind: kind, handleType: int32(binary.NativeEndian.Uint32(h[4:])), handle: h[8 : 8+size]}
	if kind == infoDFIDName || kind == infoOldDFIDName || kind == infoNewDFIDName {
		name := h[8+size:]
		end := 0
		for end < len(name) && name[end] != 0 {
			end++
		}
		info.name = string(name[:end])
	}
	return info, true, nil
}

// handleKey names a handle, for a map.
func handleKey(handleType int32, handle []byte) string {
	return fmt.Sprintf("%d:%x", handleType, handle)
}

// change is what one event says at one place, before the watcher tells who made it.
type change struct {
	path   string
	folder bool
	kind   string
}

// place says where an info leads.
type place int8

const (
	inside  place = iota // below the root, at path
	outside              // elsewhere on the disk: no part of the library
	unknown              // a folder that cannot be told any more: look at the whole disk
)

// fanChanges gives what a record says, at the paths resolve finds for its infos.
func fanChanges(r *fanRecord, resolve func(*fanInfo) (string, place)) []change {
	if r.mask&fanQOverflow != 0 {
		return []change{{kind: KindOverflow}}
	}
	folder := r.mask&fanOnDir != 0
	var out []change
	at := func(info *fanInfo, kind string) {
		path, where := "", unknown
		if info != nil {
			path, where = resolve(info)
		}
		switch {
		case where == outside:
			return
		case where == unknown && info != nil && info.kind == infoFID:
			// A file that no longer exists: its deletion comes as an event of its folder.
			return
		case where == unknown:
			path = ""
		}
		out = append(out, change{path: path, folder: folder, kind: kind})
	}
	var main, oldName, newName *fanInfo
	for i := range r.infos {
		switch info := &r.infos[i]; info.kind {
		case infoDFIDName, infoDFID:
			main = info
		case infoFID:
			if main == nil {
				main = info
			}
		case infoOldDFIDName:
			oldName = info
		case infoNewDFIDName:
			newName = info
		}
	}
	if r.mask&fanRename != 0 {
		at(oldName, KindMovedFrom)
		at(newName, KindMovedTo)
	}
	for _, k := range []struct {
		bit  uint64
		kind string
	}{{fanCreate, KindCreated}, {fanCloseWrite, KindWritten}, {fanAttrib, KindAttrib}, {fanDelete, KindDeleted}, {fanMovedFrom, KindMovedFrom}, {fanMovedTo, KindMovedTo}} {
		if r.mask&k.bit != 0 {
			at(main, k.kind)
		}
	}
	return out
}

// below gives the path of a name, in a folder given by its absolute path, relative to the root.
func below(root, dir, name string) (string, place) {
	full := dir
	if name != "" && name != "." {
		full = dir + "/" + name
	}
	switch {
	case full == root:
		return "", inside
	case len(full) > len(root) && full[:len(root)] == root && full[len(root)] == '/':
		return full[len(root)+1:], inside
	default:
		return "", outside
	}
}

// inotifyRecord is one inotify event.
type inotifyRecord struct {
	wd     int32
	mask   uint32
	cookie uint32
	name   string
}

// parseInotify reads what one read of an inotify instance gave.
func parseInotify(buf []byte) ([]inotifyRecord, error) {
	var out []inotifyRecord
	for off := 0; off < len(buf); {
		if len(buf)-off < 16 {
			return out, fmt.Errorf("%w: %d bytes left", errMalformed, len(buf)-off)
		}
		r := inotifyRecord{
			wd:     int32(binary.NativeEndian.Uint32(buf[off:])),
			mask:   binary.NativeEndian.Uint32(buf[off+4:]),
			cookie: binary.NativeEndian.Uint32(buf[off+8:]),
		}
		size := int(binary.NativeEndian.Uint32(buf[off+12:]))
		if off+16+size > len(buf) {
			return out, fmt.Errorf("%w: a name of %d bytes", errMalformed, size)
		}
		name := buf[off+16 : off+16+size]
		end := 0
		for end < len(name) && name[end] != 0 {
			end++
		}
		r.name = string(name[:end])
		out = append(out, r)
		off += 16 + size
	}
	return out, nil
}
