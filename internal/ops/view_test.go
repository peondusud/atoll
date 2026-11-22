package ops

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/fsx"
)

// What the view does on a disk on a user's behalf (specs.md §21.2): each folder missing on the way made as the view
// presents it, the mode asked for whatever the umask, nothing replaced, a folder holding something never removed, a
// regular file never removed, and the folders' times left as the change made them, as a user's change would, where
// Atoll's own operations give them back. On ext4, and with the rename that ntfs-3g refuses.
func TestUserOperations(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(call, name string) error
	}{
		{"ext4", nil},
		{"ntfs-3g", asNTFS3G},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mount := t.TempDir()
			media := filepath.Join(mount, "media")
			mustMkdir(t, media)
			d, err := Open(&onDisk{fail: tc.fail}, &config.Disk{ID: "t1", Mount: mount, Root: "media", Workdir: "Atoll"}, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
			asked := map[string]bool{}
			like := func(folder string) (*fsx.Stat, error) {
				asked[folder] = true
				return &fsx.Stat{Mode: 0o750, Uid: uid, Gid: gid}, nil
			}
			old := time.Unix(1000, 0)
			mustTime(t, media, old)

			w, st, err := d.CreateUserFile("Series/S01/e01.mkv", 0o640, uid, gid, like)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write([]byte("episode")); err != nil {
				t.Fatal(err)
			}
			_ = w.Close()
			if st.Mode != 0o640 || st.Kind != fsx.KindFile {
				t.Errorf("the file made: %+v", st)
			}
			if !asked["Series"] || !asked["Series/S01"] || len(asked) != 2 {
				t.Errorf("the folders asked for: %v", asked)
			}
			if info := mustStat(t, filepath.Join(media, "Series")); info.Mode().Perm() != 0o750 {
				t.Errorf("a folder made on the way: %v", info.Mode())
			}
			if info := mustStat(t, media); !info.ModTime().After(old) {
				t.Errorf("the root's time was given back: %v", info.ModTime())
			}
			if _, _, err := d.CreateUserFile("Series/S01/e01.mkv", 0o640, uid, gid, like); !errors.Is(err, ErrPathTaken) {
				t.Errorf("made again: %v", err)
			}

			w, _, err = d.OpenUserFile("Series/S01/e01.mkv")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.WriteAt([]byte("EPI"), 0); err != nil {
				t.Fatal(err)
			}
			_ = w.Close()
			if data := mustRead(t, filepath.Join(media, "Series", "S01", "e01.mkv")); data != "EPIsode" {
				t.Errorf("written in place: %q", data)
			}

			if _, err := d.MakeUserFolder("Series/S02", 0o700, uid, gid, like); err != nil {
				t.Fatal(err)
			}
			if _, err := d.MakeUserSymlink("Series/latest", "S01/e01.mkv", uid, gid, like); err != nil {
				t.Fatal(err)
			}
			if target, err := os.Readlink(filepath.Join(media, "Series", "latest")); err != nil || target != "S01/e01.mkv" {
				t.Errorf("the symlink: %q, %v", target, err)
			}

			key := keyOfPath(t, filepath.Join(media, "Series", "S01", "e01.mkv"))
			mustTime(t, filepath.Join(media, "Series", "S01"), old)
			if err := d.RenameUser("Series/S01/e01.mkv", "Films/Moved/e01.mkv", false, key, like); err != nil {
				t.Fatal(err)
			}
			if data := mustRead(t, filepath.Join(media, "Films", "Moved", "e01.mkv")); data != "EPIsode" {
				t.Errorf("renamed: %q", data)
			}
			if info := mustStat(t, filepath.Join(media, "Series", "S01")); !info.ModTime().After(old) {
				t.Errorf("the folder it left has its time back: %v", info.ModTime())
			}
			if err := d.RenameUser("Films/Moved/e01.mkv", "Series/S01/e01.mkv", false, "999999999", like); !errors.Is(err, ErrGone) {
				t.Errorf("another file at the name: %v", err)
			}
			if err := d.RenameUser("Films/Moved", "Series/S02", true, keyOfPath(t, filepath.Join(media, "Films", "Moved")), like); !errors.Is(err, ErrPathTaken) {
				t.Errorf("a folder renamed onto one: %v", err)
			}
			if err := d.RenameUser("Films/Moved", "Films/Kept", true, keyOfPath(t, filepath.Join(media, "Films", "Moved")), like); err != nil {
				t.Fatal(err)
			}

			if err := d.RemoveUser("Films/Kept", true); !errors.Is(err, ErrNotEmpty) {
				t.Errorf("a folder that holds a file removed: %v", err)
			}
			if err := d.RemoveUser("Films/Kept/e01.mkv", false); !errors.Is(err, ErrGone) {
				t.Errorf("a regular file removed: %v", err)
			}
			if err := d.RemoveUser("Series/latest", false); err != nil {
				t.Errorf("a symlink: %v", err)
			}
			if err := d.RemoveUser("Series/S02", true); err != nil {
				t.Errorf("an empty folder: %v", err)
			}

			mode, size := uint32(0o600), int64(3)
			mtime := fsx.TimeOf(5_000_000_000)
			st, err = d.SetUserAttrs("Films/Kept/e01.mkv", &UserAttrs{Mode: &mode, Size: &size, Mtime: &mtime})
			if err != nil {
				t.Fatal(err)
			}
			if st.Mode != 0o600 || st.Size != 3 || st.Mtime != mtime {
				t.Errorf("attributes set: %+v", st)
			}
			folderMode := uint32(0o711)
			if st, err = d.SetUserAttrs("Films", &UserAttrs{Mode: &folderMode, Mtime: &mtime}); err != nil || st.Mode != 0o711 || st.Mtime != mtime {
				t.Errorf("a folder's attributes: %+v, %v", st, err)
			}
			if err := d.TouchParent("Films/Kept/e01.mkv", fsx.TimeOf(7_000_000_000)); err != nil {
				t.Fatal(err)
			}
			if info := mustStat(t, filepath.Join(media, "Films", "Kept")); info.ModTime().UnixNano() != 7_000_000_000 {
				t.Errorf("touched: %v", info.ModTime())
			}
		})
	}
}

