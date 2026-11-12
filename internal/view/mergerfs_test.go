package view

import (
	"errors"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/config"
)

func parse(t *testing.T, text string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func disk(id, mount, more string) string {
	return "[[disk]]\nid = \"" + id + "\"\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3" + id[len(id)-1:] + "\"\nmount = \"" + mount + "\"\nfs = \"ext4\"\n" + more
}

const head = "pool_id = \"p\"\ncatalog = \"/var/lib/atoll/catalog.db\"\n[view]\nmount = \"/srv/pool\"\n"

func configure(t *testing.T, minFree, disks string, capacities map[string]int64) (*Mergerfs, error) {
	t.Helper()
	c := parse(t, head+"[placement]\nmin_free = "+minFree+"\n"+disks)
	m, err := c.MinFree()
	if err != nil {
		t.Fatal(err)
	}
	return NewMergerfs(c, m, capacities)
}

// The data disks in configuration order, cache disks left out, each with what min_free keeps free on it, written
// in the largest unit that holds it exactly (specs.md §21.1).
func TestMergerfsLine(t *testing.T) {
	disks := disk("b", "/mnt/b", "") + disk("c", "/mnt/c", "role = \"cache\"\n") + disk("a", "/mnt/a", "root = \"Media\"\n") + disk("d", "/mnt/d", "")
	m, err := configure(t, `["5%", "100GiB"]`, disks, map[string]int64{"b": 4 << 40, "c": 1 << 40, "a": 20 << 20})
	if err != nil {
		t.Fatal(err)
	}
	want := "/mnt/b/media=RW,100G:/mnt/a/Media=RW,1M:/mnt/d/media=RW,100G /srv/pool mergerfs " +
		"category.search=ff,category.action=epall,category.create=mfs,moveonenospc=false,rename-exdev=passthrough,link-exdev=passthrough,cache.attr=1,cache.entry=1,cache.negative_entry=0 0 0"
	if got := m.Fstab(); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// A disk whose capacity is not known keeps the smallest size; with percentages only, no minfreespace of its own.
func TestMergerfsCapacityUnknown(t *testing.T) {
	m, err := configure(t, `["5%"]`, disk("a", "/mnt/a", "")+disk("b", "/mnt/b", ""), map[string]int64{"a": 1000000})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Fstab(); !strings.HasPrefix(got, "/mnt/a/media=RW,50000:/mnt/b/media=RW /srv/pool ") {
		t.Errorf("got %s", got)
	}
	if m.Branches[1].MinFree != nil {
		t.Errorf("b: %d", *m.Branches[1].MinFree)
	}
}

func TestMergerfsSize(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 1: "1", 1023: "1023", 1024: "1K", 1536: "1536", 3 << 20: "3M", 1 << 30: "1G", 5 << 40: "5T", 1<<40 + 1<<30: "1025G", 9223372036854775807: "9223372036854775807"} {
		if got := mergerfsSize(n); got != want {
			t.Errorf("%d: got %s, want %s", n, got, want)
		}
	}
}

// fstab(5) reads a space, a tab and a line feed as the end of a field, and a backslash as an escape.
func TestMergerfsEscapes(t *testing.T) {
	disks := disk("a", "/mnt/my disks/a", "") + disk("b", `/mnt/back\\slash/b`, "") + disk("c", `/mnt/tab\there`, "") + disk("d", "/mnt/a=b/d", "")
	c := parse(t, "pool_id = \"p\"\ncatalog = \"/c.db\"\n[view]\nmount = \"/srv/my pool\"\n[placement]\nmin_free = [\"1GiB\"]\n"+disks)
	minFree, _ := c.MinFree()
	m, err := NewMergerfs(c, minFree, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `/mnt/my\040disks/a/media=RW,1G:/mnt/back\134slash/b/media=RW,1G:/mnt/tab\011here/media=RW,1G:/mnt/a=b/d/media=RW,1G /srv/my\040pool mergerfs `
	if got := m.Fstab(); !strings.HasPrefix(got, want) {
		t.Errorf("got  %s\nwant %s…", got, want)
	}
	if m.Branches[0].Path != "/mnt/my disks/a/media" {
		t.Errorf("path: %q", m.Branches[0].Path)
	}
}

// mergerfs ends a branch at ':' and an option at ',': such a branch is refused, and so is a pool with no data disk.
func TestMergerfsRefuses(t *testing.T) {
	for name, disks := range map[string]string{
		"colon in the mount": disk("a", "/mnt/a:b", ""),
		"comma in the root":  disk("a", "/mnt/a", "root = \"Films, Series\"\n"),
		"no data disk":       disk("a", "/mnt/a", "role = \"cache\"\n"),
		"no disk":            "",
	} {
		if m, err := configure(t, `["1GiB"]`, disks, nil); err == nil {
			t.Errorf("%s: got %s", name, m.Fstab())
		}
	}
}

// At runtime, the branches of the disks that are there, in configuration order, not escaped.
func TestMergerfsRuntime(t *testing.T) {
	m, err := configure(t, `["50%", "1GiB"]`, disk("a", "/mnt/my disks/a", "")+disk("b", "/mnt/b", "")+disk("c", "/mnt/c", ""), map[string]int64{"c": 3 << 20})
	if err != nil {
		t.Fatal(err)
	}
	all := func(string) bool { return true }
	if got := m.Runtime(all); got != "/mnt/my disks/a/media=RW,1G:/mnt/b/media=RW,1G:/mnt/c/media=RW,1536K" {
		t.Errorf("%s", got)
	}
	if got := m.Runtime(func(d string) bool { return d != "b" }); got != "/mnt/my disks/a/media=RW,1G:/mnt/c/media=RW,1536K" {
		t.Errorf("without b: %s", got)
	}
	if got := m.Runtime(func(string) bool { return false }); got != "" {
		t.Errorf("none: %s", got)
	}
	if err := WriteBranches("/nowhere", ""); err == nil {
		t.Error("wrote no branch")
	}
	if _, err := ReadBranches(t.TempDir()); !errors.Is(err, ErrNoMergerfs) {
		t.Errorf("no mergerfs: %v", err)
	}
}

// A draining disk keeps its place with NC; a failed disk comes after all the others with RO, at runtime as in the
// printed line (specs.md §18, §21.1).
func TestMergerfsModes(t *testing.T) {
	m, err := configure(t, `["1GiB"]`, disk("a", "/mnt/a", "state = \"failed\"\n")+disk("b", "/mnt/b", "state = \"draining\"\n")+disk("c", "/mnt/c", ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Runtime(func(string) bool { return true }); got != "/mnt/b/media=NC,1G:/mnt/c/media=RW,1G:/mnt/a/media=RO,1G" {
		t.Errorf("%s", got)
	}
	if got := m.Fstab(); !strings.HasPrefix(got, "/mnt/b/media=NC,1G:/mnt/c/media=RW,1G:/mnt/a/media=RO,1G /srv/pool mergerfs ") {
		t.Errorf("%s", got)
	}
}
