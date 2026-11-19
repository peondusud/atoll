package fsx

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A secret file is read with a limit, and gives its owner and its mode as the file it read says them; what is no
// regular file is refused without waiting for a writer, as a FIFO at that path would hold the reader for good.
func TestReadSecretFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "token")
	if err := os.WriteFile(file, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o640); err != nil {
		t.Fatal(err)
	}
	data, owner, err := ReadSecretFile(file, 7)
	if err != nil || string(data) != "s3cret\n" {
		t.Fatalf("%q, %v", data, err)
	}
	if owner.UID != uint32(os.Geteuid()) || owner.GID != uint32(os.Getegid()) || owner.Mode.Perm() != 0o640 { //nolint:gosec // an account's number
		t.Errorf("owner %+v", owner)
	}
	if _, _, err := ReadSecretFile(file, 6); err == nil || !strings.Contains(err.Error(), "more than 6 bytes") {
		t.Errorf("a file past its limit: %v", err)
	}
	if _, _, err := ReadSecretFile(filepath.Join(dir, "none"), 6); err == nil {
		t.Error("a file that is not there was read")
	}
	if _, _, err := ReadSecretFile(dir, 6); err == nil || !strings.Contains(err.Error(), "no regular file") {
		t.Errorf("a folder: %v", err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := ReadSecretFile(fifo, 6)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "no regular file") {
			t.Errorf("a FIFO: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a FIFO held the reader")
	}
	// Through a symbolic link, what is read and told of is the file it leads to.
	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if data, owner, err := ReadSecretFile(link, 64); err != nil || string(data) != "s3cret\n" || owner.Mode.Perm() != 0o640 {
		t.Errorf("through a link: %q, %+v, %v", data, owner, err)
	}
}

// The folders that hold a secret file's name, and the name of each symbolic link on the way to it, with their owners
// and modes: whoever may write one of them may put another file there.
func TestSecretFolders(t *testing.T) {
	top := t.TempDir()
	kept := filepath.Join(top, "kept")
	links := filepath.Join(top, "links")
	for _, d := range []string{kept, links, filepath.Join(top, "up")} {
		if err := os.Mkdir(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(links, 0o770); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(kept, "token")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths := func(list []Folder) []string {
		var out []string
		for _, f := range list {
			out = append(out, f.Path)
		}
		return out
	}
	list, err := SecretFolders(file)
	if err != nil || !slices.Equal(paths(list), []string{kept}) || list[0].Mode.Perm() != 0o750 || list[0].UID != uint32(os.Geteuid()) { //nolint:gosec // an account's number
		t.Errorf("a file: %+v, %v", list, err)
	}
	// A link to it, by a relative path, in a folder of its own, and a folder reached through a link: the folders of
	// each link's name come first, in the order the path goes through them, then the file's.
	if err := os.Symlink("../kept/token", filepath.Join(links, "token")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(links, filepath.Join(top, "up", "via")); err != nil {
		t.Fatal(err)
	}
	list, err = SecretFolders(filepath.Join(top, "up", "via", "token"))
	want := []string{filepath.Join(top, "up"), links, kept}
	if err != nil || !slices.Equal(paths(list), want) || list[1].Mode.Perm() != 0o770 {
		t.Errorf("through links: %q, %+v, %v; want %q", paths(list), list, err, want)
	}
	if _, err := SecretFolders("token"); err == nil {
		t.Error("a relative path was looked at")
	}
	if _, err := SecretFolders(filepath.Join(top, "none", "token")); err == nil {
		t.Error("a path that is not there was looked at")
	}
	loop := filepath.Join(top, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	if _, err := SecretFolders(loop); err == nil {
		t.Error("a link to itself was followed for good")
	}
}
