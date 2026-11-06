package disks

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// MountEntry is one line of the mount table, /proc/self/mountinfo (specs.md §10). Strings hold
// bytes, which need not be valid UTF-8.
type MountEntry struct {
	ID           uint64
	Major, Minor uint32
	Root         string
	MountPoint   string
	MountOptions []string
	FSType       string
	Source       string
	SuperOptions []string
}

// MountTable is what could be read of the mount table.
type MountTable struct {
	Entries    []MountEntry
	Unreadable int // the lines that could not be read
}

// ParseMountInfo reads the mount table. A line it cannot read says nothing about the others: a
// mount that is none of the pool's must not make every disk of the pool unknown.
func ParseMountInfo(data []byte) MountTable {
	table := MountTable{Entries: []MountEntry{}}
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return table
	}
	for line := range strings.SplitSeq(text, "\n") {
		entry, err := parseMountLine(line)
		if err != nil {
			table.Unreadable++
			continue
		}
		table.Entries = append(table.Entries, entry)
	}
	return table
}

// Find gives the entry of a mount id: by the id, not by the path, as with mounts stacked on one
// path the table holds them all, and only the top one is what a program reaches. With no such
// entry, a line that could not be read may have been it, so nothing can be concluded.
func (t MountTable) Find(id uint64) MountSeen {
	for i := range t.Entries {
		if t.Entries[i].ID == id {
			return MountSeen{Path: PathOK, Entry: &t.Entries[i]}
		}
	}
	if t.Unreadable > 0 {
		return MountSeen{Path: PathIOError, Note: "no entry for this mount, and the mount table has lines that cannot be read"}
	}
	return MountSeen{Path: PathOK}
}

func parseMountLine(line string) (MountEntry, error) {
	const (
		beforeOptional = 6 // id, parent, major:minor, root, mount point, mount options
		afterSeparator = 3 // type, source, superblock options
	)
	// Exactly one space between fields: strings.Fields would take an empty source for a
	// missing field, and shift what follows it.
	fields := strings.Split(line, " ")
	separator := -1
	for i := beforeOptional; i < len(fields); i++ {
		if fields[i] == "-" {
			separator = i
			break
		}
	}
	switch {
	case separator < 0:
		return MountEntry{}, errors.New("no separator")
	case len(fields)-separator-1 < afterSeparator:
		return MountEntry{}, errors.New("too few fields after the separator")
	}
	var e MountEntry
	var err error
	if e.ID, err = number(fields[0], 64); err != nil {
		return MountEntry{}, fmt.Errorf("id: %w", err)
	}
	if _, err = number(fields[1], 64); err != nil {
		return MountEntry{}, fmt.Errorf("parent: %w", err)
	}
	major, minor, _ := strings.Cut(fields[2], ":")
	bigMajor, err := number(major, 32)
	if err != nil {
		return MountEntry{}, fmt.Errorf("major: %w", err)
	}
	bigMinor, err := number(minor, 32)
	if err != nil {
		return MountEntry{}, fmt.Errorf("minor: %w", err)
	}
	e.Major, e.Minor = uint32(bigMajor), uint32(bigMinor)
	e.Root = unescape(fields[3])
	e.MountPoint = unescape(fields[4])
	e.MountOptions = options(fields[5])
	e.FSType = unescape(fields[separator+1])
	e.Source = unescape(fields[separator+2])
	// The rest of the line: the kernel escapes what it writes itself, but these options come
	// from each driver, and nothing makes a driver escape them.
	e.SuperOptions = options(strings.Join(fields[separator+3:], " "))
	return e, nil
}

// number reads decimal digits only: strconv.ParseUint refuses a sign, which Rust's parse
// accepts, and both versions must refuse it (conformance/vectors/mountinfo.json).
func number(text string, bits int) (uint64, error) {
	n, err := strconv.ParseUint(text, 10, bits)
	if err != nil {
		return 0, fmt.Errorf("number: %w", err)
	}
	return n, nil
}

// options splits a list at each comma, then reads the escapes of each item, so that an escaped
// comma stays inside its item.
func options(field string) []string {
	list := []string{}
	if field == "" {
		return list
	}
	for item := range strings.SplitSeq(field, ",") {
		list = append(list, unescape(item))
	}
	return list
}

// unescape turns each \ooo, up to \377, into its byte. Any other backslash stays as it is, as in
// util-linux: 9p writes path=C:\ in its options, measured.
func unescape(field string) string {
	if !strings.Contains(field, `\`) {
		return field
	}
	var out strings.Builder
	for i := 0; i < len(field); i++ {
		if value, ok := octalAt(field, i); ok {
			out.WriteByte(value)
			i += 3
			continue
		}
		out.WriteByte(field[i])
	}
	return out.String()
}

// octalAt reads an escape at field[i], if there is one.
func octalAt(field string, i int) (byte, bool) {
	if field[i] != '\\' || i+3 >= len(field) {
		return 0, false
	}
	value := 0
	for _, digit := range []byte(field[i+1 : i+4]) {
		if digit < '0' || digit > '7' {
			return 0, false
		}
		value = value*8 + int(digit-'0')
	}
	if value > 255 {
		return 0, false
	}
	return byte(value), true
}
