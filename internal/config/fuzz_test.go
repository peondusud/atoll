package config

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/peondusud/atoll/internal/vectors"
)

// The configuration is text whoever writes /etc/atoll controls, and atoll disk add, remove, fail and replace edit it
// in place. The fuzz targets check what loading and editing promise of any text, seeded from the vectors.

// FuzzParse: any text loads or gives an *Error, never a panic; its problems are sorted by key, one a key, and its
// warnings sorted by key (specs.md §24.3, §24.4); and the same text loads the same way twice, as nothing may depend on
// the order of a map (§29.3).
func FuzzParse(f *testing.F) {
	for i, v := range vectors.Load[configVector](f, "config.json") {
		text, err := vectors.Bytes("toml", v.TOML, v.TOMLB64)
		if err != nil {
			f.Fatalf("vector %d: %v", i, err)
		}
		f.Add([]byte(text))
	}
	f.Fuzz(func(t *testing.T, text []byte) {
		c, err := Parse(text)
		again, err2 := Parse(text)
		if err != nil {
			var e *Error
			if !errors.As(err, &e) || len(e.Problems) == 0 {
				t.Fatalf("Parse(%q): %v, want an *Error with its problems", text, err)
			}
			if !slices.IsSortedFunc(e.Problems, func(a, b Problem) int { return strings.Compare(a.Key, b.Key) }) || hasTwice(e.Problems) {
				t.Fatalf("Parse(%q): problems not one a key, by key: %v", text, e.Problems)
			}
			if err2 == nil || err2.Error() != err.Error() {
				t.Fatalf("Parse(%q) gave %v, then %v", text, err, err2)
			}
			return
		}
		if err2 != nil || !reflect.DeepEqual(c.Effective(), again.Effective()) || !reflect.DeepEqual(c.Warnings, again.Warnings) {
			t.Fatalf("Parse(%q) loaded, then gave %v, or other values", text, err2)
		}
		if !slices.IsSortedFunc(c.Warnings, func(a, b Problem) int { return strings.Compare(a.Key, b.Key) }) {
			t.Fatalf("Parse(%q): warnings not sorted by key: %v", text, c.Warnings)
		}
	})
}

// hasTwice says whether two problems name one key.
func hasTwice(list []Problem) bool {
	for i := 1; i < len(list); i++ {
		if list[i].Key == list[i-1].Key {
			return true
		}
	}
	return false
}

// tree reads a text as the edits compare it: TOML, a byte order mark left out.
func tree(text []byte) (map[string]any, error) {
	var t map[string]any
	err := toml.Unmarshal(bytes.TrimPrefix(text, []byte("\xef\xbb\xbf")), &t)
	return t, err
}

// diskOf gives the first [[disk]] of a tree whose id is that string, as the edits find it: a disk whose id is no
// string, or that has none, has the id "", which the CLI never asks for, as it takes identifiers only.
func diskOf(t map[string]any, id string) map[string]any {
	disks, _ := t["disk"].([]any)
	for _, d := range disks {
		table, _ := d.(map[string]any)
		if own, _ := table["id"].(string); table != nil && own == id {
			return table
		}
	}
	return nil
}

// FuzzEdit: on any text, the in-place edits of the configuration never panic, and an edit they make is the edit
// asked and no more: a disk added comes after every byte of the text, a disk removed takes one stretch of it out, and
// a state set is that disk's state, set again to the same bytes (specs.md §10).
func FuzzEdit(f *testing.F) {
	for _, v := range vectors.Load[editVector](f, "config_edit.json") {
		id, state := v.ID, ""
		if v.Disk != nil {
			id = v.Disk.ID
		}
		if v.State != nil {
			state = *v.State
		}
		f.Add(v.TOML, id, state)
	}
	f.Fuzz(func(t *testing.T, text, id, state string) {
		if out, err := AppendDisk([]byte(text), &NewDisk{ID: id, Mount: "/mnt/" + id, FS: "ext4", State: state}); err == nil {
			if !bytes.HasPrefix(out, []byte(text)) {
				t.Fatalf("AppendDisk(%q, %q) changed the text before it: %q", text, id, out)
			}
			after, err := tree(out)
			if err != nil {
				t.Fatalf("AppendDisk(%q, %q) = %q, which does not read: %v", text, id, out, err)
			}
			disks, _ := after["disk"].([]any)
			if len(disks) == 0 {
				t.Fatalf("AppendDisk(%q, %q) = %q, with no disk", text, id, out)
			}
			if last, _ := disks[len(disks)-1].(map[string]any); last["id"] != id && id != "" {
				t.Fatalf("AppendDisk(%q, %q) = %q, whose last disk is %v", text, id, out, last)
			}
		}
		if out, err := RemoveDisk([]byte(text), id); err == nil {
			// One stretch out, the rest as it was.
			if len(out) >= len(text) {
				t.Fatalf("RemoveDisk(%q, %q) = %q, no shorter", text, id, out)
			}
			head := 0
			for head < len(out) && out[head] == text[head] {
				head++
			}
			if !strings.HasSuffix(text, string(out[head:])) {
				t.Fatalf("RemoveDisk(%q, %q) = %q, not one stretch taken out", text, id, out)
			}
			if _, err := tree(out); err != nil {
				t.Fatalf("RemoveDisk(%q, %q) = %q, which does not read: %v", text, id, out, err)
			}
		}
		out, err := SetDiskState([]byte(text), id, state)
		if err != nil {
			return
		}
		after, err := tree(out)
		if err != nil {
			t.Fatalf("SetDiskState(%q, %q, %q) = %q, which does not read: %v", text, id, state, out, err)
		}
		d := diskOf(after, id)
		if got, set := d["state"]; d == nil || state == "" && set || state != "" && got != state {
			t.Fatalf("SetDiskState(%q, %q, %q) = %q, whose disk is %v", text, id, state, out, d)
		}
		if again, err := SetDiskState(out, id, state); err != nil || !bytes.Equal(again, out) {
			t.Fatalf("SetDiskState(%q, %q, %q) = %q, then %q, %v", text, id, state, out, again, err)
		}
	})
}
