package watch

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// fanEvent writes one fanotify event as the kernel does, with infos of handles.
func fanEvent(mask uint64, pid int32, infos ...[]byte) []byte {
	length := fanMetadataSize
	for _, i := range infos {
		length += len(i)
	}
	b := make([]byte, fanMetadataSize, length)
	binary.NativeEndian.PutUint32(b[0:], uint32(length))
	b[4] = fanVersion
	binary.NativeEndian.PutUint16(b[6:], fanMetadataSize)
	binary.NativeEndian.PutUint64(b[8:], mask)
	binary.NativeEndian.PutUint32(b[16:], ^uint32(0)) // FAN_NOFD
	binary.NativeEndian.PutUint32(b[20:], uint32(pid))
	for _, i := range infos {
		b = append(b, i...)
	}
	return b
}

// fanHandle writes one info: its kind, an fsid, a handle, and a name with its NUL, padded to 4 bytes.
func fanHandle(kind uint8, handle string, name string) []byte {
	body := make([]byte, 8)
	h := make([]byte, 8)
	binary.NativeEndian.PutUint32(h[0:], uint32(len(handle)))
	binary.NativeEndian.PutUint32(h[4:], 1)
	body = append(body, append(h, handle...)...)
	if kind == infoDFIDName || kind == infoOldDFIDName || kind == infoNewDFIDName {
		body = append(body, append([]byte(name), 0)...)
	}
	for (4+len(body))%4 != 0 {
		body = append(body, 0)
	}
	head := []byte{kind, 0, 0, 0}
	binary.NativeEndian.PutUint16(head[2:], uint16(4+len(body)))
	return append(head, body...)
}

// folders resolves the handles "root", "d" and "e" to folders below the root, "out" to a folder elsewhere,
// and any other to nothing.
func folders(info *fanInfo) (string, place) {
	switch string(info.handle) {
	case "root":
		return join("", info.name), inside
	case "d":
		return join("d", info.name), inside
	case "out":
		return "", outside
	default:
		return "", unknown
	}
}

