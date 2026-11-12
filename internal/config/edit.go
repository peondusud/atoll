package config

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

// NewDisk is the [[disk]] that AppendDisk writes: its keys in this order, each left out when empty.
type NewDisk struct {
	ID, UUID, Mount, FS, Role, Domain string
	Tags                              []string
	Root, Workdir, State              string
}

// ErrEditRefused says that the configuration cannot be edited in place: its disks are not written as [[disk]]
// tables, or the edit would change something else.
var ErrEditRefused = errors.New("the configuration cannot be edited in place")

// The edits below keep every other byte of the file, comments included, and refuse to give a text whose keys and
// values differ from the old ones in anything but the edit (specs.md §10). They do no I/O.

// AppendDisk adds a [[disk]] at the end of the text, after a blank line.
func AppendDisk(text []byte, d *NewDisk) ([]byte, error) {
	l, err := scanDisks(text)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.Write(text)
	if len(text) > l.bom && !bytes.HasSuffix(text, []byte("\n")) {
		b.WriteString(l.eol)
	}
	if len(text) > l.bom {
		b.WriteString(l.eol)
	}
	b.WriteString("[[disk]]" + l.eol)
	table := map[string]any{}
	for _, kv := range []struct{ key, value string }{
		{"id", d.ID}, {"uuid", d.UUID}, {"mount", d.Mount}, {"fs", d.FS}, {"role", d.Role}, {"domain", d.Domain},
	} {
		if kv.value == "" {
			continue
		}
		v, err := tomlString(kv.value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", kv.key, err)
		}
		b.WriteString(kv.key + " = " + v + l.eol)
		table[kv.key] = kv.value
	}
	if len(d.Tags) > 0 {
		parts := make([]string, 0, len(d.Tags))
		tags := make([]any, 0, len(d.Tags))
		for _, tag := range d.Tags {
			v, err := tomlString(tag)
			if err != nil {
				return nil, fmt.Errorf("tags: %w", err)
			}
			parts = append(parts, v)
			tags = append(tags, tag)
		}
		b.WriteString("tags = [" + strings.Join(parts, ", ") + "]" + l.eol)
		table["tags"] = tags
	}
	for _, kv := range []struct{ key, value string }{{"root", d.Root}, {"workdir", d.Workdir}, {"state", d.State}} {
		if kv.value == "" {
			continue
		}
		v, err := tomlString(kv.value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", kv.key, err)
		}
		b.WriteString(kv.key + " = " + v + l.eol)
		table[kv.key] = kv.value
	}
	out := b.Bytes()
	return out, sameBut(text, out, func(tree map[string]any) error {
		disks, _ := tree["disk"].([]any)
		tree["disk"] = append(disks, table)
		return nil
	})
}

// RemoveDisk takes a [[disk]] out of the text: from the start of its header's line to the end of its last key's
// line. What comes before its header or after its last key stays.
func RemoveDisk(text []byte, id string) ([]byte, error) {
	l, err := scanDisks(text)
	if err != nil {
		return nil, err
	}
	at := l.find(id)
	if at < 0 {
		return nil, fmt.Errorf("no disk %q", id)
	}
	t := &l.disks[at]
	out := slices.Concat(text[:t.start], text[t.lastEnd:])
	return out, sameBut(text, out, func(tree map[string]any) error {
		disks, _ := tree["disk"].([]any)
		tree["disk"] = slices.Delete(slices.Clone(disks), at, at+1)
		if len(tree["disk"].([]any)) == 0 {
			delete(tree, "disk")
		}
		return nil
	})
}

// SetDiskState gives a disk's state a value, or takes the key out for "". A state already there keeps its line and
// what follows its value; a new one goes on a line of its own after the disk's last key.
func SetDiskState(text []byte, id, state string) ([]byte, error) {
	l, err := scanDisks(text)
	if err != nil {
		return nil, err
	}
	at := l.find(id)
	if at < 0 {
		return nil, fmt.Errorf("no disk %q", id)
	}
	t := &l.disks[at]
	var out []byte
	switch {
	case t.stateLine[1] > 0 && state == "":
		out = slices.Concat(text[:t.stateLine[0]], text[t.stateLine[1]:])
	case t.stateLine[1] > 0:
		v, err := tomlString(state)
		if err != nil {
			return nil, fmt.Errorf("state: %w", err)
		}
		out = slices.Concat(text[:t.stateValue[0]], []byte(v), text[t.stateValue[1]:])
	case state == "":
		out = slices.Clone(text)
	default:
		v, err := tomlString(state)
		if err != nil {
			return nil, fmt.Errorf("state: %w", err)
		}
		line := "state = " + v + l.eol
		if t.lastEnd == len(text) && !bytes.HasSuffix(text, []byte("\n")) {
			line = l.eol + line
		}
		out = slices.Concat(text[:t.lastEnd], []byte(line), text[t.lastEnd:])
	}
	return out, sameBut(text, out, func(tree map[string]any) error {
		disks, _ := tree["disk"].([]any)
		disk, _ := disks[at].(map[string]any)
		if disk == nil {
			return ErrEditRefused
		}
		if state == "" {
			delete(disk, "state")
		} else {
			disk["state"] = state
		}
		return nil
	})
}

