package view

import (
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
)

// The form takes a whole count of K, M or G, at most 99999: the largest unit that holds the size exactly, or else the
// count rounded up in the smallest unit that holds it (specs.md §21.1).
func TestOMVSize(t *testing.T) {
	for _, tc := range []struct {
		n       int64
		count   int64
		unit    string
		rounded bool
	}{
		{0, 0, "G", false},
		{1, 1, "K", true},
		{1023, 1, "K", true},
		{1024, 1, "K", false},
		{1536, 2, "K", true},
		{50000, 49, "K", true},
		{3 << 20, 3, "M", false},
		{1536 << 20, 1536, "M", false},
		{4 << 30, 4, "G", false},
		{1 << 40, 1024, "G", false},
		{99999 << 10, 99999, "K", false},
		{100000 << 10, 98, "M", true},
		{200000000000, 187, "G", true},
		{99999 << 30, 99999, "G", false},
		{99998<<30 + 1, 99999, "G", true},
	} {
		got, ok := OMVSizeOf(tc.n)
		want := OMVSize{Count: tc.count, Unit: tc.unit, Bytes: tc.n, Rounded: tc.rounded}
		if !ok || got != want {
			t.Errorf("%d: got %+v, %v, want %+v", tc.n, got, ok, want)
		}
	}
	for _, n := range []int64{99999<<30 + 1, 100000 << 30, math.MaxInt64} {
		if got, ok := OMVSizeOf(n); ok {
			t.Errorf("%d: got %+v, want none", n, got)
		}
	}
}