func TestFanotifyRecords(t *testing.T) {
	buf := append(fanEvent(fanCreate|fanCloseWrite, 42, fanHandle(infoDFIDName, "root", "a"), fanHandle(infoFID, "file-a", "")),
		fanEvent(fanRename|fanOnDir, 7, fanHandle(infoOldDFIDName, "root", "d"), fanHandle(infoNewDFIDName, "root", "e"), fanHandle(infoFID, "dir-e", ""))...)
	records, err := parseFanotify(buf)
	if err != nil || len(records) != 2 || records[0].pid != 42 || records[0].fd != -1 || len(records[1].infos) != 3 || records[1].infos[1].name != "e" {
		t.Fatalf("%+v, %v", records, err)
	}
	var got []change
	for i := range records {
		got = append(got, fanChanges(&records[i], folders)...)
	}
	want := []change{{path: "a", kind: KindCreated}, {path: "a", kind: KindWritten}, {path: "d", folder: true, kind: KindMovedFrom}, {path: "e", folder: true, kind: KindMovedTo}}
	if !slices.Equal(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

// What is elsewhere on the disk is no part of the library; a folder that cannot be told any more is the whole
// disk; a file that no longer exists says nothing, its folder's event saying it went.
func TestFanotifyPlaces(t *testing.T) {
	cases := []struct {
		name   string
		record []byte
		want   []change
	}{
		{"outside the root", fanEvent(fanCreate, 1, fanHandle(infoDFIDName, "out", "x")), nil},
		{"a folder gone", fanEvent(fanDelete, 1, fanHandle(infoDFIDName, "gone", "x")), []change{{kind: KindDeleted}}},
		{"a file gone", fanEvent(fanAttrib, 1, fanHandle(infoFID, "gone", "")), nil},
		{"the root itself", fanEvent(fanAttrib|fanOnDir, 1, fanHandle(infoDFIDName, "root", ".")), []change{{folder: true, kind: KindAttrib}}},
		{"below a folder", fanEvent(fanDelete, 1, fanHandle(infoDFIDName, "d", "b")), []change{{path: "d/b", kind: KindDeleted}}},
		{"an overflow", fanEvent(fanQOverflow, 0), []change{{kind: KindOverflow}}},
		{"a rename out of the root", fanEvent(fanRename, 1, fanHandle(infoOldDFIDName, "d", "b"), fanHandle(infoNewDFIDName, "out", "b")), []change{{path: "d/b", kind: KindMovedFrom}}},
		{"two names, as a mark per folder gives them", fanEvent(fanMovedFrom|fanCreate, 1, fanHandle(infoDFIDName, "d", "c.part")), []change{{path: "d/c.part", kind: KindCreated}, {path: "d/c.part", kind: KindMovedFrom}}},
	}
	for _, c := range cases {
		records, err := parseFanotify(c.record)
		if err != nil || len(records) != 1 {
			t.Fatalf("%s: %+v, %v", c.name, records, err)
		}
		if got := fanChanges(&records[0], folders); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestFanotifyMalformed(t *testing.T) {
	good := fanEvent(fanCreate, 1, fanHandle(infoDFIDName, "root", "a"))
	for name, buf := range map[string][]byte{
		"cut short":       good[:len(good)-1],
		"short metadata":  good[:10],
		"another version": append([]byte{}, append(good[:4:4], append([]byte{9}, good[5:]...)...)...),
		"an info too long": func() []byte {
			b := append([]byte{}, good...)
			binary.NativeEndian.PutUint16(b[fanMetadataSize+2:], 4000)
			return b
		}(),
		"a handle too long": func() []byte {
			b := append([]byte{}, good...)
			binary.NativeEndian.PutUint32(b[fanMetadataSize+4+8:], 4000)
			return b
		}(),
	} {
		if _, err := parseFanotify(buf); !errors.Is(err, errMalformed) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestBelow(t *testing.T) {
	for _, c := range []struct {
		dir, name string
		path      string
		where     place
	}{
		{"/mnt/t1/media", "", "", inside},
		{"/mnt/t1/media", ".", "", inside},
		{"/mnt/t1/media", "a", "a", inside},
		{"/mnt/t1/media/Films", "a b", "Films/a b", inside},
		{"/mnt/t1", "media", "", inside},
		{"/mnt/t1", "Atoll", "", outside},
		{"/mnt/t1/mediax", "a", "", outside},
		{"/mnt/t1/Atoll/trash", "a", "", outside},
	} {
		if path, where := below("/mnt/t1/media", c.dir, c.name); path != c.path || where != c.where {
			t.Errorf("%s + %s: %q %d, want %q %d", c.dir, c.name, path, where, c.path, c.where)
		}
	}
}

// A folder renamed carries everything below it; one that left and has not come back has no path; one removed
// takes its keys with it.
func TestFolderTree(t *testing.T) {
	tree := newFolderTree(0)
	show := tree.add(tree.root, "Show", 1)
	season := tree.add(show, "S01", 2)
	if p, ok := season.path(); !ok || p != "Show/S01" {
		t.Fatalf("%q %v", p, ok)
	}
	n := tree.detach(tree.root, "Show")
	if p, ok := season.path(); ok || n != show {
		t.Fatalf("detached: %q %v", p, ok)
	}
	tree.attach(show, tree.root, "Show (2020)")
	if p, ok := season.path(); !ok || p != "Show (2020)/S01" {
		t.Fatalf("attached again: %q %v", p, ok)
	}
	if again := tree.add(tree.root, "Moved", 2); again != season {
		t.Fatal("a known key is the same folder")
	}
	if p, _ := season.path(); p != "Moved" || len(show.children) != 0 {
		t.Fatalf("moved by its key: %q, %d below Show", p, len(show.children))
	}
	tree.attach(tree.add(show, "x", 3), tree.root, "Moved")
	if tree.nodes[2] != nil {
		t.Error("the folder a name replaces is forgotten")
	}
	keys := tree.remove(show)
	slices.Sort(keys)
	if !slices.Equal(keys, []int{1}) || len(tree.nodes) != 2 {
		t.Errorf("removed %v, %d left", keys, len(tree.nodes))
	}
}

func inotifyEvent(wd int32, mask, cookie uint32, name string) []byte {
	size := (len(name) + 1 + 15) / 16 * 16
	if name == "" {
		size = 0
	}
	b := make([]byte, 16+size)
	binary.NativeEndian.PutUint32(b[0:], uint32(wd))
	binary.NativeEndian.PutUint32(b[4:], mask)
	binary.NativeEndian.PutUint32(b[8:], cookie)
	binary.NativeEndian.PutUint32(b[12:], uint32(size))
	copy(b[16:], name)
	return b
}

// The two ends of a folder's rename come in two reads when a read falls between the two, as the kernel queues them
// one after the other: the read waits a little for the other end before it forgets the folder, which keeps its
// watches and its place in the tree. The folder was forgotten, then watched again from scratch, listed and every
// name in it reported as created (specs.md §15.1). One that does not come back left the root.
func TestARenameInTwoReads(t *testing.T) {
	for _, back := range []bool{true, false} {
		s := &inotifySource{ino: -1, root: "/nowhere", moved: map[uint32]*folderNode[int32]{}}
		s.tree = newFolderTree[int32](1)
		s.tree.add(s.tree.root, "Show", 2)
		reads := [][]byte{inotifyEvent(1, unix.IN_MOVED_FROM|unix.IN_ISDIR, 9, "Show"), nil}
		if back {
			reads = append(reads, inotifyEvent(1, unix.IN_MOVED_TO|unix.IN_ISDIR, 9, "Show (2020)"), nil)
		}
		s.read = func(buf []byte) (int, error) {
			if len(reads) == 0 || reads[0] == nil {
				if len(reads) > 0 {
					reads = reads[1:]
				}
				return -1, unix.EAGAIN
			}
			n := copy(buf, reads[0])
			reads = reads[1:]
			return n, nil
		}
		waits := 0
		s.wait = func(time.Duration) bool {
			waits++
			return len(reads) > 0
		}
		out, _, err := s.drain(make([]byte, 4096))
		if err != nil {
			t.Fatal(err)
		}
		if waits != 1 {
			t.Errorf("back %v: waited %d times", back, waits)
		}
		node := s.tree.nodes[2]
		switch {
		case back && node == nil:
			t.Errorf("the folder renamed was forgotten: %+v", out)
		case back:
			if p, ok := node.path(); !ok || p != "Show (2020)" {
				t.Errorf("the folder renamed is at %q", p)
			}
		case node != nil:
			t.Errorf("the folder that left is still watched")
		}
		if len(s.moved) != 0 {
			t.Errorf("back %v: %d moves left", back, len(s.moved))
		}
	}
}

func TestInotifyRecords(t *testing.T) {
	event := inotifyEvent
	buf := append(event(1, 0x40, 9, "a"), event(2, 0x80, 9, "long-enough-name")...)
	buf = append(buf, event(1, 0x4, 0, "")...)
	records, err := parseInotify(buf)
	if err != nil || len(records) != 3 || records[1].name != "long-enough-name" || records[0].cookie != 9 || records[2].name != "" {
		t.Fatalf("%+v, %v", records, err)
	}
	if _, err := parseInotify(buf[:len(buf)-3]); !errors.Is(err, errMalformed) || !strings.Contains(err.Error(), ": 13 bytes left") {
		t.Errorf("cut short: %v", err)
	}
}

// What the kernel may give at the edges of a read is read as it is: an info of nothing but its header, a handle of no
// bytes, a name that runs to the end of its info with no NUL; and what is cut short anywhere is malformed, said with
// what was left, never read past (specs.md §15.11).
func TestFanotifyAtTheEdges(t *testing.T) {
	bare := []byte{0xff, 0, 4, 0}
	noHandle := fanHandle(infoFID, "", "")
	unended := append([]byte{infoDFIDName, 0, 0, 0}, make([]byte, 8)...)
	unended = binary.NativeEndian.AppendUint32(unended, 4)
	unended = binary.NativeEndian.AppendUint32(unended, 1)
	unended = append(unended, "rootname"...)
	binary.NativeEndian.PutUint16(unended[2:], uint16(len(unended)))
	records, err := parseFanotify(fanEvent(fanCreate, 1, bare, noHandle, unended))
	if err != nil || len(records) != 1 || len(records[0].infos) != 2 {
		t.Fatalf("%+v, %v", records, err)
	}
	if got := records[0].infos; len(got[0].handle) != 0 || string(got[1].handle) != "root" || got[1].name != "name" {
		t.Errorf("%+v", got)
	}
	good := fanEvent(fanCreate, 1, fanHandle(infoDFIDName, "root", "a"))
	if _, err := parseFanotify(append(append([]byte{}, good...), good[:5]...)); !errors.Is(err, errMalformed) || !strings.Contains(err.Error(), ": 5 bytes left") {
		t.Errorf("a second event of five bytes: %v", err)
	}
	long := append([]byte{}, good...)
	binary.NativeEndian.PutUint32(long[fanMetadataSize+4+8:], 4000)
	inHandle := int(binary.NativeEndian.Uint16(good[fanMetadataSize+2:])) - 4 - 8 - 8
	if _, err := parseFanotify(long); !errors.Is(err, errMalformed) || !strings.Contains(err.Error(), fmt.Sprintf("a handle of 4000 bytes in %d", inHandle)) {
		t.Errorf("a handle too long: %v", err)
	}
}
