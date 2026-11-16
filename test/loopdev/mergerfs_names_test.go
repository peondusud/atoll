//go:build loopdev

package loopdev

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/names"
)

// mergerfsOptions are those of the line atoll mergerfs-config prints, but the branches' own minfreespace (specs.md
// §21.1).
const mergerfsOptions = "category.search=ff,category.action=epall,category.create=mfs,minfreespace=1M,moveonenospc=false," +
	"rename-exdev=passthrough,link-exdev=passthrough,cache.attr=1,cache.entry=1,cache.negative_entry=0"

// TestMergerfsRenameARefusedName pins what mergerfs does when one branch refuses a rename, as
// docs/research/name-compatibility.md measured it by hand (specs.md §15.2, §15.3): an ext4 branch and an NTFS one that
// ntfs-3g mounts with windows_names, which refuses ':' in a name, in both orders. A file on both renamed to a name NTFS
// refuses: the rename succeeds, the NTFS copy goes, with no trash, and the view shows the new name. A folder on both:
// both folders stay, and the view shows both. A file only on NTFS: EINVAL, and nothing changes. A name both accept:
// both copies take it. Each look at a branch comes through a cold cache. A mergerfs release that changes any of it
// fails here, and §15.2 is to read again.
func TestMergerfsRenameARefusedName(t *testing.T) {
	needRoot(t)
	needMergerfs(t)
	for _, ntfsFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("ntfs-first-%v", ntfsFirst), func(t *testing.T) {
			ext := newVolume(t, names.Ext4)
			e := mountPoint(t)
			mountVolume(t, ext, "", e)
			ntfs := newVolume(t, names.NTFS)
			n := mountPoint(t)
			mountVolume(t, ntfs, "ntfs-3g", n, "windows_names")
			em, nm := filepath.Join(e, "media"), filepath.Join(n, "media")
			for _, top := range []string{em, nm} {
				mustDo(t, os.MkdirAll(top, 0o755))
			}
			branches := em + "=RW:" + nm + "=RW"
			if ntfsFirst {
				branches = nm + "=RW:" + em + "=RW"
			}
			pool := mountPoint(t)
			mount(t, pool, "mergerfs", "-o", mergerfsOptions, branches)
			on := func(top string, rel, text string) {
				t.Helper()
				full := filepath.Join(top, rel)
				mustDo(t, os.MkdirAll(filepath.Dir(full), 0o755))
				mustDo(t, os.WriteFile(full, []byte(text), 0o644))
			}
			for _, rel := range []string{"x.mkv", "D/in.mkv", "c.mkv"} {
				on(em, rel, rel)
				on(nm, rel, rel)
			}
			on(nm, "only.mkv", "only")
			cold := func() {
				t.Helper()
				must(t, "sync")
				mustDo(t, os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0o644))
			}
			there := func(path string) bool {
				_, err := os.Lstat(path)
				return err == nil
			}
			listed := func(dir string) []string {
				t.Helper()
				entries, err := os.ReadDir(dir)
				mustDo(t, err)
				var out []string
				for _, d := range entries {
					out = append(out, d.Name())
				}
				return out
			}

			if err := os.Rename(filepath.Join(pool, "x.mkv"), filepath.Join(pool, "f:x.mkv")); err != nil {
				t.Errorf("a file on both, to a refused name: %v", err)
			}
			if err := os.Rename(filepath.Join(pool, "D"), filepath.Join(pool, "D:x")); err != nil {
				t.Errorf("a folder on both, to a refused name: %v", err)
			}
			if err := os.Rename(filepath.Join(pool, "only.mkv"), filepath.Join(pool, "only:x.mkv")); !errors.Is(err, syscall.EINVAL) {
				t.Errorf("a file only on NTFS, to a refused name: %v, want EINVAL", err)
			}
			if err := os.Rename(filepath.Join(pool, "c.mkv"), filepath.Join(pool, "c2.mkv")); err != nil {
				t.Errorf("a file on both, to a name both accept: %v", err)
			}
			cold()
			for _, tc := range []struct {
				path string
				want bool
				what string
			}{
				{filepath.Join(em, "f:x.mkv"), true, "the ext4 copy at its new name"},
				{filepath.Join(em, "x.mkv"), false, "the ext4 copy at its old name"},
				{filepath.Join(nm, "x.mkv"), false, "the NTFS copy, which mergerfs removes"},
				{filepath.Join(nm, "f:x.mkv"), false, "the NTFS copy at the refused name"},
				{filepath.Join(em, "D:x", "in.mkv"), true, "the ext4 folder at its new name"},
				{filepath.Join(nm, "D", "in.mkv"), true, "the NTFS folder, which stays with its file"},
				{filepath.Join(nm, "only.mkv"), true, "the file only on NTFS"},
				{filepath.Join(em, "c2.mkv"), true, "the ext4 copy at the name both accept"},
				{filepath.Join(nm, "c2.mkv"), true, "the NTFS copy at the name both accept"},
				{filepath.Join(nm, "c.mkv"), false, "the NTFS copy at the old name both left"},
			} {
				if got := there(tc.path); got != tc.want {
					t.Errorf("%s: there %v, want %v", tc.what, got, tc.want)
				}
			}
			if got, want := listed(pool), []string{"D", "D:x", "c2.mkv", "f:x.mkv", "only.mkv"}; !slices.Equal(got, want) {
				t.Errorf("the merged view lists %q, want %q", got, want)
			}
		})
	}
}

