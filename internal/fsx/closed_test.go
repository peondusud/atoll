package fsx

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// What lists the library and is no pool disk's is closed to other accounts whatever the umask: each folder made for
// it 0750, its files 0640 (specs.md §26; docs/threat-model.md, G20). A folder there already, or a link to one, is left
// as it is, as it may hold more than the catalog; a file there already, or a link, is never written through.
func TestWhatTheCatalogMakesIsClosed(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	top := t.TempDir()
	if err := os.Chmod(top, 0o777); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	if err := os.Chmod(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(top, "link")); err != nil {
		t.Fatal(err)
	}
	for _, folder := range []string{filepath.Join(top, "a", "b"), filepath.Join(top, "link", "c")} {
		if err := MkdirAllClosed(folder); err != nil {
			t.Fatal(err)
		}
		if err := MkdirAllClosed(folder); err != nil {
			t.Errorf("again: %v", err)
		}
	}
	for path, mode := range map[string]os.FileMode{
		top:                           os.ModeDir | 0o777,
		filepath.Join(top, "a"):       os.ModeDir | 0o750,
		filepath.Join(top, "a", "b"):  os.ModeDir | 0o750,
		elsewhere:                     os.ModeDir | 0o755,
		filepath.Join(elsewhere, "c"): os.ModeDir | 0o750,
	} {
		switch info, err := os.Lstat(path); {
		case err != nil:
			t.Error(err)
		case info.Mode() != mode:
			t.Errorf("%s is %v, want %v", path, info.Mode(), mode)
		}
	}

	file := filepath.Join(top, "a", "b", "catalog.db")
	if err := CreateClosed(file); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(file); err != nil || info.Mode() != 0o640 || info.Size() != 0 {
		t.Errorf("the file made: %v, %v", info, err)
	}
	if err := CreateClosed(file); !errors.Is(err, fs.ErrExist) {
		t.Errorf("made again: %v", err)
	}
	target := filepath.Join(elsewhere, "target")
	if err := os.WriteFile(target, []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(top, "a", "b", "linked.db")); err != nil {
		t.Fatal(err)
	}
	if err := CreateClosed(filepath.Join(top, "a", "b", "linked.db")); !errors.Is(err, fs.ErrExist) {
		t.Errorf("through a link: %v", err)
	}
	if info, err := os.Stat(target); err != nil || info.Mode() != 0o644 {
		t.Errorf("what the link leads to: %v, %v", info, err)
	}
}