// The plugin refuses a path holding a character a shell or a systemd unit reads, and a line break; a control character
// cannot be shown as it is. ':' and ',' are NewMergerfs's to refuse.
func TestOMVRefused(t *testing.T) {
	for _, c := range []string{`"`, "'", "`", "$", `\`, ";", "|", "&", "<", ">", "\n", "\r", "\t", "\x00", "\x1b", "\x7f", "\u0085"} {
		if got, refused := OMVRefused("/mnt/a" + c + "b/media"); !refused || got != c {
			t.Errorf("%q: got %q, %v", c, got, refused)
		}
	}
	for _, p := range []string{"/mnt/a b/media", "/mnt/été/media", "/mnt/a=b/media", "/mnt/a(b)#!*?{}[]~%@+/media", "/mnt/a:b", "/mnt/a,b"} {
		if got, refused := OMVRefused(p); refused {
			t.Errorf("%q: refused for %q", p, got)
		}
	}
	if got, _ := OMVRefused("/mnt/a;b$c"); got != ";" {
		t.Errorf("not the first: %q", got)
	}
}

// The plugin mounts a pool at /srv/mergerfs/<name>, a name its form takes: view.mount gives it when it is such a path,
// and otherwise its last component, or the pool's id when the form would refuse that.
func TestOMVName(t *testing.T) {
	for _, tc := range []struct {
		mount, name string
		there       bool
	}{
		{"/srv/mergerfs/media", "media", true},
		{"/srv/mergerfs/My.pool_1-x", "My.pool_1-x", true},
		{"/srv/mergerfs/_x", "_x", true},
		{"/srv/pool", "pool", false},
		{"/srv/mergerfs/my pool", "home-media", false},
		{"/srv/mergerfs/.pool", "home-media", false},
		{"/srv/mergerfs/-pool", "home-media", false},
		{"/srv/mergerfs/a/b", "b", false},
		{"/srv/mergerfs", "mergerfs", false},
		{"/mnt/mergerfs/media", "media", false},
		{"/srv/pool-été", "home-media", false},
	} {
		if name, there := OMVName(tc.mount, "home-media"); name != tc.name || there != tc.there {
			t.Errorf("%s: got %s, %v, want %s, %v", tc.mount, name, there, tc.name, tc.there)
		}
	}
}

func omvOf(t *testing.T, view, minFree, disks string, capacities map[string]int64, policy string) (*OMV, error) {
	t.Helper()
	c := parse(t, strings.Replace(head, "/srv/pool", view, 1)+"[placement]\nnew_files = \"ordered\"\nmin_free = "+minFree+"\n"+disks)
	m, err := c.MinFree()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := NewMergerfs(c, m, capacities)
	if err != nil {
		t.Fatal(err)
	}
	return merged.OMV(c.PoolID, policy)
}

// The fields of the form: the branches with their modes in the order of the line, the create policy apart from the
// options, and the largest minfreespace of the branches that take new files (specs.md §21.1).
func TestOMVForm(t *testing.T) {
	disks := disk("a", "/mnt/a", "state = \"failed\"\n") + disk("b", "/mnt/b", "state = \"draining\"\n") + disk("c", "/mnt/c", "") + disk("d", "/mnt/d", "")
	capacities := map[string]int64{"a": 400 << 30, "b": 200 << 30, "c": 40 << 30, "d": 20 << 30}
	o, err := omvOf(t, "/srv/mergerfs/media", `["10%"]`, disks, capacities, OMVLargest)
	if err != nil {
		t.Fatal(err)
	}
	if o.Name != "media" || o.Mount != "/srv/mergerfs/media" || o.Elsewhere {
		t.Errorf("name %s, mount %s, elsewhere %v", o.Name, o.Mount, o.Elsewhere)
	}
	if want := []string{"/mnt/b/media=NC", "/mnt/c/media=RW", "/mnt/d/media=RW", "/mnt/a/media=RO"}; !slices.Equal(o.Paths, want) {
		t.Errorf("paths %q", o.Paths)
	}
	if o.CreatePolicy != "ff" {
		t.Errorf("create policy %s", o.CreatePolicy)
	}
	want := []string{"category.search=ff", "category.action=epall", "moveonenospc=false", "rename-exdev=passthrough", "link-exdev=passthrough", "cache.attr=1", "cache.entry=1", "cache.negative_entry=0"}
	if !slices.Equal(o.Options, want) {
		t.Errorf("options %q", o.Options)
	}
	// 10% of c, 4 GiB, more than the 2 GiB of d, and not the 20 GiB of b, which takes no new file, nor the 40 GiB of a,
	// failed.
	if o.MinFree == nil || *o.MinFree != (OMVSize{Count: 4, Unit: "G", Bytes: 4 << 30}) || o.Differs != nil {
		t.Errorf("min free %+v, differs %v", o.MinFree, o.Differs)
	}
}

// With "same", branches that take new files and hold different values give none; those that take none do not count,
// nor does a branch with no value of its own.
func TestOMVSame(t *testing.T) {
	disks := disk("a", "/mnt/a", "") + disk("b", "/mnt/b", "") + disk("c", "/mnt/c", "state = \"draining\"\n") + disk("d", "/mnt/d", "")
	o, err := omvOf(t, "/srv/pool", `["10%"]`, disks, map[string]int64{"a": 40 << 30, "b": 20 << 30, "c": 80 << 30}, OMVSame)
	if err != nil {
		t.Fatal(err)
	}
	if o.MinFree != nil || len(o.Differs) != 2 || o.Differs[0].Disk != "a" || o.Differs[1].Disk != "b" {
		t.Errorf("min free %+v, differs %+v", o.MinFree, o.Differs)
	}
	if o.Name != "pool" || o.Mount != "/srv/mergerfs/pool" || !o.Elsewhere {
		t.Errorf("name %s, mount %s, elsewhere %v", o.Name, o.Mount, o.Elsewhere)
	}
	o, err = omvOf(t, "/srv/pool", `["10%"]`, disks, map[string]int64{"a": 40 << 30, "b": 40 << 30, "c": 80 << 30}, OMVSame)
	if err != nil {
		t.Fatal(err)
	}
	if o.MinFree == nil || o.MinFree.Bytes != 4<<30 || o.Differs != nil {
		t.Errorf("min free %+v, differs %+v", o.MinFree, o.Differs)
	}
	o, err = omvOf(t, "/srv/pool", `["10%"]`, disks, map[string]int64{"a": 40 << 30, "b": 20 << 30, "c": 80 << 30}, OMVLargest)
	if err != nil {
		t.Fatal(err)
	}
	if o.MinFree == nil || o.MinFree.Bytes != 4<<30 || o.Differs != nil {
		t.Errorf("largest: min free %+v, differs %+v", o.MinFree, o.Differs)
	}
}

// When no branch takes new files, every branch counts; when none has a value of its own, there is none to give.
func TestOMVNoneTakesNewFiles(t *testing.T) {
	disks := disk("a", "/mnt/a", "state = \"draining\"\n") + disk("b", "/mnt/b", "state = \"failed\"\n")
	o, err := omvOf(t, "/srv/mergerfs/p", `["10%"]`, disks, map[string]int64{"a": 10 << 30, "b": 30 << 30}, OMVLargest)
	if err != nil {
		t.Fatal(err)
	}
	if o.MinFree == nil || o.MinFree.Bytes != 3<<30 {
		t.Errorf("min free %+v", o.MinFree)
	}
	o, err = omvOf(t, "/srv/mergerfs/p", `["10%"]`, disks, nil, OMVSame)
	if err != nil {
		t.Fatal(err)
	}
	if o.MinFree != nil || o.Differs != nil {
		t.Errorf("unknown: min free %+v, differs %v", o.MinFree, o.Differs)
	}
}

// A branch path the plugin refuses names its disk; a size the form cannot hold is refused too.
func TestOMVRefuses(t *testing.T) {
	_, err := omvOf(t, "/srv/pool", `["1GiB"]`, disk("a", "/mnt/a", "")+disk("b", "/mnt/b", "root = \"Films&Series\"\n"), nil, OMVLargest)
	var refusal *OMVRefusal
	if !errors.As(err, &refusal) || refusal.Disk != "b" || refusal.Char != "&" || refusal.Path != "/mnt/b/Films&Series" {
		t.Errorf("got %v", err)
	}
	_, err = omvOf(t, "/srv/pool", `["100TiB"]`, disk("a", "/mnt/a", ""), nil, OMVLargest)
	var large *OMVTooLarge
	if !errors.As(err, &large) || large.Bytes != 100<<40 {
		t.Errorf("got %v", err)
	}
	// "same" with values that differ gives none, so none is too large.
	if _, err := omvOf(t, "/srv/pool", `["10%"]`, disk("a", "/mnt/a", "")+disk("b", "/mnt/b", ""), map[string]int64{"a": 2 << 50, "b": 3 << 50}, OMVSame); err != nil {
		t.Errorf("same: %v", err)
	}
}
