package fsx

import (
	"os"
	"path/filepath"
	"testing"
)

// Adopt reaches the folder just made and nothing else. Whoever owns its parent can trade the new name for a symbolic
// link, a file or another folder between the making and the adoption: a mode set by name would then go to whatever
// the link names, with its setuid bits, and an owner to a folder that was never Atoll's (specs.md §6.1, §17.1).
func TestAdoptReachesOnlyTheNewFolder(t *testing.T) {
	top := t.TempDir()
	dir, err := OpenDir(top)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	like := &Stat{Mode: 0o4777, Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid())}
	modeOf := func(name string) os.FileMode {
		t.Helper()
		st, err := os.Lstat(filepath.Join(top, name))
		if err != nil {
			t.Fatal(err)
		}
		return st.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
	}

	// A symbolic link where the folder was: what it names keeps its mode.
	if err := os.WriteFile(filepath.Join(top, "target"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(top, "link")); err != nil {
		t.Fatal(err)
	}
	if err := dir.Adopt("link", like); err == nil {
		t.Error("a symbolic link was adopted")
	}
	if got := modeOf("target"); got != 0o600 {
		t.Errorf("what the link names now has the mode %v", got)
	}

	// A file where the folder was.
	if err := os.WriteFile(filepath.Join(top, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := dir.Adopt("file", like); err == nil {
		t.Error("a file was adopted")
	}
	if got := modeOf("file"); got != 0o600 {
		t.Errorf("the file now has the mode %v", got)
	}

	// A folder that holds something is not one just made.
	if err := os.MkdirAll(filepath.Join(top, "full", "inside"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := dir.Adopt("full", like); err == nil {
		t.Error("a folder that holds something was adopted")
	}
	if got := modeOf("full"); got != 0o700 {
		t.Errorf("the folder that holds something now has the mode %v", got)
	}

	// The folder just made takes the mode, its special bits included.
	if err := dir.Mkdir("new", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := dir.Adopt("new", &Stat{Mode: 0o2775, Uid: like.Uid, Gid: like.Gid}); err != nil {
		t.Fatal(err)
	}
	if got := modeOf("new"); got != 0o775|os.ModeSetgid {
		t.Errorf("the new folder has the mode %v", got)
	}
}
