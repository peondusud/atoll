package disks

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

// The mount table is where a mount check reads which filesystem is mounted where (specs.md §10, invariant 3), and
// its lines hold what any account that can mount, or name a folder, writes: a mount point, a source, a driver's
// options. The fuzz targets check its reading, seeded from conformance/vectors/mountinfo.json.

// escapeField writes a field as the kernel would, each byte that would split a line or a list as \ooo, the backslash
// and the comma too, so that an option holds them.
func escapeField(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case ' ', '\t', '\n', '\\', ',':
			fmt.Fprintf(&b, "\\%03o", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func escapeList(list []string) string {
	parts := make([]string, len(list))
	for i, item := range list {
		parts[i] = escapeField(item)
	}
	return strings.Join(parts, ",")
}

// mountLine writes an entry as a line of /proc/self/mountinfo, its parent 1 and no optional field.
func mountLine(e *MountEntry) string {
	return fmt.Sprintf("%d 1 %d:%d %s %s %s - %s %s %s", e.ID, e.Major, e.Minor, escapeField(e.Root), escapeField(e.MountPoint),
		escapeList(e.MountOptions), escapeField(e.FSType), escapeField(e.Source), escapeList(e.SuperOptions))
}

// FuzzParseMountInfo: any text reads without a panic, each line an entry or counted unreadable, never both and never
// neither; each entry written back as the kernel writes it reads the same; and finding an entry's id finds an entry
// of that id.
func FuzzParseMountInfo(f *testing.F) {
	for _, c := range vectors.Load[mountinfoCase](f, "mountinfo.json") {
		text, err := vectors.Bytes("text", c.Text, c.TextB64)
		if err != nil {
			f.Fatal(err)
		}
		f.Add([]byte(text))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		table := ParseMountInfo(data)
		lines := 0
		if text := strings.TrimSuffix(string(data), "\n"); text != "" {
			lines = strings.Count(text, "\n") + 1
		}
		if len(table.Entries)+table.Unreadable != lines {
			t.Fatalf("%q: %d entries and %d unreadable of %d lines", data, len(table.Entries), table.Unreadable, lines)
		}
		for i := range table.Entries {
			e := &table.Entries[i]
			again := ParseMountInfo([]byte(mountLine(e)))
			if again.Unreadable != 0 || len(again.Entries) != 1 || !reflect.DeepEqual(again.Entries[0], *e) {
				t.Fatalf("%q: %+v, written %q, reads %+v", data, *e, mountLine(e), again)
			}
			if found := table.Find(e.ID); found.Path != PathOK || found.Entry == nil || found.Entry.ID != e.ID {
				t.Fatalf("%q: finding %d gives %+v", data, e.ID, found)
			}
		}
	})
}

// FuzzMountLine: an entry of any bytes, written as the kernel writes it, reads as it was.
func FuzzMountLine(f *testing.F) {
	f.Add(uint64(36), uint32(98), uint32(0), "/mnt1", "/mnt2", "rw", "noatime", "ext3", "/dev/root", "errors=continue")
	f.Add(uint64(1), uint32(0), uint32(4294967295), "/", "/mnt/a b\\c", "ro", "", "fuse.mergerfs", "a:b", "path=C:\\")
	f.Fuzz(func(t *testing.T, id uint64, major, minor uint32, root, point, opt, opt2, fstype, source, super string) {
		e := MountEntry{
			ID: id, Major: major, Minor: minor, Root: root, MountPoint: point, MountOptions: []string{opt, opt2},
			FSType: fstype, Source: source, SuperOptions: []string{super, "rw"},
		}
		table := ParseMountInfo([]byte(mountLine(&e) + "\n"))
		if table.Unreadable != 0 || len(table.Entries) != 1 || !reflect.DeepEqual(table.Entries[0], e) {
			t.Fatalf("%+v, written %q, reads %+v", e, mountLine(&e), table)
		}
	})
}