// TestMergerfsRenameOntoAnADSStream pins what ntfs3 does with ads on, where the kernel knows that option and has it on
// by default, as Ubuntu's mainline 7.3 does (docs/research/name-compatibility.md): it takes "m1:n" for the stream n of
// the file m1. A file on both disks renamed through mergerfs onto "m1:n", where m1 is another file with its only copy
// on NTFS, succeeds, and m1 then holds the renamed file's content: its own is gone, read through a cold cache. This is
// why ntfs3 with ads is a refused variant (specs.md §10, §15.8). A kernel that changes it fails here.
func TestMergerfsRenameOntoAnADSStream(t *testing.T) {
	needRoot(t)
	needMergerfs(t)
	ntfs3Type(t)
	if ntfs3Options(t) == nil {
		t.Skip("this kernel's ntfs3 has no ads option")
	}
	ext := newVolume(t, names.Ext4)
	e := mountPoint(t)
	mountVolume(t, ext, "", e)
	ntfs := newVolume(t, names.NTFS)
	n := mountPoint(t)
	mountVolume(t, ntfs, "ntfs3", n)
	em, nm := filepath.Join(e, "media"), filepath.Join(n, "media")
	for _, top := range []string{em, nm} {
		mustDo(t, os.MkdirAll(top, 0o755))
	}
	// ads is on: a name with ':' is a stream of the file before it, which a listing does not show.
	probe := filepath.Join(nm, "probe")
	mustDo(t, os.WriteFile(probe, []byte("p"), 0o644))
	mustDo(t, os.WriteFile(probe+":s", []byte("s"), 0o644))
	entries, err := os.ReadDir(nm)
	mustDo(t, err)
	if len(entries) != 1 {
		t.Skip("ads is not on for this mount")
	}
	mustDo(t, os.Remove(probe))
	pool := mountPoint(t)
	mount(t, pool, "mergerfs", "-o", mergerfsOptions, em+"=RW:"+nm+"=RW")
	for _, top := range []string{em, nm} {
		mustDo(t, os.WriteFile(filepath.Join(top, "x.mkv"), []byte("the content of x"), 0o644))
	}
	mustDo(t, os.WriteFile(filepath.Join(nm, "m1"), []byte("m1's own content"), 0o644))
	must(t, "sync")
	mustDo(t, os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0o644))

	if err := os.Rename(filepath.Join(pool, "x.mkv"), filepath.Join(pool, "m1:n")); err != nil {
		t.Fatalf("the rename onto a stream: %v", err)
	}
	must(t, "sync")
	mustDo(t, os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0o644))
	if data, err := os.ReadFile(filepath.Join(nm, "m1")); err != nil || string(data) != "the content of x" {
		t.Errorf("m1 on NTFS holds %q, %v: it was overwritten, measured by hand", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(em, "m1:n")); err != nil || string(data) != "the content of x" {
		t.Errorf("the ext4 copy at the new name: %q, %v", data, err)
	}
	entries, err = os.ReadDir(pool)
	mustDo(t, err)
	var listed []string
	for _, d := range entries {
		listed = append(listed, d.Name())
	}
	if want := []string{"m1", "m1:n"}; !slices.Equal(listed, want) {
		t.Errorf("the merged view lists %q, want %q", listed, want)
	}
}