// diskTable is where a [[disk]] sits in the text.
type diskTable struct {
	id         string
	start      int    // the start of its header's line
	lastEnd    int    // the end of the line of its last key, line feed included
	stateLine  [2]int // the line of its state, line feed included; [0, 0] without one
	stateValue [2]int // the value of its state
}

type diskLayout struct {
	bom   int    // 3 when the text starts with a byte order mark, which Parse ignores
	eol   string // "\r\n" when the text has one, "\n" otherwise
	disks []diskTable
}

func (l *diskLayout) find(id string) int {
	return slices.IndexFunc(l.disks, func(t diskTable) bool { return t.id == id })
}

// scanDisks finds each [[disk]] of the text. It refuses disks written another way: as an array of inline tables, or
// with a table of their own below them, which an edit could not follow.
func scanDisks(text []byte) (*diskLayout, error) {
	l := &diskLayout{eol: "\n"}
	if bytes.HasPrefix(text, []byte("\xef\xbb\xbf")) {
		l.bom = 3
	}
	if bytes.Contains(text, []byte("\r\n")) {
		l.eol = "\r\n"
	}
	body := text[l.bom:]
	var p unstable.Parser
	p.Reset(body)
	var current *diskTable
	inRoot := true
	for p.NextExpression() {
		e := p.Expression()
		if e.Kind == unstable.ArrayTable || e.Kind == unstable.Table {
			key, offset := keyOf(e)
			inRoot = false
			current = nil
			switch {
			case e.Kind == unstable.ArrayTable && slices.Equal(key, []string{"disk"}):
				l.disks = append(l.disks, diskTable{start: l.bom + lineStart(body, offset)})
				current = &l.disks[len(l.disks)-1]
			case len(key) > 0 && key[0] == "disk":
				return nil, fmt.Errorf("%w: a table below a disk", ErrEditRefused)
			}
			continue
		}
		if e.Kind != unstable.KeyValue {
			continue
		}
		key, _ := keyOf(e)
		if inRoot && len(key) > 0 && key[0] == "disk" {
			return nil, fmt.Errorf("%w: its disks are not written as [[disk]] tables", ErrEditRefused)
		}
		if current == nil {
			continue
		}
		end := lineEnd(body, int(e.Raw.Offset+e.Raw.Length))
		current.lastEnd = l.bom + end
		value := e.Value()
		switch {
		case slices.Equal(key, []string{"id"}) && value.Kind == unstable.String:
			current.id = string(value.Data)
		case slices.Equal(key, []string{"state"}):
			current.stateLine = [2]int{l.bom + lineStart(body, int(e.Raw.Offset)), l.bom + end}
			current.stateValue = [2]int{l.bom + int(value.Raw.Offset), l.bom + int(value.Raw.Offset+value.Raw.Length)}
		}
	}
	if err := p.Error(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrEditRefused, err)
	}
	return l, nil
}

// keyOf gives the parts of a key, and where its first part starts.
func keyOf(e *unstable.Node) ([]string, int) {
	var key []string
	offset := -1
	it := e.Key()
	for it.Next() {
		if offset < 0 {
			offset = int(it.Node().Raw.Offset)
		}
		key = append(key, string(it.Node().Data))
	}
	return key, max(offset, 0)
}

// lineStart gives the start of the line that holds offset.
func lineStart(b []byte, offset int) int {
	return bytes.LastIndexByte(b[:offset], '\n') + 1
}

// lineEnd gives the end of the line that holds offset, its line feed included.
func lineEnd(b []byte, offset int) int {
	if i := bytes.IndexByte(b[offset:], '\n'); i >= 0 {
		return offset + i + 1
	}
	return len(b)
}

// sameBut checks that the new text holds the keys and values of the old one, with change applied to them, and
// nothing else changed.
func sameBut(old, edited []byte, change func(map[string]any) error) error {
	want, err := decodeTree(old)
	if err != nil {
		return err
	}
	if err := change(want); err != nil {
		return err
	}
	got, err := decodeTree(edited)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(want, got) {
		return fmt.Errorf("%w: the edit would change more than the disk", ErrEditRefused)
	}
	return nil
}

func decodeTree(text []byte) (map[string]any, error) {
	var tree map[string]any
	if err := toml.Unmarshal(bytes.TrimPrefix(text, []byte("\xef\xbb\xbf")), &tree); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrEditRefused, err)
	}
	if tree == nil {
		tree = map[string]any{}
	}
	return tree, nil
}

// tomlString writes a basic string: a quote, a backslash and the control characters escaped, anything else as it is.
func tomlString(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", errors.New("not valid UTF-8, which TOML cannot hold")
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String(), nil
}
