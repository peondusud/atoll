package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// ReplaceFile writes all or nothing, keeps the mode, follows a symbolic link, and refuses a file changed since it
// was read.
func TestReplaceFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "atoll.toml")
	if err := os.WriteFile(path, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.toml")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFile(link, []byte("old\n"), []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new\n" {
		t.Errorf("%q, %v", data, err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link was replaced: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o640 {
		t.Errorf("mode %v, %v", info.Mode(), err)
	}
	if err := ReplaceFile(path, []byte("old\n"), []byte("newer\n")); !errors.Is(err, ErrChangedMeanwhile) {
		t.Errorf("changed meanwhile: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "new\n" {
		t.Errorf("written anyway: %q", data)
	}
	if names, _ := os.ReadDir(dir); len(names) != 2 {
		t.Errorf("left behind: %v", names)
	}
}

// The new text goes through a file made for it alone, of a name nobody can tell beforehand. At a name anyone could
// tell, <file>.new, a draft left there was written over and renamed away, and a link there was followed: its
// target took the configuration's text, and the link took the configuration's place.
func TestReplaceFileLeavesWhatIsBesideIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "atoll.toml")
	if err := os.WriteFile(path, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	draft := path + ".new"
	if err := os.WriteFile(draft, []byte("a draft\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFile(path, []byte("old\n"), []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(draft); err != nil || string(data) != "a draft\n" {
		t.Errorf("the draft beside the file: %q, %v", data, err)
	}
	if err := os.Remove(draft); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(other, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, draft); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFile(path, []byte("new\n"), []byte("newer\n")); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(other); err != nil || string(data) != "keep\n" {
		t.Errorf("what a link beside the file names: %q, %v", data, err)
	}
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		t.Errorf("the configuration is no longer a file: %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "newer\n" {
		t.Errorf("%q, %v", data, err)
	}
	if names, _ := os.ReadDir(dir); len(names) != 3 {
		t.Errorf("left behind: %v", names)
	}
}

// disksBelow follows partitions to their disk, and device-mapper or md devices to the disks below them.
func TestDisksBelow(t *testing.T) {
	sys := t.TempDir()
	mk := func(path string) string {
		t.Helper()
		full := filepath.Join(sys, path)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		return full
	}
	touch := func(path string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(sys, path), []byte("1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := func(target, at string) {
		t.Helper()
		if err := os.Symlink(filepath.Join(sys, target), filepath.Join(sys, at)); err != nil {
			t.Fatal(err)
		}
	}
	sda1 := mk("block/sda/sda1")
	touch("block/sda/sda1/partition")
	mk("block/sdb/sdb2")
	touch("block/sdb/sdb2/partition")
	loop := mk("block/loop3")
	dm := mk("virtual/block/dm-0/slaves")
	link("block/sda/sda1", "virtual/block/dm-0/slaves/sda1")
	link("block/sdb/sdb2", "virtual/block/dm-0/slaves/sdb2")
	md := mk("virtual/block/md0/slaves")
	link("virtual/block/dm-0", "virtual/block/md0/slaves/dm-0")
	link("block/sda/sda1", "virtual/block/md0/slaves/sda1")
	for dir, want := range map[string][]string{
		sda1: {"sda"}, loop: {"loop3"}, filepath.Dir(dm): {"sda", "sdb"}, filepath.Dir(md): {"sda", "sdb"},
	} {
		got, err := disksBelow(dir, 0)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%s: %v, %v", dir, got, err)
		}
	}
}

// Once the new file has the name, the file is replaced, whatever comes after: a folder that cannot be synced then is
// ErrNotSynced, which says so, and never an error like those that leave the file as it was (specs.md §10). The
// commands said "the configuration is unchanged" of a configuration they had just changed.
func TestReplaceFileSaysWhenTheFileIsInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "atoll.toml")
	if err := os.WriteFile(path, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	failed := errors.New("input/output error")
	err := replaceFile(path, []byte("old\n"), []byte("new\n"), func(string) error { return failed })
	if !errors.Is(err, ErrNotSynced) || !errors.Is(err, failed) {
		t.Errorf("a folder that could not be synced: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "new\n" {
		t.Errorf("the file holds %q", got)
	}
	// What fails before leaves the file, and is no ErrNotSynced.
	err = replaceFile(path, []byte("old\n"), []byte("newer\n"), func(string) error { return failed })
	if !errors.Is(err, ErrChangedMeanwhile) || errors.Is(err, ErrNotSynced) {
		t.Errorf("a file that changed meanwhile: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "new\n" {
		t.Errorf("the file holds %q", got)
	}
}

// A file that is not there, or a folder at its path, is never replaced, and nothing is left beside it; a folder that
// is not there cannot be synced.
func TestReplaceFileOfWhatIsNoFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("none", filepath.Join(dir, "dangling")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"none", "dangling", "folder"} {
		err := ReplaceFile(filepath.Join(dir, name), nil, []byte("new\n"))
		if err == nil || errors.Is(err, ErrNotSynced) || errors.Is(err, ErrChangedMeanwhile) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if names, _ := os.ReadDir(dir); len(names) != 2 {
		t.Errorf("left behind: %v", names)
	}
	if err := syncFolder(filepath.Join(dir, "none")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a folder that is not there: %v", err)
	}
	if err := syncFolder(dir); err != nil {
		t.Error(err)
	}
}

// RenameFileNoReplace renames a file of the system disk to a free name, and refuses a name taken, leaving both files
// as they were.
func TestRenameFileNoReplace(t *testing.T) {
	dir := t.TempDir()
	from, to, taken := filepath.Join(dir, "a"), filepath.Join(dir, "b"), filepath.Join(dir, "c")
	for name, text := range map[string]string{from: "a", taken: "c"} {
		if err := os.WriteFile(name, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := RenameFileNoReplace(from, taken); !errors.Is(err, os.ErrExist) {
		t.Errorf("a name taken: %v", err)
	}
	for name, want := range map[string]string{from: "a", taken: "c"} {
		if data, err := os.ReadFile(name); err != nil || string(data) != want {
			t.Errorf("%s: %q, %v", name, data, err)
		}
	}
	if err := RenameFileNoReplace(from, to); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(to); err != nil || string(data) != "a" {
		t.Errorf("%q, %v", data, err)
	}
	if _, err := os.Stat(from); !os.IsNotExist(err) {
		t.Errorf("%s is still there: %v", from, err)
	}
	if err := RenameFileNoReplace(from, filepath.Join(dir, "d")); !IsAbsent(err) {
		t.Errorf("nothing to rename: %v", err)
	}
}