// A file the view makes is private until it is the caller's: made 0600, given its owner, then its mode, setuid and
// setgid included, as a copy's part is (specs.md §14.2, §21.2; docs/threat-model.md, G13). Made with the mode asked, it
// was for that moment a file of the account atoll, setuid, that another account could open. And one whose owner the
// driver refuses keeps neither bit, which would run it as atoll, as a replica keeps neither then (§14).
func TestAFileTheViewMakesIsPrivateUntilItIsTheCallers(t *testing.T) {
	for _, refused := range []bool{false, true} {
		mount := t.TempDir()
		media := filepath.Join(mount, "media")
		mustMkdir(t, media)
		made := filepath.Join(media, "Series", "e01.mkv")
		seen := map[string]os.FileMode{}
		look := func(call, name string) error {
			if name != "e01.mkv" || (call != "setowner" && call != "setmode") {
				return nil
			}
			if info, err := os.Lstat(made); err == nil {
				seen[call] = info.Mode()
			}
			if call == "setowner" && refused {
				return syscall.EPERM
			}
			return nil
		}
		d, err := Open(&onDisk{fail: look}, &config.Disk{ID: "t1", Mount: mount, Root: "media", Workdir: "Atoll"}, 0)
		if err != nil {
			t.Fatal(err)
		}
		uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
		like := func(string) (*fsx.Stat, error) { return &fsx.Stat{Mode: 0o755, Uid: uid, Gid: gid}, nil }
		w, st, err := d.CreateUserFile("Series/e01.mkv", 0o6755, uid, gid, like)
		if err != nil {
			t.Fatal(err)
		}
		_ = w.Close()
		d.Close()
		for _, call := range []string{"setowner", "setmode"} {
			if got, ok := seen[call]; !ok || got != 0o600 {
				t.Errorf("owner refused %v: before %s the file is %v (%v), want -rw-------", refused, call, got, ok)
			}
		}
		want := os.FileMode(0o755) | os.ModeSetuid | os.ModeSetgid
		if refused {
			want = 0o755
		}
		if info := mustStat(t, made); info.Mode() != want {
			t.Errorf("owner refused %v: the file is %v, want %v", refused, info.Mode(), want)
		}
		if refused && st.Mode&0o6000 != 0 || !refused && st.Mode != 0o6755 {
			t.Errorf("owner refused %v: what it says it made: %o", refused, st.Mode)
		}
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustTime(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func keyOfPath(t *testing.T, path string) string {
	t.Helper()
	dir, err := fsx.OpenDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	st, err := dir.Stat(filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	return keyOf(&st)
}
