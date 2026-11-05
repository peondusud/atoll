package names

import (
	"reflect"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

type corpusName struct {
	Name    *string  `json:"name"`
	NameB64 *string  `json:"name_b64"`
	Ext4    []Reason `json:"ext4"`
	XFS     []Reason `json:"xfs"`
	Btrfs   []Reason `json:"btrfs"`
	NTFS    []Reason `json:"ntfs"`
	Note    string   `json:"note"`
}

// TestNameCorpus checks every name of conformance/corpus/names/ on every
// filesystem.
func TestNameCorpus(t *testing.T) {
	for _, c := range vectors.LoadCorpus[corpusName](t, "names/names.json") {
		name, err := vectors.Name(c.Name, c.NameB64)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range []struct {
			fs   Filesystem
			want []Reason
		}{{Ext4, c.Ext4}, {XFS, c.XFS}, {Btrfs, c.Btrfs}, {NTFS, c.NTFS}} {
			if v.want == nil {
				t.Fatalf("%+q: no verdict for %s", name, v.fs)
			}
			got, err := ComponentReasons(name, v.fs)
			if err != nil {
				t.Errorf("ComponentReasons(%+q, %s): %v", name, v.fs, err)
				continue
			}
			if len(got) == 0 && len(v.want) == 0 {
				continue
			}
			if !reflect.DeepEqual(got, v.want) {
				t.Errorf("ComponentReasons(%+q, %s) = %v, want %v", name, v.fs, got, v.want)
			}
		}
	}
}

type compatVector struct {
	Path     *string `json:"path"`
	PathB64  *string `json:"path_b64"`
	FS       string  `json:"fs"`
	Problems []struct {
		Component int      `json:"component"`
		Reasons   []Reason `json:"reasons"`
	} `json:"problems"`
	Error bool   `json:"error"`
	Note  string `json:"note"`
}

func TestNameCompatVectors(t *testing.T) {
	for _, v := range vectors.Load[compatVector](t, "name_compat.json") {
		path, err := vectors.Path(v.Path, v.PathB64)
		if err != nil {
			t.Fatal(err)
		}
		got, err := CheckPath(path, Filesystem(v.FS))
		if v.Error {
			if err == nil {
				t.Errorf("CheckPath(%+q, %s) = %v, want an error", path, v.FS, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("CheckPath(%+q, %s): %v", path, v.FS, err)
			continue
		}
		var want []Problem
		for _, p := range v.Problems {
			want = append(want, Problem{Component: p.Component, Reasons: p.Reasons})
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("CheckPath(%+q, %s) = %v, want %v", path, v.FS, got, want)
		}
	}
}

// The reasons follow the documented order, and ext4, XFS and btrfs share one
// rule.
func TestComponentReasonsOrder(t *testing.T) {
	order := []Reason{TooLong, InvalidUTF8, ControlCharacter, ForbiddenCharacter, TrailingDotOrSpace, ReservedName}
	rank := map[Reason]int{}
	for i, r := range order {
		rank[r] = i
	}
	names := []string{
		"CON. ", "\xff\x01:.", "a:b ", strings.Repeat("\xff", 300) + ":", "con\x01.", "AUX:?",
		strings.Repeat("a", 256), "CON." + strings.Repeat("a", 252),
	}
	for _, name := range names {
		got, err := ComponentReasons(name, NTFS)
		if err != nil {
			t.Fatal(err)
		}
		for i := 1; i < len(got); i++ {
			if rank[got[i-1]] >= rank[got[i]] {
				t.Errorf("ComponentReasons(%+q, ntfs) = %v: out of order", name, got)
			}
		}
		e, _ := ComponentReasons(name, Ext4)
		x, _ := ComponentReasons(name, XFS)
		b, _ := ComponentReasons(name, Btrfs)
		if !reflect.DeepEqual(e, x) || !reflect.DeepEqual(e, b) {
			t.Errorf("%+q: ext4 %v, xfs %v, btrfs %v", name, e, x, b)
		}
	}
}

func TestComponentReasonsErrors(t *testing.T) {
	for _, name := range []string{"", ".", "..", "a/b", "a\x00b"} {
		if _, err := ComponentReasons(name, Ext4); err == nil {
			t.Errorf("ComponentReasons(%q, ext4): no error", name)
		}
	}
	if _, err := ComponentReasons("a", Filesystem("vfat")); err == nil {
		t.Error("ComponentReasons(a, vfat): no error")
	}
}

// The UTF-16 rule never refuses a name that the byte rule accepts: a valid
// UTF-8 character never has more UTF-16 code units than bytes.
func TestUTF16NeverLongerThanUTF8(t *testing.T) {
	for r := rune(0); r <= 0x10FFFF; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		s := string(r)
		if utf16Len(s) > len(s) {
			t.Fatalf("%U: %d UTF-16 units, %d bytes", r, utf16Len(s), len(s))
		}
	}
}

// FuzzCheckPath checks that CheckPath reports exactly the components that
// ComponentReasons refuses.
func FuzzCheckPath(f *testing.F) {
	for _, s := range []string{"Films/a.mkv", "a:b/CON/c.", "\xff/x", "a//b", "COM\u00b9/LPT1.txt"} {
		f.Add(s, true)
	}
	f.Fuzz(func(t *testing.T, path string, ntfs bool) {
		fs := Ext4
		if ntfs {
			fs = NTFS
		}
		got, err := CheckPath(path, fs)
		parts := strings.Split(path, "/")
		var want []Problem
		for i, c := range parts {
			reasons, cerr := ComponentReasons(c, fs)
			if cerr != nil {
				if err == nil {
					t.Fatalf("CheckPath(%+q) accepted a malformed component %+q", path, c)
				}
				return
			}
			if reasons != nil {
				want = append(want, Problem{i, reasons})
			}
		}
		if err != nil {
			t.Fatalf("CheckPath(%+q): %v", path, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("CheckPath(%+q) = %v, want %v", path, got, want)
		}
	})
}
