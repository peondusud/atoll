package disks

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLookAtRoot uses real folders of one filesystem. Another mount and a folder that answers in
// the other case need a mount: test/loopdev has those.
func TestLookAtRoot(t *testing.T) {
	top := t.TempDir()
	mk := func(name string) {
		t.Helper()
		if err := os.Mkdir(filepath.Join(top, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mk("media")
	mk("elsewhere")
	if err := os.Symlink("elsewhere", filepath.Join(top, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(top, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	mk("both")
	mk("BOTH")
	mk("123")
	for _, tc := range []struct {
		root string
		want FolderAnswer
	}{
		{"media", FolderOK},
		{"absent", FolderAbsent},
		{"linked", FolderNotAFolder},
		{"file", FolderNotAFolder},
		{"both", FolderOK},
		{"123", FolderOK},
		{"file/below", FolderAbsent},
	} {
		got := lookAtRoot(top, tc.root)
		if got.Folder != tc.want {
			t.Errorf("%s: got %s, want %s", tc.root, got.Folder, tc.want)
		}
		if got.OtherCase {
			t.Errorf("%s: answers in the other case, here on a filesystem where a name has one case", tc.root)
		}
	}
	if got := lookAtRoot(filepath.Join(top, "absent"), "media"); got.Folder != FolderIOError {
		t.Errorf("no mount path: got %s: the path that vanished after question 1 says nothing about the root", got.Folder)
	}
}
